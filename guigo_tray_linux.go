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

//go:build !android && (amd64 || arm64)

package guigo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"sync/atomic"
)

// 托盘对象、菜单及会话总线使用的接口与路径
const (
	trayNotifierInterface = "org.kde.StatusNotifierItem"
	trayMenuInterface     = "com.canonical.dbusmenu"
	trayWatcher           = "org.kde.StatusNotifierWatcher"
	trayBusInterface      = "org.freedesktop.DBus"
	trayBusPath           = "/org/freedesktop/DBus"
)

// linuxTrayXML 让GDBus检查方法签名，并自动提供属性、内省及Peer接口
const linuxTrayXML = `<node>
<interface name="org.kde.StatusNotifierItem">
 <method name="Activate"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
 <method name="SecondaryActivate"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
 <method name="ContextMenu"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
 <method name="Scroll"><arg type="i" direction="in"/><arg type="s" direction="in"/></method>
 <property name="Category" type="s" access="read"/>
 <property name="Id" type="s" access="read"/>
 <property name="Title" type="s" access="read"/>
 <property name="Status" type="s" access="read"/>
 <property name="IconName" type="s" access="read"/>
 <property name="IconPixmap" type="a(iiay)" access="read"/>
 <property name="ToolTip" type="(sa(iiay)ss)" access="read"/>
 <property name="ItemIsMenu" type="b" access="read"/>
 <property name="Menu" type="o" access="read"/>
 <signal name="NewTitle"/>
 <signal name="NewIcon"/>
 <signal name="NewToolTip"/>
</interface>
<interface name="com.canonical.dbusmenu">
 <method name="GetLayout"><arg type="i" direction="in"/><arg type="i" direction="in"/><arg type="as" direction="in"/><arg type="u" direction="out"/><arg type="(ia{sv}av)" direction="out"/></method>
 <method name="GetGroupProperties"><arg type="ai" direction="in"/><arg type="as" direction="in"/><arg type="a(ia{sv})" direction="out"/></method>
 <method name="GetProperty"><arg type="i" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
 <method name="Event"><arg type="i" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="in"/><arg type="u" direction="in"/></method>
 <method name="EventGroup"><arg type="a(isvu)" direction="in"/><arg type="ai" direction="out"/></method>
 <method name="AboutToShow"><arg type="i" direction="in"/><arg type="b" direction="out"/></method>
 <method name="AboutToShowGroup"><arg type="ai" direction="in"/><arg type="ai" direction="out"/><arg type="ai" direction="out"/></method>
 <property name="Version" type="u" access="read"/>
 <property name="TextDirection" type="s" access="read"/>
 <property name="Status" type="s" access="read"/>
 <property name="IconThemePath" type="as" access="read"/>
 <signal name="LayoutUpdated"><arg type="u"/><arg type="i"/></signal>
 <signal name="ItemsPropertiesUpdated"><arg type="a(ia{sv})"/><arg type="a(ias)"/></signal>
</interface>
</node>`

// 托盘注册表、递增标识与GDBus原生信号回调表
var (
	linuxTrays            sync.Map
	linuxTrayIDs          atomic.Uint64
	linuxTrayVTable       [11]uintptr
	linuxTraySignal       uintptr
	linuxTrayClosedSignal uintptr
)

// linuxTray 的配置与原生字段仅由GTK线程访问
type linuxTray struct {
	owner                 *nativeWindow
	options               TrayOptions
	handle                *Tray
	id, connection        uintptr
	registrations         []uint32
	subscription          uint32
	hostUnregSubscription uint32
	hostRegSubscription   uint32
	closedSignal          uint64
	closed                bool
	hostGeneration        uint64
	reregisterGen         atomic.Uint64
	reregistering         atomic.Bool
	reregisterPending     atomic.Bool
	pixmapVariant         uintptr
	tooltipVariant        uintptr
	ids                   map[string]int32
	items                 map[int32]TrayItem
	order                 []int32
	children              map[int32][]int32
	parents               map[int32]int32
	nextID                int32
	revision              uint32
}

// trayIconName 仅在未指定PNG时使用通用应用图标
// 入参: icon PNG图标数据
// 返回: string 桌面图标名称
func trayIconName(icon []byte) string {
	if len(icon) == 0 {
		return "application-x-executable"
	}
	return ""
}

// trayPixmap 将PNG缩放为托盘常用尺寸，按网络字节序生成ARGB像素并修正图像起点偏移
// 入参: data PNG数据，空值使用桌面默认图标
// 返回: string GVariant像素数组, error 解码错误
func trayPixmap(data []byte) (string, error) {
	if len(data) == 0 {
		return "@a(iiay) []", nil
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	bounds := img.Bounds()
	var text strings.Builder
	text.Grow((32*32+64*64)*20 + 64)
	text.WriteString("@a(iiay) [")
	for i, size := range []int{32, 64} {
		if i != 0 {
			text.WriteByte(',')
		}
		fmt.Fprintf(&text, "(%d,%d,@ay [", size, size)
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				if x != 0 || y != 0 {
					text.WriteByte(',')
				}
				pixel := color.NRGBAModel.Convert(img.At(bounds.Min.X+x*bounds.Dx()/size, bounds.Min.Y+y*bounds.Dy()/size)).(color.NRGBA)
				fmt.Fprintf(&text, "0x%02x,0x%02x,0x%02x,0x%02x", pixel.A, pixel.R, pixel.G, pixel.B)
			}
		}
		text.WriteString("])")
	}
	text.WriteByte(']')
	return text.String(), nil
}

// trayBuildVariants 预解析图标和提示GVariant以复用缓存，避免GTK主线程重复解析大文本
// 入参: options 托盘配置, pixmap 已编码像素数组文本
// 返回: uintptr 图标GVariant, uintptr 提示GVariant, error 解析错误
func trayBuildVariants(options TrayOptions, pixmap string) (uintptr, uintptr, error) {
	pixmapVar, err := trayVariant(pixmap)
	if err != nil {
		return 0, 0, err
	}
	tooltipText := fmt.Sprintf("(%s, %s, %s, \"\")", trayQuote(trayIconName(options.Icon)), pixmap, trayQuote(options.Title))
	tooltipVar, err := trayVariant(tooltipText)
	if err != nil {
		gdbus.Unref(pixmapVar)
		return 0, 0, err
	}
	return pixmapVar, tooltipVar, nil
}

// linuxTrayHostChanged 响应监视服务或宿主离线上线，离线时恢复窗口，重建时自动重新注册
// 入参: connection 总线连接, sender 发送者, path 对象路径, iface 接口, signal 信号名, params 信号参数, data 托盘标识
func linuxTrayHostChanged(connection uintptr, sender, path, iface, signal *byte, params, data uintptr) {
	value, ok := linuxTrays.Load(data)
	if !ok {
		return
	}
	t := value.(*linuxTray)
	if t.closed {
		return
	}
	switch nativeString(signal) {
	case "NameOwnerChanged":
		newOwner, ok := trayString(params, 2)
		if !ok {
			return
		}
		t.hostGeneration++
		if newOwner == "" {
			t.handle.available.Store(false)
			_ = t.owner.activate()
		} else {
			t.reregister(t.hostGeneration)
		}
	case "StatusNotifierHostUnregistered":
		t.hostGeneration++
		t.checkHostOffline(t.hostGeneration)
	case "StatusNotifierHostRegistered":
		t.hostGeneration++
		t.reregister(t.hostGeneration)
	}
}

// linuxTrayConnectionClosed 可能由GIO工作线程触发，只排队操作GTK资源
// 入参: connection 总线连接, vanished 远端断开标志, failure 原生错误, data 托盘标识
func linuxTrayConnectionClosed(connection uintptr, vanished int32, failure, data uintptr) {
	if value, ok := linuxTrays.Load(data); ok {
		t := value.(*linuxTray)
		_ = t.owner.enqueue(func() {
			if !t.closed {
				t.release()
				_ = t.owner.activate()
			}
		})
	}
}

// linuxTrayMethod 为原生调用返回唯一响应，原生侧随后释放invocation
// 入参: connection 总线连接, sender 发送者, path 对象路径, iface 接口, method 方法名, params 请求参数, invocation 调用对象, data 托盘标识
func linuxTrayMethod(connection uintptr, sender, path, iface, method *byte, params, invocation, data uintptr) {
	value, ok := linuxTrays.Load(data)
	if !ok {
		gdbus.ReturnError(invocation, "org.freedesktop.DBus.Error.Disconnected", "tray closed")
		return
	}
	t := value.(*linuxTray)
	text := "()"
	var err error
	if nativeString(iface) == trayMenuInterface {
		text, err = t.menuReply(nativeString(method), params)
	} else if nativeString(method) == "Activate" {
		if !t.options.MenuOnActivate && t.options.OnActivate != nil {
			t.runTrayCallback(t.options.OnActivate)
		}
	} else if nativeString(method) == "SecondaryActivate" {
		if t.options.OnSecondaryActivate != nil {
			t.runTrayCallback(t.options.OnSecondaryActivate)
		}
	}
	if err == nil {
		var result uintptr
		result, err = trayVariant(text)
		if err == nil {
			gdbus.Return(invocation, result)
			gdbus.Unref(result)
			return
		}
	}
	gdbus.ReturnError(invocation, "org.freedesktop.DBus.Error.InvalidArgs", err.Error())
}

// linuxTrayProperty 返回完整GVariant引用，所有权交给GDBus，失败时遵循契约设置GError
// 入参: connection 总线连接, sender 发送者, path 对象路径, iface 接口, property 属性名, failure 原生错误输出, data 托盘标识
// 返回: uintptr 属性值
func linuxTrayProperty(connection uintptr, sender, path, iface, property *byte, failure **linuxError, data uintptr) uintptr {
	value, ok := linuxTrays.Load(data)
	if !ok {
		if failure != nil {
			gdbus.SetErrorLiteral(failure, gdbus.DBusErrorQuark(), 0, "tray closed")
		}
		return 0
	}
	t := value.(*linuxTray)
	var text string
	if nativeString(iface) == trayNotifierInterface {
		switch nativeString(property) {
		case "Category":
			text = `"ApplicationStatus"`
		case "Id":
			prefix := t.owner.options.AppID
			if prefix == "" {
				prefix = "guigo"
			}
			text = trayQuote(fmt.Sprintf("%s-%d", prefix, t.id))
		case "Title":
			text = trayQuote(t.options.Title)
		case "Status":
			text = `"Active"`
		case "IconName":
			text = trayQuote(trayIconName(t.options.Icon))
		case "IconPixmap":
			if t.pixmapVariant != 0 {
				return gdbus.VariantRef(t.pixmapVariant)
			}
		case "ToolTip":
			if t.tooltipVariant != 0 {
				return gdbus.VariantRef(t.tooltipVariant)
			}
		case "ItemIsMenu":
			text = fmt.Sprint(t.options.MenuOnActivate)
		case "Menu":
			text = `objectpath "/MenuBar"`
		}
	} else {
		switch nativeString(property) {
		case "Version":
			text = "uint32 3"
		case "TextDirection":
			text = `"ltr"`
		case "Status":
			text = `"normal"`
		case "IconThemePath":
			text = "@as []"
		}
	}
	if text == "" {
		if failure != nil {
			gdbus.SetErrorLiteral(failure, gdbus.DBusErrorQuark(), 0, "unknown property")
		}
		return 0
	}
	return gdbus.Parse(0, text, 0, 0, failure)
}

// newTray 导出对象后在后台注册宿主，通过队列移交全部原生资源所有权以消除取消竞态
// 入参: ctx 调用上下文, options 托盘配置
// 返回: *Tray 托盘句柄, error 初始化错误
func (n *nativeWindow) newTray(ctx context.Context, options TrayOptions) (*Tray, error) {
	options, err := prepareTray(options, n.options.Title, n.iconData)
	if err != nil {
		return nil, err
	}
	pixmap, err := trayPixmap(options.Icon)
	if err != nil {
		return nil, err
	}
	if err := loadGDBus(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTrayUnavailable, err)
	}
	pixmapVar, tooltipVar, err := trayBuildVariants(options, pixmap)
	if err != nil {
		return nil, err
	}
	connection, err := trayConnection(ctx)
	if err != nil {
		gdbus.Unref(pixmapVar)
		gdbus.Unref(tooltipVar)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrTrayUnavailable, err)
	}
	defer gtk.Unref(connection)
	owner, err := trayHost(ctx, connection)
	if err != nil {
		gdbus.CloseAsync(connection, 0, 0, 0)
		gdbus.Unref(pixmapVar)
		gdbus.Unref(tooltipVar)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrTrayUnavailable, err)
	}
	t := &linuxTray{owner: n, id: uintptr(linuxTrayIDs.Add(1)), ids: make(map[string]int32)}
	t.updateMenu(options)
	t.handle = &Tray{setOptions: t.setOptions, close: t.close}
	connRef := gtk.Ref(connection)
	releasePending := func() {
		gdbus.CloseAsync(connRef, 0, 0, 0)
		gtk.Unref(connRef)
		gdbus.Unref(pixmapVar)
		gdbus.Unref(tooltipVar)
	}
	exportDone := make(chan error, 1)
	if err = n.enqueueCleanup(func() {
		if err := ctx.Err(); err != nil {
			releasePending()
			exportDone <- err
			return
		}
		if n.closed || t.closed {
			releasePending()
			exportDone <- ErrClosed
			return
		}
		t.pixmapVariant = pixmapVar
		t.tooltipVariant = tooltipVar
		exportErr := t.export(connRef)
		gtk.Unref(connRef)
		exportDone <- exportErr
	}, func() {
		releasePending()
		exportDone <- ErrClosed
	}); err != nil {
		releasePending()
		return nil, err
	}
	select {
	case exportErr := <-exportDone:
		if exportErr != nil {
			return nil, exportErr
		}
	case <-ctx.Done():
		_ = t.handle.Close()
		return nil, ctx.Err()
	}
	reply, callErr := trayCall(ctx, connection, owner, "/StatusNotifierWatcher", trayWatcher, "RegisterStatusNotifierItem", `("/StatusNotifierItem",)`)
	if reply != 0 {
		gdbus.Unref(reply)
	}
	if callErr == nil {
		var current string
		current, callErr = trayHost(ctx, connection)
		if callErr == nil && current != owner {
			callErr = errors.New("desktop tray host changed during registration")
		}
	}
	if callErr != nil {
		_ = t.handle.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrTrayUnavailable, callErr)
	}
	confirmDone := make(chan error, 1)
	if err = n.enqueueCleanup(func() {
		if err := ctx.Err(); err != nil {
			confirmDone <- err
			return
		}
		if n.closed {
			confirmDone <- ErrClosed
			return
		}
		if t.closed || t.connection == 0 || gdbus.Closed(t.connection) != 0 {
			confirmDone <- ErrTrayUnavailable
			return
		}
		t.handle.available.Store(true)
		confirmDone <- nil
	}, func() { confirmDone <- ErrClosed }); err != nil {
		_ = t.handle.Close()
		return nil, err
	}
	select {
	case err = <-confirmDone:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		_ = t.handle.Close()
		return nil, err
	}
	return t.handle, nil
}

// checkHostOffline 异步复查监视服务是否已无在线托盘宿主，核对世代号后再恢复窗口
// 入参: gen 触发复查时的宿主变更世代号
func (t *linuxTray) checkHostOffline(gen uint64) {
	if t.closed || t.connection == 0 {
		return
	}
	connection := gtk.Ref(t.connection)
	go func() {
		defer gtk.Unref(connection)
		if t.owner.ctx.Err() != nil || t.handle.closed.Load() {
			return
		}
		if _, err := trayHost(t.owner.ctx, connection); err == nil {
			return
		}
		if t.owner.ctx.Err() != nil {
			return
		}
		_ = t.owner.enqueue(func() {
			if !t.closed && t.connection == connection && gen == t.hostGeneration {
				t.handle.available.Store(false)
				_ = t.owner.activate()
			}
		})
	}()
}

// reregister 在桌面托盘宿主重启或重新上线后异步重新向监视服务注册，保留并发期间到达的重试信号
// 入参: gen 触发重新注册时的宿主变更世代号
func (t *linuxTray) reregister(gen uint64) {
	if t.closed || t.connection == 0 {
		return
	}
	t.reregisterGen.Store(gen)
	t.reregisterPending.Store(true)
	if !t.reregistering.CompareAndSwap(false, true) {
		return
	}
	connection := gtk.Ref(t.connection)
	go func() {
		defer gtk.Unref(connection)
		for {
			t.reregisterPending.Store(false)
			currentGen := t.reregisterGen.Load()
			if t.owner.ctx.Err() != nil || t.handle.closed.Load() || gdbus.Closed(connection) != 0 {
				t.reregistering.Store(false)
				return
			}
			owner, err := trayHost(t.owner.ctx, connection)
			if err == nil {
				reply, callErr := trayCall(t.owner.ctx, connection, owner, "/StatusNotifierWatcher", trayWatcher, "RegisterStatusNotifierItem", `("/StatusNotifierItem",)`)
				if reply != 0 {
					gdbus.Unref(reply)
				}
				if callErr == nil {
					_ = t.owner.enqueue(func() {
						if !t.closed && t.connection == connection && currentGen == t.hostGeneration && gdbus.Closed(connection) == 0 {
							t.handle.available.Store(true)
						}
					})
				}
			}
			if t.reregisterPending.Load() {
				continue
			}
			t.reregistering.Store(false)
			if !t.reregisterPending.Load() || !t.reregistering.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

// runTrayCallback 在后台Goroutine执行托盘回调并隔离异常
// 入参: fn 业务回调
func (t *linuxTray) runTrayCallback(fn func()) {
	owner := t.owner
	go func() {
		defer func() {
			if v := recover(); v != nil {
				owner.reportError(fmt.Errorf("tray callback panic: %v", v))
			}
		}()
		fn()
	}()
}

// setOptions 通过队列移交新值所有权并在GTK线程替换配置，信号失败时回滚状态且不回退已发revision
// 入参: ctx 调用上下文, options 完整新配置
// 返回: error 配置或通信错误
func (t *linuxTray) setOptions(ctx context.Context, options TrayOptions) error {
	options, err := prepareTray(options, t.owner.options.Title, t.owner.iconData)
	if err != nil {
		return err
	}
	pixmap, err := trayPixmap(options.Icon)
	if err != nil {
		return err
	}
	newPixmapVar, newTooltipVar, err := trayBuildVariants(options, pixmap)
	if err != nil {
		return err
	}
	freeNew := func() {
		gdbus.Unref(newPixmapVar)
		gdbus.Unref(newTooltipVar)
	}
	done := make(chan error, 1)
	if err = t.owner.enqueueCleanup(func() {
		if err := ctx.Err(); err != nil {
			freeNew()
			done <- err
			return
		}
		if t.closed {
			freeNew()
			done <- ErrClosed
			return
		}
		oldOptions := t.options
		oldIDs := t.ids
		oldItems := t.items
		oldOrder := t.order
		oldChildren := t.children
		oldParents := t.parents
		oldNextID := t.nextID
		oldRevision := t.revision
		oldPixmapVar := t.pixmapVariant
		oldTooltipVar := t.tooltipVariant
		t.updateMenu(options)
		t.pixmapVariant = newPixmapVar
		t.tooltipVariant = newTooltipVar
		layoutSent := false
		rollback := func() {
			t.options = oldOptions
			t.ids = oldIDs
			t.items = oldItems
			t.order = oldOrder
			t.children = oldChildren
			t.parents = oldParents
			t.nextID = oldNextID
			if !layoutSent {
				t.revision = oldRevision
			}
			t.pixmapVariant = oldPixmapVar
			t.tooltipVariant = oldTooltipVar
			freeNew()
		}
		for _, signal := range []string{"NewTitle", "NewIcon", "NewToolTip"} {
			if err := trayEmit(t.connection, "/StatusNotifierItem", trayNotifierInterface, signal, "()"); err != nil {
				rollback()
				done <- err
				return
			}
		}
		if err := trayEmit(t.connection, "/StatusNotifierItem", "org.freedesktop.DBus.Properties", "PropertiesChanged", fmt.Sprintf("(%s, {'ItemIsMenu': <%t>}, @as [])", trayQuote(trayNotifierInterface), options.MenuOnActivate)); err != nil {
			rollback()
			done <- err
			return
		}
		if err := trayEmit(t.connection, "/MenuBar", trayMenuInterface, "LayoutUpdated", fmt.Sprintf("(uint32 %d, int32 0)", t.revision)); err != nil {
			rollback()
			done <- err
			return
		}
		layoutSent = true
		properties := make([]string, 0, len(t.order))
		for _, id := range t.order {
			properties = append(properties, fmt.Sprintf("(int32 %d,%s)", id, t.itemProperties(id, nil)))
		}
		if err := trayEmit(t.connection, "/MenuBar", trayMenuInterface, "ItemsPropertiesUpdated", "(@a(ia{sv}) ["+strings.Join(properties, ",")+"], @a(ias) [])"); err != nil {
			rollback()
			done <- err
			return
		}
		if oldPixmapVar != 0 {
			gdbus.Unref(oldPixmapVar)
		}
		if oldTooltipVar != 0 {
			gdbus.Unref(oldTooltipVar)
		}
		done <- nil
	}, func() {
		freeNew()
		done <- ErrClosed
	}); err != nil {
		freeNew()
		return err
	}
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close 同步在GTK线程释放托盘对象与连接
// 返回: error 投递或关闭错误
func (t *linuxTray) close() error {
	done := make(chan error, 1)
	if err := t.owner.enqueueCleanup(func() {
		t.release()
		done <- nil
	}, func() { done <- ErrClosed }); err != nil {
		return err
	}
	return <-done
}

// export 在GTK线程注册对象和信号，确保GDBus回调使用GTK主上下文
// 入参: connection 私有总线连接
// 返回: error 注册错误
func (t *linuxTray) export(connection uintptr) (err error) {
	t.connection = gtk.Ref(connection)
	defer func() {
		if err != nil {
			t.release()
		}
	}()
	if t.owner.trays == nil {
		t.owner.trays = make(map[uintptr]*linuxTray)
	}
	t.owner.trays[t.id] = t
	linuxTrays.Store(t.id, t)
	var failure *linuxError
	node := gdbus.Node(linuxTrayXML, &failure)
	if node == 0 {
		return nativeError(failure)
	}
	defer gdbus.NodeUnref(node)
	for _, entry := range []struct{ path, name string }{{"/StatusNotifierItem", trayNotifierInterface}, {"/MenuBar", trayMenuInterface}} {
		id := gdbus.Register(connection, entry.path, gdbus.Interface(node, entry.name), &linuxTrayVTable, t.id, 0, &failure)
		if id == 0 {
			return nativeError(failure)
		}
		t.registrations = append(t.registrations, id)
	}
	watcher := append([]byte(trayWatcher), 0)
	t.subscription = gdbus.Subscribe(connection, trayBusInterface, trayBusInterface, "NameOwnerChanged", trayBusPath, &watcher[0], 0, linuxTraySignal, t.id, 0)
	t.hostUnregSubscription = gdbus.Subscribe(connection, trayWatcher, trayWatcher, "StatusNotifierHostUnregistered", "/StatusNotifierWatcher", nil, 0, linuxTraySignal, t.id, 0)
	t.hostRegSubscription = gdbus.Subscribe(connection, trayWatcher, trayWatcher, "StatusNotifierHostRegistered", "/StatusNotifierWatcher", nil, 0, linuxTraySignal, t.id, 0)
	t.closedSignal = gtk.Connect(connection, "closed", linuxTrayClosedSignal, t.id, 0, 0)
	return nil
}

// release 在GTK线程注销对象并异步关闭私有连接，释放缓存的GVariant，可重复调用
func (t *linuxTray) release() {
	if t.closed {
		return
	}
	t.closed = true
	t.handle.closed.Store(true)
	delete(t.owner.trays, t.id)
	t.handle.available.Store(false)
	linuxTrays.Delete(t.id)
	if t.pixmapVariant != 0 {
		gdbus.Unref(t.pixmapVariant)
		t.pixmapVariant = 0
	}
	if t.tooltipVariant != 0 {
		gdbus.Unref(t.tooltipVariant)
		t.tooltipVariant = 0
	}
	if t.connection != 0 {
		if t.closedSignal != 0 {
			gtk.SignalDisconnect(t.connection, t.closedSignal)
		}
		if t.subscription != 0 {
			gdbus.Unsubscribe(t.connection, t.subscription)
		}
		if t.hostUnregSubscription != 0 {
			gdbus.Unsubscribe(t.connection, t.hostUnregSubscription)
		}
		if t.hostRegSubscription != 0 {
			gdbus.Unsubscribe(t.connection, t.hostRegSubscription)
		}
		for _, id := range t.registrations {
			gdbus.Unregister(t.connection, id)
		}
		gdbus.CloseAsync(t.connection, 0, 0, 0)
		gtk.Unref(t.connection)
		t.connection = 0
	}
}
