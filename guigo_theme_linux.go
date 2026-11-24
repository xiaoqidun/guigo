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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// 系统外观探测所需的GIO与GLib函数入口及加载状态
var (
	linuxTheme     linuxThemeAPI
	linuxThemeOnce sync.Once
	linuxThemeErr  error
)

// linuxThemeAPI 保存通过门户读取外观偏好的定长参数入口
type linuxThemeAPI struct {
	Address      func(int32, uintptr, **linuxError) *byte                                                                      `symbol:"g_dbus_address_get_for_bus_sync"`
	Connect      func(string, uint32, uintptr, uintptr, **linuxError) uintptr                                                  `symbol:"g_dbus_connection_new_for_address_sync"`
	ExitOnClose  func(uintptr, int32)                                                                                          `symbol:"g_dbus_connection_set_exit_on_close"`
	CloseAsync   func(uintptr, uintptr, uintptr, uintptr)                                                                      `symbol:"g_dbus_connection_close"`
	Call         func(uintptr, string, string, string, string, uintptr, uintptr, uint32, int32, uintptr, **linuxError) uintptr `symbol:"g_dbus_connection_call_sync"`
	CancelNew    func() uintptr                                                                                                `symbol:"g_cancellable_new"`
	Cancel       func(uintptr)                                                                                                 `symbol:"g_cancellable_cancel"`
	ObjectUnref  func(uintptr)                                                                                                 `symbol:"g_object_unref"`
	Free         func(unsafe.Pointer)                                                                                          `symbol:"g_free"`
	ErrorFree    func(*linuxError)                                                                                             `symbol:"g_error_free"`
	Parse        func(uintptr, string, uintptr, uintptr, **linuxError) uintptr                                                 `symbol:"g_variant_parse"`
	VariantUnref func(uintptr)                                                                                                 `symbol:"g_variant_unref"`
	Count        func(uintptr) uintptr                                                                                         `symbol:"g_variant_n_children"`
	Child        func(uintptr, uintptr) uintptr                                                                                `symbol:"g_variant_get_child_value"`
	Variant      func(uintptr) uintptr                                                                                         `symbol:"g_variant_get_variant"`
	TypeString   func(uintptr) *byte                                                                                           `symbol:"g_variant_get_type_string"`
	Uint32       func(uintptr) uint32                                                                                          `symbol:"g_variant_get_uint32"`
}

// systemDark 检测Linux桌面环境是否偏好深色外观
// 返回: bool 是否偏好深色外观
func systemDark() bool {
	if theme := strings.ToLower(strings.TrimSpace(os.Getenv("GTK_THEME"))); theme != "" {
		if strings.Contains(theme, ":light") {
			return false
		}
		if strings.Contains(theme, "dark") {
			return true
		}
	}
	if dark, ok := linuxPortalDark(); ok {
		return dark
	}
	return linuxGTKSettingsDark()
}

// loadLinuxTheme 按需加载GIO与GLib符号供系统外观查询使用
// 返回: error 动态库或符号加载错误
func loadLinuxTheme() error {
	linuxThemeOnce.Do(func() {
		names := []string{"libgio-2.0.so.0", "libgobject-2.0.so.0", "libglib-2.0.so.0"}
		var handles []uintptr
		for _, name := range names {
			handle, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
			if err != nil {
				for i := len(handles) - 1; i >= 0; i-- {
					_ = purego.Dlclose(handles[i])
				}
				linuxThemeErr = err
				return
			}
			handles = append(handles, handle)
		}
		var api linuxThemeAPI
		value := reflect.ValueOf(&api).Elem()
		for i := 0; i < value.NumField(); i++ {
			symbol := value.Type().Field(i).Tag.Get("symbol")
			var address uintptr
			for _, handle := range handles {
				address, _ = purego.Dlsym(handle, symbol)
				if address != 0 {
					break
				}
			}
			if address == 0 {
				for j := len(handles) - 1; j >= 0; j-- {
					_ = purego.Dlclose(handles[j])
				}
				linuxThemeErr = fmt.Errorf("linux theme: missing %s", symbol)
				return
			}
			purego.RegisterFunc(value.Field(i).Addr().Interface(), address)
		}
		linuxTheme = api
	})
	return linuxThemeErr
}

// linuxPortalDark 通过XDG桌面门户查询系统色彩偏好并设置150毫秒取消超时
// 返回: bool 是否偏好深色外观, bool 是否获得明确结果
func linuxPortalDark() (bool, bool) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			return false, false
		}
		if info, err := os.Stat(filepath.Join(runtimeDir, "bus")); err != nil || info.IsDir() {
			return false, false
		}
	}
	if err := loadLinuxTheme(); err != nil {
		return false, false
	}
	cancel := linuxTheme.CancelNew()
	if cancel == 0 {
		return false, false
	}
	var mu sync.Mutex
	done := false
	timer := time.AfterFunc(150*time.Millisecond, func() {
		mu.Lock()
		if !done {
			linuxTheme.Cancel(cancel)
		}
		mu.Unlock()
	})
	defer func() {
		timer.Stop()
		mu.Lock()
		done = true
		mu.Unlock()
		linuxTheme.ObjectUnref(cancel)
	}()
	var failure *linuxError
	address := linuxTheme.Address(2, cancel, &failure)
	if address == nil {
		if failure != nil {
			linuxTheme.ErrorFree(failure)
		}
		return false, false
	}
	addr := nativeString(address)
	linuxTheme.Free(unsafe.Pointer(address))
	conn := linuxTheme.Connect(addr, 9, 0, cancel, &failure)
	if conn == 0 {
		if failure != nil {
			linuxTheme.ErrorFree(failure)
		}
		return false, false
	}
	linuxTheme.ExitOnClose(conn, 0)
	defer func() {
		linuxTheme.CloseAsync(conn, 0, 0, 0)
		linuxTheme.ObjectUnref(conn)
	}()
	params := linuxTheme.Parse(0, `("org.freedesktop.appearance", "color-scheme")`, 0, 0, &failure)
	if params == 0 {
		if failure != nil {
			linuxTheme.ErrorFree(failure)
		}
		return false, false
	}
	defer linuxTheme.VariantUnref(params)
	reply := linuxTheme.Call(conn, "org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop", "org.freedesktop.portal.Settings", "Read", params, 0, 0, 150, cancel, &failure)
	if reply == 0 {
		if failure != nil {
			linuxTheme.ErrorFree(failure)
		}
		return false, false
	}
	defer linuxTheme.VariantUnref(reply)
	if linuxTheme.Count(reply) == 0 {
		return false, false
	}
	cur := linuxTheme.Child(reply, 0)
	if cur == 0 {
		return false, false
	}
	for range 4 {
		if nativeString(linuxTheme.TypeString(cur)) != "v" {
			break
		}
		next := linuxTheme.Variant(cur)
		linuxTheme.VariantUnref(cur)
		if next == 0 {
			return false, false
		}
		cur = next
	}
	defer linuxTheme.VariantUnref(cur)
	if nativeString(linuxTheme.TypeString(cur)) != "u" {
		return false, false
	}
	scheme := linuxTheme.Uint32(cur)
	if scheme == 1 {
		return true, true
	}
	if scheme == 2 {
		return false, true
	}
	return false, false
}

// linuxGTKSettingsDark 读取用户GTK配置文件回退判断深色偏好
// 返回: bool 是否偏好深色外观
func linuxGTKSettingsDark() bool {
	configDir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return false
		}
		configDir = dir
	}
	for _, sub := range []string{"gtk-4.0", "gtk-3.0"} {
		data, err := os.ReadFile(filepath.Join(configDir, sub, "settings.ini"))
		if err != nil {
			continue
		}
		inSettings := false
		hasPreferDark := false
		preferDark := false
		hasTheme := false
		themeDark := false
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				inSettings = strings.EqualFold(strings.TrimSpace(line[1:len(line)-1]), "settings")
				continue
			}
			if !inSettings {
				continue
			}
			key, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.ToLower(strings.TrimSpace(key))
			val = strings.ToLower(strings.TrimSpace(val))
			if key == "gtk-application-prefer-dark-theme" {
				hasPreferDark = true
				preferDark = val == "1" || val == "true"
			} else if key == "gtk-theme-name" && val != "" {
				hasTheme = true
				if strings.Contains(val, ":light") {
					themeDark = false
				} else {
					themeDark = strings.Contains(val, "dark")
				}
			}
		}
		if hasPreferDark {
			return preferDark
		}
		if hasTheme {
			return themeDark
		}
	}
	return false
}
