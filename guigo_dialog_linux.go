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
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

// linuxDialog 保存异步对话框状态
type linuxDialog struct {
	object, cancel uintptr
	mode           int
	result         chan linuxDialogResult
}

// linuxDialogResult 保存文件路径及结束原因
type linuxDialogResult struct {
	paths []string
	err   error
}

// dialog 选择单个文件或目录
// 入参: o 对话框选项, mode 对话框类型
// 返回: string 文件路径, error 对话框错误
func (n *nativeWindow) dialog(o DialogOptions, mode int) (string, error) {
	paths, err := n.dialogs(o, mode)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", ErrCanceled
	}
	return paths[0], nil
}

// dialogs 在GTK线程显示异步文件对话框并等待结果
// 入参: o 对话框选项, mode 对话框类型
// 返回: []string 文件路径, error 对话框错误
func (n *nativeWindow) dialogs(o DialogOptions, mode int) ([]string, error) {
	if n.ctx.Err() != nil {
		return nil, ErrClosed
	}
	result := make(chan linuxDialogResult, 1)
	err := n.enqueue(func() {
		if n.ctx.Err() != nil {
			result <- linuxDialogResult{err: ErrClosed}
			return
		}
		n.nextDialog++
		id := n.nextDialog
		dialog := &linuxDialog{object: gtk.DialogNew(), cancel: gtk.CancelNew(), mode: mode, result: result}
		n.dialogsPending[id] = dialog
		gtk.DialogModal(dialog.object, true)
		if o.Title != "" {
			gtk.DialogTitle(dialog.object, o.Title)
		}
		if o.DefaultPath != "" {
			path, err := filepath.Abs(o.DefaultPath)
			if err != nil {
				n.finishDialog(id, nil, err)
				return
			}
			file := gtk.FileNew(path)
			info, err := os.Stat(path)
			if err == nil && info.IsDir() {
				gtk.DialogFolder(dialog.object, file)
			} else if mode == 1 {
				parent := gtk.FileNew(filepath.Dir(path))
				gtk.DialogFolder(dialog.object, parent)
				gtk.Unref(parent)
				if o.DefaultName == "" {
					gtk.DialogName(dialog.object, filepath.Base(path))
				}
			} else {
				gtk.DialogFile(dialog.object, file)
			}
			gtk.Unref(file)
		}
		if o.DefaultName != "" && mode != 2 {
			gtk.DialogName(dialog.object, o.DefaultName)
		}
		if len(o.Filters) > 0 && mode != 2 {
			list := gtk.ListNew(gtk.FilterType())
			for _, f := range o.Filters {
				filter := gtk.FilterNew()
				gtk.FilterName(filter, f.Name)
				for _, pattern := range strings.Split(f.Pattern, ";") {
					if pattern = strings.TrimSpace(pattern); pattern != "" {
						gtk.FilterPattern(filter, pattern)
					}
				}
				gtk.ListAppend(list, filter)
				gtk.Unref(filter)
			}
			gtk.DialogFilters(dialog.object, list)
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
		if n.ctx.Err() != nil {
			n.finishDialog(id, nil, ErrClosed)
			return
		}
		launch(dialog.object, n.window, dialog.cancel, linuxCallbacks.dialog, id)
	})
	if err != nil {
		return nil, err
	}
	select {
	case value := <-result:
		if n.ctx.Err() != nil {
			return nil, ErrClosed
		}
		return value.paths, value.err
	case <-n.done:
		return nil, ErrClosed
	}
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
		if n.ctx.Err() != nil {
			err = ErrClosed
		} else if err == nil || canceled {
			err = ErrCanceled
		}
		n.finishDialog(id, nil, err)
		return
	}
	defer gtk.Unref(value)
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
		for i := uint32(0); i < count; i++ {
			file := gtk.ListItem(value, i)
			appendFile(file)
			gtk.Unref(file)
		}
	} else {
		appendFile(value)
	}
	if len(paths) == 0 && err == nil {
		err = ErrCanceled
	}
	n.finishDialog(id, paths, err)
}

// finishDialog 回收对话框资源并通知等待方
// 入参: id 对话框标识, paths 选择路径, err 完成错误
func (n *nativeWindow) finishDialog(id uintptr, paths []string, err error) {
	dialog := n.dialogsPending[id]
	if dialog == nil {
		return
	}
	delete(n.dialogsPending, id)
	gtk.Unref(dialog.object)
	gtk.Unref(dialog.cancel)
	dialog.result <- linuxDialogResult{paths: paths, err: err}
	if n.ctx.Err() != nil && len(n.dialogsPending) == 0 {
		n.shutdown()
	}
}
