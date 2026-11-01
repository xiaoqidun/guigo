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
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"syscall"
	"unsafe"
)

var (
	dwmapi           = syscall.NewLazyDLL("dwmapi.dll")
	gdi32            = syscall.NewLazyDLL("gdi32.dll")
	titleBarCallback = syscall.NewCallback(titleBarChildProc)
)

// titleBarButtonInfo 保存系统标题栏各元素的状态及屏幕坐标
type titleBarButtonInfo struct {
	Size   uint32
	Bounds rect
	States [6]uint32
	Rects  [6]rect
}

// initializeTitleBarZoom 禁止内容标题栏模式的整页捏合缩放，保持网页与原生按钮对齐
// 返回: error WebView接口或缩放设置错误
func (n *nativeWindow) initializeTitleBarZoom() error {
	if n.options.TitleBar != TitleBarContent {
		return nil
	}
	var settings *comObject
	if err := hresult(com(n.webview, 3, uintptr(unsafe.Pointer(&settings)))); err != nil {
		return err
	}
	defer release(settings)
	iid := guid("{183E7052-1D03-43A0-AB99-98E043B66B39}")
	var settings5 *comObject
	if err := hresult(com(settings, 0, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&settings5)))); err != nil {
		return err
	}
	defer release(settings5)
	return hresult(com(settings5, 30, 0))
}

// titleBarInfo 在宿主线程查询标题栏布局
// 返回: TitleBarInfo 标题栏信息, error 查询错误
func (n *nativeWindow) titleBarInfo() (info TitleBarInfo, err error) {
	err = n.invoke(func() error {
		info = n.currentTitleBarInfo()
		return nil
	})
	return
}

// setTitleBarColors 在宿主线程更新系统标题栏并通知内容区域
// 入参: colors 已校验的标题栏配色
// 返回: error 模式或系统配色错误
func (n *nativeWindow) setTitleBarColors(colors TitleBarColors) error {
	return n.invoke(func() error {
		if n.options.TitleBar != TitleBarContent {
			return errors.New("title bar colors require content title bar")
		}
		if n.options.TitleBarColors == colors {
			return nil
		}
		previous := n.options.TitleBarColors
		n.options.TitleBarColors = colors
		if err := n.applyTitleBarColors(n.currentTitleBarInfo().Active); err != nil {
			n.options.TitleBarColors = previous
			n.applyTitleBarColors(n.currentTitleBarInfo().Active)
			return err
		}
		n.publishTitleBarChange(false)
		return nil
	})
}

// setDragRegions 在宿主线程替换拖动区域
// 入参: regions 已复制并校验的逻辑像素矩形
// 返回: error 设置错误
func (n *nativeWindow) setDragRegions(regions []Rect) error {
	return n.invoke(func() error {
		if n.options.TitleBar != TitleBarContent {
			return errors.New("drag regions require content title bar")
		}
		if slices.Equal(n.dragRegions, regions) {
			return nil
		}
		previous := n.dragRegions
		n.dragRegions = regions
		if err := n.layoutDragRegions(); err != nil {
			n.dragRegions = previous
			return err
		}
		return nil
	})
}

// titleBarMetric 返回当前DPI下的系统尺寸
// 入参: index 系统尺寸索引
// 返回: int32 物理像素尺寸
func (n *nativeWindow) titleBarMetric(index uintptr) int32 {
	value, _, _ := user32.NewProc("GetSystemMetricsForDpi").Call(index, uintptr(n.dpi))
	return int32(value)
}

// titleBarBorder 返回系统调整大小边缘的物理高度
// 返回: int32 物理像素高度
func (n *nativeWindow) titleBarBorder() int32 {
	return n.titleBarMetric(33) + n.titleBarMetric(92)
}

// titleBarOffset 返回最大化时移出屏幕的顶部边框高度
// 返回: int32 内容区域的物理纵向偏移
func (n *nativeWindow) titleBarOffset() int32 {
	zoomed, _, _ := user32.NewProc("IsZoomed").Call(n.hwnd)
	if zoomed != 0 {
		return n.titleBarBorder()
	}
	return 0
}

// titleBarBounds 计算可见内容区的标题栏高度及右侧系统按钮保留矩形
// 返回: int32 标题栏高度, rect 系统按钮保留矩形
func (n *nativeWindow) titleBarBounds() (int32, rect) {
	var client rect
	user32.NewProc("GetClientRect").Call(n.hwnd, uintptr(unsafe.Pointer(&client)))
	offset := n.titleBarOffset()
	client.Bottom -= offset
	height := n.titleBarMetric(4) + n.titleBarBorder()
	width := 3 * max(n.pixels(46), n.titleBarMetric(30))
	origin := point{}
	user32.NewProc("ClientToScreen").Call(n.hwnd, uintptr(unsafe.Pointer(&origin)))
	origin.Y += offset
	visible, _, _ := user32.NewProc("IsWindowVisible").Call(n.hwnd)
	iconic, _, _ := user32.NewProc("IsIconic").Call(n.hwnd)
	if visible != 0 && iconic == 0 {
		var buttons titleBarButtonInfo
		buttons.Size = uint32(unsafe.Sizeof(buttons))
		user32.NewProc("DefWindowProcW").Call(n.hwnd, 0x33f, 0, uintptr(unsafe.Pointer(&buttons)))
		left, bottom := client.Right, int32(0)
		for _, index := range []int{2, 3, 4, 5} {
			r := buttons.Rects[index]
			if buttons.States[index]&0x18000 == 0 && r.Right > r.Left && r.Bottom > r.Top {
				left = min(left, r.Left-origin.X)
				bottom = max(bottom, r.Bottom-origin.Y)
			}
		}
		if left >= 0 && left < client.Right && bottom > 0 && bottom <= height+n.titleBarBorder() {
			width = client.Right - left
		}
	}
	height = min(max(0, client.Bottom), height)
	return height, rect{Left: max(0, client.Right-width), Right: client.Right, Bottom: height}
}

// currentTitleBarInfo 读取当前系统状态并换算逻辑尺寸
// 返回: TitleBarInfo 标题栏信息
func (n *nativeWindow) currentTitleBarInfo() TitleBarInfo {
	foreground, _, _ := user32.NewProc("GetForegroundWindow").Call()
	zoomed, _, _ := user32.NewProc("IsZoomed").Call(n.hwnd)
	info := TitleBarInfo{Active: foreground == n.hwnd, Maximized: zoomed != 0, Material: n.options.WindowMaterial}
	if n.options.TitleBar == TitleBarContent {
		info.Colors = n.options.TitleBarColors
		height, buttons := n.titleBarBounds()
		scale := float64(n.dpi) / 96
		info.Height = float64(height) / scale
		info.Right = float64(buttons.Right-buttons.Left) / scale
	}
	return info
}

// initializeTitleBar 创建自有WebView容器及半透明原生命中层，不访问浏览器内部窗口
// 命中层使用1/255不透明度，零不透明度会使Windows跳过鼠标命中
// 返回: error 初始化错误
func (n *nativeWindow) initializeTitleBar() error {
	if n.options.TitleBar != TitleBarContent {
		return nil
	}
	var composition int32
	hr, _, _ := dwmapi.NewProc("DwmIsCompositionEnabled").Call(uintptr(unsafe.Pointer(&composition)))
	if err := hresult(hr); err != nil {
		return err
	}
	if composition == 0 {
		return errors.New("content title bar requires desktop composition")
	}
	if SupportsWindowMaterial(WindowMicaAlt) {
		if err := n.applyWindowBackdrop(); err != nil {
			return err
		}
	}
	if err := n.applyTitleBarColors(n.currentTitleBarInfo().Active); err != nil {
		return err
	}
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	cursor, _, _ := user32.NewProc("LoadCursorW").Call(0, 32512)
	background, _, _ := gdi32.NewProc("GetStockObject").Call(5)
	class := windowClass{Style: 8, Proc: titleBarCallback, Instance: instance, Cursor: cursor, Background: background, Name: wide("GUIGo.Native.Content")}
	class.Size = uint32(unsafe.Sizeof(class))
	atom, _, err := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class)))
	if atom == 0 && err != syscall.Errno(1410) {
		return err
	}
	n.contentWindow, _, err = user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class.Name)), 0, 0x52000000, 0, 0, 1, 1, n.hwnd, 0, instance, 0)
	if n.contentWindow == 0 {
		return err
	}
	n.dragWindow, _, err = user32.NewProc("CreateWindowExW").Call(0x08080000, uintptr(unsafe.Pointer(class.Name)), 0, 0x54000000, 0, 0, 1, 1, n.hwnd, 0, instance, 0)
	if n.dragWindow == 0 {
		return err
	}
	ok, _, err := user32.NewProc("SetLayeredWindowAttributes").Call(n.dragWindow, 0, 1, 2)
	if ok == 0 {
		return err
	}
	ok, _, err = user32.NewProc("SetWindowPos").Call(n.hwnd, 0, 0, 0, 0, 0, 0x37)
	if ok == 0 {
		return err
	}
	return n.layoutTitleBar()
}

// titleBarColor 将已校验的RGB颜色转换为Win32的COLORREF
// 入参: value #RRGGBB颜色
// 返回: uint32 Win32颜色值
func titleBarColor(value string) uint32 {
	rgb, _ := strconv.ParseUint(value[1:], 16, 24)
	return uint32(rgb&0xff)<<16 | uint32(rgb&0xff00) | uint32(rgb>>16)
}

// applyTitleBarColors 同步系统标题栏配色，不替换窗口按钮或命中行为
// 入参: focused 窗口是否激活
// 返回: error 系统配色错误，不支持配色属性的系统保留原生绘制
func (n *nativeWindow) applyTitleBarColors(focused bool) error {
	colors := n.options.TitleBarColors
	if n.hwnd == 0 {
		return nil
	}
	background, foreground, dark := uint32(0xffffffff), uint32(0xffffffff), uint32(0)
	if colors != (TitleBarColors{}) {
		bg, fg := colors.Background, colors.Foreground
		if !focused {
			bg, fg = colors.InactiveBackground, colors.InactiveForeground
		}
		background, foreground = titleBarColor(bg), titleBarColor(fg)
		if (background&0xff)*299+((background>>8)&0xff)*587+((background>>16)&0xff)*114 < 128000 {
			dark = 1
		}
	}
	if n.options.WindowMaterial != WindowSolid {
		background = 0xffffffff
	}
	for _, attribute := range []struct {
		id    uintptr
		color uint32
	}{{20, dark}, {35, background}, {36, foreground}} {
		hr, _, _ := dwmapi.NewProc("DwmSetWindowAttribute").Call(n.hwnd, attribute.id, uintptr(unsafe.Pointer(&attribute.color)), unsafe.Sizeof(attribute.color))
		if uint32(hr) != 0x80070057 && uint32(hr) != 0x80004001 {
			if err := hresult(hr); err != nil {
				return err
			}
		}
	}
	user32.NewProc("RedrawWindow").Call(n.hwnd, 0, 0, 0x405)
	return nil
}

// unregisterTitleBar 在所有自有子窗口销毁后释放窗口类
func (n *nativeWindow) unregisterTitleBar() {
	if n.options.TitleBar == TitleBarContent {
		instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
		user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(wide("GUIGo.Native.Content"))), instance)
	}
}

// titleBarRegion 创建物理像素GDI区域
// 入参: r 物理像素矩形
// 返回: uintptr 区域句柄, error 创建错误
func titleBarRegion(r rect) (uintptr, error) {
	region, _, err := gdi32.NewProc("CreateRectRgn").Call(uintptr(r.Left), uintptr(r.Top), uintptr(r.Right), uintptr(r.Bottom))
	if region == 0 {
		return 0, err
	}
	return region, nil
}

// combineTitleBarRegion 合并区域并释放临时矩形句柄
// 入参: region 目标区域, r 物理矩形, mode 合并方式
// 返回: error 合并错误
func combineTitleBarRegion(region uintptr, r rect, mode uintptr) error {
	part, err := titleBarRegion(r)
	if err != nil {
		return err
	}
	defer gdi32.NewProc("DeleteObject").Call(part)
	ok, _, err := gdi32.NewProc("CombineRgn").Call(region, region, part, mode)
	if ok == 0 {
		return err
	}
	return nil
}

// applyTitleBarRegion 将区域所有权转交给窗口，失败时释放句柄
// 入参: hwnd 自有窗口, region 区域句柄
// 返回: error 应用错误
func applyTitleBarRegion(hwnd, region uintptr) error {
	ok, _, err := user32.NewProc("SetWindowRgn").Call(hwnd, region, 1)
	if ok == 0 {
		gdi32.NewProc("DeleteObject").Call(region)
		return err
	}
	return nil
}

// dragRegionPixels 先裁剪再向内取整，防止坐标溢出及命中范围扩展到相邻控件
// 入参: r 逻辑矩形, bounds 物理客户区, dpi 当前显示器DPI
// 返回: rect 裁剪后的物理矩形
func dragRegionPixels(r Rect, bounds rect, dpi uint32) rect {
	scale := float64(dpi) / 96
	width, height := float64(bounds.Right)/scale, float64(bounds.Bottom)/scale
	left, top := min(width, max(0, r.X)), min(height, max(0, r.Y))
	right, bottom := min(width, max(0, r.X+r.Width)), min(height, max(0, r.Y+r.Height))
	if r.Width == 0 || r.Height == 0 || right <= left || bottom <= top {
		return rect{}
	}
	pixels := rect{Left: int32(math.Ceil(left * scale)), Top: int32(math.Ceil(top * scale)), Right: min(bounds.Right, int32(math.Floor(right*scale))), Bottom: min(bounds.Bottom, int32(math.Floor(bottom*scale)))}
	if pixels.Right <= pixels.Left || pixels.Bottom <= pixels.Top {
		return rect{}
	}
	return pixels
}

// layoutTitleBar 更新自有窗口的裁剪、层次和DWM原生按钮区域
// 返回: error 布局错误
func (n *nativeWindow) layoutTitleBar() error {
	if n.contentWindow == 0 || n.dragWindow == 0 || n.titleBarLayout {
		return nil
	}
	n.titleBarLayout = true
	defer func() { n.titleBarLayout = false }()
	var bounds rect
	user32.NewProc("GetClientRect").Call(n.hwnd, uintptr(unsafe.Pointer(&bounds)))
	offset := n.titleBarOffset()
	bounds.Bottom -= offset
	_, buttons := n.titleBarBounds()
	if err := n.extendWindowFrame(); err != nil {
		return err
	}
	for _, hwnd := range []uintptr{n.contentWindow, n.dragWindow} {
		flags := uintptr(0x14)
		if hwnd == n.dragWindow {
			flags = 0x10
		}
		ok, _, err := user32.NewProc("SetWindowPos").Call(hwnd, 0, 0, uintptr(offset), uintptr(bounds.Right), uintptr(bounds.Bottom), flags)
		if ok == 0 {
			return err
		}
	}
	content, err := titleBarRegion(bounds)
	if err != nil {
		return err
	}
	if err := combineTitleBarRegion(content, buttons, 4); err != nil {
		gdi32.NewProc("DeleteObject").Call(content)
		return err
	}
	if err := applyTitleBarRegion(n.contentWindow, content); err != nil {
		return err
	}
	return n.layoutDragRegions()
}

// layoutDragRegions 更新命中层区域，不改变内容窗口的布局和裁剪
// 返回: error 区域错误
func (n *nativeWindow) layoutDragRegions() error {
	if n.dragWindow == 0 {
		return nil
	}
	var bounds rect
	user32.NewProc("GetClientRect").Call(n.contentWindow, uintptr(unsafe.Pointer(&bounds)))
	_, buttons := n.titleBarBounds()
	top := rect{Right: bounds.Right}
	zoomed, _, _ := user32.NewProc("IsZoomed").Call(n.hwnd)
	if zoomed == 0 && !n.options.FixedSize {
		top.Bottom = min(bounds.Bottom, n.titleBarBorder())
	}
	drag, err := titleBarRegion(top)
	if err != nil {
		return err
	}
	for _, r := range n.dragRegions {
		if err := combineTitleBarRegion(drag, dragRegionPixels(r, bounds, n.dpi), 2); err != nil {
			gdi32.NewProc("DeleteObject").Call(drag)
			return err
		}
	}
	if err := combineTitleBarRegion(drag, buttons, 4); err != nil {
		gdi32.NewProc("DeleteObject").Call(drag)
		return err
	}
	return applyTitleBarRegion(n.dragWindow, drag)
}

// titleBarChildProc 将自有命中层的非客户区输入交给顶层窗口的系统处理
// 入参: hwnd 子窗口, msg 消息类型, wparam 消息参数, lparam 消息参数
// 返回: uintptr 消息处理结果
func titleBarChildProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	n := active.Load()
	if n != nil && hwnd == n.dragWindow {
		switch {
		case msg == 0x84:
			result, _, _ := user32.NewProc("SendMessageW").Call(n.hwnd, msg, wparam, lparam)
			return result
		case msg >= 0xa0 && msg <= 0xad || msg == 0x7b:
			result, _, _ := user32.NewProc("SendMessageW").Call(n.hwnd, msg, wparam, lparam)
			return result
		case msg == 0x20:
			result, _, _ := user32.NewProc("SendMessageW").Call(n.hwnd, msg, n.hwnd, lparam)
			return result
		}
	}
	result, _, _ := user32.NewProc("DefWindowProcW").Call(hwnd, msg, wparam, lparam)
	return result
}

// titleBarButtonHit 根据系统按钮的实际矩形识别未由DWM处理的命中
// 入参: p 屏幕物理像素坐标
// 返回: uintptr 原生按钮命中代码，未命中返回HTCLIENT
func (n *nativeWindow) titleBarButtonHit(p point) uintptr {
	var info titleBarButtonInfo
	info.Size = uint32(unsafe.Sizeof(info))
	user32.NewProc("DefWindowProcW").Call(n.hwnd, 0x33f, 0, uintptr(unsafe.Pointer(&info)))
	for _, button := range []struct {
		index int
		hit   uintptr
	}{{2, 8}, {3, 9}, {4, 21}, {5, 20}} {
		r := info.Rects[button.index]
		if info.States[button.index]&0x18001 == 0 && p.X >= r.Left && p.X < r.Right && p.Y >= r.Top && p.Y < r.Bottom {
			return button.hit
		}
	}
	return 1
}

// titleBarProc 处理自定义非客户区和合并后的布局通知
// 入参: hwnd 顶层窗口, msg 消息类型, wparam 消息参数, lparam 消息参数
// 返回: uintptr 消息结果, bool 是否已经处理
func (n *nativeWindow) titleBarProc(hwnd, msg, wparam, lparam uintptr) (uintptr, bool) {
	if msg == 0x8003 {
		n.titleBarPending = false
		if err := n.layoutTitleBar(); err != nil {
			n.fail(err)
		}
		n.publishTitleBarChange(false)
		return 0, true
	}
	if msg == 6 || msg == 0x18 || msg == 0x31a || msg == 0x1a || msg == 0x31e {
		n.scheduleTitleBarChange()
	}
	if msg == 0x31e && n.options.WindowMaterial != WindowSolid {
		if err := n.extendWindowFrame(); err != nil {
			n.fail(err)
		}
	}
	if n.options.TitleBar != TitleBarContent {
		return 0, false
	}
	if n.options.TitleBarColors != (TitleBarColors{}) && n.hwnd != 0 {
		if msg == 0x86 || msg == 0x31a || msg == 0x31e {
			focused := n.currentTitleBarInfo().Active
			if msg == 0x86 {
				focused = wparam != 0
			}
			if err := n.applyTitleBarColors(focused); err != nil {
				n.fail(err)
			}
		}
	}
	if msg == 0xa5 && wparam == 2 {
		n.showTitleBarMenu(lparam)
		return 0, true
	}
	if msg == 0x83 {
		r := *(**rect)(unsafe.Pointer(&lparam))
		top := r.Top
		user32.NewProc("DefWindowProcW").Call(hwnd, msg, wparam, lparam)
		r.Top = top
		return 0, true
	}
	var result uintptr
	handled, _, _ := dwmapi.NewProc("DwmDefWindowProc").Call(hwnd, msg, wparam, lparam, uintptr(unsafe.Pointer(&result)))
	if handled != 0 {
		return result, true
	}
	if msg == 0x84 {
		screen := point{X: int32(int16(lparam)), Y: int32(int16(lparam >> 16))}
		p := screen
		user32.NewProc("ScreenToClient").Call(hwnd, uintptr(unsafe.Pointer(&p)))
		p.Y -= n.titleBarOffset()
		var client rect
		user32.NewProc("GetClientRect").Call(n.contentWindow, uintptr(unsafe.Pointer(&client)))
		if p.X >= 0 && p.X < client.Right && p.Y >= 0 && p.Y < client.Bottom {
			zoomed, _, _ := user32.NewProc("IsZoomed").Call(hwnd)
			if zoomed == 0 && !n.options.FixedSize && p.Y < n.titleBarBorder() {
				if p.X < n.titleBarBorder() {
					return 13, true
				}
				if p.X >= client.Right-n.titleBarBorder() {
					return 14, true
				}
				return 12, true
			}
			_, buttons := n.titleBarBounds()
			if p.X >= buttons.Left && p.Y < buttons.Bottom {
				return n.titleBarButtonHit(screen), true
			}
			for _, region := range n.dragRegions {
				r := dragRegionPixels(region, client, n.dpi)
				if p.X >= r.Left && p.X < r.Right && p.Y >= r.Top && p.Y < r.Bottom {
					return 2, true
				}
			}
			return 1, true
		}
	}
	return 0, false
}

// showTitleBarMenu 在拖动区域显示原生窗口菜单并交回系统命令处理
// 入参: position 屏幕坐标组成的消息参数
func (n *nativeWindow) showTitleBarMenu(position uintptr) {
	menu, _, _ := user32.NewProc("GetSystemMenu").Call(n.hwnd, 0)
	if menu == 0 {
		return
	}
	zoomed, _, _ := user32.NewProc("IsZoomed").Call(n.hwnd)
	iconic, _, _ := user32.NewProc("IsIconic").Call(n.hwnd)
	for _, item := range []struct {
		command uintptr
		enabled bool
	}{
		{0xf120, zoomed != 0 || iconic != 0},
		{0xf010, zoomed == 0 && iconic == 0},
		{0xf000, zoomed == 0 && iconic == 0 && !n.options.FixedSize},
		{0xf020, iconic == 0},
		{0xf030, zoomed == 0 && !n.options.FixedSize},
	} {
		flags := uintptr(0)
		if !item.enabled {
			flags = 1
		}
		user32.NewProc("EnableMenuItem").Call(menu, item.command, flags)
	}
	x, y := int32(int16(position)), int32(int16(position>>16))
	command, _, _ := user32.NewProc("TrackPopupMenuEx").Call(menu, 0x102, uintptr(x), uintptr(y), n.hwnd, 0)
	if command != 0 {
		user32.NewProc("SendMessageW").Call(n.hwnd, 0x112, command, position)
	}
}

// scheduleTitleBarChange 合并宿主消息循环中的布局变化
func (n *nativeWindow) scheduleTitleBarChange() {
	if n.hwnd != 0 && n.titleBarReady && !n.titleBarPending {
		n.titleBarPending = true
		ok, _, _ := user32.NewProc("PostMessageW").Call(n.hwnd, 0x8003, 0, 0)
		if ok == 0 {
			n.titleBarPending = false
		}
	}
}

// publishTitleBarChange 仅发布变化后的最新布局，避免后台回调阻塞宿主线程
// 入参: initial 是否强制发送初始状态
func (n *nativeWindow) publishTitleBarChange(initial bool) {
	info := n.currentTitleBarInfo()
	if !initial && info == n.titleBarState && n.dpi == n.titleBarDPI {
		return
	}
	n.titleBarState = info
	n.titleBarDPI = n.dpi
	if n.titleBarEvents == nil {
		return
	}
	select {
	case n.titleBarEvents <- info:
	default:
		select {
		case <-n.titleBarEvents:
		default:
		}
		n.titleBarEvents <- info
	}
}

// deliverTitleBarChanges 串行执行业务回调并隔离异常
func (n *nativeWindow) deliverTitleBarChanges() {
	for {
		select {
		case <-n.ctx.Done():
			return
		case info := <-n.titleBarEvents:
			if n.ctx.Err() != nil {
				return
			}
			func() {
				defer func() {
					if value := recover(); value != nil && n.options.OnError != nil {
						go n.options.OnError(fmt.Errorf("title bar callback panic: %v", value))
					}
				}()
				n.options.OnTitleBarChanged(info)
			}()
		}
	}
}
