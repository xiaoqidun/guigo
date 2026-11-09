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

// linuxTray 的配置与原生字段仅由GTK线程访问
type linuxTray struct {
	owner            *nativeWindow
	options          TrayOptions
	handle           *Tray
	id, connection   uintptr
	registrations    []uint32
	subscription     uint32
	hostSubscription uint32
	closedSignal     uint64
	closed           bool
	pixmap           string
	ids              map[string]int32
	items            map[int32]TrayItem
	order            []int32
	children         map[int32][]int32
	parents          map[int32]int32
	nextID           int32
	revision         uint32
}

// linuxTrays 按数字标识保存原生回调对应的托盘
var linuxTrays sync.Map

// linuxTrayIDs 分配独立托盘标识
var linuxTrayIDs atomic.Uint64

// linuxTrayVTable 保存总线对象回调表
var linuxTrayVTable [11]uintptr

// linuxTraySignal 接收宿主变更通知
var linuxTraySignal uintptr

// linuxTrayClosedSignal 接收连接关闭通知
var linuxTrayClosedSignal uintptr

// newTray 导出对象后在后台注册宿主，避免宿主回查属性时阻塞GTK回调
// 入参: options 托盘配置
// 返回: *Tray 托盘句柄, error 初始化错误
func (n *nativeWindow) newTray(options TrayOptions) (*Tray, error) {
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
	connection, err := trayConnection()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTrayUnavailable, err)
	}
	defer gtk.Unref(connection)
	owner, err := trayHost(connection)
	if err != nil {
		gdbus.Close(connection, 0, nil)
		return nil, fmt.Errorf("%w: %w", ErrTrayUnavailable, err)
	}
	t := &linuxTray{owner: n, pixmap: pixmap, id: uintptr(linuxTrayIDs.Add(1)), ids: make(map[string]int32)}
	t.updateMenu(options)
	t.handle = &Tray{setOptions: t.setOptions, close: func() error { return t.onThread(t.release) }}
	var exportErr error
	err = t.onThread(func() { exportErr = t.export(connection) })
	if err != nil {
		gdbus.Close(connection, 0, nil)
		return nil, err
	}
	if exportErr != nil {
		return nil, exportErr
	}
	reply, callErr := trayCall(connection, owner, "/StatusNotifierWatcher", trayWatcher, "RegisterStatusNotifierItem", `("/StatusNotifierItem",)`)
	if reply != 0 {
		gdbus.Unref(reply)
	}
	if callErr == nil {
		var current string
		current, callErr = trayHost(connection)
		if callErr == nil && current != owner {
			callErr = errors.New("desktop tray host changed during registration")
		}
	}
	if callErr != nil {
		t.handle.Close()
		return nil, fmt.Errorf("%w: %w", ErrTrayUnavailable, callErr)
	}
	var stopped bool
	if err = t.onThread(func() {
		stopped = t.closed || gdbus.Closed(connection) != 0
		if !stopped {
			t.handle.available.Store(true)
		}
	}); err != nil || stopped {
		t.handle.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrTrayUnavailable
	}
	return t.handle, nil
}

// setOptions 在GTK线程替换配置并通知宿主重新读取属性与菜单
// 入参: options 完整新配置
// 返回: error 配置或通信错误
func (t *linuxTray) setOptions(options TrayOptions) error {
	options, err := prepareTray(options, t.owner.options.Title, t.owner.iconData)
	if err != nil {
		return err
	}
	pixmap, err := trayPixmap(options.Icon)
	if err != nil {
		return err
	}
	var updateErr error
	err = t.onThread(func() {
		if t.closed {
			updateErr = ErrClosed
			return
		}
		t.updateMenu(options)
		t.pixmap = pixmap
		for _, signal := range []string{"NewTitle", "NewIcon", "NewToolTip"} {
			if updateErr = trayEmit(t.connection, "/StatusNotifierItem", trayNotifierInterface, signal, "()"); updateErr != nil {
				return
			}
		}
		if updateErr = trayEmit(t.connection, "/StatusNotifierItem", "org.freedesktop.DBus.Properties", "PropertiesChanged", fmt.Sprintf("(%s, {'ItemIsMenu': <%t>}, @as [])", trayQuote(trayNotifierInterface), options.MenuOnActivate)); updateErr != nil {
			return
		}
		if updateErr = trayEmit(t.connection, "/MenuBar", trayMenuInterface, "LayoutUpdated", fmt.Sprintf("(uint32 %d, int32 0)", t.revision)); updateErr != nil {
			return
		}
		properties := make([]string, 0, len(t.order))
		for _, id := range t.order {
			properties = append(properties, fmt.Sprintf("(int32 %d,%s)", id, t.itemProperties(id, nil)))
		}
		updateErr = trayEmit(t.connection, "/MenuBar", trayMenuInterface, "ItemsPropertiesUpdated", "(@a(ia{sv}) ["+strings.Join(properties, ",")+"], @a(ias) [])")
	})
	if err != nil {
		return err
	}
	return updateErr
}

// trayPixmap 将PNG缩放为托盘常用尺寸，按网络字节序生成ARGB像素
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
	var text strings.Builder
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
				pixel := color.NRGBAModel.Convert(img.At(x*img.Bounds().Dx()/size, y*img.Bounds().Dy()/size)).(color.NRGBA)
				fmt.Fprintf(&text, "0x%02x,0x%02x,0x%02x,0x%02x", pixel.A, pixel.R, pixel.G, pixel.B)
			}
		}
		text.WriteString("])")
	}
	text.WriteByte(']')
	return text.String(), nil
}

// iconName 仅在未指定PNG时使用通用应用图标
// 返回: string 桌面图标名称
func (t *linuxTray) iconName() string {
	if len(t.options.Icon) == 0 {
		return "application-x-executable"
	}
	return ""
}

// onThread 同步执行GTK资源操作，窗口关闭时不遗留等待者
// 入参: fn 原生资源操作
// 返回: error 请求投递或窗口关闭错误
func (t *linuxTray) onThread(fn func()) error {
	done := make(chan error, 1)
	if err := t.owner.enqueueCleanup(func() {
		fn()
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
	t.hostSubscription = gdbus.Subscribe(connection, trayWatcher, trayWatcher, "StatusNotifierHostUnregistered", "/StatusNotifierWatcher", nil, 0, linuxTraySignal, t.id, 0)
	t.closedSignal = gtk.Connect(connection, "closed", linuxTrayClosedSignal, t.id, 0, 0)
	return nil
}

// release 在GTK线程注销对象并释放私有连接，可重复调用
func (t *linuxTray) release() {
	if t.closed {
		return
	}
	t.closed = true
	t.handle.closed.Store(true)
	delete(t.owner.trays, t.id)
	t.handle.available.Store(false)
	linuxTrays.Delete(t.id)
	if t.connection != 0 {
		if t.closedSignal != 0 {
			gtk.SignalDisconnect(t.connection, t.closedSignal)
		}
		if t.subscription != 0 {
			gdbus.Unsubscribe(t.connection, t.subscription)
		}
		if t.hostSubscription != 0 {
			gdbus.Unsubscribe(t.connection, t.hostSubscription)
		}
		for _, id := range t.registrations {
			gdbus.Unregister(t.connection, id)
		}
		gdbus.Close(t.connection, 0, nil)
		gtk.Unref(t.connection)
		t.connection = 0
	}
}

// linuxTrayHostChanged 在宿主注销或监视服务丢失时恢复窗口，避免用户失去入口
// 入参: connection 总线连接, sender 发送者, path 对象路径, iface 接口, signal 信号名, params 信号参数, data 托盘标识
func linuxTrayHostChanged(connection uintptr, sender, path, iface, signal *byte, params, data uintptr) {
	if nativeString(signal) == "NameOwnerChanged" && trayString(params, 1) == "" {
		return
	}
	if value, ok := linuxTrays.Load(data); ok {
		t := value.(*linuxTray)
		t.release()
		t.owner.activate()
	}
}

// linuxTrayConnectionClosed 可能由GIO工作线程触发，只排队操作GTK资源
// 入参: connection 总线连接, vanished 远端断开标志, failure 原生错误, data 托盘标识
func linuxTrayConnectionClosed(connection uintptr, vanished int32, failure, data uintptr) {
	if value, ok := linuxTrays.Load(data); ok {
		t := value.(*linuxTray)
		t.owner.enqueue(func() {
			if !t.closed {
				t.release()
				t.owner.activate()
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
			go t.options.OnActivate()
		}
	} else if nativeString(method) == "SecondaryActivate" {
		if t.options.OnSecondaryActivate != nil {
			go t.options.OnSecondaryActivate()
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

// linuxTrayProperty 返回完整GVariant引用，所有权交给GDBus
// 入参: connection 总线连接, sender 发送者, path 对象路径, iface 接口, property 属性名, failure 原生错误输出, data 托盘标识
// 返回: uintptr 属性值
func linuxTrayProperty(connection uintptr, sender, path, iface, property *byte, failure **linuxError, data uintptr) uintptr {
	value, ok := linuxTrays.Load(data)
	if !ok {
		return 0
	}
	t := value.(*linuxTray)
	var text string
	if nativeString(iface) == trayNotifierInterface {
		switch nativeString(property) {
		case "Category":
			text = `"ApplicationStatus"`
		case "Id":
			text = trayQuote(fmt.Sprintf("%s-%d", t.owner.options.AppID, t.id))
		case "Title":
			text = trayQuote(t.options.Title)
		case "Status":
			text = `"Active"`
		case "IconName":
			text = trayQuote(t.iconName())
		case "IconPixmap":
			text = t.pixmap
		case "ToolTip":
			text = fmt.Sprintf("(%s, %s, %s, \"\")", trayQuote(t.iconName()), t.pixmap, trayQuote(t.options.Title))
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
	return gdbus.Parse(0, text, 0, 0, failure)
}
