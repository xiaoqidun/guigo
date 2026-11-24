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
	"math"
	"runtime"
	"strconv"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// 原生打印作业表与GTK打印信号回调地址
var (
	linuxPrintOperations       sync.Map
	linuxPrintFinishedCallback = purego.NewCallback(linuxPrintFinished)
	linuxPrintFailedCallback   = purego.NewCallback(linuxPrintFailed)
)

// linuxStandardPaper 描述GTK支持的标准纸张名称与未旋转毫米尺寸
type linuxStandardPaper struct {
	name          string
	width, height float64
}

// linuxStandardPapers 保存常见ISO、JIS与北美标准纸张规格表
var linuxStandardPapers = []linuxStandardPaper{
	{name: "iso_a3", width: 297, height: 420},
	{name: "iso_a4", width: 210, height: 297},
	{name: "iso_a5", width: 148, height: 210},
	{name: "iso_a6", width: 105, height: 148},
	{name: "iso_b4", width: 250, height: 353},
	{name: "iso_b5", width: 176, height: 250},
	{name: "jis_b4", width: 257, height: 364},
	{name: "jis_b5", width: 182, height: 257},
	{name: "na_letter", width: 215.9, height: 279.4},
	{name: "na_legal", width: 215.9, height: 355.6},
	{name: "na_executive", width: 184.15, height: 266.7},
	{name: "na_ledger", width: 279.4, height: 431.8},
}

// linuxPrintOperation 持有底层消费内容期间的打印对象
type linuxPrintOperation struct {
	owner            *nativeWindow
	result           chan error
	err              error
	failed, finished uint64
}

// linuxPrintFailed 保留底层错误，finished随后统一释放原生对象
// 入参: operation 原生打印对象, failure 底层错误, _ 信号用户数据
func linuxPrintFailed(operation uintptr, failure *linuxError, _ uintptr) {
	if value, ok := linuxPrintOperations.Load(operation); ok {
		job := value.(*linuxPrintOperation)
		if failure != nil && failure.Domain == gtk.PrintErrorQuark() && failure.Code == 500 {
			if failure.Message != nil {
				job.err = fmt.Errorf("%w: %s", ErrPrinterNotFound, nativeString(failure.Message))
			} else {
				job.err = ErrPrinterNotFound
			}
		} else if failure != nil && failure.Message != nil {
			job.err = fmt.Errorf("printing failed: %s", nativeString(failure.Message))
		} else {
			job.err = fmt.Errorf("printing failed")
		}
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
	select {
	case job.result <- failure:
	default:
	}
}

// linuxPaperSize 优先匹配GTK标准纸张规格，未命中时回退为自定义纸张
// 入参: width 纸张毫米宽度, height 纸张毫米高度
// 返回: uintptr 原生纸张对象
func linuxPaperSize(width, height float64) uintptr {
	for _, spec := range linuxStandardPapers {
		if math.Abs(width-spec.width) <= 0.5 && math.Abs(height-spec.height) <= 0.5 {
			if paper := gtk.PaperStandard(spec.name); paper != 0 {
				return paper
			}
		}
	}
	return gtk.PaperNew("guigo-custom", "Custom", width, height, 3)
}

// printCapabilities 返回WebKitGTK支持的通用打印机制
// 入参: ctx 查询上下文
// 返回: PrintCapabilities 打印机制能力, error 查询或取消错误
func (n *nativeWindow) printCapabilities(ctx context.Context) (PrintCapabilities, error) {
	if n.isClosed() {
		return PrintCapabilities{}, ErrClosed
	}
	return PrintCapabilities{SystemDialog: true, DialogCompletion: true, Direct: true, PageRanges: true, DeviceSettings: true}, nil
}

// showPrintDialog 打开WebKitGTK系统打印对话框并等待底层消费结束
// 入参: ctx 调用上下文, kind 打印窗口类型
// 返回: error 不支持、取消或窗口调用错误
func (n *nativeWindow) showPrintDialog(ctx context.Context, kind PrintDialogKind) error {
	if n.isClosed() {
		return ErrClosed
	}
	if kind != PrintDialogSystem {
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

// runPrint 在GTK线程配置、调起并跟踪一次打印，模态结束时与关闭流程协同退出主循环
// 入参: ctx 调用上下文, o 设备打印选项, dialog 是否显示系统窗口
// 返回: PrintResult 提交结果, error 配置、提交或取消错误
func (n *nativeWindow) runPrint(ctx context.Context, o PrintOptions, dialog bool) (PrintResult, error) {
	if n.isClosed() {
		return PrintResult{}, ErrClosed
	}
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
		defer gtk.Unref(settings)
		set := func(key, value string) {
			if value != "" {
				gtk.PrintSet(settings, key, value)
			}
		}
		set("printer", o.Printer)
		set("print-pages", "all")
		set("page-set", "all")
		set("reverse", "false")
		set("orientation", string(o.Orientation))
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
			set("collate", strconv.FormatBool(o.Collation == PrintCollationCollated))
		}
		if o.Color != "" {
			set("use-color", strconv.FormatBool(o.Color == PrintColorModeColor))
		}
		if o.Duplex != "" {
			set("duplex", map[PrintDuplex]string{PrintDuplexSimplex: "simplex", PrintDuplexLongEdge: "horizontal", PrintDuplexShortEdge: "vertical"}[o.Duplex])
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
			runtime.KeepAlive(ranges)
		}
		gtk.PrintSettings(operation, settings)
		if o.Width > 0 || o.Orientation != "" || o.Margins != nil {
			setup := gtk.PageSetupNew()
			defer gtk.Unref(setup)
			if o.Width > 0 {
				paper := linuxPaperSize(o.Width, o.Height)
				gtk.PagePaper(setup, paper)
				gtk.PaperFree(paper)
			}
			if o.Orientation != "" {
				orientation := int32(0)
				if o.Orientation == PrintOrientationLandscape {
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
			gtk.PrintPageSetup(operation, setup)
		}
		job := &linuxPrintOperation{owner: n, result: result}
		job.failed = gtk.Connect(operation, "failed", linuxPrintFailedCallback, 0, 0, 0)
		job.finished = gtk.Connect(operation, "finished", linuxPrintFinishedCallback, 0, 0, 0)
		linuxPrintOperations.Store(operation, job)
		gtk.Ref(operation)
		defer gtk.Unref(operation)
		if dialog {
			n.printModalDepth++
			defer func() {
				n.printModalDepth--
				if n.printModalDepth == 0 && n.quitPending {
					n.quitPending = false
					n.shutdown()
				}
			}()
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
