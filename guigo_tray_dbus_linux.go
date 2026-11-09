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
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// gdbusAPI 保存系统GIO和GLib的定长参数入口，避免C可变参数调用
type gdbusAPI struct {
	Address     func(int32, uintptr, **linuxError) *byte                                                                      `symbol:"g_dbus_address_get_for_bus_sync"`
	Connect     func(string, uint32, uintptr, uintptr, **linuxError) uintptr                                                  `symbol:"g_dbus_connection_new_for_address_sync"`
	ExitOnClose func(uintptr, int32)                                                                                          `symbol:"g_dbus_connection_set_exit_on_close"`
	Closed      func(uintptr) int32                                                                                           `symbol:"g_dbus_connection_is_closed"`
	Close       func(uintptr, uintptr, **linuxError) int32                                                                    `symbol:"g_dbus_connection_close_sync"`
	Call        func(uintptr, string, string, string, string, uintptr, uintptr, uint32, int32, uintptr, **linuxError) uintptr `symbol:"g_dbus_connection_call_sync"`
	Node        func(string, **linuxError) uintptr                                                                            `symbol:"g_dbus_node_info_new_for_xml"`
	NodeUnref   func(uintptr)                                                                                                 `symbol:"g_dbus_node_info_unref"`
	Interface   func(uintptr, string) uintptr                                                                                 `symbol:"g_dbus_node_info_lookup_interface"`
	Register    func(uintptr, string, uintptr, *[11]uintptr, uintptr, uintptr, **linuxError) uint32                           `symbol:"g_dbus_connection_register_object"`
	Unregister  func(uintptr, uint32) int32                                                                                   `symbol:"g_dbus_connection_unregister_object"`
	Subscribe   func(uintptr, string, string, string, string, *byte, uint32, uintptr, uintptr, uintptr) uint32                `symbol:"g_dbus_connection_signal_subscribe"`
	Unsubscribe func(uintptr, uint32)                                                                                         `symbol:"g_dbus_connection_signal_unsubscribe"`
	Emit        func(uintptr, uintptr, string, string, string, uintptr, **linuxError) int32                                   `symbol:"g_dbus_connection_emit_signal"`
	Return      func(uintptr, uintptr)                                                                                        `symbol:"g_dbus_method_invocation_return_value"`
	ReturnError func(uintptr, string, string)                                                                                 `symbol:"g_dbus_method_invocation_return_dbus_error"`
	Parse       func(uintptr, string, uintptr, uintptr, **linuxError) uintptr                                                 `symbol:"g_variant_parse"`
	Unref       func(uintptr)                                                                                                 `symbol:"g_variant_unref"`
	Child       func(uintptr, uintptr) uintptr                                                                                `symbol:"g_variant_get_child_value"`
	Count       func(uintptr) uintptr                                                                                         `symbol:"g_variant_n_children"`
	Int         func(uintptr) int32                                                                                           `symbol:"g_variant_get_int32"`
	Boolean     func(uintptr) int32                                                                                           `symbol:"g_variant_get_boolean"`
	Variant     func(uintptr) uintptr                                                                                         `symbol:"g_variant_get_variant"`
	String      func(uintptr, uintptr) *byte                                                                                  `symbol:"g_variant_get_string"`
}

// gdbus 保存托盘总线入口
var gdbus gdbusAPI

// gdbusOnce 保证托盘总线入口和回调只初始化一次
var gdbusOnce sync.Once

// gdbusError 保存托盘总线加载错误
var gdbusError error

// loadGDBus 按需绑定GTK运行时已有的GIO，不引入额外Go模块或系统组件
// 返回: error 动态库或符号加载错误
func loadGDBus() error {
	gdbusOnce.Do(func() {
		handle, err := purego.Dlopen("libgio-2.0.so.0", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			gdbusError = err
			return
		}
		value := reflect.ValueOf(&gdbus).Elem()
		for i := 0; i < value.NumField(); i++ {
			name := value.Type().Field(i).Tag.Get("symbol")
			address, err := purego.Dlsym(handle, name)
			if err != nil {
				gdbusError = fmt.Errorf("GDBus: %s: %w", name, err)
				purego.Dlclose(handle)
				return
			}
			purego.RegisterFunc(value.Field(i).Addr().Interface(), address)
		}
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

// trayCall 在非GTK线程执行有超时的总线调用，调用者释放返回值
// 入参: connection 总线连接, destination 总线名, path 对象路径, iface 接口, method 方法, args 参数文本
// 返回: uintptr 响应, error 远端或通信错误
func trayCall(connection uintptr, destination, path, iface, method, args string) (uintptr, error) {
	params, err := trayVariant(args)
	if err != nil {
		return 0, err
	}
	defer gdbus.Unref(params)
	var failure *linuxError
	value := gdbus.Call(connection, destination, path, iface, method, params, 0, 0, 3000, 0, &failure)
	if value == 0 {
		return 0, nativeError(failure)
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

// trayString 复制元组中的字符串并释放临时子值
// 入参: value 元组, index 子值位置
// 返回: string 字符串内容
func trayString(value uintptr, index uintptr) string {
	child := gdbus.Child(value, index)
	defer gdbus.Unref(child)
	return nativeString(gdbus.String(child, 0))
}

// trayInt 读取元组中的32位整数并释放临时子值
// 入参: value 元组, index 子值位置
// 返回: int32 整数值
func trayInt(value uintptr, index uintptr) int32 {
	child := gdbus.Child(value, index)
	defer gdbus.Unref(child)
	return gdbus.Int(child)
}

// trayConnection 创建独立会话总线连接，关闭托盘不会影响GTK共享连接
// 返回: uintptr 调用方拥有的连接引用, error 连接错误
func trayConnection() (uintptr, error) {
	var failure *linuxError
	address := gdbus.Address(2, 0, &failure)
	if address == nil {
		return 0, nativeError(failure)
	}
	defer gtk.Free(unsafe.Pointer(address))
	connection := gdbus.Connect(nativeString(address), 9, 0, 0, &failure)
	if connection == 0 {
		return 0, nativeError(failure)
	}
	gdbus.ExitOnClose(connection, 0)
	return connection, nil
}

// trayHost 确认已注册托盘宿主并获取监视服务唯一总线名
// 入参: connection 总线连接
// 返回: string 宿主总线名, error 通信错误
func trayHost(connection uintptr) (string, error) {
	reply, err := trayCall(connection, trayBusInterface, trayBusPath, trayBusInterface, "GetNameOwner", `("org.kde.StatusNotifierWatcher",)`)
	if err != nil {
		return "", err
	}
	defer gdbus.Unref(reply)
	owner := trayString(reply, 0)
	property, err := trayCall(connection, owner, "/StatusNotifierWatcher", "org.freedesktop.DBus.Properties", "Get", `("org.kde.StatusNotifierWatcher", "IsStatusNotifierHostRegistered")`)
	if err != nil {
		return "", err
	}
	defer gdbus.Unref(property)
	child := gdbus.Child(property, 0)
	defer gdbus.Unref(child)
	value := gdbus.Variant(child)
	defer gdbus.Unref(value)
	if gdbus.Boolean(value) == 0 {
		return "", ErrTrayUnavailable
	}
	return owner, nil
}
