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
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// guid 解析系统接口标识
// 入参: text 标识文本
// 返回: syscall.GUID 接口标识
func guid(text string) syscall.GUID {
	var value syscall.GUID
	ole32.NewProc("CLSIDFromString").Call(uintptr(unsafe.Pointer(wide(text))), uintptr(unsafe.Pointer(&value)))
	return value
}

// dialogDefaultLocation 归一化对话框默认目录与文件名，忽略不存在的目录
// 入参: defaultPath 默认路径, defaultName 默认文件名
// 返回: string 存在的绝对目录, string 默认文件名
func dialogDefaultLocation(defaultPath, defaultName string) (string, string) {
	if defaultPath == "" {
		return "", defaultName
	}
	abs, err := filepath.Abs(defaultPath)
	if err != nil {
		return "", defaultName
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return abs, defaultName
	}
	name := defaultName
	if name == "" {
		if base := filepath.Base(abs); base != "" && base != "." && base != string(filepath.Separator) {
			name = base
		}
	}
	parent := filepath.Dir(abs)
	if info, err := os.Stat(parent); err == nil && info.IsDir() {
		return parent, name
	}
	return "", name
}

// shellPath 获取原生文件系统路径
// 入参: item 文件系统对象
// 返回: string 文件路径, error 读取错误
func shellPath(item *comObject) (string, error) {
	var value *uint16
	if err := hresult(com(item, 5, 0x80058000, uintptr(unsafe.Pointer(&value)))); err != nil {
		return "", err
	}
	defer ole32.NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(value)))
	return windows.UTF16PtrToString(value), nil
}

// dialog 运行单结果原生对话框
// 入参: ctx 调用上下文, o 对话框选项, mode 对话框类型
// 返回: string 文件路径, error 对话框错误
func (n *nativeWindow) dialog(ctx context.Context, o DialogOptions, mode int) (string, error) {
	paths, err := n.dialogs(ctx, o, mode)
	if err != nil {
		return "", err
	}
	return paths[0], nil
}

// dialogs 在宿主线程显示IFileDialog并响应上下文取消
// 入参: ctx 调用上下文, o 对话框选项, mode 对话框类型
// 返回: []string 文件路径, error 对话框错误
func (n *nativeWindow) dialogs(ctx context.Context, o DialogOptions, mode int) (paths []string, err error) {
	err = n.invokeTop(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.activeDialog != nil {
			return ErrDialogBusy
		}
		clsid := guid("{DC1C5A9C-E88A-4DDE-A5A1-60F82A20AEF7}")
		iid := guid("{D57C7288-D4AD-4768-BE02-9D969532D960}")
		if mode == 1 {
			clsid = guid("{C0B4E2F3-BA21-4773-8DBA-335EC946EB8B}")
			iid = guid("{84BCCD23-5FDE-4CDB-AEA4-AF64B83D78AB}")
		}
		var dialog *comObject
		hr, _, _ := ole32.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&clsid)), 0, 1, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&dialog)))
		if err := hresult(hr); err != nil {
			return err
		}
		defer release(dialog)
		flags := uintptr(0x40 | 0x800 | 0x02000000 | 8)
		switch mode {
		case 0:
			flags |= 0x1000
		case 1:
			flags |= 2
		case 2:
			flags |= 0x20
		case 3:
			flags |= 0x1000 | 0x200
		}
		if err := hresult(com(dialog, 9, flags)); err != nil {
			return err
		}
		if o.Title != "" {
			if err := hresult(com(dialog, 17, uintptr(unsafe.Pointer(wide(o.Title))))); err != nil {
				return err
			}
		}
		type filter struct {
			Name    *uint16
			Pattern *uint16
		}
		filters := make([]filter, len(o.Filters))
		for i, f := range o.Filters {
			filters[i] = filter{wide(f.Name), wide(f.Pattern)}
		}
		if len(filters) > 0 && mode != 2 {
			if err := hresult(com(dialog, 4, uintptr(len(filters)), uintptr(unsafe.Pointer(&filters[0])))); err != nil {
				return err
			}
		}
		folder, name := dialogDefaultLocation(o.DefaultPath, o.DefaultName)
		if name != "" && mode != 2 {
			if err := hresult(com(dialog, 15, uintptr(unsafe.Pointer(wide(name))))); err != nil {
				return err
			}
		}
		if folder != "" {
			var item *comObject
			iid := guid("{43826D1E-E718-42EE-BC55-A1E261C37BFE}")
			hr, _, _ := shell32.NewProc("SHCreateItemFromParsingName").Call(uintptr(unsafe.Pointer(wide(folder))), 0, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&item)))
			if hresult(hr) == nil && item != nil {
				_ = com(dialog, 12, uintptr(unsafe.Pointer(item)))
				release(item)
			}
		}
		if n.ctx.Err() != nil || n.quitAfterModal || n.closed {
			return ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		n.activeDialog = dialog
		n.modalActive++
		stop := context.AfterFunc(ctx, func() {
			_ = n.enqueue(func() {
				if n.activeDialog == dialog {
					com(dialog, 23, 0x800704c7)
				}
			})
		})
		defer func() {
			stop()
			n.activeDialog = nil
			n.leaveModal()
		}()
		hr = com(dialog, 3, n.hwnd)
		runtime.KeepAlive(filters)
		if uint32(hr) == 0x800704c7 {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n.ctx.Err() != nil || n.quitAfterModal || n.closed {
				return ErrClosed
			}
			return ErrCanceled
		}
		if err := hresult(hr); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if mode == 3 {
			var array *comObject
			if err := hresult(com(dialog, 27, uintptr(unsafe.Pointer(&array)))); err != nil {
				return err
			}
			defer release(array)
			var count uint32
			if err := hresult(com(array, 7, uintptr(unsafe.Pointer(&count)))); err != nil {
				return err
			}
			for i := uint32(0); i < count; i++ {
				var item *comObject
				if err := hresult(com(array, 8, uintptr(i), uintptr(unsafe.Pointer(&item)))); err != nil {
					return err
				}
				path, err := shellPath(item)
				release(item)
				if err != nil {
					return err
				}
				paths = append(paths, path)
			}
		} else {
			var item *comObject
			if err := hresult(com(dialog, 20, uintptr(unsafe.Pointer(&item)))); err != nil {
				return err
			}
			defer release(item)
			path, err := shellPath(item)
			if err != nil {
				return err
			}
			paths = []string{path}
		}
		return nil
	})
	return
}
