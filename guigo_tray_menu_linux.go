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
	"errors"
	"fmt"
	"math"
	"strings"
)

// trayNames 读取菜单属性过滤列表
// 入参: params 请求参数, index 列表所在位置
// 返回: map[string]bool 属性名集合
func trayNames(params uintptr, index uintptr) map[string]bool {
	values := trayChild(params, index)
	if values == 0 {
		return map[string]bool{}
	}
	defer gdbus.Unref(values)
	names := map[string]bool{}
	if gdbus.IsContainer(values) == 0 {
		return names
	}
	for i := uintptr(0); i < gdbus.Count(values); i++ {
		if name, ok := trayString(values, i); ok {
			names[name] = true
		}
	}
	return names
}

// allocTrayID 分配非零且当前未占用的32位正整数菜单项标识，防止int32溢出
// 入参: used 当前菜单树已占用的标识集合
// 返回: int32 新分配的菜单项标识
func (t *linuxTray) allocTrayID(used map[int32]bool) int32 {
	for {
		if t.nextID >= math.MaxInt32 {
			t.nextID = 1
		} else {
			t.nextID++
		}
		if t.nextID > 0 && !used[t.nextID] {
			used[t.nextID] = true
			return t.nextID
		}
	}
}

// updateMenu 保留现有菜单项的原生标识，移除的标识不再复用，避免旧事件选择新项
// 入参: options 完整新配置
func (t *linuxTray) updateMenu(options TrayOptions) {
	ids := make(map[string]int32, len(options.Items))
	used := make(map[int32]bool, len(options.Items))
	var collectUsed func([]TrayItem)
	collectUsed = func(items []TrayItem) {
		for _, item := range items {
			if item.ID != "" {
				if id, ok := t.ids[item.ID]; ok && id > 0 {
					used[id] = true
				}
			}
			collectUsed(item.Children)
		}
	}
	collectUsed(options.Items)
	t.items = make(map[int32]TrayItem, len(options.Items))
	t.order = make([]int32, 0, len(options.Items))
	t.children = make(map[int32][]int32)
	t.parents = make(map[int32]int32)
	var visit func([]TrayItem, int32)
	visit = func(items []TrayItem, parent int32) {
		for _, item := range items {
			id, ok := t.ids[item.ID]
			if !ok || item.ID == "" || id <= 0 {
				id = t.allocTrayID(used)
			}
			if item.ID != "" {
				ids[item.ID] = id
			}
			t.items[id] = item
			t.order = append(t.order, id)
			t.children[parent] = append(t.children[parent], id)
			t.parents[id] = parent
			visit(item.Children, id)
		}
	}
	visit(options.Items, 0)
	t.ids = ids
	t.options = options
	t.revision++
}

// itemProperties 根据请求的属性名生成菜单属性
// 入参: id 菜单标识, names 属性名过滤，空值返回全部属性
// 返回: string GVariant属性字典
func (t *linuxTray) itemProperties(id int32, names map[string]bool) string {
	fields := []string{}
	for _, name := range []string{"type", "label", "enabled", "visible", "toggle-type", "toggle-state", "children-display"} {
		if len(names) == 0 || names[name] {
			if value, ok := t.itemProperty(id, name); ok {
				fields = append(fields, trayQuote(name)+": <"+value+">")
			}
		}
	}
	return "@a{sv} {" + strings.Join(fields, ",") + "}"
}

// itemProperty 返回菜单属性值，显式传递默认值以清除宿主缓存的旧状态
// 入参: id 菜单标识, name 属性名
// 返回: string GVariant值, bool 属性是否存在
func (t *linuxTray) itemProperty(id int32, name string) (string, bool) {
	if id == 0 && name == "children-display" {
		if len(t.children[0]) != 0 {
			return `"submenu"`, true
		}
		return `""`, true
	}
	item, ok := t.items[id]
	if !ok {
		return "", false
	}
	switch name {
	case "type":
		if item.Separator {
			return `"separator"`, true
		}
		return `"standard"`, true
	case "label":
		return trayQuote(strings.ReplaceAll(item.Label, "_", "__")), true
	case "enabled":
		return fmt.Sprint(!item.Disabled && !item.Separator), true
	case "visible":
		return fmt.Sprint(!item.Hidden), true
	case "toggle-type":
		if item.Checked != nil {
			if item.Radio {
				return `"radio"`, true
			}
			return `"checkmark"`, true
		}
		return `""`, true
	case "toggle-state":
		if item.Checked != nil && *item.Checked {
			return "int32 1", true
		}
		return "int32 0", true
	case "children-display":
		if len(item.Children) != 0 {
			return `"submenu"`, true
		}
		return `""`, true
	}
	return "", false
}

// menuLayout 按请求深度生成菜单树，负数表示全部层级
// 入参: id 菜单标识, depth 子菜单深度, names 属性名过滤
// 返回: string GVariant菜单树
func (t *linuxTray) menuLayout(id, depth int32, names map[string]bool) string {
	children := []string{}
	if depth != 0 {
		nextDepth := depth - 1
		if depth < 0 {
			nextDepth = -1
		}
		for _, child := range t.children[id] {
			children = append(children, "<"+t.menuLayout(child, nextDepth, names)+">")
		}
	}
	return fmt.Sprintf("(int32 %d, %s, @av [%s])", id, t.itemProperties(id, names), strings.Join(children, ","))
}

// click 将菜单动作交给调用方的后台回调并隔离异常
// 入参: id 菜单标识, event 菜单事件
// 返回: bool 菜单项是否存在
func (t *linuxTray) click(id int32, event string) bool {
	item, ok := t.items[id]
	if !ok {
		return false
	}
	if item.Separator || len(item.Children) != 0 {
		return true
	}
	for current := id; current != 0; current = t.parents[current] {
		ancestor := t.items[current]
		if ancestor.Disabled || ancestor.Hidden {
			return true
		}
	}
	if event == "clicked" && t.options.OnSelect != nil {
		onSelect := t.options.OnSelect
		itemID := item.ID
		t.runTrayCallback(func() { onSelect(itemID) })
	}
	return true
}

// menuReply 处理DBusMenu请求，参数类型已由GDBus按XML验证
// 入参: method 方法名, params 请求参数
// 返回: string GVariant响应文本, error 请求错误
func (t *linuxTray) menuReply(method string, params uintptr) (string, error) {
	switch method {
	case "GetLayout":
		id, okID := trayInt(params, 0)
		depth, okDepth := trayInt(params, 1)
		if !okID || !okDepth {
			return "", errors.New("invalid layout parameters")
		}
		if _, ok := t.items[id]; id != 0 && !ok {
			return "", errors.New("unknown menu item")
		}
		names := trayNames(params, 2)
		return fmt.Sprintf("(uint32 %d, %s)", t.revision, t.menuLayout(id, depth, names)), nil
	case "GetGroupProperties":
		ids := trayChild(params, 0)
		if ids == 0 || gdbus.IsContainer(ids) == 0 {
			if ids != 0 {
				gdbus.Unref(ids)
			}
			return "", errors.New("invalid group parameters")
		}
		defer gdbus.Unref(ids)
		names := trayNames(params, 1)
		items := []string{}
		requested := []int32{}
		for i := uintptr(0); i < gdbus.Count(ids); i++ {
			if id, ok := trayInt(ids, i); ok {
				requested = append(requested, id)
			}
		}
		if len(requested) == 0 {
			requested = append([]int32{0}, t.order...)
		}
		for _, id := range requested {
			if _, ok := t.items[id]; id == 0 || ok {
				items = append(items, fmt.Sprintf("(int32 %d,%s)", id, t.itemProperties(id, names)))
			}
		}
		return "(@a(ia{sv}) [" + strings.Join(items, ",") + "],)", nil
	case "GetProperty":
		id, okID := trayInt(params, 0)
		name, okName := trayString(params, 1)
		if !okID || !okName {
			return "", errors.New("invalid property parameters")
		}
		if value, ok := t.itemProperty(id, name); ok {
			return "(<" + value + ">,)", nil
		}
		return "", errors.New("unknown menu property")
	case "Event":
		id, okID := trayInt(params, 0)
		event, okEvent := trayString(params, 1)
		if !okID || !okEvent {
			return "", errors.New("invalid event parameters")
		}
		if !t.click(id, event) {
			return "", errors.New("unknown menu item")
		}
		return "()", nil
	case "EventGroup":
		events := trayChild(params, 0)
		if events == 0 || gdbus.IsContainer(events) == 0 {
			if events != 0 {
				gdbus.Unref(events)
			}
			return "", errors.New("invalid event group parameters")
		}
		defer gdbus.Unref(events)
		rejected := []string{}
		for i := uintptr(0); i < gdbus.Count(events); i++ {
			event := trayChild(events, i)
			if event == 0 {
				continue
			}
			id, okID := trayInt(event, 0)
			name, okName := trayString(event, 1)
			if okID && okName && !t.click(id, name) {
				rejected = append(rejected, fmt.Sprint(id))
			}
			gdbus.Unref(event)
		}
		return "(@ai [" + strings.Join(rejected, ",") + "],)", nil
	case "AboutToShow":
		id, ok := trayInt(params, 0)
		if !ok {
			return "", errors.New("invalid menu parameters")
		}
		if _, exists := t.items[id]; id != 0 && !exists {
			return "", errors.New("unknown menu item")
		}
		return "(false,)", nil
	case "AboutToShowGroup":
		ids := trayChild(params, 0)
		if ids == 0 || gdbus.IsContainer(ids) == 0 {
			if ids != 0 {
				gdbus.Unref(ids)
			}
			return "", errors.New("invalid group parameters")
		}
		defer gdbus.Unref(ids)
		rejected := []string{}
		for i := uintptr(0); i < gdbus.Count(ids); i++ {
			if id, ok := trayInt(ids, i); ok {
				if _, exists := t.items[id]; id != 0 && !exists {
					rejected = append(rejected, fmt.Sprint(id))
				}
			}
		}
		return "(@ai [], @ai [" + strings.Join(rejected, ",") + "])", nil
	}
	return "", errors.New("unknown method")
}
