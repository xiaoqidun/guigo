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
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// menuItemInfo 对应Windows菜单项结构
type menuItemInfo struct {
	Size, Mask, Type, State, ID       uint32
	SubMenu, Checked, Unchecked, Data uintptr
	Text                              *uint16
	Length                            uint32
	Bitmap                            uintptr
}

// createTrayMenu 创建原生菜单树，由调用方销毁返回的根菜单
// 入参: items 菜单项, commands 命令与菜单项映射, disabled 祖先是否禁用
// 返回: uintptr 根菜单, error 创建错误
func createTrayMenu(items []TrayItem, commands map[uint32]TrayItem, disabled bool) (uintptr, error) {
	menu, _, err := user32.NewProc("CreatePopupMenu").Call()
	if menu == 0 {
		return 0, fmt.Errorf("create tray menu: %w", err)
	}
	for _, item := range items {
		if item.Hidden {
			continue
		}
		info := menuItemInfo{Mask: 0x143}
		info.Size = uint32(unsafe.Sizeof(info))
		info.Text = wide(strings.ReplaceAll(item.Label, "&", "&&"))
		item.Disabled = item.Disabled || disabled
		if item.Disabled {
			info.State |= 3
		}
		if item.Checked != nil && *item.Checked {
			info.State |= 8
		}
		if item.Radio {
			info.Type |= 0x200
		}
		if item.Separator {
			info.Type |= 0x800
		} else if len(item.Children) != 0 {
			info.Mask |= 4
			info.SubMenu, err = createTrayMenu(item.Children, commands, item.Disabled)
			if err != nil {
				user32.NewProc("DestroyMenu").Call(menu)
				return 0, err
			}
		} else {
			info.ID = uint32(len(commands) + 1)
			commands[info.ID] = item
		}
		ok, _, err := user32.NewProc("InsertMenuItemW").Call(menu, 0xffffffff, 1, uintptr(unsafe.Pointer(&info)))
		runtime.KeepAlive(info)
		if ok == 0 {
			if info.SubMenu != 0 {
				user32.NewProc("DestroyMenu").Call(info.SubMenu)
			}
			user32.NewProc("DestroyMenu").Call(menu)
			return 0, fmt.Errorf("insert tray menu item: %w", err)
		}
	}
	return menu, nil
}

// showMenu 在最外层消息循环显示托盘菜单，菜单展开期间允许关闭及更新托盘
// 入参: position 通知消息携带的屏幕坐标
// 返回: error 菜单创建错误
func (t *windowsTray) showMenu(position uintptr) error {
	n := t.owner
	if n.trays[t.data.ID] != t || n.ctx.Err() != nil || n.activeTrayMenu != nil {
		return nil
	}
	options, revision := t.options, t.revision
	commands := make(map[uint32]TrayItem)
	menu, err := createTrayMenu(options.Items, commands, false)
	if err != nil {
		return err
	}
	defer user32.NewProc("DestroyMenu").Call(menu)
	count, _, _ := user32.NewProc("GetMenuItemCount").Call(menu)
	if count == 0 {
		return nil
	}
	cursor := point{X: int32(int16(position)), Y: int32(int16(position >> 16))}
	if cursor.X == -1 && cursor.Y == -1 {
		identifier := struct {
			Size   uint32
			Window uintptr
			ID     uint32
			GUID   syscall.GUID
		}{Window: n.hwnd, ID: t.data.ID}
		identifier.Size = uint32(unsafe.Sizeof(identifier))
		var bounds rect
		hr, _, _ := shell32.NewProc("Shell_NotifyIconGetRect").Call(uintptr(unsafe.Pointer(&identifier)), uintptr(unsafe.Pointer(&bounds)))
		if hresult(hr) == nil {
			cursor = point{X: bounds.Left, Y: bounds.Bottom}
		} else {
			user32.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&cursor)))
		}
	}
	n.activeTrayMenu = t
	defer func() { n.activeTrayMenu = nil }()
	flags := uintptr(0x182)
	if alignment, _, _ := user32.NewProc("GetSystemMetrics").Call(40); alignment != 0 {
		flags |= 8
	}
	user32.NewProc("SetForegroundWindow").Call(n.hwnd)
	selected, _, _ := user32.NewProc("TrackPopupMenu").Call(menu, flags, uintptr(cursor.X), uintptr(cursor.Y), 0, n.hwnd, 0)
	user32.NewProc("PostMessageW").Call(n.hwnd, 0, 0, 0)
	if n.trays[t.data.ID] == t && revision == t.revision && n.ctx.Err() == nil {
		if item, ok := commands[uint32(selected)]; ok && !item.Disabled && options.OnSelect != nil {
			go options.OnSelect(item.ID)
		}
		shell32.NewProc("Shell_NotifyIconW").Call(3, uintptr(unsafe.Pointer(&t.data)))
	}
	return nil
}
