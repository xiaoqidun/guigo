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
	"errors"
	"fmt"
	"math"
)

// PrinterInfo 标识系统打印队列名及是否为默认设备，不表示设备一定在线
type PrinterInfo struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// PrintPaper 保存设备支持的纸张标识、名称及未旋转方向毫米尺寸
type PrintPaper struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// PrinterProfile 是一次设备设置查询的独立结果，不缓存为永久设备能力
// Width/Height为应用方向后的物理纸面毫米尺寸，PrintableArea为硬件可打印毫米区域
// Fingerprint标识当前设置与设备几何信息，供提交前复查
type PrinterProfile struct {
	Printer       PrinterInfo  `json:"printer"`
	Options       PrintOptions `json:"options"`
	Papers        []PrintPaper `json:"papers"`
	Width         float64      `json:"width"`
	Height        float64      `json:"height"`
	PrintableArea Rect         `json:"printableArea"`
	Color         bool         `json:"color"`
	Duplex        bool         `json:"duplex"`
	MaxCopies     int          `json:"maxCopies"`
	CustomPaper   bool         `json:"customPaper"`
	Properties    bool         `json:"properties"`
	Fingerprint   string       `json:"fingerprint"`
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
			return errors.New("invalid printer geometry")
		}
	}
	if p.Printer.Name == "" || p.Width <= 0 || p.Height <= 0 || r.Width <= 0 || r.Height <= 0 || r.X+r.Width > p.Width+0.1 || r.Y+r.Height > p.Height+0.1 {
		return errors.New("invalid printer printable area")
	}
	if p.Options.Color == PrintColorModeColor && !p.Color || p.Options.Duplex != "" && p.Options.Duplex != PrintDuplexSimplex && !p.Duplex || p.MaxCopies > 0 && p.Options.Copies > p.MaxCopies {
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
		return nil, errors.New("nil context")
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
		return PrinterProfile{}, errors.New("nil context")
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
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if printer == "" {
		return fmt.Errorf("%w: missing printer", ErrPrinterNotFound)
	}
	if err := validatePrint(PrintOptions{Printer: printer}); err != nil {
		return err
	}
	return w.native.showPrinterProperties(ctx, printer)
}
