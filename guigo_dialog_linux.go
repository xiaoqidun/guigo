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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unsafe"
)

// linuxDialogIDs 为原生异步对话框分配进程级递增标识
var linuxDialogIDs atomic.Uint64

// linuxDialog 保存异步对话框状态
type linuxDialog struct {
	object, cancel uintptr
	mode           int
	ctx            context.Context
	stop           func() bool
	result         chan linuxDialogResult
}

// linuxDialogResult 保存文件路径及结束原因
type linuxDialogResult struct {
	paths []string
	err   error
}

// linuxDialogFinished 完成原生异步对话框并读取本地路径
// 入参: object 对话框对象, result 异步结果, id 对话框标识
func linuxDialogFinished(object, result, id uintptr) {
	n := active.Load()
	if n == nil {
		return
	}
	dialog := n.dialogsPending[id]
	if dialog == nil || dialog.object != object {
		return
	}
	finish := gtk.OpenFinish
	switch dialog.mode {
	case 1:
		finish = gtk.SaveFinish
	case 2:
		finish = gtk.DirectoryFinish
	case 3:
		finish = gtk.MultipleFinish
	}
	var nativeErr *linuxError
	value := finish(object, result, &nativeErr)
	if value == 0 {
		canceled := false
		if nativeErr != nil {
			domain := nativeErr.Domain
			code := nativeErr.Code
			canceled = domain == gtk.DialogErrorQuark() && (code == 1 || code == 2) || domain == gtk.IOErrorQuark() && code == 19
		}
		err := nativeError(nativeErr)
		if dialog.ctx.Err() != nil {
			err = dialog.ctx.Err()
		} else if n.ctx.Err() != nil {
			err = ErrClosed
		} else if err == nil || canceled {
			err = ErrCanceled
		}
		n.finishDialog(id, nil, err)
		return
	}
	defer gtk.Unref(value)
	if err := dialog.ctx.Err(); err != nil {
		n.finishDialog(id, nil, err)
		return
	}
	if n.ctx.Err() != nil {
		n.finishDialog(id, nil, ErrClosed)
		return
	}
	var paths []string
	var err error
	appendFile := func(file uintptr) {
		path := gtk.FilePath(file)
		if path == nil {
			err = errors.New("selected file has no local path")
			return
		}
		paths = append(paths, nativeString(path))
		gtk.Free(unsafe.Pointer(path))
	}
	if dialog.mode == 3 {
		count := gtk.ListCount(value)
		for i := uint32(0); i < count && err == nil; i++ {
			file := gtk.ListItem(value, i)
			appendFile(file)
			gtk.Unref(file)
		}
	} else {
		appendFile(value)
	}
	if err != nil {
		paths = nil
	} else if len(paths) == 0 {
		err = ErrCanceled
	}
	n.finishDialog(id, paths, err)
}

// linuxExistingDir 沿路径向上查找首个存在的目录
// 入参: path 起始绝对路径
// 返回: string 已存在的目录路径，未找到返回空字符串
func linuxExistingDir(path string) string {
	current := filepath.Clean(path)
	for {
		if info, err := os.Stat(current); err == nil && info.IsDir() {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

// linuxFilterSuffix 提取简单通配符模式中的文件扩展名后缀
// 入参: pattern 单个通配符模式
// 返回: string 扩展名后缀, bool 是否为简单扩展名模式
func linuxFilterSuffix(pattern string) (string, bool) {
	suffix, ok := strings.CutPrefix(pattern, "*.")
	if !ok || suffix == "" || strings.ContainsAny(suffix, "*?[]/\\.") {
		return "", false
	}
	return suffix, true
}

// dialog 选择单个文件或目录
// 入参: ctx 调用上下文, o 对话框选项, mode 对话框类型
// 返回: string 文件路径, error 对话框错误
func (n *nativeWindow) dialog(ctx context.Context, o DialogOptions, mode int) (string, error) {
	paths, err := n.dialogs(ctx, o, mode)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", ErrCanceled
	}
	return paths[0], nil
}

// dialogs 在GTK线程显示异步文件对话框，限制单窗口并发并响应上下文取消
// 入参: ctx 调用上下文, o 对话框选项, mode 对话框类型
// 返回: []string 文件路径, error 对话框错误
func (n *nativeWindow) dialogs(ctx context.Context, o DialogOptions, mode int) ([]string, error) {
	if n.isClosed() || n.ctx.Err() != nil {
		return nil, ErrClosed
	}
	result := make(chan linuxDialogResult, 1)
	err := n.enqueueCleanup(func() {
		if err := ctx.Err(); err != nil {
			result <- linuxDialogResult{err: err}
			return
		}
		if n.ctx.Err() != nil {
			result <- linuxDialogResult{err: ErrClosed}
			return
		}
		if len(n.dialogsPending) > 0 {
			result <- linuxDialogResult{err: ErrDialogBusy}
			return
		}
		id := uintptr(linuxDialogIDs.Add(1))
		dialog := &linuxDialog{object: gtk.DialogNew(), cancel: gtk.CancelNew(), mode: mode, ctx: ctx, result: result}
		dialog.stop = context.AfterFunc(ctx, func() {
			_ = n.enqueue(func() {
				if activeDialog := n.dialogsPending[id]; activeDialog == dialog {
					gtk.Cancel(activeDialog.cancel)
				}
			})
		})
		n.dialogsPending[id] = dialog
		gtk.DialogModal(dialog.object, 1)
		if o.Title != "" {
			gtk.DialogTitle(dialog.object, o.Title)
		}
		if o.DefaultPath != "" {
			path, err := filepath.Abs(o.DefaultPath)
			if err != nil {
				n.finishDialog(id, nil, err)
				return
			}
			info, statErr := os.Stat(path)
			if mode == 2 {
				if dir := linuxExistingDir(path); dir != "" {
					folder := gtk.FileNew(dir)
					gtk.DialogFolder(dialog.object, folder)
					gtk.Unref(folder)
				}
			} else if statErr == nil && info.IsDir() {
				folder := gtk.FileNew(path)
				gtk.DialogFolder(dialog.object, folder)
				gtk.Unref(folder)
			} else if mode == 1 {
				if dir := linuxExistingDir(filepath.Dir(path)); dir != "" {
					parent := gtk.FileNew(dir)
					gtk.DialogFolder(dialog.object, parent)
					gtk.Unref(parent)
				}
				if o.DefaultName == "" {
					gtk.DialogName(dialog.object, filepath.Base(path))
				}
			} else {
				file := gtk.FileNew(path)
				gtk.DialogFile(dialog.object, file)
				gtk.Unref(file)
			}
		}
		if o.DefaultName != "" && mode != 2 {
			gtk.DialogName(dialog.object, o.DefaultName)
		}
		if len(o.Filters) > 0 && mode != 2 {
			list := gtk.ListNew(gtk.FilterType())
			var firstFilter uintptr
			for _, f := range o.Filters {
				filter := gtk.FilterNew()
				added := 0
				for _, pattern := range strings.Split(f.Pattern, ";") {
					if pattern = strings.TrimSpace(pattern); pattern == "" {
						continue
					}
					if suffix, ok := linuxFilterSuffix(pattern); ok && gtk.FilterSuffix != nil {
						gtk.FilterSuffix(filter, suffix)
					} else {
						gtk.FilterPattern(filter, pattern)
					}
					added++
				}
				if added == 0 {
					gtk.Unref(filter)
					continue
				}
				if f.Name != "" {
					gtk.FilterName(filter, f.Name)
				}
				gtk.ListAppend(list, filter)
				if firstFilter == 0 {
					firstFilter = gtk.Ref(filter)
				}
				gtk.Unref(filter)
			}
			if firstFilter != 0 {
				gtk.DialogFilters(dialog.object, list)
				if gtk.DialogDefaultFilter != nil {
					gtk.DialogDefaultFilter(dialog.object, firstFilter)
				}
				gtk.Unref(firstFilter)
			}
			gtk.Unref(list)
		}
		launch := gtk.DialogOpen
		switch mode {
		case 1:
			launch = gtk.DialogSave
		case 2:
			launch = gtk.DialogDirectory
		case 3:
			launch = gtk.DialogMultiple
		}
		if err := ctx.Err(); err != nil {
			n.finishDialog(id, nil, err)
			return
		}
		if n.ctx.Err() != nil {
			n.finishDialog(id, nil, ErrClosed)
			return
		}
		launch(dialog.object, n.window, dialog.cancel, linuxCallbacks.dialog, id)
	}, func() { result <- linuxDialogResult{err: ErrClosed} })
	if err != nil {
		return nil, err
	}
	select {
	case value := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if n.ctx.Err() != nil {
			return nil, ErrClosed
		}
		return value.paths, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.done:
		return nil, ErrClosed
	}
}

// finishDialog 回收对话框资源并通知等待方
// 入参: id 对话框标识, paths 选择路径, err 完成错误
func (n *nativeWindow) finishDialog(id uintptr, paths []string, err error) {
	dialog := n.dialogsPending[id]
	if dialog == nil {
		return
	}
	delete(n.dialogsPending, id)
	if dialog.stop != nil {
		dialog.stop()
	}
	gtk.Unref(dialog.object)
	gtk.Unref(dialog.cancel)
	select {
	case dialog.result <- linuxDialogResult{paths: paths, err: err}:
	default:
	}
	if n.closed && len(n.dialogsPending) == 0 {
		n.shutdown()
	}
}
