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

//go:build !(windows && amd64) && !(linux && !android && (amd64 || arm64))

package guigo

import "context"

// nativeWindow 提供未适配平台的宿主占位类型
type nativeWindow struct{}

// SupportsWindowMaterial 查询当前平台是否提供指定窗口材质
// 入参: material 窗口材质
// 返回: bool 未适配平台返回false
func SupportsWindowMaterial(material WindowMaterial) bool {
	return false
}

// activate 返回未适配平台错误
// 返回: error 平台错误
func (n *nativeWindow) activate() error {
	return ErrUnsupportedPlatform
}

// hide 返回未适配平台错误
// 返回: error 平台错误
func (n *nativeWindow) hide() error {
	return ErrUnsupportedPlatform
}

// listPrinters 返回未适配平台的设备查询错误
// 入参: ctx 查询上下文
// 返回: []PrinterInfo 空队列, error 平台错误
func (n *nativeWindow) listPrinters(ctx context.Context) ([]PrinterInfo, error) {
	return nil, ErrUnsupportedPlatform
}

// queryPrinter 返回未适配平台的设备查询错误
// 入参: ctx 查询上下文, o 设备打印选项
// 返回: PrinterProfile 空设备信息, error 平台错误
func (n *nativeWindow) queryPrinter(ctx context.Context, o PrintOptions) (PrinterProfile, error) {
	return PrinterProfile{}, ErrUnsupportedPlatform
}

// showPrinterProperties 返回未适配平台的设备属性错误
// 入参: ctx 调用上下文, name 系统队列名称
// 返回: error 平台错误
func (n *nativeWindow) showPrinterProperties(ctx context.Context, name string) error {
	return ErrUnsupportedPlatform
}

// printCapabilities 返回未适配平台的打印错误
// 入参: ctx 查询上下文
// 返回: PrintCapabilities 空能力信息, error 平台错误
func (n *nativeWindow) printCapabilities(ctx context.Context) (PrintCapabilities, error) {
	return PrintCapabilities{}, ErrUnsupportedPlatform
}

// showPrintUI 返回未适配平台的打印错误
// 入参: ctx 调用上下文, kind 打印窗口类型
// 返回: error 平台错误
func (n *nativeWindow) showPrintUI(ctx context.Context, kind string) error {
	return ErrUnsupportedPlatform
}

// print 返回未适配平台的打印错误
// 入参: ctx 调用上下文, o 设备打印选项
// 返回: PrintResult 空提交结果, error 平台错误
func (n *nativeWindow) print(ctx context.Context, o PrintOptions) (PrintResult, error) {
	return PrintResult{}, ErrUnsupportedPlatform
}

// resourceURL 返回未适配平台的空资源地址
// 入参: path 资源路径
// 返回: string 空地址
func resourceURL(path string) string {
	return ""
}

// openURL 返回平台不支持错误
// 入参: raw 外部地址
// 返回: error 平台错误
func openURL(raw string) error {
	return ErrUnsupportedPlatform
}

// run 返回平台不支持错误
// 入参: parent 运行上下文, o 宿主选项
// 返回: error 平台错误
func run(parent context.Context, o Options) error {
	return ErrUnsupportedPlatform
}

// emit 返回平台不支持错误
// 入参: raw 消息文本
// 返回: error 平台错误
func (n *nativeWindow) emit(raw string) error {
	return ErrUnsupportedPlatform
}

// close 返回平台不支持错误
// 返回: error 平台错误
func (n *nativeWindow) close() error {
	return ErrUnsupportedPlatform
}

// setTitle 返回平台不支持错误
// 入参: title 窗口标题
// 返回: error 平台错误
func (n *nativeWindow) setTitle(title string) error {
	return ErrUnsupportedPlatform
}

// titleBarInfo 返回平台不支持错误
// 返回: TitleBarInfo 空标题栏信息, error 平台错误
func (n *nativeWindow) titleBarInfo() (TitleBarInfo, error) {
	return TitleBarInfo{}, ErrUnsupportedPlatform
}

// setDragRegions 返回平台不支持错误
// 入参: regions 拖动矩形集合
// 返回: error 平台错误
func (n *nativeWindow) setDragRegions(regions []Rect) error {
	return ErrUnsupportedPlatform
}

// setSize 返回平台不支持错误
// 入参: w 客户区宽度, h 客户区高度
// 返回: error 平台错误
func (n *nativeWindow) setSize(w, h int) error {
	return ErrUnsupportedPlatform
}

// dialog 返回平台不支持错误
// 入参: o 对话框选项, mode 对话框类型
// 返回: string 空路径, error 平台错误
func (n *nativeWindow) dialog(o DialogOptions, mode int) (string, error) {
	return "", ErrUnsupportedPlatform
}

// dialogs 返回平台不支持错误
// 入参: o 对话框选项, mode 对话框类型
// 返回: []string 空路径列表, error 平台错误
func (n *nativeWindow) dialogs(o DialogOptions, mode int) ([]string, error) {
	return nil, ErrUnsupportedPlatform
}

// setTitleBarColors 返回未适配平台错误
// 入参: colors 标题栏配色
// 返回: error 平台错误
func (n *nativeWindow) setTitleBarColors(colors TitleBarColors) error {
	return ErrUnsupportedPlatform
}

// setWindowMaterial 返回未适配平台错误
// 入参: material 窗口材质
// 返回: error 平台错误
func (n *nativeWindow) setWindowMaterial(material WindowMaterial) error {
	return ErrUnsupportedPlatform
}
