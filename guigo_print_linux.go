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
	"fmt"
	"strconv"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// linuxPrintOperation 持有底层消费内容期间的打印对象
type linuxPrintOperation struct {
	owner            *nativeWindow
	result           chan error
	err              error
	failed, finished uint64
}

// linuxPrintOperations 保存尚未结束消费内容的原生打印对象
var linuxPrintOperations sync.Map

// linuxPrintFinishedCallback 保存GTK打印完成回调地址
var linuxPrintFinishedCallback = purego.NewCallback(linuxPrintFinished)

// linuxPrintFailedCallback 保存GTK打印失败回调地址
var linuxPrintFailedCallback = purego.NewCallback(linuxPrintFailed)

// printCapabilities 返回WebKitGTK支持的通用打印机制
// 入参: ctx 查询上下文
// 返回: PrintCapabilities 打印机制能力, error 查询或取消错误
func (n *nativeWindow) printCapabilities(ctx context.Context) (PrintCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return PrintCapabilities{}, err
	}
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return PrintCapabilities{}, ErrClosed
	}
	return PrintCapabilities{SystemDialog: true, DialogCompletion: true, Direct: true, PageRanges: true, DeviceSettings: true}, nil
}

// linuxPrintFailed 保留底层错误，finished随后统一释放原生对象
// 入参: operation 原生打印对象, failure 底层错误, _ 信号用户数据
func linuxPrintFailed(operation uintptr, failure *linuxError, _ uintptr) {
	if value, ok := linuxPrintOperations.Load(operation); ok {
		job := value.(*linuxPrintOperation)
		job.err = fmt.Errorf("printing failed: %s", nativeString(failure.Message))
	}
}

// linuxPrintFinished 完成一次底层资源消费
// 入参: operation 原生打印对象, _ 信号用户数据
func linuxPrintFinished(operation uintptr, _ uintptr) {
	linuxEndPrint(operation, nil)
}

// linuxEndPrint 断开信号并交付一次结果，仅从GTK线程调用
// 入参: operation 原生打印对象, failure 提交或取消错误
func linuxEndPrint(operation uintptr, failure error) {
	value, ok := linuxPrintOperations.LoadAndDelete(operation)
	if !ok {
		return
	}
	job := value.(*linuxPrintOperation)
	gtk.SignalDisconnect(operation, job.failed)
	gtk.SignalDisconnect(operation, job.finished)
	gtk.Unref(operation)
	if failure == nil {
		failure = job.err
	}
	job.result <- failure
}

// showPrintUI 打开WebKitGTK系统打印对话框并等待底层消费结束
// 入参: ctx 调用上下文, kind 打印窗口类型
// 返回: error 不支持、取消或窗口调用错误
func (n *nativeWindow) showPrintUI(ctx context.Context, kind string) error {
	if kind != "system" {
		return ErrPrintUnsupported
	}
	_, err := n.runPrint(ctx, PrintOptions{}, true)
	return err
}

// print 提交当前文档并等待打印结果
// 入参: ctx 调用上下文, o 设备打印选项
// 返回: PrintResult 提交结果, error 配置、提交或取消错误
func (n *nativeWindow) print(ctx context.Context, o PrintOptions) (PrintResult, error) {
	return n.runPrint(ctx, o, false)
}

// runPrint 在GTK线程配置、调起并跟踪一次打印
// 入参: ctx 调用上下文, o 设备打印选项, dialog 是否显示系统窗口
// 返回: PrintResult 提交结果, error 配置、提交或取消错误
func (n *nativeWindow) runPrint(ctx context.Context, o PrintOptions, dialog bool) (PrintResult, error) {
	if o.HeadersFooters != nil && *o.HeadersFooters || o.Backgrounds != nil {
		return PrintResult{}, ErrPrintUnsupported
	}
	result := make(chan error, 1)
	err := n.enqueueCleanup(func() {
		busy := false
		linuxPrintOperations.Range(func(_, value any) bool {
			if value.(*linuxPrintOperation).owner == n {
				busy = true
				return false
			}
			return true
		})
		if busy {
			result <- ErrPrintBusy
			return
		}
		if err := ctx.Err(); err != nil {
			result <- err
			return
		}
		operation := gtk.PrintNew(n.view)
		if operation == 0 {
			result <- fmt.Errorf("create print operation")
			return
		}
		settings := gtk.PrintSettingsNew()
		setup := gtk.PageSetupNew()
		defer gtk.Unref(settings)
		defer gtk.Unref(setup)
		set := func(key, value string) {
			if value != "" {
				gtk.PrintSet(settings, key, value)
			}
		}
		set("printer", o.Printer)
		set("print-pages", "all")
		set("page-set", "all")
		set("reverse", "false")
		set("orientation", o.Orientation)
		if o.Copies > 0 {
			set("n-copies", strconv.Itoa(o.Copies))
		}
		if o.Scale > 0 {
			set("scale", strconv.FormatFloat(o.Scale*100, 'g', -1, 64))
		}
		if o.PagesPerSide > 0 {
			set("number-up", strconv.Itoa(o.PagesPerSide))
		}
		if o.Collation != "" {
			set("collate", strconv.FormatBool(o.Collation == "collated"))
		}
		if o.Color != "" {
			set("use-color", strconv.FormatBool(o.Color == "color"))
		}
		if o.Duplex != "" {
			set("duplex", map[string]string{"simplex": "simplex", "longEdge": "horizontal", "shortEdge": "vertical"}[o.Duplex])
		}
		if o.PageRanges != "" {
			ranges, err := printPageRanges(o.PageRanges)
			if err != nil {
				gtk.Unref(operation)
				result <- err
				return
			}
			set("print-pages", "ranges")
			gtk.PrintRanges(settings, unsafe.Pointer(&ranges[0]), int32(len(ranges)))
		}
		if o.Width > 0 {
			paper := gtk.PaperNew("guigo-custom", "Custom", o.Width, o.Height, 3)
			gtk.PagePaper(setup, paper)
			gtk.PaperFree(paper)
		}
		if o.Orientation != "" {
			orientation := int32(0)
			if o.Orientation == "landscape" {
				orientation = 1
			}
			gtk.PageOrientation(setup, orientation)
		}
		if o.Margins != nil {
			gtk.PageTop(setup, o.Margins.Top, 3)
			gtk.PageBottom(setup, o.Margins.Bottom, 3)
			gtk.PageLeft(setup, o.Margins.Left, 3)
			gtk.PageRight(setup, o.Margins.Right, 3)
		}
		gtk.PrintSettings(operation, settings)
		gtk.PrintPageSetup(operation, setup)
		job := &linuxPrintOperation{owner: n, result: result}
		job.failed = gtk.Connect(operation, "failed", linuxPrintFailedCallback, 0, 0, 0)
		job.finished = gtk.Connect(operation, "finished", linuxPrintFinishedCallback, 0, 0, 0)
		linuxPrintOperations.Store(operation, job)
		if dialog {
			if gtk.PrintDialog(operation, n.window) != 0 {
				linuxEndPrint(operation, ErrCanceled)
			}
		} else {
			gtk.PrintRun(operation)
		}
	}, func() { result <- ErrClosed })
	if err != nil {
		return PrintResult{}, err
	}
	select {
	case err := <-result:
		return PrintResult{Submitted: err == nil}, err
	case <-ctx.Done():
		return PrintResult{}, ctx.Err()
	case <-n.done:
		return PrintResult{}, ErrClosed
	}
}
