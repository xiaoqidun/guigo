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
	"sync/atomic"
	"syscall"
	"unsafe"
)

// trayMessage 接收通知区域图标消息
const trayMessage = 0x8031

// taskbarCreated 保存系统任务栏重建通知消息编号
var taskbarCreated, _, _ = user32.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(wide("TaskbarCreated"))))

// notifyIconData 对应Windows通知区域图标结构
type notifyIconData struct {
	Size                uint32
	Window              uintptr
	ID, Flags, Callback uint32
	Icon                uintptr
	Tip                 [128]uint16
	State, StateMask    uint32
	Info                [256]uint16
	Version             uint32
	InfoTitle           [64]uint16
	InfoFlags           uint32
	GUID                syscall.GUID
	BalloonIcon         uintptr
}

// windowsTray 的配置与原生图标仅由STA线程访问，图标随窗口DPI变化同步刷新
type windowsTray struct {
	owner       *nativeWindow
	handle      *Tray
	options     TrayOptions
	data        notifyIconData
	revision    uint64
	menuPending atomic.Bool
}

// newTray 在STA线程创建独立托盘
// 入参: ctx 调用上下文, options 托盘配置
// 返回: *Tray 托盘句柄, error 创建错误
func (n *nativeWindow) newTray(ctx context.Context, options TrayOptions) (*Tray, error) {
	if taskbarCreated == 0 {
		return nil, errors.New("cannot register taskbar notification")
	}
	options, err := prepareTray(options, n.options.Title, n.iconData)
	if err != nil {
		return nil, err
	}
	t := &windowsTray{owner: n, options: options}
	t.handle = &Tray{setOptions: t.setOptions, close: func() error {
		return n.invoke(func() error {
			t.release()
			return nil
		})
	}}
	err = n.invoke(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.trays == nil {
			n.trays = make(map[uint32]*windowsTray)
		}
		for range 0xffff {
			n.nextTrayID = n.nextTrayID%0xffff + 1
			if n.trays[n.nextTrayID] == nil {
				break
			}
		}
		if n.trays[n.nextTrayID] != nil {
			return errors.New("notification icon ID space exhausted")
		}
		icon, err := n.trayIcon(options.Icon)
		if err != nil {
			return err
		}
		t.data = notifyIconData{Window: n.hwnd, ID: n.nextTrayID, Flags: 0x87, Callback: trayMessage, Icon: icon, Version: 4}
		t.data.Size = uint32(unsafe.Sizeof(t.data))
		t.setTip(options.Title)
		if !t.register() {
			user32.NewProc("DestroyIcon").Call(icon)
			return ErrTrayUnavailable
		}
		n.trays[t.data.ID] = t
		t.handle.available.Store(true)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t.handle, nil
}

// trayIcon 创建托盘独占的图标，避免窗口调整DPI后缓存失效句柄
// 入参: data PNG数据，空值使用系统默认图标
// 返回: uintptr 独占图标句柄, error 创建错误
func (n *nativeWindow) trayIcon(data []byte) (uintptr, error) {
	var icon uintptr
	if len(data) == 0 {
		stock, _, _ := user32.NewProc("LoadIconW").Call(0, 32512)
		icon, _, _ = user32.NewProc("CopyIcon").Call(stock)
	} else {
		size, _, _ := user32.NewProc("GetSystemMetricsForDpi").Call(49, uintptr(n.dpi))
		icon, _, _ = user32.NewProc("CreateIconFromResourceEx").Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 1, 0x00030000, size, size, 0)
		runtime.KeepAlive(data)
	}
	if icon == 0 {
		return 0, errors.New("cannot create tray icon")
	}
	return icon, nil
}

// updateTrayIcons 在窗口DPI变化时按新缩放比例刷新所有活动托盘图标
func (n *nativeWindow) updateTrayIcons() {
	for _, t := range n.trays {
		if len(t.options.Icon) == 0 {
			continue
		}
		icon, err := n.trayIcon(t.options.Icon)
		if err != nil {
			n.reportError(err)
			continue
		}
		old := t.data.Icon
		t.data.Icon = icon
		ok, _, _ := shell32.NewProc("Shell_NotifyIconW").Call(1, uintptr(unsafe.Pointer(&t.data)))
		if ok == 0 {
			t.data.Icon = old
			user32.NewProc("DestroyIcon").Call(icon)
			continue
		}
		user32.NewProc("DestroyIcon").Call(old)
	}
}

// setTip 按系统缓冲区长度截断提示，保留完整UTF16字符
// 入参: title 提示文字
func (t *windowsTray) setTip(title string) {
	t.data.Tip = [128]uint16{}
	tip, _ := syscall.UTF16FromString(title)
	count := copy(t.data.Tip[:127], tip)
	if count > 0 && t.data.Tip[count-1] >= 0xd800 && t.data.Tip[count-1] <= 0xdbff {
		t.data.Tip[count-1] = 0
	}
}

// register 添加图标并启用统一的鼠标与键盘激活通知
// 返回: bool 系统是否接受注册
func (t *windowsTray) register() bool {
	ok, _, _ := shell32.NewProc("Shell_NotifyIconW").Call(0, uintptr(unsafe.Pointer(&t.data)))
	if ok == 0 {
		ok, _, _ = shell32.NewProc("Shell_NotifyIconW").Call(1, uintptr(unsafe.Pointer(&t.data)))
		if ok == 0 {
			return false
		}
	}
	ok, _, _ = shell32.NewProc("Shell_NotifyIconW").Call(4, uintptr(unsafe.Pointer(&t.data)))
	if ok == 0 {
		shell32.NewProc("Shell_NotifyIconW").Call(2, uintptr(unsafe.Pointer(&t.data)))
	}
	return ok != 0
}

// setOptions 在原生更新成功后替换配置，失败时保留旧图标与菜单
// 入参: ctx 调用上下文, options 完整新配置
// 返回: error 配置或系统错误
func (t *windowsTray) setOptions(ctx context.Context, options TrayOptions) error {
	options, err := prepareTray(options, t.owner.options.Title, t.owner.iconData)
	if err != nil {
		return err
	}
	return t.owner.invoke(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.owner.trays[t.data.ID] != t {
			return ErrClosed
		}
		icon, err := t.owner.trayIcon(options.Icon)
		if err != nil {
			return err
		}
		old := t.data
		t.data.Icon = icon
		t.setTip(options.Title)
		ok, _, _ := shell32.NewProc("Shell_NotifyIconW").Call(1, uintptr(unsafe.Pointer(&t.data)))
		if ok == 0 {
			t.data = old
			user32.NewProc("DestroyIcon").Call(icon)
			t.handle.available.Store(false)
			_ = t.owner.activate()
			return ErrTrayUnavailable
		}
		t.options = options
		t.handle.available.Store(true)
		t.revision++
		t.endMenu()
		user32.NewProc("DestroyIcon").Call(old.Icon)
		return nil
	})
}

// release 移除托盘并释放独占图标，窗口退出时同样调用
func (t *windowsTray) release() {
	if t.owner.trays[t.data.ID] != t {
		return
	}
	t.endMenu()
	delete(t.owner.trays, t.data.ID)
	t.handle.closed.Store(true)
	shell32.NewProc("Shell_NotifyIconW").Call(2, uintptr(unsafe.Pointer(&t.data)))
	user32.NewProc("DestroyIcon").Call(t.data.Icon)
	t.handle.available.Store(false)
}

// endMenu 结束当前托盘拥有的原生菜单，避免更新或关闭后提交旧选项
func (t *windowsTray) endMenu() {
	if t.owner.activeTrayMenu == t {
		user32.NewProc("EndMenu").Call()
	}
}

// runTrayCallback 在后台Goroutine执行托盘回调并隔离异常
// 入参: fn 业务回调
func (t *windowsTray) runTrayCallback(fn func()) {
	n := t.owner
	go func() {
		defer func() {
			if v := recover(); v != nil {
				n.reportError(fmt.Errorf("tray callback panic: %v", v))
			}
		}()
		fn()
	}()
}

// trayProc 处理任务栏恢复、激活及菜单选择，回调交给后台Goroutine
// 入参: msg 窗口消息, wparam 坐标参数, lparam 通知数据
// 返回: bool 是否已处理
func (n *nativeWindow) trayProc(msg, wparam, lparam uintptr) bool {
	if taskbarCreated != 0 && msg == taskbarCreated {
		for _, t := range n.trays {
			available := t.register()
			t.handle.available.Store(available)
			if !available {
				_ = n.activate()
			}
		}
		return true
	}
	if msg != trayMessage {
		return false
	}
	t := n.trays[uint32(lparam>>16)]
	if t == nil || !t.handle.Available() {
		return true
	}
	switch lparam & 0xffff {
	case 0x400, 0x401:
		if t.options.MenuOnActivate {
			t.queueMenu(wparam)
		} else if fn := t.options.OnActivate; fn != nil {
			t.runTrayCallback(fn)
		}
	case 0x208:
		if fn := t.options.OnSecondaryActivate; fn != nil {
			t.runTrayCallback(fn)
		}
	case 0x7b:
		t.queueMenu(wparam)
	}
	return true
}

// queueMenu 将菜单显示排到最外层消息循环，合并尚未执行的打开请求并确保释放标志
// 入参: position 通知消息携带的屏幕坐标
func (t *windowsTray) queueMenu(position uintptr) {
	n := t.owner
	if n.activeTrayMenu != nil || !t.menuPending.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer t.menuPending.Store(false)
		if err := n.invokeTop(func() error {
			return t.showMenu(position)
		}); err != nil && !errors.Is(err, ErrClosed) {
			n.reportError(err)
		}
	}()
}
