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
	"fmt"
	"math"
	"runtime"
	"syscall"
	"unsafe"
)

// comFloat64PC 保存单浮点参数COM调用蹦床入口地址
var comFloat64PC = comFloat64ThunkPC()

// comFloat64ThunkPC 返回单浮点参数COM调用蹦床入口地址
// 返回: uintptr 蹦床入口地址
func comFloat64ThunkPC() uintptr

// comFloat64 通过Windows x64浮点ABI调用接收单个float64参数的COM虚函数
// 入参: object 接口对象, index 方法索引, value 浮点参数
// 返回: uintptr 调用结果
func comFloat64(object *comObject, index int, value float64) uintptr {
	fn := *(*uintptr)(unsafe.Add(unsafe.Pointer(object.table), uintptr(index)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(comFloat64PC, uintptr(unsafe.Pointer(object)), uintptr(math.Float64bits(value)), fn)
	runtime.KeepAlive(object)
	return r
}

// printInterface 获取当前WebView2的指定打印接口，调用方在STA线程释放
// 入参: object 原生COM对象, id 打印接口标识
// 返回: *comObject 打印接口, error 接口查询错误
func printInterface(object *comObject, id string) (*comObject, error) {
	if object == nil {
		return nil, ErrClosed
	}
	iid := guid(id)
	var result *comObject
	if err := hresult(com(object, 0, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&result)))); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPrintUnsupported, err)
	}
	return result, nil
}

// printCapabilities 在STA线程核对WebView2打印接口
// 入参: ctx 查询上下文
// 返回: PrintCapabilities 打印机制能力, error 查询或取消错误
func (n *nativeWindow) printCapabilities(ctx context.Context) (PrintCapabilities, error) {
	var result PrintCapabilities
	err := n.invoke(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		view, err := printInterface(n.webview, "{0EB34DC9-9F91-41E1-8639-95CD5943906B}")
		if err != nil {
			return err
		}
		release(view)
		result = PrintCapabilities{SystemDialog: true, BrowserDialog: true, Direct: true, PageRanges: true, DeviceSettings: true, Backgrounds: true, HeadersFooters: true}
		return nil
	})
	return result, err
}

// showPrintUI 在最外层消息循环发起WebView2打印对话框请求
// 入参: ctx 调用上下文, kind 打印窗口类型
// 返回: error 不支持、取消或窗口调用错误
func (n *nativeWindow) showPrintUI(ctx context.Context, kind PrintDialogKind) error {
	return n.invokeTop(func() error {
		if n.printActive {
			return ErrPrintBusy
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		view, err := printInterface(n.webview, "{0EB34DC9-9F91-41E1-8639-95CD5943906B}")
		if err != nil {
			return err
		}
		defer release(view)
		value := uintptr(1)
		if kind == PrintDialogBrowser {
			value = 0
		}
		return hresult(com(view, 114, value))
	})
}

// printSettings 创建并配置原生设置，不改变调用方页面
// 入参: o 设备打印选项
// 返回: *comObject 原生打印设置, error 接口或设置错误
func (n *nativeWindow) printSettings(o PrintOptions) (*comObject, error) {
	environment, err := printInterface(n.environment, "{e59ee362-acbd-4857-9a8e-d3644d9459a9}")
	if err != nil {
		return nil, err
	}
	defer release(environment)
	var basic *comObject
	if err := hresult(com(environment, 14, uintptr(unsafe.Pointer(&basic)))); err != nil {
		return nil, err
	}
	defer release(basic)
	settings, err := printInterface(basic, "{CA7F0E1F-3484-41D1-8C1A-65CD44A63F8D}")
	if err != nil {
		return nil, err
	}
	var failure error
	set := func(index int, value uintptr) {
		if failure == nil {
			failure = hresult(com(settings, index, value))
		}
	}
	setWide := func(index int, value *uint16) {
		if failure == nil {
			failure = hresult(com(settings, index, uintptr(unsafe.Pointer(value))))
			runtime.KeepAlive(value)
		}
	}
	double := func(index int, value float64) {
		if failure == nil {
			failure = hresult(comFloat64(settings, index, value))
		}
	}
	if o.Orientation != "" {
		value := uintptr(0)
		if o.Orientation == PrintOrientationLandscape {
			value = 1
		}
		set(4, value)
	}
	if o.Scale != 0 {
		double(6, o.Scale)
	}
	if o.Width > 0 {
		set(42, 1)
		double(8, o.Width/25.4)
		double(10, o.Height/25.4)
	}
	if o.Margins != nil {
		double(12, o.Margins.Top/25.4)
		double(14, o.Margins.Bottom/25.4)
		double(16, o.Margins.Left/25.4)
		double(18, o.Margins.Right/25.4)
	}
	for index, value := range map[int]*bool{20: o.Backgrounds, 24: o.HeadersFooters} {
		if value != nil {
			v := uintptr(0)
			if *value {
				v = 1
			}
			set(index, v)
		}
	}
	if o.Copies > 0 {
		set(34, uintptr(o.Copies))
	}
	if o.PagesPerSide > 0 {
		set(32, uintptr(o.PagesPerSide))
	}
	if o.PageRanges != "" {
		setWide(30, wide(o.PageRanges))
	}
	if o.Printer != "" {
		setWide(44, wide(o.Printer))
	}
	if o.Collation != "" {
		value := uintptr(1)
		if o.Collation == PrintCollationUncollated {
			value = 2
		}
		set(36, value)
	}
	if o.Color != "" {
		value := uintptr(1)
		if o.Color == PrintColorModeGrayscale {
			value = 2
		}
		set(38, value)
	}
	if o.Duplex != "" {
		value := map[PrintDuplex]uintptr{PrintDuplexOneSided: 1, PrintDuplexLongEdge: 2, PrintDuplexShortEdge: 3}[o.Duplex]
		set(40, value)
	}
	if failure != nil {
		release(settings)
		return nil, failure
	}
	return settings, nil
}

// print 等待WebView2提交结果，取消等待不承诺撤回设备作业
// 入参: ctx 调用上下文, o 设备打印选项
// 返回: PrintResult 提交结果, error 配置、提交或取消错误
func (n *nativeWindow) print(ctx context.Context, o PrintOptions) (PrintResult, error) {
	result := make(chan error, 1)
	err := n.enqueueCleanup(func() {
		if n.printActive {
			result <- ErrPrintBusy
			return
		}
		if err := ctx.Err(); err != nil {
			result <- err
			return
		}
		view, err := printInterface(n.webview, "{0EB34DC9-9F91-41E1-8639-95CD5943906B}")
		if err != nil {
			result <- err
			return
		}
		defer release(view)
		settings, err := n.printSettings(o)
		if err != nil {
			result <- err
			return
		}
		defer release(settings)
		cb, callback := newScalarCallback("{8fd80075-ed08-42db-8570-f5d14977461e}", func(hr, status uintptr) uintptr {
			n.printActive = false
			failure := hresult(hr)
			if failure == nil {
				switch uint32(status) {
				case 0:
				case 1:
					failure = fmt.Errorf("%w: printer unavailable", ErrPrinterNotFound)
				default:
					failure = fmt.Errorf("print submission failed")
				}
			}
			select {
			case result <- failure:
			default:
			}
			return 0
		})
		defer releaseCallback(cb)
		n.printActive = true
		if err := hresult(com(view, 113, uintptr(unsafe.Pointer(settings)), callback)); err != nil {
			n.printActive = false
			result <- err
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
