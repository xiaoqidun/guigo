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
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// ErrTrayUnavailable 表示当前桌面没有可用托盘宿主
var ErrTrayUnavailable = errors.New("system tray is unavailable")

// TrayItem 定义托盘菜单项，支持子菜单、分隔线及由调用方维护的选中状态
// ID在菜单树内唯一(分隔线可省略)，Label按原文显示，Separator不可带文本、选中态或子菜单
// Radio须配合Checked使用；含Children的父项仅用于展开子菜单，不触发OnSelect且不带选中态
type TrayItem struct {
	ID        string
	Label     string
	Disabled  bool
	Hidden    bool
	Separator bool
	Checked   *bool
	Radio     bool
	Children  []TrayItem
}

// TrayOptions 定义托盘图标、菜单和交互回调
// Title为空沿用窗口标题；Icon与IconPath互斥且约束同Options.Icon，均为空沿用窗口或系统默认图标
// MenuOnActivate使主激活优先打开菜单；OnSelect、OnActivate、OnSecondaryActivate均在后台并发回调
type TrayOptions struct {
	Title               string
	Icon                []byte
	IconPath            string
	Items               []TrayItem
	MenuOnActivate      bool
	OnSelect            func(string)
	OnActivate          func()
	OnSecondaryActivate func()
}

// Tray 持有窗口所属的系统托盘，可由后台Goroutine操作，随窗口销毁而释放
// 托盘宿主丢失且无法恢复时窗口会重新显示，调用方可用Available查询当前状态
type Tray struct {
	available  atomic.Bool
	closed     atomic.Bool
	once       sync.Once
	close      func() error
	setOptions func(context.Context, TrayOptions) error
	err        error
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
		return TrayOptions{}, errors.New("icon data and path are mutually exclusive")
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
			if !utf8.ValidString(item.ID) || strings.ContainsRune(item.ID, 0) {
				return nil, fmt.Errorf("invalid tray item ID: %q", item.ID)
			}
			if seen[item.ID] {
				return nil, fmt.Errorf("duplicate tray item ID: %q", item.ID)
			}
			seen[item.ID] = true
		}
		if item.Separator {
			if item.Label != "" || item.Checked != nil || item.Radio || len(item.Children) != 0 {
				return nil, errors.New("tray separator cannot have text, selection or children")
			}
		} else if item.ID == "" || item.Label == "" || !utf8.ValidString(item.Label) || strings.ContainsRune(item.Label, 0) || item.Radio && item.Checked == nil || len(item.Children) != 0 && (item.Checked != nil || item.Radio) {
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

// NewTray 为当前窗口创建独立托盘，可创建多个，随窗口销毁而释放
// Windows使用系统通知区域，Linux需要支持StatusNotifierItem和DBusMenu的桌面宿主
// 无可用宿主时返回ErrTrayUnavailable，未适配的平台返回ErrUnsupportedPlatform
// 入参: ctx 调用上下文, options 托盘配置，图标及菜单数据在返回前复制
// 返回: *Tray 托盘句柄, error 创建错误
func (w *Window) NewTray(ctx context.Context, options TrayOptions) (*Tray, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w == nil || w.native == nil {
		return nil, ErrClosed
	}
	return w.native.newTray(ctx, options)
}

// Available 查询托盘是否已注册且尚未关闭，不表示当前图标一定展开显示
// 返回: bool 托盘当前是否可用
func (t *Tray) Available() bool {
	return t != nil && !t.closed.Load() && t.available.Load()
}

// SetOptions 完整替换托盘配置，包括回调，零值含义与NewTray相同
// 更新时可关闭已展开的菜单，已开始执行的回调不受影响
// 入参: ctx 调用上下文, options 新托盘配置，图标及菜单数据在返回前复制
// 返回: error 配置或更新错误，已关闭时返回ErrClosed
func (t *Tray) SetOptions(ctx context.Context, options TrayOptions) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if t == nil || t.setOptions == nil || t.closed.Load() {
		return ErrClosed
	}
	return t.setOptions(ctx, options)
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
