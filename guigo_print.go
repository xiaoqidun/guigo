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
	"math"
	"slices"
	"strconv"
	"strings"
)

const (
	// PrintDialogSystem 使用系统打印窗口
	PrintDialogSystem PrintDialogKind = "system"
	// PrintDialogBrowser 使用WebView内置打印窗口
	PrintDialogBrowser PrintDialogKind = "browser"
)

const (
	// PrintOrientationPortrait 使用纵向打印
	PrintOrientationPortrait PrintOrientation = "portrait"
	// PrintOrientationLandscape 使用横向打印
	PrintOrientationLandscape PrintOrientation = "landscape"
)

const (
	// PrintCollationCollated 按份逐套整理打印
	PrintCollationCollated PrintCollation = "collated"
	// PrintCollationUncollated 按页分组不逐套整理打印
	PrintCollationUncollated PrintCollation = "uncollated"
)

const (
	// PrintColorModeColor 使用彩色打印
	PrintColorModeColor PrintColorMode = "color"
	// PrintColorModeGrayscale 使用灰度打印
	PrintColorModeGrayscale PrintColorMode = "grayscale"
)

const (
	// PrintDuplexSimplex 使用单面打印
	PrintDuplexSimplex PrintDuplex = "simplex"
	// PrintDuplexLongEdge 使用长边翻页双面打印
	PrintDuplexLongEdge PrintDuplex = "longEdge"
	// PrintDuplexShortEdge 使用短边翻页双面打印
	PrintDuplexShortEdge PrintDuplex = "shortEdge"
)

var (
	// ErrPrintUnsupported 表示当前运行时没有请求的打印能力
	ErrPrintUnsupported = errors.New("requested printing capability is unavailable")
	// ErrPrintBusy 表示当前视图仍在处理另一个直接打印作业
	ErrPrintBusy = errors.New("another print operation is active")
	// ErrPrinterNotFound 表示指定的打印机或系统默认打印机不存在
	ErrPrinterNotFound = errors.New("printer not found")
)

// PrintDialogKind 定义打印对话框类型
type PrintDialogKind string

// PrintOrientation 定义纸张打印方向
type PrintOrientation string

// PrintCollation 定义多份打印的逐份整理方式
type PrintCollation string

// PrintColorMode 定义打印色彩模式
type PrintColorMode string

// PrintDuplex 定义单双面打印方式
type PrintDuplex string

// PrintCapabilities 描述运行时支持的打印机制，不代表打印机已连接或具备所有硬件功能
// 分别表示系统/浏览器对话框、对话框返回即消费完成、直接提交、页码范围、设备设置、背景及页眉页脚支持
type PrintCapabilities struct {
	SystemDialog     bool `json:"systemDialog"`
	BrowserDialog    bool `json:"browserDialog"`
	DialogCompletion bool `json:"dialogCompletion"`
	Direct           bool `json:"direct"`
	PageRanges       bool `json:"pageRanges"`
	DeviceSettings   bool `json:"deviceSettings"`
	Backgrounds      bool `json:"backgrounds"`
	HeadersFooters   bool `json:"headersFooters"`
}

// PrintOptions 设置当前顶层文档的直接打印参数，尺寸及边距单位均为毫米
// Width/Height为未旋转纸张尺寸且须同时设置；Scale范围0.1至2，Copies范围1至999，PagesPerSide支持1/2/4/6/9/16
// PageRanges为一基闭区间如1-3,5；空值或nil字段采用系统默认设置，提交期间须保持文档内容不变
type PrintOptions struct {
	Printer        string           `json:"printer,omitempty"`
	Orientation    PrintOrientation `json:"orientation,omitempty"`
	Width          float64          `json:"width,omitempty"`
	Height         float64          `json:"height,omitempty"`
	Margins        *PrintMargins    `json:"margins,omitempty"`
	Scale          float64          `json:"scale,omitempty"`
	Copies         int              `json:"copies,omitempty"`
	Collation      PrintCollation   `json:"collation,omitempty"`
	Color          PrintColorMode   `json:"color,omitempty"`
	Duplex         PrintDuplex      `json:"duplex,omitempty"`
	PageRanges     string           `json:"pageRanges,omitempty"`
	PagesPerSide   int              `json:"pagesPerSide,omitempty"`
	Backgrounds    *bool            `json:"backgrounds,omitempty"`
	HeadersFooters *bool            `json:"headersFooters,omitempty"`
}

// PrintMargins 指定应用方向后物理纸面的四边毫米边距
type PrintMargins struct {
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
	Left   float64 `json:"left"`
}

// PrintResult 表示作业提交结果，Submitted为真不确认实体打印机已出纸
type PrintResult struct {
	Submitted bool `json:"submitted"`
}

// clonePrintOptions 隔离异步设备操作使用的可选设置，不保留调用方指针
// 入参: o 调用方设备选项
// 返回: PrintOptions 可选字段的独立副本
func clonePrintOptions(o PrintOptions) PrintOptions {
	if o.Margins != nil {
		margins := *o.Margins
		o.Margins = &margins
	}
	if o.Backgrounds != nil {
		backgrounds := *o.Backgrounds
		o.Backgrounds = &backgrounds
	}
	if o.HeadersFooters != nil {
		headers := *o.HeadersFooters
		o.HeadersFooters = &headers
	}
	return o
}

// validatePrint 校验跨平台设备参数，不访问打印机
// 入参: o 设备打印选项
// 返回: error 参数校验错误
func validatePrint(o PrintOptions) error {
	for _, v := range []float64{o.Width, o.Height, o.Scale} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return errors.New("invalid print dimensions")
		}
	}
	if (o.Width == 0) != (o.Height == 0) || o.Copies < 0 || o.Copies > 999 || o.Scale != 0 && (o.Scale < 0.1 || o.Scale > 2) {
		return errors.New("invalid print settings")
	}
	if o.Margins != nil {
		for _, v := range []float64{o.Margins.Top, o.Margins.Right, o.Margins.Bottom, o.Margins.Left} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return errors.New("invalid print margins")
			}
		}
		width, height := o.Width, o.Height
		if o.Orientation == PrintOrientationLandscape {
			width, height = height, width
		}
		if width > 0 && (o.Margins.Left+o.Margins.Right >= width || o.Margins.Top+o.Margins.Bottom >= height) {
			return errors.New("print margins exceed paper size")
		}
	}
	if !slices.Contains([]PrintOrientation{"", PrintOrientationPortrait, PrintOrientationLandscape}, o.Orientation) ||
		!slices.Contains([]PrintColorMode{"", PrintColorModeColor, PrintColorModeGrayscale}, o.Color) ||
		!slices.Contains([]PrintDuplex{"", PrintDuplexSimplex, PrintDuplexLongEdge, PrintDuplexShortEdge}, o.Duplex) ||
		!slices.Contains([]PrintCollation{"", PrintCollationCollated, PrintCollationUncollated}, o.Collation) ||
		!slices.Contains([]int{0, 1, 2, 4, 6, 9, 16}, o.PagesPerSide) {
		return errors.New("invalid print option")
	}
	if strings.ContainsAny(o.Printer, "\x00\r\n") {
		return errors.New("invalid printer name")
	}
	_, err := printPageRanges(o.PageRanges)
	return err
}

// printPageRanges 将一基设备页码范围校验并转换为零基闭区间
// 入参: value 一基页码范围文本
// 返回: [][2]int32 零基闭区间, error 页码范围错误
func printPageRanges(value string) ([][2]int32, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > 4096 || strings.Trim(value, "0123456789,- ") != "" {
		return nil, errors.New("invalid print page range")
	}
	var ranges [][2]int32
	for _, part := range strings.Split(value, ",") {
		first, last, found := strings.Cut(strings.TrimSpace(part), "-")
		start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 32)
		if err != nil || start < 1 {
			return nil, errors.New("invalid print page range")
		}
		end := start
		if found {
			end, err = strconv.ParseInt(strings.TrimSpace(last), 10, 32)
			if err != nil || end < start {
				return nil, errors.New("invalid print page range")
			}
		}
		ranges = append(ranges, [2]int32{int32(start - 1), int32(end - 1)})
	}
	return ranges, nil
}

// PrintCapabilities 查询当前窗口的打印接口能力，可从后台Goroutine调用
// 入参: ctx 调用上下文
// 返回: PrintCapabilities 打印机制能力, error 查询或取消错误
func (w *Window) PrintCapabilities(ctx context.Context) (PrintCapabilities, error) {
	if ctx == nil {
		return PrintCapabilities{}, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return PrintCapabilities{}, err
	}
	if w == nil || w.native == nil {
		return PrintCapabilities{}, ErrClosed
	}
	return w.native.printCapabilities(ctx)
}

// ShowPrintDialog 为当前顶层文档打开打印对话框，空值默认采用PrintDialogSystem
// Windows返回表示对话框请求已交付，不能据此释放内容或认定打印成功
// 调用方可以监听页面afterprint，在对话框关闭后恢复界面；Linux取消返回ErrCanceled
// 入参: ctx 调用上下文, kind 打印窗口类型
// 返回: error 不支持、取消或窗口调用错误
func (w *Window) ShowPrintDialog(ctx context.Context, kind PrintDialogKind) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if kind == "" {
		kind = PrintDialogSystem
	}
	if kind != PrintDialogSystem && kind != PrintDialogBrowser {
		return errors.New("invalid print dialog kind")
	}
	return w.native.showPrintDialog(ctx, kind)
}

// Print 直接提交当前顶层文档，设备设置仍受打印机驱动能力约束
// 返回前内容须保持不变；取消上下文不保证撤回已经提交给系统的作业
// 入参: ctx 调用上下文, options 设备打印选项
// 返回: PrintResult 提交结果, error 配置、提交或取消错误
func (w *Window) Print(ctx context.Context, options PrintOptions) (PrintResult, error) {
	if ctx == nil {
		return PrintResult{}, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return PrintResult{}, err
	}
	if w == nil || w.native == nil {
		return PrintResult{}, ErrClosed
	}
	if err := validatePrint(options); err != nil {
		return PrintResult{}, err
	}
	return w.native.print(ctx, clonePrintOptions(options))
}
