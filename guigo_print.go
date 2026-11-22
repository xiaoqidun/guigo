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
	"math"
	"slices"
	"strconv"
	"strings"
)

// ErrPrintUnsupported 表示当前运行时没有请求的打印能力
var ErrPrintUnsupported = errors.New("requested printing capability is unavailable")

// ErrPrintBusy 表示当前视图仍在处理另一个直接打印作业
var ErrPrintBusy = errors.New("another print operation is active")

// PrintCapabilities 描述运行时支持的打印机制，不代表打印机已连接或具备所有硬件功能
type PrintCapabilities struct {
	// SystemDialog 表示支持系统打印窗口
	SystemDialog bool `json:"systemDialog"`
	// BrowserDialog 表示支持WebView内置打印窗口
	BrowserDialog bool `json:"browserDialog"`
	// DialogCompletion 表示打印窗口返回前底层已结束消费页面内容
	DialogCompletion bool `json:"dialogCompletion"`
	// Direct 表示支持按设备选项直接提交打印
	Direct bool `json:"direct"`
	// PageRanges 表示直接打印支持一基页码范围
	PageRanges bool `json:"pageRanges"`
	// DeviceSettings 表示支持指定设备、纸张及打印作业设置
	DeviceSettings bool `json:"deviceSettings"`
	// Backgrounds 表示支持显式控制背景打印
	Backgrounds bool `json:"backgrounds"`
	// HeadersFooters 表示支持显式控制浏览器页眉页脚
	HeadersFooters bool `json:"headersFooters"`
}

// PrintOptions 设置当前顶层文档的直接打印参数，长度以毫米为单位
// 调用方负责准备完整文档、分页和资源就绪，并在底层消费结束前保留内容
type PrintOptions struct {
	// Printer 指定系统队列名称，空值采用默认设备
	Printer string `json:"printer,omitempty"`
	// Orientation 指定portrait或landscape，空值采用系统默认方向
	Orientation string `json:"orientation,omitempty"`
	// Width 指定未旋转纸张宽度，单位为毫米，与Height同时设置
	Width float64 `json:"width,omitempty"`
	// Height 指定未旋转纸张高度，单位为毫米，与Width同时设置
	Height float64 `json:"height,omitempty"`
	// Margins 指定应用方向后物理纸面的毫米边距，nil采用系统默认值
	Margins *PrintMargins `json:"margins,omitempty"`
	// Scale 指定0.1至2之间的缩放比例，1表示100%，零值采用系统默认值
	Scale float64 `json:"scale,omitempty"`
	// Copies 指定1至999之间的份数，零值采用系统默认值
	Copies int `json:"copies,omitempty"`
	// Collation 指定collated或uncollated，空值采用系统默认顺序
	Collation string `json:"collation,omitempty"`
	// Color 指定color或grayscale，空值采用系统默认颜色
	Color string `json:"color,omitempty"`
	// Duplex 指定simplex、longEdge或shortEdge，空值采用系统默认单双面
	Duplex string `json:"duplex,omitempty"`
	// PageRanges 指定一基闭区间，如1-3,5，空值打印全部页面
	PageRanges string `json:"pageRanges,omitempty"`
	// PagesPerSide 指定每面1、2、4、6、9或16页，零值采用系统默认值
	PagesPerSide int `json:"pagesPerSide,omitempty"`
	// Backgrounds 控制背景打印，nil采用系统默认值
	Backgrounds *bool `json:"backgrounds,omitempty"`
	// HeadersFooters 控制浏览器页眉页脚，nil采用系统默认值
	HeadersFooters *bool `json:"headersFooters,omitempty"`
}

// PrintMargins 指定设备打印时的纸面边距
type PrintMargins struct {
	// Top 指定上边距，单位为毫米
	Top float64 `json:"top"`
	// Right 指定右边距，单位为毫米
	Right float64 `json:"right"`
	// Bottom 指定下边距，单位为毫米
	Bottom float64 `json:"bottom"`
	// Left 指定左边距，单位为毫米
	Left float64 `json:"left"`
}

// PrintResult 表示作业已提交，不确认实体打印机已经出纸
type PrintResult struct {
	// Submitted 表示底层已接受提交，不代表设备出纸或作业完成
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
			return fmt.Errorf("invalid print dimensions")
		}
	}
	if (o.Width == 0) != (o.Height == 0) || o.Copies < 0 || o.Copies > 999 || o.Scale != 0 && (o.Scale < 0.1 || o.Scale > 2) {
		return fmt.Errorf("invalid print settings")
	}
	if o.Margins != nil {
		for _, v := range []float64{o.Margins.Top, o.Margins.Right, o.Margins.Bottom, o.Margins.Left} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return fmt.Errorf("invalid print margins")
			}
		}
		width, height := o.Width, o.Height
		if o.Orientation == "landscape" {
			width, height = height, width
		}
		if width > 0 && (o.Margins.Left+o.Margins.Right >= width || o.Margins.Top+o.Margins.Bottom >= height) {
			return fmt.Errorf("print margins exceed paper size")
		}
	}
	if !slices.Contains([]string{"", "portrait", "landscape"}, o.Orientation) || !slices.Contains([]string{"", "color", "grayscale"}, o.Color) || !slices.Contains([]string{"", "simplex", "longEdge", "shortEdge"}, o.Duplex) || !slices.Contains([]string{"", "collated", "uncollated"}, o.Collation) || !slices.Contains([]int{0, 1, 2, 4, 6, 9, 16}, o.PagesPerSide) {
		return fmt.Errorf("invalid print option")
	}
	if strings.ContainsAny(o.Printer, "\x00\r\n") {
		return fmt.Errorf("invalid printer name")
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
		return nil, fmt.Errorf("invalid print page range")
	}
	var ranges [][2]int32
	for _, part := range strings.Split(value, ",") {
		first, last, found := strings.Cut(strings.TrimSpace(part), "-")
		start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 32)
		if err != nil || start < 1 {
			return nil, fmt.Errorf("invalid print page range")
		}
		end := start
		if found {
			end, err = strconv.ParseInt(strings.TrimSpace(last), 10, 32)
			if err != nil || end < start {
				return nil, fmt.Errorf("invalid print page range")
			}
		}
		ranges = append(ranges, [2]int32{int32(start - 1), int32(end - 1)})
	}
	return ranges, nil
}

// PrintCapabilities 查询当前窗口的打印接口能力，可从后台协程调用
// 入参: ctx 调用上下文
// 返回: PrintCapabilities 打印机制能力, error 查询或取消错误
func (w *Window) PrintCapabilities(ctx context.Context) (PrintCapabilities, error) {
	if ctx == nil {
		return PrintCapabilities{}, fmt.Errorf("nil print context")
	}
	if err := ctx.Err(); err != nil {
		return PrintCapabilities{}, err
	}
	if w == nil || w.native == nil {
		return PrintCapabilities{}, ErrClosed
	}
	return w.native.printCapabilities(ctx)
}

// ShowPrintUI 为当前顶层文档打开打印对话框，kind支持system、browser，空值采用system
// Windows返回表示对话框请求已交付，不能据此释放内容或认定打印成功
// 调用方可以监听页面afterprint，在对话框关闭后恢复界面；Linux取消返回ErrCanceled
// 入参: ctx 调用上下文, kind 打印窗口类型
// 返回: error 不支持、取消或窗口调用错误
func (w *Window) ShowPrintUI(ctx context.Context, kind string) error {
	if ctx == nil {
		return fmt.Errorf("nil print context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if kind == "" {
		kind = "system"
	}
	if kind != "system" && kind != "browser" {
		return fmt.Errorf("invalid print dialog kind")
	}
	return w.native.showPrintUI(ctx, kind)
}

// Print 直接提交当前顶层文档，设备设置仍受打印机驱动能力约束
// 返回前内容须保持不变；取消上下文不保证撤回已经提交给系统的作业
// 入参: ctx 调用上下文, options 设备打印选项
// 返回: PrintResult 提交结果, error 配置、提交或取消错误
func (w *Window) Print(ctx context.Context, options PrintOptions) (PrintResult, error) {
	if ctx == nil {
		return PrintResult{}, fmt.Errorf("nil print context")
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
