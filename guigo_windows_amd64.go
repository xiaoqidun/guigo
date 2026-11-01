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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// resourceOrigin 为WebView2拦截的内部资源来源
const resourceOrigin = "https://guigo.localhost"

var (
	user32        = syscall.NewLazyDLL("user32.dll")
	ole32         = syscall.NewLazyDLL("ole32.dll")
	shell32       = syscall.NewLazyDLL("shell32.dll")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	active        atomic.Pointer[nativeWindow]
	wndCallback   = syscall.NewCallback(windowProc)
	callbackTable = [4]uintptr{
		syscall.NewCallback(callbackQuery),
		syscall.NewCallback(callbackAdd),
		syscall.NewCallback(callbackRelease),
		syscall.NewCallback(callbackInvoke),
	}
	callbackRoots sync.Map
)

// rect 保存Win32客户区矩形
type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

// point 保存Win32坐标
type point struct {
	X int32
	Y int32
}

// monitorInfo 保存显示器范围和工作区
type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

// message 对应Win32 MSG
type message struct {
	Window  uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   point
	Private uint32
}

// windowClass 对应WNDCLASSEXW
type windowClass struct {
	Size        uint32
	Style       uint32
	Proc        uintptr
	ClassExtra  int32
	WindowExtra int32
	Instance    uintptr
	Icon        uintptr
	Cursor      uintptr
	Background  uintptr
	Menu        *uint16
	Name        *uint16
	SmallIcon   uintptr
}

// comCallback 保存COM回调与引用计数
type comCallback struct {
	Table *[4]uintptr
	Refs  atomic.Int32
	IID   syscall.GUID
	Fn    func(uintptr, *comObject) uintptr
}

// comObject 保存COM接口的虚函数表指针
type comObject struct {
	table *uintptr
}

// nativeWindow 保存STA线程拥有的原生对象
type nativeWindow struct {
	options         Options
	mu              sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	queue           []queuedCall
	topQueue        []func()
	hwnd            uintptr
	contentWindow   uintptr
	dragWindow      uintptr
	dragRegions     []Rect
	titleBarState   TitleBarInfo
	titleBarDPI     uint32
	titleBarEvents  chan TitleBarInfo
	titleBarReady   bool
	titleBarPending bool
	titleBarLayout  bool
	dpi             uint32
	iconData        []byte
	icons           [2]uintptr
	environment     *comObject
	controller      *comObject
	webview         *comObject
	activeDialog    *comObject
	printActive     bool
	callbacks       []*comCallback
	generation      uint64
	pageCtx         context.Context
	pageCancel      context.CancelFunc
	calls           *callQueue
	pendingEvents   []string
	resources       map[*comObject]resourceRequest
	temporary       temporaryDirectory
	initializing    bool
	ready           bool
	shown           bool
	closing         bool
	closePending    bool
	closeRequested  bool
	closed          bool
	err             error
}

// queuedCall 保存主线程操作及关闭时的资源回收操作
type queuedCall struct {
	run     func()
	discard func()
}

// wide 编码已校验的系统字符串
// 入参: s 字符串
// 返回: *uint16 UTF16字符串
func wide(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(strings.ReplaceAll(s, "\x00", ""))
	return p
}

// com 调用COM虚函数
// 入参: object 接口对象, index 方法索引, args 方法参数
// 返回: uintptr 调用结果
//
//go:uintptrescapes
func com(object *comObject, index int, args ...uintptr) uintptr {
	fn := *(*uintptr)(unsafe.Add(unsafe.Pointer(object.table), uintptr(index)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(unsafe.Pointer(object))}, args...)...)
	runtime.KeepAlive(object)
	return r
}

// hresult 检查HRESULT失败位
// 入参: hr 原生返回值
// 返回: error 调用错误
func hresult(hr uintptr) error {
	if int32(hr) < 0 {
		return fmt.Errorf("HRESULT 0x%08X", uint32(hr))
	}
	return nil
}

// release 释放COM引用
// 入参: p 接口对象
func release(p *comObject) {
	if p != nil {
		com(p, 2)
	}
}

// comString 读取并释放COM分配的字符串
// 入参: object 接口对象, index 方法索引
// 返回: string 字符串, error 调用错误
func comString(object *comObject, index int) (string, error) {
	var p *uint16
	if err := hresult(com(object, index, uintptr(unsafe.Pointer(&p)))); err != nil {
		return "", err
	}
	if p == nil {
		return "", nil
	}
	defer ole32.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(p)))
	return utf16String(p), nil
}

// utf16String 读取零结尾UTF16字符串
// 入参: p 字符串指针
// 返回: string 字符串
func utf16String(p *uint16) string {
	if p == nil {
		return ""
	}
	var b []uint16
	for i := uintptr(0); ; i++ {
		c := *(*uint16)(unsafe.Add(unsafe.Pointer(p), i*2))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return syscall.UTF16ToString(b)
}

// callbackQuery 提供IUnknown和指定回调接口
// 入参: c 回调对象, iid 接口标识, out 接口输出
// 返回: uintptr HRESULT结果
func callbackQuery(c *comCallback, iid *syscall.GUID, out **comCallback) uintptr {
	if out == nil {
		return 0x80004003
	}
	*out = nil
	g := *iid
	unknown := syscall.GUID{Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
	if g != unknown && g != c.IID {
		return 0x80004002
	}
	*out = c
	c.Refs.Add(1)
	return 0
}

// callbackAdd 增加回调引用
// 入参: c 回调对象
// 返回: uintptr 引用计数
func callbackAdd(c *comCallback) uintptr {
	return uintptr(c.Refs.Add(1))
}

// callbackRelease 释放回调引用与全局根
// 入参: c 回调对象
// 返回: uintptr 引用计数
func callbackRelease(c *comCallback) uintptr {
	n := c.Refs.Add(-1)
	if n == 0 {
		callbackRoots.Delete(uintptr(unsafe.Pointer(c)))
	}
	return uintptr(n)
}

// callbackInvoke 调度回调并隔离异常
// 入参: c 回调对象, a 数值参数, b 对象参数
// 返回: uintptr HRESULT结果
func callbackInvoke(c *comCallback, a uintptr, b *comObject) (result uintptr) {
	defer func() {
		if recover() != nil {
			result = 0x80004005
		}
	}()
	return c.Fn(a, b)
}

// callback 注册具有确定接口标识的回调
// 入参: iid 接口标识, fn 回调函数
// 返回: uintptr 回调地址
func (n *nativeWindow) callback(iid string, fn func(uintptr, *comObject) uintptr) uintptr {
	var g syscall.GUID
	ole32.NewProc("CLSIDFromString").Call(uintptr(unsafe.Pointer(wide(iid))), uintptr(unsafe.Pointer(&g)))
	c := &comCallback{Table: &callbackTable, IID: g, Fn: fn}
	c.Refs.Store(1)
	n.callbacks = append(n.callbacks, c)
	p := uintptr(unsafe.Pointer(c))
	callbackRoots.Store(p, c)
	return p
}

// run 创建窗口和WebView2并处理原生消息
// 入参: parent 运行上下文, o 宿主选项
// 返回: error 启动或运行错误
func run(parent context.Context, o Options) error {
	n := &nativeWindow{options: o, done: make(chan struct{})}
	if !active.CompareAndSwap(nil, n) {
		return errors.New("another Run is active")
	}
	defer func() {
		if n.hwnd != 0 && o.OnClose != nil {
			o.OnClose()
		}
	}()
	defer active.Store(nil)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if proc := user32.NewProc("SetThreadDpiAwarenessContext"); proc.Find() == nil {
		old, _, err := proc.Call(^uintptr(3))
		if old == 0 {
			return fmt.Errorf("set DPI awareness: %w", err)
		}
		defer proc.Call(old)
	}
	n.dpi = 96
	if proc := user32.NewProc("GetDpiForSystem"); proc.Find() == nil {
		dpi, _, _ := proc.Call()
		if dpi != 0 {
			n.dpi = uint32(dpi)
		}
	}
	n.ctx, n.cancel = context.WithCancel(parent)
	defer n.temporary.close()
	defer n.cancel()
	if o.OnTitleBarChanged != nil {
		n.titleBarEvents = make(chan TitleBarInfo, 1)
		go n.deliverTitleBarChanges()
	}
	hr, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 2)
	if err := hresult(hr); err != nil {
		return err
	}
	defer func() {
		ole32.NewProc("CoUninitialize").Call()
		releaseRuntimes()
	}()
	if err := parent.Err(); err != nil {
		return err
	}
	dll, runtimeType, err := loadRuntime(o.LibraryPath)
	if err != nil {
		return err
	}
	create, err := dll.FindProc("CreateWebViewEnvironmentWithOptionsInternal")
	if err != nil {
		return err
	}
	if o.UserDataDir == "" {
		if o.AppID == "" {
			return errors.New("AppID or UserDataDir is required")
		}
		o.UserDataDir, err = DefaultUserDataDir(o.AppID)
		if err != nil {
			return err
		}
	}
	o.UserDataDir, err = filepath.Abs(o.UserDataDir)
	if err != nil {
		return err
	}
	n.options.UserDataDir = o.UserDataDir
	n.iconData, err = iconData(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.UserDataDir, 0700); err != nil {
		return err
	}
	defer func() { destroyIcons(n.icons) }()
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	cursor, _, _ := user32.NewProc("LoadCursorW").Call(0, 32512)
	class := windowClass{Style: 8, Proc: wndCallback, Instance: instance, Cursor: cursor, Name: wide("GUIGo.Native.Window")}
	if o.TitleBar == TitleBarContent {
		class.Background, _, _ = gdi32.NewProc("GetStockObject").Call(4)
	}
	class.Size = uint32(unsafe.Sizeof(class))
	atom, _, e := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class)))
	if atom == 0 && e != syscall.Errno(1410) {
		return e
	}
	defer user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(class.Name)), instance)
	style := n.style()
	r := rect{Right: n.pixels(o.Width), Bottom: n.pixels(o.Height)}
	if err := n.adjustRect(&r); err != nil {
		return err
	}
	n.hwnd, _, e = user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class.Name)), uintptr(unsafe.Pointer(wide(o.Title))), style, 0x80000000, 0x80000000, uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 0, 0, instance, 0)
	if n.hwnd == 0 {
		return e
	}
	defer func() {
		n.cancel()
		var browser uintptr
		if n.webview != nil {
			var pid uint32
			if hresult(com(n.webview, 37, uintptr(unsafe.Pointer(&pid)))) == nil {
				browser, _, _ = kernel32.NewProc("OpenProcess").Call(0x100000, 0, uintptr(pid))
			}
		}
		n.mu.Lock()
		n.closed = true
		close(n.done)
		queue := n.queue
		n.queue = nil
		n.topQueue = nil
		n.mu.Unlock()
		for _, call := range queue {
			if call.discard != nil {
				call.discard()
			}
		}
		for args := range n.resources {
			n.finishResource(args)
		}
		if n.controller != nil {
			com(n.controller, 24)
		}
		release(n.webview)
		release(n.controller)
		release(n.environment)
		n.webview = nil
		n.controller = nil
		n.environment = nil
		if o.AppID != "" {
			_ = n.setAppID("")
		}
		user32.NewProc("DestroyWindow").Call(n.hwnd)
		n.unregisterTitleBar()
		if browser != 0 {
			waitBrowserExit(browser)
			kernel32.NewProc("CloseHandle").Call(browser)
		}
		for _, c := range n.callbacks {
			callbackRelease(c)
		}
	}()
	if o.AppID != "" {
		if err := n.setAppID(o.AppID); err != nil {
			return err
		}
	}
	if o.Instance != nil {
		if entry, ok := o.Instance.listener.(*windowInstance); ok {
			if err := entry.attachWindow(n); err != nil {
				return err
			}
			defer entry.Close()
		}
	}
	if err := n.initializeDPI(); err != nil {
		return err
	}
	if err := n.initializeTitleBar(); err != nil {
		return err
	}
	if err := n.updateIcons(); err != nil {
		return err
	}
	shell32.NewProc("DragAcceptFiles").Call(n.hwnd, 1)
	envCB := n.callback("{4E8A3389-C9D8-4BD2-B6B5-124FEE6CC14D}", func(hr uintptr, env *comObject) uintptr {
		if n.ctx.Err() != nil {
			n.initializing = false
			n.fail(n.ctx.Err())
			return 0
		}
		if err := hresult(hr); err != nil {
			n.initializing = false
			n.fail(err)
			return 0
		}
		n.environment = env
		com(env, 1)
		version, err := comString(env, 5)
		if err == nil {
			err = checkRuntimeVersion(version)
		}
		if err != nil {
			n.fail(err)
			return 0
		}
		cb := n.callback("{6C4819F3-C9B7-4260-8127-C9F5BDE7F68C}", func(hr uintptr, controller *comObject) uintptr {
			n.initializing = false
			if n.ctx.Err() != nil {
				n.fail(n.ctx.Err())
				return 0
			}
			if err := hresult(hr); err != nil {
				n.fail(err)
				return 0
			}
			n.controller = controller
			com(controller, 1)
			if err := hresult(com(controller, 25, uintptr(unsafe.Pointer(&n.webview)))); err != nil {
				n.fail(err)
				return 0
			}
			if err := n.setup(); err != nil {
				n.fail(err)
			}
			return 0
		})
		parent := n.hwnd
		if n.contentWindow != 0 {
			parent = n.contentWindow
		}
		if err := hresult(com(env, 3, parent, cb)); err != nil {
			n.fail(err)
		}
		return 0
	})
	n.initializing = true
	hr, _, _ = create.Call(1, runtimeType, uintptr(unsafe.Pointer(wide(o.UserDataDir))), 0, envCB)
	if err := hresult(hr); err != nil {
		return fmt.Errorf("create WebView2 (install Microsoft WebView2 Runtime): %w", err)
	}
	go func() {
		select {
		case <-n.ctx.Done():
			_ = n.close()
		case <-n.done:
		}
	}()
	var msg message
	for {
		n.drainTop()
		r, _, err := user32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) == -1 {
			return err
		}
		if r == 0 {
			break
		}
		if msg.Message == 0x8002 {
			continue
		}
		user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
		user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
	}
	if n.err != nil {
		return n.err
	}
	return parent.Err()
}

// activate 恢复最小化窗口并请求前台激活
// 返回: error 请求投递错误
func (n *nativeWindow) activate() error {
	return n.enqueue(func() {
		iconic, _, _ := user32.NewProc("IsIconic").Call(n.hwnd)
		if iconic != 0 {
			user32.NewProc("ShowWindow").Call(n.hwnd, 9)
		} else if n.shown {
			user32.NewProc("ShowWindow").Call(n.hwnd, 5)
		}
		user32.NewProc("SetForegroundWindow").Call(n.hwnd)
	})
}

// setAppID 设置窗口应用标识，空字符串移除窗口属性
// 入参: appID 应用标识
// 返回: error 设置错误
func (n *nativeWindow) setAppID(appID string) error {
	iid := guid("{886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99}")
	var store *comObject
	hr, _, _ := shell32.NewProc("SHGetPropertyStoreForWindow").Call(n.hwnd, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&store)))
	if err := hresult(hr); err != nil {
		return err
	}
	defer release(store)
	key := struct {
		Format syscall.GUID
		ID     uint32
	}{guid("{9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3}"), 5}
	value := struct {
		Type     uint16
		Reserved [3]uint16
		Text     *uint16
		Padding  uintptr
	}{}
	if appID != "" {
		value.Type = 31
		value.Text = wide(appID)
	}
	return hresult(com(store, 6, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value))))
}

// drainTop 仅在最外层消息循环运行模态操作，避免重入WebView2回调
func (n *nativeWindow) drainTop() {
	n.mu.Lock()
	queue := n.topQueue
	n.topQueue = nil
	n.mu.Unlock()
	for _, fn := range queue {
		fn()
	}
}

// invokeTop 等待最外层消息循环执行操作，不在WndProc或COM回调栈显示模态窗口
// 入参: fn 待执行操作
// 返回: error 执行错误
func (n *nativeWindow) invokeTop(fn func() error) error {
	result := make(chan error, 1)
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return ErrClosed
	}
	n.topQueue = append(n.topQueue, func() { result <- fn() })
	ok, _, err := user32.NewProc("PostMessageW").Call(n.hwnd, 0x8002, 0, 0)
	if ok == 0 {
		n.topQueue = n.topQueue[:len(n.topQueue)-1]
		n.mu.Unlock()
		return err
	}
	n.mu.Unlock()
	select {
	case err := <-result:
		return err
	case <-n.done:
		return ErrClosed
	}
}

// waitBrowserExit 在清理阶段继续处理COM消息并等待浏览器释放用户目录
// 入参: process 浏览器进程句柄
func waitBrowserExit(process uintptr) {
	for i := 0; i < 50; i++ {
		status, _, _ := user32.NewProc("MsgWaitForMultipleObjects").Call(1, uintptr(unsafe.Pointer(&process)), 0, 100, 0x4ff)
		if status == 0 {
			return
		}
		var msg message
		for {
			ok, _, _ := user32.NewProc("PeekMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1)
			if ok == 0 {
				break
			}
			if msg.Message != 0x12 {
				user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
				user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
			}
		}
	}
}

// style 返回窗口样式
// 返回: uintptr 窗口样式
func (n *nativeWindow) style() uintptr {
	if n.options.FixedSize {
		return 0x00CA0000
	}
	return 0x00CF0000
}

// fail 保存初始化错误并退出消息循环
// 入参: err 运行错误
func (n *nativeWindow) fail(err error) {
	n.err = err
	user32.NewProc("PostQuitMessage").Call(0)
}

// enqueue 将操作投递到STA线程
// 入参: fn 待执行操作
// 返回: error 投递错误
func (n *nativeWindow) enqueue(fn func()) error {
	return n.enqueueCleanup(fn, nil)
}

// enqueueCleanup 将带有取消清理的操作投递到主线程
// 入参: fn 待执行操作, discard 取消清理操作
// 返回: error 投递错误
func (n *nativeWindow) enqueueCleanup(fn, discard func()) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return ErrClosed
	}
	n.queue = append(n.queue, queuedCall{run: fn, discard: discard})
	r, _, err := user32.NewProc("PostMessageW").Call(n.hwnd, 0x8001, 0, 0)
	if r == 0 {
		n.queue = n.queue[:len(n.queue)-1]
		return err
	}
	return nil
}

// invoke 等待STA操作执行完成或被关闭流程丢弃
// 入参: fn 待执行操作
// 返回: error 执行错误
func (n *nativeWindow) invoke(fn func() error) error {
	result := make(chan error, 1)
	if err := n.enqueueCleanup(func() { result <- fn() }, func() { result <- ErrClosed }); err != nil {
		return err
	}
	return <-result
}

// close 异步请求关闭
// 返回: error 请求错误
func (n *nativeWindow) close() error {
	return n.enqueue(func() { user32.NewProc("PostMessageW").Call(n.hwnd, 0x10, 0, 0) })
}

// emit 投递JSON消息
// 入参: raw 消息文本
// 返回: error 投递错误
func (n *nativeWindow) emit(raw string) error {
	return n.invoke(func() error {
		if n.webview == nil {
			return errors.New("webview not ready")
		}
		if !n.ready {
			n.pendingEvents = append(n.pendingEvents, raw)
			return nil
		}
		return hresult(com(n.webview, 32, uintptr(unsafe.Pointer(wide(raw)))))
	})
}

// setTitle 设置原生标题
// 入参: title 窗口标题
// 返回: error 设置错误
func (n *nativeWindow) setTitle(title string) error {
	return n.invoke(func() error {
		r, _, e := user32.NewProc("SetWindowTextW").Call(n.hwnd, uintptr(unsafe.Pointer(wide(title))))
		if r == 0 {
			return e
		}
		return nil
	})
}

// setSize 调整窗口客户区
// 入参: w 客户区宽度, h 客户区高度
// 返回: error 调整错误
func (n *nativeWindow) setSize(w, h int) error {
	return n.invoke(func() error {
		return n.setClientSize(w, h)
	})
}

// initializeDPI 按实际窗口所在显示器校正初始客户区
// 返回: error 初始化错误
func (n *nativeWindow) initializeDPI() error {
	var monitor uintptr
	if n.options.Center {
		monitor, _, _ = user32.NewProc("MonitorFromWindow").Call(n.hwnd, 2)
		if monitor == 0 {
			return errors.New("cannot determine window monitor")
		}
	}
	proc := user32.NewProc("GetDpiForWindow")
	if err := proc.Find(); err != nil {
		return err
	}
	dpi, _, _ := proc.Call(n.hwnd)
	if dpi == 0 {
		return errors.New("cannot determine window DPI")
	}
	n.dpi = uint32(dpi)
	if err := n.setClientSize(n.options.Width, n.options.Height); err != nil {
		return err
	}
	if n.options.Center {
		return n.center(monitor)
	}
	return nil
}

// center 按指定显示器工作区限制可调整窗口的初始尺寸并将可见边框居中
// 入参: monitor 显示器句柄
// 返回: error 定位错误
func (n *nativeWindow) center(monitor uintptr) error {
	info := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	ok, _, err := user32.NewProc("GetMonitorInfoW").Call(monitor, uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return err
	}
	var bounds rect
	ok, _, err = user32.NewProc("GetWindowRect").Call(n.hwnd, uintptr(unsafe.Pointer(&bounds)))
	if ok == 0 {
		return err
	}
	var frame rect
	hr, _, _ := dwmapi.NewProc("DwmGetWindowAttribute").Call(n.hwnd, 9, uintptr(unsafe.Pointer(&frame)), unsafe.Sizeof(frame))
	if err := hresult(hr); err != nil {
		return err
	}
	border := rect{Left: frame.Left - bounds.Left, Top: frame.Top - bounds.Top, Right: bounds.Right - frame.Right, Bottom: bounds.Bottom - frame.Bottom}
	if !n.options.FixedSize {
		bounds.Right = bounds.Left + min(bounds.Right-bounds.Left, info.Work.Right-info.Work.Left)
		bounds.Bottom = bounds.Top + min(bounds.Bottom-bounds.Top, info.Work.Bottom-info.Work.Top)
	}
	frame = rect{Right: bounds.Right - bounds.Left - border.Left - border.Right, Bottom: bounds.Bottom - bounds.Top - border.Top - border.Bottom}
	position := centerPosition(frame, info.Work)
	position.X -= border.Left
	position.Y -= border.Top
	ok, _, err = user32.NewProc("SetWindowPos").Call(n.hwnd, 0, uintptr(position.X), uintptr(position.Y), uintptr(bounds.Right-bounds.Left), uintptr(bounds.Bottom-bounds.Top), 0x14)
	if ok == 0 {
		return err
	}
	return nil
}

// centerPosition 计算工作区内的可见窗口位置，保留负坐标显示器原点
// 入参: bounds 可见边框, work 显示器工作区
// 返回: point 窗口左上角坐标
func centerPosition(bounds, work rect) point {
	return point{
		X: work.Left + max(0, (work.Right-work.Left-(bounds.Right-bounds.Left))/2),
		Y: work.Top + max(0, (work.Bottom-work.Top-(bounds.Bottom-bounds.Top))/2),
	}
}

// setClientSize 在宿主线程应用逻辑客户区尺寸及约束
// 入参: w 客户区宽度, h 客户区高度
// 返回: error 调整错误
func (n *nativeWindow) setClientSize(w, h int) error {
	o := n.options
	o.Width = w
	o.Height = h
	if w <= 0 || h <= 0 {
		return errors.New("invalid size")
	}
	if err := validate(&o); err != nil {
		return err
	}
	r := rect{Right: n.pixels(o.Width), Bottom: n.pixels(o.Height)}
	if err := n.adjustRect(&r); err != nil {
		return err
	}
	previousWidth, previousHeight := n.options.Width, n.options.Height
	n.options.Width, n.options.Height = o.Width, o.Height
	ok, _, e := user32.NewProc("SetWindowPos").Call(n.hwnd, 0, 0, 0, uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 0x16)
	if ok == 0 {
		n.options.Width, n.options.Height = previousWidth, previousHeight
		return e
	}
	return nil
}

// resize 更新WebView客户区
func (n *nativeWindow) resize() {
	if n.contentWindow != 0 {
		if err := n.layoutTitleBar(); err != nil {
			n.fail(err)
		}
	}
	if n.controller != nil {
		var r rect
		hwnd := n.hwnd
		if n.contentWindow != 0 {
			hwnd = n.contentWindow
		}
		user32.NewProc("GetClientRect").Call(hwnd, uintptr(unsafe.Pointer(&r)))
		com(n.controller, 6, uintptr(unsafe.Pointer(&r)))
	}
	n.scheduleTitleBarChange()
}

// windowProc 处理Win32消息
// 入参: hwnd 窗口句柄, msg 消息类型, wparam 消息参数, lparam 消息参数
// 返回: uintptr 消息处理结果
func windowProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	n := active.Load()
	if n != nil {
		if result, handled := n.titleBarProc(hwnd, msg, wparam, lparam); handled {
			return result
		}
		switch msg {
		case 0x14:
			if n.options.WindowMaterial != WindowSolid {
				var bounds rect
				user32.NewProc("GetClientRect").Call(hwnd, uintptr(unsafe.Pointer(&bounds)))
				brush, _, _ := gdi32.NewProc("GetStockObject").Call(4)
				user32.NewProc("FillRect").Call(wparam, uintptr(unsafe.Pointer(&bounds)), brush)
				return 1
			}
		case 0x8001:
			n.mu.Lock()
			q := n.queue
			n.queue = nil
			n.mu.Unlock()
			for _, fn := range q {
				fn.run()
			}
			return 0
		case 5:
			n.resize()
			return 0
		case 3:
			if n.controller != nil {
				com(n.controller, 23)
			}
		case 6:
			if wparam&0xffff != 0 && n.options.Instance != nil {
				recordInstanceActivation(hwnd)
			}
			n.scheduleTitleBarChange()
		case 0x10:
			n.requestClose()
			return 0
		case 0x2e0:
			n.dpi = uint32(wparam & 0xffff)
			r := *(**rect)(unsafe.Pointer(&lparam))
			user32.NewProc("SetWindowPos").Call(hwnd, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 0x14)
			if err := n.updateIcons(); err != nil && n.options.OnError != nil {
				go n.options.OnError(err)
			}
			n.resize()
			return 0
		case 0x24:
			info := *(**[5]point)(unsafe.Pointer(&lparam))
			o := n.options
			minw, minh, maxw, maxh := o.MinWidth, o.MinHeight, o.MaxWidth, o.MaxHeight
			if o.FixedSize {
				minw = o.Width
				maxw = o.Width
				minh = o.Height
				maxh = o.Height
			}
			r := rect{}
			if err := n.adjustRect(&r); err != nil {
				n.fail(err)
				return 0
			}
			dx, dy := r.Right-r.Left, r.Bottom-r.Top
			if minw > 0 {
				info[3].X = n.pixels(minw) + dx
			}
			if minh > 0 {
				info[3].Y = n.pixels(minh) + dy
			}
			if maxw > 0 {
				info[4].X = n.pixels(maxw) + dx
				if info[1].X > info[4].X {
					info[1].X = info[4].X
				}
			}
			if maxh > 0 {
				info[4].Y = n.pixels(maxh) + dy
				if info[1].Y > info[4].Y {
					info[1].Y = info[4].Y
				}
			}
			return 0
		case 0x233:
			count, _, _ := shell32.NewProc("DragQueryFileW").Call(wparam, 0xffffffff, 0, 0)
			paths := make([]string, 0, count)
			for i := uintptr(0); i < count; i++ {
				size, _, _ := shell32.NewProc("DragQueryFileW").Call(wparam, i, 0, 0)
				b := make([]uint16, size+1)
				shell32.NewProc("DragQueryFileW").Call(wparam, i, uintptr(unsafe.Pointer(&b[0])), size+1)
				paths = append(paths, syscall.UTF16ToString(b))
			}
			shell32.NewProc("DragFinish").Call(wparam)
			if n.options.OnDrop != nil {
				go n.options.OnDrop(paths)
			}
			return 0
		}
	}
	r, _, _ := user32.NewProc("DefWindowProcW").Call(hwnd, msg, wparam, lparam)
	return r
}

// setup 注册资源与桥接并等待注入完成后导航
// 返回: error 初始化错误
func (n *nativeWindow) setup() error {
	if err := n.initializeTitleBarZoom(); err != nil {
		return err
	}
	if err := n.initializeWindowMaterial(); err != nil {
		return err
	}
	n.resize()
	if err := hresult(com(n.controller, 4, 1)); err != nil {
		return err
	}
	var token int64
	nav := n.callback("{9ADBE429-F36D-432B-9DDC-F8881FBD76E3}", func(_ uintptr, args *comObject) uintptr {
		uri, err := comString(args, 3)
		if err != nil || !localURL(uri) {
			com(args, 8, 1)
			if err == nil && externalURL(uri) {
				n.external(uri)
			}
		} else {
			n.generation++
			if n.pageCancel != nil {
				n.pageCancel()
			}
			n.pageCtx, n.pageCancel = context.WithCancel(n.ctx)
			n.calls = newCallQueue(n.pageCtx)
			n.ready = false
		}
		return 0
	})
	if err := hresult(com(n.webview, 7, nav, uintptr(unsafe.Pointer(&token)))); err != nil {
		return err
	}
	popup := n.callback("{D4C185FE-C81C-4989-97AF-2D3FA7AB5651}", func(_ uintptr, args *comObject) uintptr {
		com(args, 6, 1)
		uri, err := comString(args, 3)
		if err == nil && externalURL(uri) && !localURL(uri) {
			n.external(uri)
		}
		return 0
	})
	if err := hresult(com(n.webview, 44, popup, uintptr(unsafe.Pointer(&token)))); err != nil {
		return err
	}
	msg := n.callback("{57213F19-00E6-49FA-8E07-898EA01ECBD2}", func(_ uintptr, args *comObject) uintptr {
		source, err := comString(args, 3)
		if err != nil || !localURL(source) {
			return 0
		}
		raw, err := comString(args, 4)
		if err != nil {
			return 0
		}
		generation := n.generation
		var message envelope
		if json.Unmarshal([]byte(raw), &message) != nil {
			return 0
		}
		if message.Type == "drop" {
			n.fileDrop(args)
			return 0
		}
		calls := n.calls
		if calls == nil {
			return 0
		}
		if message.Type == "cancel" {
			calls.remove(message.ID)
			return 0
		}
		if message.Type == "accept" {
			calls.accept(message.ID)
			return 0
		}
		if message.Type != "call" {
			return 0
		}
		call, err := calls.add(message)
		if err != nil {
			b := marshalReply(envelope{ID: message.ID, Type: "reply", Error: err.Error()})
			com(n.webview, 32, uintptr(unsafe.Pointer(wide(string(b)))))
			return 0
		}
		if message.Binary {
			b := marshalReply(envelope{ID: message.ID, Type: "upload"})
			com(n.webview, 32, uintptr(unsafe.Pointer(wide(string(b)))))
			return 0
		}
		go func() {
			reply := calls.run(call, n.options.Bindings, nil, 0)
			b := marshalReply(reply)
			err := n.enqueue(func() {
				if generation == n.generation {
					if hresult(com(n.webview, 32, uintptr(unsafe.Pointer(wide(string(b)))))) != nil {
						calls.remove(message.ID)
					}
				} else {
					calls.remove(message.ID)
				}
			})
			if err != nil {
				calls.remove(message.ID)
			}
		}()
		return 0
	})
	if err := hresult(com(n.webview, 34, msg, uintptr(unsafe.Pointer(&token)))); err != nil {
		return err
	}
	resource := n.callback("{AB00B74C-15F1-4646-80E8-E76341D25D71}", func(_ uintptr, args *comObject) uintptr {
		n.resource(args)
		return 0
	})
	if err := hresult(com(n.webview, 55, resource, uintptr(unsafe.Pointer(&token)))); err != nil {
		return err
	}
	if err := hresult(com(n.webview, 57, uintptr(unsafe.Pointer(wide("*"))), 0)); err != nil {
		return err
	}
	completed := n.callback("{B99369F3-9B11-47B5-BC6F-8E7895FCEA17}", func(hr uintptr, _ *comObject) uintptr {
		if err := hresult(hr); err != nil {
			n.fail(err)
			return 0
		}
		go func() {
			if n.options.OnReady != nil {
				n.options.OnReady(&Window{native: n})
			}
			_ = n.enqueue(func() {
				if n.closeRequested {
					return
				}
				n.titleBarReady = true
				n.publishTitleBarChange(true)
				if err := hresult(com(n.webview, 5, uintptr(unsafe.Pointer(wide(resourceURL(n.options.EntryPath)))))); err != nil {
					n.fail(err)
				}
			})
		}()
		return 0
	})
	loaded := n.callback("{D33A35BF-1C49-4F98-93AB-006E0533FE1C}", func(_ uintptr, args *comObject) uintptr {
		if n.ctx.Err() != nil {
			n.fail(n.ctx.Err())
			return 0
		}
		var success int32
		com(args, 3, uintptr(unsafe.Pointer(&success)))
		if success == 0 {
			var status int32
			if hresult(com(args, 4, uintptr(unsafe.Pointer(&status)))) == nil && (status == 14 || status == 9 && n.closeRequested) {
				return 0
			}
			n.fail(errors.New("navigation failed"))
			return 0
		}
		if !n.ready {
			n.ready = true
			for _, event := range n.pendingEvents {
				com(n.webview, 32, uintptr(unsafe.Pointer(wide(event))))
			}
			n.pendingEvents = nil
		}
		if !n.shown {
			n.shown = true
			show := uintptr(5)
			if n.options.Maximized && !n.options.FixedSize {
				show = 3
			}
			user32.NewProc("ShowWindow").Call(n.hwnd, show)
		}
		return 0
	})
	if err := hresult(com(n.webview, 15, loaded, uintptr(unsafe.Pointer(&token)))); err != nil {
		return err
	}
	closed := n.callback("{5C19E9E0-092F-486B-AFFA-CA8231913039}", func(_ uintptr, args *comObject) uintptr {
		user32.NewProc("PostQuitMessage").Call(0)
		return 0
	})
	if err := hresult(com(n.webview, 59, closed, uintptr(unsafe.Pointer(&token)))); err != nil {
		return err
	}
	return hresult(com(n.webview, 27, uintptr(unsafe.Pointer(wide(bridgeScript))), completed))
}

// openURL 调用系统浏览器并返回ShellExecute失败信息
// 入参: raw 外部地址
// 返回: error 打开错误
func openURL(raw string) error {
	result, _, _ := shell32.NewProc("ShellExecuteW").Call(0, uintptr(unsafe.Pointer(wide("open"))), uintptr(unsafe.Pointer(wide(raw))), 0, 0, 1)
	if result <= 32 {
		return fmt.Errorf("open external URL failed (%d)", result)
	}
	return nil
}

// external 异步打开外部地址并通知宿主错误
// 入参: raw 外部地址
func (n *nativeWindow) external(raw string) {
	go func() {
		if err := OpenURL(raw); err != nil && n.options.OnError != nil {
			n.options.OnError(err)
		}
	}()
}

// pixels 将逻辑像素换算为当前显示器物理像素
// 入参: value 逻辑像素
// 返回: int32 物理像素
func (n *nativeWindow) pixels(value int) int32 {
	return int32((int64(value)*int64(n.dpi) + 48) / 96)
}

// requestClose 异步检查宿主关闭条件，保留校验期间的新请求并触发浏览器beforeunload
func (n *nativeWindow) requestClose() {
	if n.initializing {
		n.cancel()
		return
	}
	if n.ctx.Err() != nil || n.webview == nil {
		if n.activeDialog != nil {
			com(n.activeDialog, 23, 0x800704c7)
		}
		user32.NewProc("PostQuitMessage").Call(0)
		return
	}
	if n.closing {
		n.closePending = true
		return
	}
	if n.closeRequested {
		return
	}
	n.closing = true
	go func() {
		allowed := true
		if n.options.OnClosing != nil {
			func() {
				defer func() {
					if recover() != nil {
						allowed = false
					}
				}()
				allowed = n.options.OnClosing(n.ctx)
			}()
		}
		_ = n.enqueue(func() {
			n.closing = false
			pending := n.closePending
			n.closePending = false
			if !allowed {
				if pending {
					n.requestClose()
				}
				return
			}
			callback := n.callback("{5C4889F0-5EF6-4C5A-952C-D8F1B92D0574}", func(hr uintptr, _ *comObject) uintptr {
				n.closeRequested = false
				if err := hresult(hr); err != nil {
					n.err = err
				}
				return 0
			})
			n.closeRequested = true
			if err := hresult(com(n.webview, 36, uintptr(unsafe.Pointer(wide("Page.close"))), uintptr(unsafe.Pointer(wide("{}"))), callback)); err != nil {
				n.closeRequested = false
				n.err = err
			}
		})
	}()
}

// adjustRect 根据当前显示器DPI计算窗口边框
// 入参: r 客户区矩形，返回窗口矩形
// 返回: error 计算错误
func (n *nativeWindow) adjustRect(r *rect) error {
	top := r.Top
	proc := user32.NewProc("AdjustWindowRectExForDpi")
	if err := proc.Find(); err != nil {
		return err
	}
	ok, _, err := proc.Call(uintptr(unsafe.Pointer(r)), n.style(), 0, 0, uintptr(n.dpi))
	if ok == 0 {
		return err
	}
	if n.options.TitleBar == TitleBarContent {
		r.Top = top
	}
	return nil
}
