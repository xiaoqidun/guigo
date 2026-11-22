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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

// PrinterInfo 标识系统打印队列，不表示设备一定在线
type PrinterInfo struct {
	// Name 指定系统队列名称
	Name string `json:"name"`
	// Default 表示当前系统默认队列
	Default bool `json:"default"`
}

// PrintPaper 保存设备支持的纸张，宽高按未旋转方向以毫米表示
type PrintPaper struct {
	// ID 保存驱动或打印服务的纸张标识
	ID string `json:"id"`
	// Name 保存纸张显示名称
	Name string `json:"name"`
	// Width 保存未旋转纸张宽度，单位为毫米
	Width float64 `json:"width"`
	// Height 保存未旋转纸张高度，单位为毫米
	Height float64 `json:"height"`
}

// PrintRectangle 保存以物理纸张左上角为原点的毫米区域
type PrintRectangle struct {
	// X 指定区域左边位置，单位为毫米
	X float64 `json:"x"`
	// Y 指定区域上边位置，单位为毫米
	Y float64 `json:"y"`
	// Width 指定区域宽度，单位为毫米
	Width float64 `json:"width"`
	// Height 指定区域高度，单位为毫米
	Height float64 `json:"height"`
}

// PrinterProfile 是一次设备设置查询的独立结果，不缓存为永久设备能力
type PrinterProfile struct {
	// Printer 保存实际查询的系统队列
	Printer PrinterInfo `json:"printer"`
	// Options 保存已解析默认值的设备选项，纸张宽高仍为未旋转尺寸
	Options PrintOptions `json:"options"`
	// Papers 保存设备声明支持的纸张
	Papers []PrintPaper `json:"papers"`
	// Width 保存应用方向后的物理纸面宽度，单位为毫米
	Width float64 `json:"width"`
	// Height 保存应用方向后的物理纸面高度，单位为毫米
	Height float64 `json:"height"`
	// PrintableArea 保存当前纸面坐标下的硬件可打印区域
	PrintableArea PrintRectangle `json:"printableArea"`
	// Color 表示设备支持彩色打印
	Color bool `json:"color"`
	// Duplex 表示设备支持自动双面，具体翻转方式仍需逐项查询
	Duplex bool `json:"duplex"`
	// MaxCopies 保存设备单次作业的份数上限
	MaxCopies int `json:"maxCopies"`
	// CustomPaper 表示设备支持自定义纸张，具体尺寸仍需查询校验
	CustomPaper bool `json:"customPaper"`
	// Properties 表示平台支持系统设备属性窗口
	Properties bool `json:"properties"`
	// Fingerprint 标识当前设置及设备几何信息，供提交前重新查询比较
	Fingerprint string `json:"fingerprint"`
}

// validatePrinterProfile 统一检查返回区域及作业能力，并计算可复查的设备指纹
// 入参: p 待校验设备信息，成功时写入指纹
// 返回: error 设备能力或区域错误
func validatePrinterProfile(p *PrinterProfile) error {
	if err := validatePrint(p.Options); err != nil {
		return err
	}
	r := p.PrintableArea
	for _, v := range []float64{p.Width, p.Height, r.X, r.Y, r.Width, r.Height} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("invalid printer geometry")
		}
	}
	if p.Printer.Name == "" || p.Width <= 0 || p.Height <= 0 || r.Width <= 0 || r.Height <= 0 || r.X+r.Width > p.Width+0.1 || r.Y+r.Height > p.Height+0.1 {
		return fmt.Errorf("invalid printer printable area")
	}
	if p.Options.Color == "color" && !p.Color || p.Options.Duplex != "" && p.Options.Duplex != "simplex" && !p.Duplex || p.MaxCopies > 0 && p.Options.Copies > p.MaxCopies {
		return fmt.Errorf("%w: device settings", ErrPrintUnsupported)
	}
	p.Fingerprint = ""
	stable := *p
	stable.Options.Scale = 0
	stable.Options.Margins = nil
	stable.Options.PageRanges = ""
	stable.Options.PagesPerSide = 0
	stable.Options.Backgrounds, stable.Options.HeadersFooters = nil, nil
	data, err := json.Marshal(stable)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	p.Fingerprint = hex.EncodeToString(sum[:])
	return nil
}

// ListPrinters 查询当前系统队列，返回空列表时没有可用设备，调用方可提供文件输出
// 入参: ctx 查询上下文
// 返回: []PrinterInfo 系统打印队列, error 查询或取消错误
func (w *Window) ListPrinters(ctx context.Context) ([]PrinterInfo, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil print context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w == nil || w.native == nil {
		return nil, ErrClosed
	}
	return w.native.listPrinters(ctx)
}

// QueryPrinter 根据纸张、方向、颜色及双面请求读取设备可打印区域，不提交作业
// 空Printer选择系统默认设备；不支持的请求返回错误，不静默替换纸张或双面方式
// 入参: ctx 查询上下文, options 待核对设备设置
// 返回: PrinterProfile 设备能力及当前纸面, error 配置、查询或取消错误
func (w *Window) QueryPrinter(ctx context.Context, options PrintOptions) (PrinterProfile, error) {
	if ctx == nil {
		return PrinterProfile{}, fmt.Errorf("nil print context")
	}
	if err := ctx.Err(); err != nil {
		return PrinterProfile{}, err
	}
	if w == nil || w.native == nil {
		return PrinterProfile{}, ErrClosed
	}
	if err := validatePrint(options); err != nil {
		return PrinterProfile{}, err
	}
	profile, err := w.native.queryPrinter(ctx, clonePrintOptions(options))
	if err != nil {
		return PrinterProfile{}, err
	}
	if err := validatePrinterProfile(&profile); err != nil {
		return PrinterProfile{}, err
	}
	return profile, ctx.Err()
}

// ShowPrinterProperties 打开系统设备属性，不修改调用方内容或自行提交作业
// 用户可能在系统窗口修改设备默认值，返回后调用方应重新查询设备并更新预览
// 入参: ctx 调用上下文, printer 系统队列名称
// 返回: error 不支持、取消或窗口调用错误
func (w *Window) ShowPrinterProperties(ctx context.Context, printer string) error {
	if ctx == nil {
		return fmt.Errorf("nil print context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if printer == "" {
		return fmt.Errorf("missing printer")
	}
	if err := validatePrint(PrintOptions{Printer: printer}); err != nil {
		return err
	}
	return w.native.showPrinterProperties(ctx, printer)
}
