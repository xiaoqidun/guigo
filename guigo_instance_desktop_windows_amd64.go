// Copyright 2026 肖其顿 (XIAO QI DUN)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package guigo

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// instanceActiveProperty 保存窗口最近一次激活的系统时间
const instanceActiveProperty = "GUIGo.Instance.Active"

// windowInstanceSequence 区分同一进程创建的不同窗口入口
var windowInstanceSequence atomic.Uint32

// instanceWindowSearches 保持同步枚举期间的查询对象，避免回调转换Go指针
var instanceWindowSearches sync.Map

// enumInstanceWindows 接收同步窗口枚举回调并筛选当前桌面的同组窗口
var enumInstanceWindows = syscall.NewCallback(func(hwnd, parameter uintptr) uintptr {
	value, _ := instanceWindowSearches.Load(parameter)
	search := value.(*instanceWindowSearch)
	id, _, _ := user32.NewProc("GetPropW").Call(hwnd, uintptr(unsafe.Pointer(search.property)))
	if id == 0 || !instanceOnCurrentDesktop(search.manager, hwnd) {
		return 1
	}
	var pid uint32
	user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	activated, _, _ := user32.NewProc("GetPropW").Call(hwnd, uintptr(unsafe.Pointer(wide(instanceActiveProperty))))
	search.windows = append(search.windows, instanceWindow{hwnd: hwnd, id: id, address: fmt.Sprintf("%s.%d.%d", search.address, pid, id), activated: activated})
	return 1
})

// windowInstance 将管道入口与实时窗口绑定，不保存启动时的虚拟桌面编号
type windowInstance struct {
	instanceListener
	property string
	id       uintptr
	bound    chan struct{}
	once     sync.Once
	mu       sync.Mutex
	window   *nativeWindow
	closed   bool
}

// instanceWindow 保存候选窗口的入口地址及最近激活时间
type instanceWindow struct {
	hwnd      uintptr
	id        uintptr
	address   string
	activated uintptr
}

// instanceWindowSearch 保存一次窗口枚举的参数和结果
type instanceWindowSearch struct {
	property *uint16
	address  string
	manager  *comObject
	windows  []instanceWindow
}

// openWindowInstance 串行选择当前桌面的接收端，新入口登记窗口后释放选择锁
// 入参: ctx 生命周期上下文, key 调用方实例键, data 消息, receive 接收回调
// 返回: *Instance 新窗口入口或nil, error 创建或转交错误
func openWindowInstance(ctx context.Context, key string, data []byte, receive func(context.Context, []byte) error) (*Instance, error) {
	address, err := instanceAddress(key)
	if err != nil {
		return nil, err
	}
	address += ".window"
	type result struct {
		instance *Instance
		err      error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		request, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		unlock, err := lockWindowInstances(request, address)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer unlock()
		instance, err := selectWindowInstance(ctx, request, address, data, receive)
		done <- result{instance: instance, err: err}
		if instance != nil {
			select {
			case <-instance.listener.(*windowInstance).bound:
			case <-ctx.Done():
			}
		}
	}()
	r := <-done
	return r.instance, r.err
}

// lockWindowInstances 在当前线程取得同组选择锁，异常退出由系统释放
// 入参: ctx 等待上下文, address 同组入口前缀
// 返回: func() 同线程释放操作, error 加锁错误
func lockWindowInstances(ctx context.Context, address string) (func(), error) {
	identity, err := windowsInstanceIdentity(windows.GetCurrentProcessId())
	if err != nil {
		return nil, err
	}
	sid, _, _ := strings.Cut(identity, ":")
	security, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: security}
	name, _ := windows.UTF16PtrFromString(`Local\` + strings.TrimPrefix(address, `\\.\pipe\`))
	mutex, err := windows.CreateMutex(&attributes, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			windows.CloseHandle(mutex)
			return nil, err
		}
		state, err := windows.WaitForSingleObject(mutex, 50)
		if err != nil {
			windows.CloseHandle(mutex)
			return nil, err
		}
		if state == windows.WAIT_OBJECT_0 || state == windows.WAIT_ABANDONED {
			return func() { windows.ReleaseMutex(mutex); windows.CloseHandle(mutex) }, nil
		}
	}
}

// selectWindowInstance 按实际桌面和最近激活时间转交，接收端迁移后重新选择
// 入参: ctx 生命周期上下文, request 转交上下文, address 同组入口前缀, data 消息, receive 接收回调
// 返回: *Instance 新入口或nil, error 创建或转交错误
func selectWindowInstance(ctx, request context.Context, address string, data []byte, receive func(context.Context, []byte) error) (*Instance, error) {
	hr, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 2)
	if err := hresult(hr); err != nil {
		return nil, err
	}
	defer ole32.NewProc("CoUninitialize").Call()
	manager := instanceDesktopManager()
	defer release(manager)
	property := strings.TrimPrefix(address, `\\.\pipe\`)
	for {
		if err := request.Err(); err != nil {
			return nil, err
		}
		search := instanceWindowSearch{property: wide(property), address: address, manager: manager}
		parameter := uintptr(unsafe.Pointer(&search))
		instanceWindowSearches.Store(parameter, &search)
		user32.NewProc("EnumWindows").Call(enumInstanceWindows, parameter)
		instanceWindowSearches.Delete(parameter)
		slices.SortStableFunc(search.windows, func(a, b instanceWindow) int {
			if a.activated > b.activated {
				return -1
			}
			if a.activated < b.activated {
				return 1
			}
			return 0
		})
		retry := false
		for _, candidate := range search.windows {
			conn, err := dialWindowInstance(request, candidate, search.property, manager)
			if err != nil {
				if !errors.Is(err, errInstanceWindow) {
					return nil, err
				}
				retry = true
				break
			}
			err = forwardInstance(request, conn, data)
			if errors.Is(err, errInstanceWindow) {
				retry = true
				break
			}
			if errors.Is(err, ErrClosed) {
				continue
			}
			return nil, err
		}
		if retry {
			continue
		}
		return newWindowInstance(ctx, address, receive)
	}
}

// newWindowInstance 创建独立窗口入口，并在接收消息前复核窗口所属桌面
// 入参: ctx 生命周期上下文, address 同组入口前缀, receive 接收回调
// 返回: *Instance 新入口, error 创建错误
func newWindowInstance(ctx context.Context, address string, receive func(context.Context, []byte) error) (*Instance, error) {
	id := uintptr(windowInstanceSequence.Add(1))
	listener, err := listenInstance(fmt.Sprintf("%s.%d.%d", address, windows.GetCurrentProcessId(), id))
	if err != nil {
		return nil, err
	}
	if listener == nil {
		return nil, errors.New("instance window entry already exists")
	}
	entry := &windowInstance{instanceListener: listener, property: strings.TrimPrefix(address, `\\.\pipe\`), id: id, bound: make(chan struct{})}
	return startInstance(ctx, entry, func(ctx context.Context, data []byte) error {
		entry.mu.Lock()
		window, closed := entry.window, entry.closed
		entry.mu.Unlock()
		if closed || window == nil {
			return ErrClosed
		}
		var current bool
		if err := window.invoke(func() error {
			manager := instanceDesktopManager()
			defer release(manager)
			current = instanceOnCurrentDesktop(manager, window.hwnd)
			return nil
		}); err != nil {
			return err
		}
		if !current {
			return errInstanceWindow
		}
		return receive(ctx, data)
	}), nil
}

// dialWindowInstance 等待已登记入口就绪，窗口退出或移桌面时重新选择
// 入参: ctx 请求上下文, candidate 候选窗口, property 同组窗口属性, manager 桌面查询接口
// 返回: instanceConn 连接, error 连接或重新选择原因
func dialWindowInstance(ctx context.Context, candidate instanceWindow, property *uint16, manager *comObject) (instanceConn, error) {
	for {
		id, _, _ := user32.NewProc("GetPropW").Call(candidate.hwnd, uintptr(unsafe.Pointer(property)))
		if id != candidate.id || !instanceOnCurrentDesktop(manager, candidate.hwnd) {
			return nil, errInstanceWindow
		}
		conn, err := dialInstance(ctx, candidate.address)
		if err == nil || !instanceUnavailable(err) {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// instanceDesktopManager 创建公开的虚拟桌面查询接口，系统不支持时返回nil
// 返回: *comObject 查询接口或nil
func instanceDesktopManager() *comObject {
	class := guid("{AA509086-5CA9-4C25-8F95-589D3C07B48A}")
	iid := guid("{A5CD92FF-29BE-454C-8D04-D82879FB3F1B}")
	var manager *comObject
	hr, _, _ := ole32.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&class)), 0, 1, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&manager)))
	if hresult(hr) != nil {
		return nil
	}
	return manager
}

// instanceOnCurrentDesktop 查询窗口是否实际属于当前虚拟桌面，无法确认时不复用
// 入参: manager 查询接口, hwnd 窗口句柄
// 返回: bool 是否属于当前桌面
func instanceOnCurrentDesktop(manager *comObject, hwnd uintptr) bool {
	if manager == nil {
		return false
	}
	var current int32
	return hresult(com(manager, 3, hwnd, uintptr(unsafe.Pointer(&current)))) == nil && current != 0
}

// recordInstanceActivation 更新窗口最近激活时间，不改变窗口位置和桌面
// 入参: hwnd 窗口句柄
func recordInstanceActivation(hwnd uintptr) {
	now, _, _ := kernel32.NewProc("GetTickCount64").Call()
	user32.NewProc("SetPropW").Call(hwnd, uintptr(unsafe.Pointer(wide(instanceActiveProperty))), max(now, 1))
}

// attachWindow 登记同组窗口并结束启动等待，在窗口线程执行
// 入参: window 原生窗口
// 返回: error 登记错误
func (l *windowInstance) attachWindow(window *nativeWindow) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if l.window != nil {
		return errors.New("instance window already attached")
	}
	ok, _, err := user32.NewProc("SetPropW").Call(window.hwnd, uintptr(unsafe.Pointer(wide(l.property))), l.id)
	if ok == 0 {
		return err
	}
	l.window = window
	recordInstanceActivation(window.hwnd)
	l.once.Do(func() { close(l.bound) })
	return nil
}

// Close 移除窗口登记并关闭入口，释放尚未完成的启动等待
// 返回: error 关闭错误
func (l *windowInstance) Close() error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		if l.window != nil {
			hwnd := l.window.hwnd
			property := wide(l.property)
			id, _, _ := user32.NewProc("GetPropW").Call(hwnd, uintptr(unsafe.Pointer(property)))
			if id == l.id {
				user32.NewProc("RemovePropW").Call(hwnd, uintptr(unsafe.Pointer(property)))
				user32.NewProc("RemovePropW").Call(hwnd, uintptr(unsafe.Pointer(wide(instanceActiveProperty))))
			}
		}
	}
	l.mu.Unlock()
	l.once.Do(func() { close(l.bound) })
	return l.instanceListener.Close()
}
