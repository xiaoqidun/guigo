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
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// ErrTrayUnavailable 表示当前桌面没有可用托盘宿主
var ErrTrayUnavailable = errors.New("system tray is unavailable")

// TrayItem 定义托盘菜单项，支持子菜单、分隔线及由调用方维护的选中状态
type TrayItem struct {
	// ID 指定菜单项标识，在整个菜单树内唯一，由OnSelect接收，分隔线可省略
	ID string
	// Label 指定菜单项显示文字，按原文显示，不解释为助记键标记
	Label string
	// Disabled 禁止选择当前菜单项
	Disabled bool
	// Hidden 隐藏当前菜单项及其子菜单
	Hidden bool
	// Separator 将当前项显示为分隔线，不设置文字、选中状态及子菜单
	Separator bool
	// Checked 指定选中状态，nil表示普通菜单项，点击后由调用方通过SetOptions更新
	Checked *bool
	// Radio 将选中标记显示为单选圆点，需要同时设置Checked，分组互斥由调用方维护
	Radio bool
	// Children 指定子菜单，非空时当前项仅打开子菜单，不触发OnSelect
	Children []TrayItem
}

// TrayOptions 定义托盘图标、菜单和交互回调
type TrayOptions struct {
	// Title 指定托盘标题和提示文字，空值使用创建窗口时的标题，Windows提示最多127个UTF16编码单元
	Title string
	// Icon 指定PNG图标数据，与IconPath互斥，格式及尺寸限制与Options.Icon一致
	// Icon和IconPath均为空时使用创建窗口时的图标，窗口也未指定时使用系统默认图标
	Icon []byte
	// IconPath 指定外置PNG图标路径，调用时读取，之后不依赖该文件
	IconPath string
	// Items 指定菜单树，空值表示没有菜单
	Items []TrayItem
	// MenuOnActivate 在主激活时打开菜单而不调用OnActivate，桌面也可能将所有点击统一为菜单操作
	MenuOnActivate bool
	// OnSelect 在后台协程接收启用菜单项的ID，多次回调可能并发执行
	OnSelect func(string)
	// OnActivate 在后台协程接收系统激活操作，多次回调可能并发执行
	OnActivate func()
	// OnSecondaryActivate 接收次要激活操作，如鼠标中键，具体手势由桌面决定
	// 回调在后台协程执行，多次回调可能并发执行
	OnSecondaryActivate func()
}

// Tray 持有窗口所属的系统托盘，可由后台协程操作，随窗口销毁而释放
// 托盘宿主丢失且无法恢复时窗口会重新显示，调用方可用Available查询当前状态
type Tray struct {
	available  atomic.Bool
	closed     atomic.Bool
	once       sync.Once
	close      func() error
	setOptions func(TrayOptions) error
	err        error
}

// NewTray 为当前窗口创建独立托盘，可创建多个，随窗口销毁而释放
// Windows使用系统通知区域，Linux需要支持StatusNotifierItem和DBusMenu的桌面宿主
// 无可用宿主时返回ErrTrayUnavailable，未适配的平台返回ErrUnsupportedPlatform
// 入参: options 托盘配置，图标及菜单数据在返回前复制
// 返回: *Tray 托盘句柄, error 创建错误
func (w *Window) NewTray(options TrayOptions) (*Tray, error) {
	if w == nil || w.native == nil {
		return nil, ErrClosed
	}
	return w.native.newTray(options)
}

// Available 查询托盘是否已注册且尚未关闭，不表示当前图标一定展开显示
// 返回: bool 托盘当前是否可用
func (t *Tray) Available() bool {
	return t != nil && !t.closed.Load() && t.available.Load()
}

// SetOptions 完整替换托盘配置，包括回调，零值含义与NewTray相同
// 更新时可关闭已展开的菜单，已开始执行的回调不受影响
// 入参: options 新托盘配置，图标及菜单数据在返回前复制
// 返回: error 配置或更新错误，已关闭时返回ErrClosed
func (t *Tray) SetOptions(options TrayOptions) error {
	if t == nil || t.setOptions == nil || t.closed.Load() {
		return ErrClosed
	}
	return t.setOptions(options)
}

// Close 移除托盘，不关闭窗口，重复调用或窗口已销毁时返回成功
// 已开始执行的回调不受影响
// 返回: error 移除错误
func (t *Tray) Close() error {
	if t == nil {
		return nil
	}
	t.once.Do(func() {
		t.closed.Store(true)
		t.available.Store(false)
		if t.close != nil {
			t.err = t.close()
			if errors.Is(t.err, ErrClosed) {
				t.err = nil
			}
		}
	})
	return t.err
}

// prepareTray 校验并复制配置，统一使用窗口标题及图标作为默认值
// 入参: options 托盘配置, title 默认标题, icon 默认PNG数据
// 返回: TrayOptions 独立配置快照, error 配置错误
func prepareTray(options TrayOptions, title string, icon []byte) (TrayOptions, error) {
	if options.Title == "" {
		options.Title = title
	}
	if !utf8.ValidString(options.Title) || strings.ContainsRune(options.Title, 0) {
		return TrayOptions{}, errors.New("invalid tray title")
	}
	if len(options.Icon) != 0 && options.IconPath != "" {
		return TrayOptions{}, errors.New("Icon and IconPath are mutually exclusive")
	}
	if len(options.Icon) == 0 && options.IconPath == "" {
		options.Icon = icon
	}
	data, err := iconData(Options{Icon: options.Icon, IconPath: options.IconPath})
	if err != nil {
		return TrayOptions{}, err
	}
	options.Icon = append([]byte(nil), data...)
	options.IconPath = ""
	options.Items, err = prepareTrayItems(options.Items, make(map[string]bool))
	return options, err
}

// prepareTrayItems 复制菜单树并检查标识与原生菜单约束
// 入参: items 菜单项, seen 当前菜单树已使用的标识
// 返回: []TrayItem 独立菜单快照, error 配置错误
func prepareTrayItems(items []TrayItem, seen map[string]bool) ([]TrayItem, error) {
	result := make([]TrayItem, len(items))
	for i, item := range items {
		if item.ID != "" {
			if seen[item.ID] {
				return nil, fmt.Errorf("duplicate tray item ID: %q", item.ID)
			}
			seen[item.ID] = true
		}
		if item.Separator {
			if item.Label != "" || item.Checked != nil || item.Radio || len(item.Children) != 0 {
				return nil, errors.New("tray separator cannot have text, selection or children")
			}
		} else if item.ID == "" || item.Label == "" || !utf8.ValidString(item.Label) || strings.ContainsRune(item.Label, 0) || item.Radio && item.Checked == nil {
			return nil, fmt.Errorf("invalid tray item: %q", item.ID)
		}
		if item.Checked != nil {
			checked := *item.Checked
			item.Checked = &checked
		}
		var err error
		item.Children, err = prepareTrayItems(item.Children, seen)
		if err != nil {
			return nil, err
		}
		result[i] = item
	}
	return result, nil
}
