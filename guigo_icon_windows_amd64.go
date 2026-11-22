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
	"runtime"
	"unsafe"
)

// destroyIcons 释放窗口独占图标句柄
// 入参: icons 大小图标句柄
func destroyIcons(icons [2]uintptr) {
	for _, icon := range icons {
		if icon != 0 {
			user32.NewProc("DestroyIcon").Call(icon)
		}
	}
}

// updateIcons 按当前DPI更新窗口大小图标
// 返回: error 错误信息
func (n *nativeWindow) updateIcons() error {
	if len(n.iconData) == 0 || n.hwnd == 0 {
		return nil
	}
	var icons [2]uintptr
	for i, metric := range []uintptr{49, 11} {
		size, _, _ := user32.NewProc("GetSystemMetricsForDpi").Call(metric, uintptr(n.dpi))
		if size == 0 {
			destroyIcons(icons)
			return errors.New("cannot determine icon size")
		}
		handle, _, err := user32.NewProc("CreateIconFromResourceEx").Call(
			uintptr(unsafe.Pointer(&n.iconData[0])), uintptr(len(n.iconData)), 1, 0x00030000, size, size, 0,
		)
		runtime.KeepAlive(n.iconData)
		if handle == 0 {
			destroyIcons(icons)
			return err
		}
		icons[i] = handle
	}
	for i, icon := range icons {
		user32.NewProc("SendMessageW").Call(n.hwnd, 0x80, uintptr(i), icon)
	}
	destroyIcons(n.icons)
	n.icons = icons
	return nil
}
