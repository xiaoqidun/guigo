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
	"reflect"
	"strconv"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// cupsDestination 对应CUPS队列及当前用户保存的选项
type cupsDestination struct {
	Name, Instance *byte
	Default, Count int32
	Options        uintptr
}

// cupsMedia 对应cups_size_t，尺寸及硬件边距均为百分之一毫米
type cupsMedia struct {
	Name                                    [128]byte
	Width, Height, Bottom, Left, Right, Top int32
}

// cupsAPI 按需加载队列发现与IPP能力查询，不在GTK主线程等待网络
type cupsAPI struct {
	Dests         func(uintptr, **cupsDestination) int32                                           `symbol:"cupsGetDests2"`
	FreeDests     func(int32, *cupsDestination)                                                    `symbol:"cupsFreeDests"`
	Info          func(uintptr, *cupsDestination) uintptr                                          `symbol:"cupsCopyDestInfo"`
	FreeInfo      func(uintptr)                                                                    `symbol:"cupsFreeDestInfo"`
	Option        func(string, int32, uintptr) *byte                                               `symbol:"cupsGetOption"`
	Supported     func(uintptr, *cupsDestination, uintptr, string, string) int32                   `symbol:"cupsCheckDestSupported"`
	FindDefault   func(uintptr, *cupsDestination, uintptr, string) uintptr                         `symbol:"cupsFindDestDefault"`
	FindSupported func(uintptr, *cupsDestination, uintptr, string) uintptr                         `symbol:"cupsFindDestSupported"`
	Integer       func(uintptr, int32) int32                                                       `symbol:"ippGetInteger"`
	Range         func(uintptr, int32, *int32) int32                                               `symbol:"ippGetRange"`
	String        func(uintptr, int32, uintptr) *byte                                              `symbol:"ippGetString"`
	MediaCount    func(uintptr, *cupsDestination, uintptr, uint32) int32                           `symbol:"cupsGetDestMediaCount"`
	MediaIndex    func(uintptr, *cupsDestination, uintptr, int32, uint32, *cupsMedia) int32        `symbol:"cupsGetDestMediaByIndex"`
	MediaDefault  func(uintptr, *cupsDestination, uintptr, uint32, *cupsMedia) int32               `symbol:"cupsGetDestMediaDefault"`
	MediaSize     func(uintptr, *cupsDestination, uintptr, int32, int32, uint32, *cupsMedia) int32 `symbol:"cupsGetDestMediaBySize"`
}

// cups 保存按需加载的CUPS查询接口
var cups cupsAPI

// cupsOnce 保证CUPS查询接口只加载一次
var cupsOnce sync.Once

// cupsError 保存CUPS接口加载错误
var cupsError error

// loadCUPS 将可选设备查询与WebKitGTK基本打印能力分开，缺少CUPS时返回明确错误
// 返回: error 设备查询库加载错误
func loadCUPS() error {
	cupsOnce.Do(func() {
		handle, err := purego.Dlopen("libcups.so.2", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			cupsError = fmt.Errorf("%w: %v", ErrPrintUnsupported, err)
			return
		}
		var api cupsAPI
		value := reflect.ValueOf(&api).Elem()
		for i := 0; i < value.NumField(); i++ {
			address, err := purego.Dlsym(handle, value.Type().Field(i).Tag.Get("symbol"))
			if err != nil {
				cupsError = fmt.Errorf("%w: %v", ErrPrintUnsupported, err)
				purego.Dlclose(handle)
				return
			}
			purego.RegisterFunc(value.Field(i).Addr().Interface(), address)
		}
		cups = api
	})
	return cupsError
}

// listPrinters 读取CUPS队列，按队列名称排除保存的重复实例
// 入参: ctx 查询上下文
// 返回: []PrinterInfo 系统打印队列, error 查询或取消错误
func (n *nativeWindow) listPrinters(ctx context.Context) ([]PrinterInfo, error) {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := loadCUPS(); err != nil {
		return nil, err
	}
	var destinations *cupsDestination
	count := cups.Dests(0, &destinations)
	defer cups.FreeDests(count, destinations)
	if count < 0 || count > 65536 || count > 0 && destinations == nil {
		return nil, fmt.Errorf("invalid CUPS destinations")
	}
	result := []PrinterInfo{}
	for _, item := range unsafe.Slice(destinations, count) {
		if item.Instance != nil {
			continue
		}
		result = append(result, PrinterInfo{Name: nativeString(item.Name), Default: item.Default != 0})
	}
	return result, ctx.Err()
}

// queryPrinter 查询IPP纸张及边距，拒绝CUPS返回的近似尺寸
// 入参: ctx 查询上下文, o 待核对设备设置
// 返回: PrinterProfile 设备能力及当前纸面, error 配置、查询或取消错误
func (n *nativeWindow) queryPrinter(ctx context.Context, o PrintOptions) (PrinterProfile, error) {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return PrinterProfile{}, ErrClosed
	}
	if err := loadCUPS(); err != nil {
		return PrinterProfile{}, err
	}
	return queryCUPSPrinter(ctx, o, cups)
}

// queryCUPSPrinter 将独立CUPS能力快照转换为跨平台设备数据
// 入参: ctx 查询上下文, o 待核对设备设置, cups 已加载的CUPS接口
// 返回: PrinterProfile 设备能力及当前纸面, error 配置、查询或取消错误
func queryCUPSPrinter(ctx context.Context, o PrintOptions, cups cupsAPI) (PrinterProfile, error) {
	if err := ctx.Err(); err != nil {
		return PrinterProfile{}, err
	}
	var destinations *cupsDestination
	count := cups.Dests(0, &destinations)
	defer cups.FreeDests(count, destinations)
	if count < 0 || count > 65536 || count > 0 && destinations == nil {
		return PrinterProfile{}, fmt.Errorf("invalid CUPS destinations")
	}
	var selected *cupsDestination
	for i := range int(count) {
		item := (*cupsDestination)(unsafe.Add(unsafe.Pointer(destinations), uintptr(i)*unsafe.Sizeof(cupsDestination{})))
		if item.Instance == nil && (o.Printer != "" && nativeString(item.Name) == o.Printer || o.Printer == "" && item.Default != 0) {
			selected = item
			break
		}
	}
	if selected == nil {
		return PrinterProfile{}, fmt.Errorf("printer not found")
	}
	info := cups.Info(0, selected)
	if info == 0 {
		return PrinterProfile{}, fmt.Errorf("printer capabilities unavailable")
	}
	defer cups.FreeInfo(info)
	option := func(key string) string { return nativeString(cups.Option(key, selected.Count, selected.Options)) }
	defaultString := func(key string) string {
		if v := option(key); v != "" {
			return v
		}
		return nativeString(cups.String(cups.FindDefault(0, selected, info, key), 0, 0))
	}
	p := PrinterProfile{Printer: PrinterInfo{Name: nativeString(selected.Name), Default: selected.Default != 0}, Papers: []PrintPaper{}, Color: cups.Supported(0, selected, info, "print-color-mode", "color") != 0, Duplex: cups.Supported(0, selected, info, "sides", "two-sided-long-edge") != 0 || cups.Supported(0, selected, info, "sides", "two-sided-short-edge") != 0, MaxCopies: 1}
	if attr := cups.FindSupported(0, selected, info, "copies"); attr != 0 {
		var upper int32
		cups.Range(attr, 0, &upper)
		p.MaxCopies = max(1, min(999, int(upper)))
	}
	mediaCount := cups.MediaCount(0, selected, info, 0)
	if mediaCount < 0 || mediaCount > 4096 {
		return p, fmt.Errorf("invalid CUPS paper count")
	}
	for i := int32(0); i < mediaCount; i++ {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		var media cupsMedia
		if cups.MediaIndex(0, selected, info, i, 0, &media) != 0 && media.Width > 0 && media.Height > 0 {
			name := nativeString(&media.Name[0])
			p.Papers = append(p.Papers, PrintPaper{ID: name, Name: name, Width: float64(media.Width) / 100, Height: float64(media.Height) / 100})
		}
	}
	var media cupsMedia
	if o.Width == 0 {
		if cups.MediaDefault(0, selected, info, 0, &media) == 0 {
			return p, fmt.Errorf("printer has no default media")
		}
		o.Width, o.Height = float64(media.Width)/100, float64(media.Height)/100
	}
	flags := uint32(4)
	if o.Duplex == "" {
		o.Duplex = map[string]string{"two-sided-long-edge": "longEdge", "two-sided-short-edge": "shortEdge"}[defaultString("sides")]
		if o.Duplex == "" {
			o.Duplex = "simplex"
		}
	}
	if o.Duplex != "simplex" {
		if cups.Supported(0, selected, info, "sides", map[string]string{"longEdge": "two-sided-long-edge", "shortEdge": "two-sided-short-edge"}[o.Duplex]) == 0 {
			return p, fmt.Errorf("%w: duplex mode", ErrPrintUnsupported)
		}
		flags |= 2
	}
	if o.Width > 21474836 || o.Height > 21474836 || cups.MediaSize(0, selected, info, int32(math.Round(o.Width*100)), int32(math.Round(o.Height*100)), flags, &media) == 0 || math.Abs(float64(media.Width)/100-o.Width) > 0.1 || math.Abs(float64(media.Height)/100-o.Height) > 0.1 {
		return p, fmt.Errorf("%w: paper size", ErrPrintUnsupported)
	}
	if o.Orientation == "" {
		o.Orientation = "portrait"
		orientation := option("orientation-requested")
		if orientation == "4" || orientation == "" && cups.Integer(cups.FindDefault(0, selected, info, "orientation-requested"), 0) == 4 {
			o.Orientation = "landscape"
		}
	}
	if o.Copies == 0 {
		o.Copies, _ = strconv.Atoi(option("copies"))
		if o.Copies < 1 {
			o.Copies = int(cups.Integer(cups.FindDefault(0, selected, info, "copies"), 0))
		}
		if o.Copies < 1 {
			o.Copies = 1
		}
	}
	if o.Color == "" {
		o.Color = "grayscale"
		if p.Color && defaultString("print-color-mode") != "monochrome" {
			o.Color = "color"
		}
	}
	if o.Collation == "" {
		o.Collation = "collated"
	}
	o.Printer = p.Printer.Name
	p.Options = o
	p.Width, p.Height = o.Width, o.Height
	left, top, right, bottom := float64(media.Left)/100, float64(media.Top)/100, float64(media.Right)/100, float64(media.Bottom)/100
	if o.Orientation == "landscape" {
		p.Width, p.Height = p.Height, p.Width
		left, top, right, bottom = top, right, bottom, left
	}
	p.PrintableArea = PrintRectangle{X: left, Y: top, Width: p.Width - left - right, Height: p.Height - top - bottom}
	return p, ctx.Err()
}

// showPrinterProperties 保留不支持平台属性窗口的明确能力边界
// 入参: ctx 调用上下文, name 系统队列名称
// 返回: error 平台不支持错误
func (n *nativeWindow) showPrinterProperties(ctx context.Context, name string) error {
	return ErrPrintUnsupported
}
