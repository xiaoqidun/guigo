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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// 托盘总线原生入口与按需初始化状态
var (
	gdbus      gdbusAPI
	gdbusOnce  sync.Once
	gdbusError error
)

// gdbusAPI 保存系统GIO和GLib的定长参数入口，避免C可变参数调用
type gdbusAPI struct {
	Address         func(int32, uintptr, **linuxError) *byte                                                                      `symbol:"g_dbus_address_get_for_bus_sync"`
	Connect         func(string, uint32, uintptr, uintptr, **linuxError) uintptr                                                  `symbol:"g_dbus_connection_new_for_address_sync"`
	ExitOnClose     func(uintptr, int32)                                                                                          `symbol:"g_dbus_connection_set_exit_on_close"`
	Closed          func(uintptr) int32                                                                                           `symbol:"g_dbus_connection_is_closed"`
	CloseAsync      func(uintptr, uintptr, uintptr, uintptr)                                                                      `symbol:"g_dbus_connection_close"`
	Call            func(uintptr, string, string, string, string, uintptr, uintptr, uint32, int32, uintptr, **linuxError) uintptr `symbol:"g_dbus_connection_call_sync"`
	Node            func(string, **linuxError) uintptr                                                                            `symbol:"g_dbus_node_info_new_for_xml"`
	NodeUnref       func(uintptr)                                                                                                 `symbol:"g_dbus_node_info_unref"`
	Interface       func(uintptr, string) uintptr                                                                                 `symbol:"g_dbus_node_info_lookup_interface"`
	Register        func(uintptr, string, uintptr, *[11]uintptr, uintptr, uintptr, **linuxError) uint32                           `symbol:"g_dbus_connection_register_object"`
	Unregister      func(uintptr, uint32) int32                                                                                   `symbol:"g_dbus_connection_unregister_object"`
	Subscribe       func(uintptr, string, string, string, string, *byte, uint32, uintptr, uintptr, uintptr) uint32                `symbol:"g_dbus_connection_signal_subscribe"`
	Unsubscribe     func(uintptr, uint32)                                                                                         `symbol:"g_dbus_connection_signal_unsubscribe"`
	Emit            func(uintptr, uintptr, string, string, string, uintptr, **linuxError) int32                                   `symbol:"g_dbus_connection_emit_signal"`
	Return          func(uintptr, uintptr)                                                                                        `symbol:"g_dbus_method_invocation_return_value"`
	ReturnError     func(uintptr, string, string)                                                                                 `symbol:"g_dbus_method_invocation_return_dbus_error"`
	DBusErrorQuark  func() uint32                                                                                                 `symbol:"g_dbus_error_quark"`
	SetErrorLiteral func(**linuxError, uint32, int32, string)                                                                     `symbol:"g_set_error_literal"`
	Parse           func(uintptr, string, uintptr, uintptr, **linuxError) uintptr                                                 `symbol:"g_variant_parse"`
	VariantRef      func(uintptr) uintptr                                                                                         `symbol:"g_variant_ref"`
	Unref           func(uintptr)                                                                                                 `symbol:"g_variant_unref"`
	TypeString      func(uintptr) *byte                                                                                           `symbol:"g_variant_get_type_string"`
	IsContainer     func(uintptr) int32                                                                                           `symbol:"g_variant_is_container"`
	Child           func(uintptr, uintptr) uintptr                                                                                `symbol:"g_variant_get_child_value"`
	Count           func(uintptr) uintptr                                                                                         `symbol:"g_variant_n_children"`
	Int             func(uintptr) int32                                                                                           `symbol:"g_variant_get_int32"`
	Boolean         func(uintptr) int32                                                                                           `symbol:"g_variant_get_boolean"`
	Variant         func(uintptr) uintptr                                                                                         `symbol:"g_variant_get_variant"`
	String          func(uintptr, uintptr) *byte                                                                                  `symbol:"g_variant_get_string"`
}

// loadGDBus 按需加载系统GIO与GLib动态库并绑定托盘总线符号及原生回调
// 返回: error 动态库或符号加载错误
func loadGDBus() error {
	gdbusOnce.Do(func() {
		names := []string{"libgio-2.0.so.0", "libglib-2.0.so.0"}
		var handles []uintptr
		for _, name := range names {
			handle, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
			if err != nil {
				for i := len(handles) - 1; i >= 0; i-- {
					_ = purego.Dlclose(handles[i])
				}
				gdbusError = err
				return
			}
			handles = append(handles, handle)
		}
		var api gdbusAPI
		value := reflect.ValueOf(&api).Elem()
		for i := 0; i < value.NumField(); i++ {
			name := value.Type().Field(i).Tag.Get("symbol")
			var address uintptr
			for _, handle := range handles {
				address, _ = purego.Dlsym(handle, name)
				if address != 0 {
					break
				}
			}
			if address == 0 {
				for j := len(handles) - 1; j >= 0; j-- {
					_ = purego.Dlclose(handles[j])
				}
				gdbusError = fmt.Errorf("GDBus: missing %s", name)
				return
			}
			purego.RegisterFunc(value.Field(i).Addr().Interface(), address)
		}
		gdbus = api
		linuxTrayVTable[0] = purego.NewCallback(linuxTrayMethod)
		linuxTrayVTable[1] = purego.NewCallback(linuxTrayProperty)
		linuxTraySignal = purego.NewCallback(linuxTrayHostChanged)
		linuxTrayClosedSignal = purego.NewCallback(linuxTrayConnectionClosed)
	})
	return gdbusError
}

// trayQuote 使用JSON字符串转义，防止菜单内容改变GVariant文本结构
// 入参: text 原始文字
// 返回: string 带引号的转义文字
func trayQuote(text string) string {
	value, _ := json.Marshal(text)
	return string(value)
}

// trayVariant 解析内部生成的类型明确的GVariant文本，调用者持有一个完整引用
// 入参: text GVariant文本，动态字符串须经过trayQuote转义
// 返回: uintptr 原生值, error 解析错误
func trayVariant(text string) (uintptr, error) {
	var failure *linuxError
	value := gdbus.Parse(0, text, 0, 0, &failure)
	if value == 0 {
		return 0, nativeError(failure)
	}
	return value, nil
}

// trayCancellable 创建与上下文关联的原生取消对象并返回清理操作
// 入参: ctx 调用上下文
// 返回: uintptr 原生取消对象, func() 释放清理函数
func trayCancellable(ctx context.Context) (uintptr, func()) {
	if ctx.Done() == nil {
		return 0, func() {}
	}
	cancel := gtk.CancelNew()
	if cancel == 0 {
		return 0, func() {}
	}
	var mu sync.Mutex
	released := false
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		if !released {
			gtk.Cancel(cancel)
		}
		mu.Unlock()
	})
	return cancel, func() {
		stop()
		mu.Lock()
		released = true
		mu.Unlock()
		gtk.Unref(cancel)
	}
}

// trayCall 在非GTK线程执行支持上下文取消的总线调用，调用者释放返回值
// 入参: ctx 调用上下文, connection 总线连接, destination 总线名, path 对象路径, iface 接口, method 方法, args 参数文本
// 返回: uintptr 响应, error 远端或通信错误
func trayCall(ctx context.Context, connection uintptr, destination, path, iface, method, args string) (uintptr, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if connection == 0 {
		return 0, errors.New("invalid DBus connection")
	}
	params, err := trayVariant(args)
	if err != nil {
		return 0, err
	}
	defer gdbus.Unref(params)
	cancel, cleanup := trayCancellable(ctx)
	defer cleanup()
	var failure *linuxError
	value := gdbus.Call(connection, destination, path, iface, method, params, 0, 0, 3000, cancel, &failure)
	if value == 0 {
		if ctx.Err() != nil {
			_ = nativeError(failure)
			return 0, ctx.Err()
		}
		if err := nativeError(failure); err != nil {
			return 0, err
		}
		return 0, errors.New("empty DBus reply")
	}
	return value, nil
}

// trayEmit 发送属性或菜单变更信号
// 入参: connection 总线连接, path 对象路径, iface 接口, signal 信号名, args 参数文本
// 返回: error 参数或通信错误
func trayEmit(connection uintptr, path, iface, signal, args string) error {
	params, err := trayVariant(args)
	if err != nil {
		return err
	}
	defer gdbus.Unref(params)
	var failure *linuxError
	if gdbus.Emit(connection, 0, path, iface, signal, params, &failure) == 0 {
		return nativeError(failure)
	}
	return nil
}

// trayType 读取非零GVariant的类型签名字符串
// 入参: value 原生GVariant指针
// 返回: string 类型签名
func trayType(value uintptr) string {
	if value == 0 {
		return ""
	}
	return nativeString(gdbus.TypeString(value))
}

// trayChild 在校验非零、容器类型与索引越界后读取子值，调用者释放返回值
// 入参: value 容器GVariant, index 子值位置
// 返回: uintptr 子值指针，无效时返回0
func trayChild(value, index uintptr) uintptr {
	if value == 0 || gdbus.IsContainer(value) == 0 || index >= gdbus.Count(value) {
		return 0
	}
	return gdbus.Child(value, index)
}

// trayString 安全读取容器中的字符串子值并校验类型
// 入参: value 容器GVariant, index 子值位置
// 返回: string 字符串内容, bool 是否成功读取
func trayString(value, index uintptr) (string, bool) {
	child := trayChild(value, index)
	if child == 0 {
		return "", false
	}
	defer gdbus.Unref(child)
	switch trayType(child) {
	case "s", "o", "g":
		return nativeString(gdbus.String(child, 0)), true
	default:
		return "", false
	}
}

// trayInt 安全读取容器中的32位整数子值并校验类型
// 入参: value 容器GVariant, index 子值位置
// 返回: int32 整数值, bool 是否成功读取
func trayInt(value, index uintptr) (int32, bool) {
	child := trayChild(value, index)
	if child == 0 {
		return 0, false
	}
	defer gdbus.Unref(child)
	if trayType(child) != "i" {
		return 0, false
	}
	return gdbus.Int(child), true
}

// trayConnection 创建独立会话总线连接，关闭托盘不会影响GTK共享连接
// 入参: ctx 调用上下文
// 返回: uintptr 调用方拥有的连接引用, error 连接错误
func trayConnection(ctx context.Context) (uintptr, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	cancel, cleanup := trayCancellable(ctx)
	defer cleanup()
	var failure *linuxError
	address := gdbus.Address(2, cancel, &failure)
	if address == nil {
		if ctx.Err() != nil {
			_ = nativeError(failure)
			return 0, ctx.Err()
		}
		return 0, nativeError(failure)
	}
	defer gtk.Free(unsafe.Pointer(address))
	connection := gdbus.Connect(nativeString(address), 9, 0, cancel, &failure)
	if connection == 0 {
		if ctx.Err() != nil {
			_ = nativeError(failure)
			return 0, ctx.Err()
		}
		return 0, nativeError(failure)
	}
	gdbus.ExitOnClose(connection, 0)
	return connection, nil
}

// trayHost 确认已注册托盘宿主并获取监视服务唯一总线名
// 入参: ctx 调用上下文, connection 总线连接
// 返回: string 宿主总线名, error 通信错误
func trayHost(ctx context.Context, connection uintptr) (string, error) {
	reply, err := trayCall(ctx, connection, trayBusInterface, trayBusPath, trayBusInterface, "GetNameOwner", `("org.kde.StatusNotifierWatcher",)`)
	if err != nil {
		return "", err
	}
	defer gdbus.Unref(reply)
	owner, ok := trayString(reply, 0)
	if !ok || owner == "" {
		return "", errors.New("invalid DBus name owner reply")
	}
	property, err := trayCall(ctx, connection, owner, "/StatusNotifierWatcher", "org.freedesktop.DBus.Properties", "Get", `("org.kde.StatusNotifierWatcher", "IsStatusNotifierHostRegistered")`)
	if err != nil {
		return "", err
	}
	defer gdbus.Unref(property)
	child := trayChild(property, 0)
	if child == 0 {
		return "", errors.New("invalid DBus property reply")
	}
	defer gdbus.Unref(child)
	if trayType(child) != "v" {
		return "", errors.New("unexpected DBus property type")
	}
	value := gdbus.Variant(child)
	if value == 0 {
		return "", errors.New("empty DBus variant value")
	}
	defer gdbus.Unref(value)
	if trayType(value) != "b" {
		return "", errors.New("unexpected tray host state type")
	}
	if gdbus.Boolean(value) == 0 {
		return "", ErrTrayUnavailable
	}
	return owner, nil
}
