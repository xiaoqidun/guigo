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

// Package guigo 轻量、基于系统 WebView 的 Go 语言图形界面库
package guigo

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// TitleBarSystem 使用系统标题栏，为默认值
	TitleBarSystem TitleBarMode = "system"
	// TitleBarContent 将内容延伸到标题栏，保留系统窗口按钮
	TitleBarContent TitleBarMode = "content"
)

const (
	// WindowSolid 使用纯色窗口材质，为默认值
	WindowSolid WindowMaterial = "solid"
	// WindowMicaAlt 使用云母Alt窗口材质
	WindowMicaAlt WindowMaterial = "micaAlt"
	// WindowAcrylic 使用亚克力窗口材质
	WindowAcrylic WindowMaterial = "acrylic"
)

var (
	// ErrUnsupportedPlatform 表示当前平台没有可用宿主实现
	ErrUnsupportedPlatform = errors.New("unsupported platform")
	// ErrCanceled 表示用户取消操作
	ErrCanceled = errors.New("canceled")
	// ErrClosed 表示窗口、托盘或实例已经关闭
	ErrClosed = errors.New("closed")
	// ErrAlreadyRunning 表示当前进程已有活动窗口循环
	ErrAlreadyRunning = errors.New("another Run is active")
	// ErrDialogBusy 表示当前窗口已有活动文件对话框
	ErrDialogBusy = errors.New("another dialog is active")
	// ErrRuntimeUnavailable 表示系统原生浏览器或窗口运行时不可用
	ErrRuntimeUnavailable = errors.New("native runtime unavailable")
)

// HandlerFunc 异步处理页面通过window.guigo.call(name, args, {signal})发起的调用
// 支持Blob/ArrayBuffer/TypedArray二进制直传与BridgeError结构化错误，调用结束、取消或页面退出时取消上下文
// 返回带Discard的Result时，上下文保持到页面确认接收或调用取消
// 入参: ctx 调用上下文, args 已解码参数，二进制为[]byte
// 返回: any 可JSON编码的值或Result, error 错误信息
type HandlerFunc func(context.Context, any) (any, error)

// Result 保存调用结果，Discard在结果未交付时异步释放资源，页面确认接收后不再调用
type Result struct {
	Value   any
	Discard func()
}

// TitleBarMode 定义窗口标题栏的显示方式
type TitleBarMode string

// WindowMaterial 定义窗口的背景材质
type WindowMaterial string

// TitleBarColors 定义内容标题栏激活与失焦的前景色及背景色，颜色使用#RRGGBB格式
type TitleBarColors struct {
	Background         string `json:"background"`
	Foreground         string `json:"foreground"`
	InactiveBackground string `json:"inactiveBackground"`
	InactiveForeground string `json:"inactiveForeground"`
}

// Rect 定义二维平面矩形区域，用于逻辑像素拖动区或毫米可打印区域，零宽高表示空矩形
type Rect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// TitleBarInfo 保存逻辑像素标题栏布局和窗口状态
// Height在系统模式下为零，Left/Right为系统窗口按钮保留宽度，非纯色Material需页面使用透明背景
type TitleBarInfo struct {
	Height    float64        `json:"height"`
	Left      float64        `json:"left"`
	Right     float64        `json:"right"`
	Active    bool           `json:"active"`
	Maximized bool           `json:"maximized"`
	Colors    TitleBarColors `json:"colors"`
	Material  WindowMaterial `json:"material"`
}

// Options 定义窗口、资源和生命周期配置，客户区尺寸单位为逻辑像素，路径前缀/__guigo/由宿主保留
// Instance传入OpenInstance返回的窗口入口并由调用方关闭；AppID与UserDataDir至少设置其一
// TitleBarColors非零时需启用TitleBarContent并提供全部四种颜色；WindowMaterial非纯色需系统支持，否则返回ErrUnsupportedPlatform
// Icon与IconPath互斥，均要求边长不超过2048像素且不超过16MiB的正方形PNG
// Width/Height零值默认1024x768，Min/Max零值不限，FixedSize生效时忽略Maximized
// Handler非空时忽略FS，ResourceHandler返回nil, nil时回退到Handler或FS
// OnReady在首次导航前于后台调用，OnTitleBarChanged在其后串行通知，OnClose在宿主完全退出后执行
type Options struct {
	Instance          *Instance
	AppID             string
	Title             string
	TitleBar          TitleBarMode
	TitleBarColors    TitleBarColors
	WindowMaterial    WindowMaterial
	Icon              []byte
	IconPath          string
	Width             int
	Height            int
	MinWidth          int
	MinHeight         int
	MaxWidth          int
	MaxHeight         int
	FixedSize         bool
	Center            bool
	Maximized         bool
	UserDataDir       string
	LibraryPath       string
	EntryPath         string
	FS                fs.FS
	Handler           http.Handler
	ResourceHandler   func(*http.Request) (*Resource, error)
	Bindings          map[string]HandlerFunc
	OnReady           func(*Window)
	OnTitleBarChanged func(TitleBarInfo)
	OnClosing         func(context.Context) bool
	OnClose           func()
	OnDrop            func([]string)
	OnError           func(error)
}

// Resource 提供已就绪的可寻址响应流，StatusCode零值默认200，为204或304时允许Body为nil，非空Body所有权交给宿主并由宿主关闭
// ResourceHandler返回nil, nil时回退到Handler或FS，范围请求需由处理器提供对应状态与区间内容
type Resource struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadSeekCloser
}

// FileFilter 定义文件类型显示名及分号分隔的通配符模式，如*.txt;*.md
type FileFilter struct {
	Name    string
	Pattern string
}

// DialogOptions 定义原生文件对话框标题、初始路径、默认文件名及文件类型过滤，OpenDirectory忽略DefaultName与Filters
type DialogOptions struct {
	Title       string
	DefaultPath string
	DefaultName string
	Filters     []FileFilter
}

// BridgeError 定义传递给JavaScript的结构化错误，Name为空时默认Error，Code为空时不传递
type BridgeError struct {
	Name    string
	Code    string
	Message string
}

// Window 持有窗口宿主，由OnReady提供，所有公开方法可由后台Goroutine调用
type Window struct {
	native *nativeWindow
}

// envelope 表示双向消息信封
type envelope struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`
	Args      any    `json:"args,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
	Value     any    `json:"value"`
	Error     string `json:"error,omitempty"`
	ErrorName string `json:"errorName,omitempty"`
	Code      string `json:"code,omitempty"`
}

// SupportsWindowMaterial 查询当前平台是否提供指定窗口材质，空值视为WindowSolid
// 入参: material 窗口材质
// 返回: bool 是否支持
func SupportsWindowMaterial(material WindowMaterial) bool {
	if material == "" {
		material = WindowSolid
	}
	return supportsWindowMaterial(material)
}

// SystemDark 返回系统当前是否偏好深色外观，未适配平台或查询失败时返回false
// 返回: bool 是否偏好深色外观
func SystemDark() bool {
	return systemDark()
}

// DefaultUserDataDir 返回应用的默认浏览器数据目录，不创建或删除目录
// 目录位于os.UserCacheDir返回的位置下，使用guigo/应用标识作为子目录，Windows使用GUIGo目录名
// 入参: appID 稳定的应用标识
// 返回: string 绝对目录路径, error 参数或路径错误
func DefaultUserDataDir(appID string) (string, error) {
	if err := validateAppID(appID); err != nil {
		return "", err
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	name := "guigo"
	if runtime.GOOS == "windows" {
		name = "GUIGo"
	}
	return filepath.Abs(filepath.Join(base, name, appID))
}

// OpenURL 使用系统默认浏览器打开http或https地址
// 入参: rawURL 外部地址
// 返回: error 错误信息
func OpenURL(rawURL string) error {
	if !externalURL(rawURL) {
		return errors.New("external URL must use http or https")
	}
	return openURL(rawURL)
}

// Run 运行窗口事件循环，关闭后返回，同一进程不支持同时运行多个实例
// 当前实现支持windows/amd64及linux/amd64、linux/arm64，其他平台返回ErrUnsupportedPlatform
// Windows使用调用线程，Linux使用进程内固定的GTK线程
// 浏览器数据在关闭、启动失败和版本升级时保留，删除由调用方负责
// 入参: ctx 运行上下文, options 宿主选项
// 返回: error 启动、运行或上下文错误
func Run(ctx context.Context, options Options) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := validate(&options); err != nil {
		return err
	}
	if options.AppID == "" && options.UserDataDir == "" {
		return errors.New("AppID or UserDataDir is required")
	}
	return run(ctx, options)
}

// validateTitleBarColors 校验内容标题栏的完整配色或系统默认值
// 入参: colors 标题栏配色
// 返回: error 配色错误
func validateTitleBarColors(colors TitleBarColors) error {
	if colors == (TitleBarColors{}) {
		return nil
	}
	for _, value := range []string{colors.Background, colors.Foreground, colors.InactiveBackground, colors.InactiveForeground} {
		if len(value) != 7 || value[0] != '#' {
			return errors.New("title bar color must use #RRGGBB")
		}
		if _, err := hex.DecodeString(value[1:]); err != nil {
			return errors.New("title bar color must use #RRGGBB")
		}
	}
	return nil
}

// validateDialog 检查传入原生对话框的字符串
// 入参: o 对话框选项
// 返回: error 校验错误
func validateDialog(o DialogOptions) error {
	values := []string{o.Title, o.DefaultPath, o.DefaultName}
	for _, filter := range o.Filters {
		values = append(values, filter.Name, filter.Pattern)
	}
	for _, value := range values {
		if strings.ContainsRune(value, 0) {
			return errors.New("dialog option contains NUL")
		}
	}
	return nil
}

// dispatch 校验调用并隔离业务处理器异常
// 入参: ctx 调用上下文, bindings 方法集合, call 已解码请求
// 返回: envelope 调用结果
func dispatch(ctx context.Context, bindings map[string]HandlerFunc, call envelope) (reply envelope) {
	reply.Type = "reply"
	defer func() {
		if recover() != nil {
			reply.Error = "handler panic"
			reply.Value = nil
		}
	}()
	reply.ID = call.ID
	if err := ctx.Err(); err != nil {
		reply.Error, reply.ErrorName, reply.Code = err.Error(), "AbortError", "canceled"
		return
	}
	if call.Type != "call" || call.ID == "" || call.Name == "" {
		reply.Error = "invalid call"
		return
	}
	handler := bindings[call.Name]
	if handler == nil {
		reply.Error = "unknown method: " + call.Name
		return
	}
	value, err := handler(ctx, call.Args)
	if err != nil {
		reply.Error = err.Error()
		if reply.Error == "" {
			reply.Error = "handler failed"
		}
		var bridge *BridgeError
		if errors.As(err, &bridge) && bridge != nil {
			reply.ErrorName = bridge.Name
			reply.Code = bridge.Code
		}
		if errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reply.ErrorName = "AbortError"
			reply.Code = "canceled"
		}
		return
	}
	reply.Value = value
	return
}

// marshalReply 编码调用结果并保留编码错误的响应标识
// 入参: reply 调用结果
// 返回: []byte 响应数据
func marshalReply(reply envelope) (data []byte) {
	defer func() {
		if recover() != nil {
			reply.Value = nil
			reply.Error = "result is not JSON encodable"
			data, _ = json.Marshal(reply)
		}
	}()
	data, err := json.Marshal(reply)
	if err == nil {
		return data
	}
	reply.Value = nil
	reply.Error = "result is not JSON encodable"
	data, _ = json.Marshal(reply)
	return data
}

// validateAppID 检查应用标识及目录字符限制
// 入参: appID 应用标识
// 返回: error 校验错误
func validateAppID(appID string) error {
	if len(appID) == 0 || len(appID) > 128 || !(appID[0] >= 'a' && appID[0] <= 'z' || appID[0] >= 'A' && appID[0] <= 'Z') || strings.HasSuffix(appID, ".") {
		return errors.New("invalid application ID")
	}
	for _, c := range appID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return errors.New("invalid application ID")
		}
	}
	return nil
}

// clampWindowSize 按最小最大约束限制客户区逻辑尺寸
// 入参: o 宿主选项, width 目标宽度, height 目标高度
// 返回: int 限制后宽度, int 限制后高度
func clampWindowSize(o Options, width, height int) (int, int) {
	width = max(width, o.MinWidth)
	height = max(height, o.MinHeight)
	if o.MaxWidth > 0 {
		width = min(width, o.MaxWidth)
	}
	if o.MaxHeight > 0 {
		height = min(height, o.MaxHeight)
	}
	return width, height
}

// validate 补全默认尺寸并检查约束
// 入参: o 宿主选项
// 返回: error 校验错误
func validate(o *Options) error {
	if o.Instance != nil && (o.Instance.listener == nil || o.Instance.closed.Load()) {
		return ErrClosed
	}
	if o.TitleBar == "" {
		o.TitleBar = TitleBarSystem
	}
	if o.TitleBar != TitleBarSystem && o.TitleBar != TitleBarContent {
		return errors.New("invalid title bar mode")
	}
	if o.WindowMaterial == "" {
		o.WindowMaterial = WindowSolid
	}
	if o.WindowMaterial != WindowSolid && o.WindowMaterial != WindowMicaAlt && o.WindowMaterial != WindowAcrylic {
		return errors.New("invalid window material")
	}
	if o.WindowMaterial != WindowSolid {
		if !SupportsWindowMaterial(o.WindowMaterial) {
			return ErrUnsupportedPlatform
		}
	}
	if o.TitleBarColors != (TitleBarColors{}) {
		if o.TitleBar != TitleBarContent {
			return errors.New("title bar colors require content title bar")
		}
		if err := validateTitleBarColors(o.TitleBarColors); err != nil {
			return err
		}
	}
	if o.AppID != "" {
		if err := validateAppID(o.AppID); err != nil {
			return err
		}
	}
	for _, value := range []string{o.Title, o.IconPath, o.EntryPath, o.LibraryPath, o.UserDataDir} {
		if strings.ContainsRune(value, 0) {
			return errors.New("option contains NUL")
		}
	}
	if o.EntryPath != "" && isReservedPath(strings.SplitN(strings.SplitN(o.EntryPath, "?", 2)[0], "#", 2)[0]) {
		return errors.New("entry path uses reserved prefix")
	}
	if len(o.Icon) > 0 && o.IconPath != "" {
		return errors.New("icon data and path are mutually exclusive")
	}
	if len(o.Icon) > 16<<20 {
		return errors.New("icon exceeds 16 MiB")
	}
	o.Icon = append([]byte(nil), o.Icon...)
	for _, value := range []int{o.Width, o.Height, o.MinWidth, o.MinHeight, o.MaxWidth, o.MaxHeight} {
		if value > 32767 {
			return errors.New("window dimension exceeds 32767")
		}
	}
	if o.Width == 0 {
		o.Width = 1024
	}
	if o.Height == 0 {
		o.Height = 768
	}
	if o.Width < 1 || o.Height < 1 || o.MinWidth < 0 || o.MinHeight < 0 || o.MaxWidth < 0 || o.MaxHeight < 0 {
		return errors.New("invalid window dimensions")
	}
	if o.MaxWidth > 0 && o.MinWidth > o.MaxWidth || o.MaxHeight > 0 && o.MinHeight > o.MaxHeight {
		return errors.New("invalid size constraints")
	}
	o.Width, o.Height = clampWindowSize(*o, o.Width, o.Height)
	if o.Handler == nil {
		if o.FS != nil {
			o.Handler = fileHandler{http.FileServerFS(o.FS)}
		} else {
			o.Handler = http.NotFoundHandler()
		}
	}
	bindings := make(map[string]HandlerFunc, len(o.Bindings))
	for k, v := range o.Bindings {
		if k == "" || v == nil {
			return errors.New("invalid binding")
		}
		bindings[k] = v
	}
	o.Bindings = bindings
	return nil
}

// Error 返回错误说明，nil接收者安全返回空字符串
// 返回: string 错误信息
func (e *BridgeError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Emit 向页面发送命名事件，前端通过window.guigo.on(name, listener)订阅，返回值表示已交给宿主而非已消费
// 首屏就绪前最多缓存1024条待发事件，超限返回错误
// 入参: name 非空事件名称, value 可JSON编码的数据
// 返回: error 错误信息
func (w *Window) Emit(name string, value any) error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if name == "" {
		return errors.New("empty event name")
	}
	b, err := json.Marshal(envelope{Type: "event", Name: name, Value: value})
	if err != nil {
		return err
	}
	return w.native.emit(string(b))
}

// Close 请求关闭窗口，OnClosing和页面beforeunload可以取消此次请求
// 返回: error 请求投递错误
func (w *Window) Close() error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	return w.native.close()
}

// Hide 隐藏窗口但保留宿主，首次页面加载完成后仍保持隐藏，可用Activate恢复
// 调用方应保留托盘或其他恢复入口
// 返回: error 请求投递错误
func (w *Window) Hide() error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	return w.native.hide()
}

// Activate 请求恢复并显示窗口，是否获得焦点由桌面系统决定
// 首屏加载完成前调用时，窗口仍按正常启动流程显示
// 返回: error 请求投递错误
func (w *Window) Activate() error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	return w.native.activate()
}

// SetTitle 修改窗口标题
// 入参: title 窗口标题
// 返回: error 错误信息
func (w *Window) SetTitle(title string) error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if strings.ContainsRune(title, 0) {
		return errors.New("title contains NUL")
	}
	return w.native.setTitle(title)
}

// TitleBarInfo 返回当前标题栏布局和窗口状态
// 返回: TitleBarInfo 标题栏信息, error 查询错误
func (w *Window) TitleBarInfo() (TitleBarInfo, error) {
	if w == nil || w.native == nil {
		return TitleBarInfo{}, ErrClosed
	}
	return w.native.titleBarInfo()
}

// SetTitleBarColors 更新内容标题栏配色，零值恢复系统外观，系统标题栏模式返回错误
// 入参: colors 激活及失焦配色，非零值必须完整提供四种#RRGGBB颜色
// 返回: error 配色、模式或窗口错误
func (w *Window) SetTitleBarColors(colors TitleBarColors) error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if err := validateTitleBarColors(colors); err != nil {
		return err
	}
	return w.native.setTitleBarColors(colors)
}

// SetWindowMaterial 更新窗口背景材质，空值视为WindowSolid，不支持的材质返回ErrUnsupportedPlatform
// 入参: material 窗口材质，纯色关闭系统背景材质
// 返回: error 材质或窗口错误
func (w *Window) SetWindowMaterial(material WindowMaterial) error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if material == "" {
		material = WindowSolid
	}
	if material != WindowSolid && material != WindowMicaAlt && material != WindowAcrylic {
		return errors.New("invalid window material")
	}
	if !SupportsWindowMaterial(material) {
		return ErrUnsupportedPlatform
	}
	return w.native.setWindowMaterial(material)
}

// SetDragRegions 替换内容标题栏的原生拖动区域，空切片清除区域，系统模式返回错误
// 区域使用客户区逻辑像素，不随页面缩放变化，调用方负责排除交互控件并随布局更新
// 超出客户区的部分裁剪，系统按钮及调整大小的边缘优先，不改变页面内容或注入JS接口
// 入参: regions 拖动矩形集合，调用时复制，坐标和尺寸必须有限且尺寸非负
// 返回: error 设置错误
func (w *Window) SetDragRegions(regions []Rect) error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	regions = append([]Rect(nil), regions...)
	for _, r := range regions {
		for _, v := range []float64{r.X, r.Y, r.Width, r.Height, r.X + r.Width, r.Y + r.Height} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("drag region must be finite")
			}
		}
		if r.Width < 0 || r.Height < 0 {
			return errors.New("invalid drag region dimensions")
		}
	}
	return w.native.setDragRegions(regions)
}

// SetSize 修改客户区尺寸并遵循约束
// 入参: width 客户区宽度, height 客户区高度
// 返回: error 错误信息
func (w *Window) SetSize(width, height int) error {
	if w == nil || w.native == nil {
		return ErrClosed
	}
	if width < 1 || height < 1 || width > 32767 || height > 32767 {
		return errors.New("invalid window dimensions")
	}
	return w.native.setSize(width, height)
}

// OpenFile 选择单个已有文件
// 入参: ctx 调用上下文, options 对话框选项
// 返回: string 文件路径, error 错误信息，用户取消返回ErrCanceled，并发调用返回ErrDialogBusy
func (w *Window) OpenFile(ctx context.Context, options DialogOptions) (string, error) {
	if ctx == nil {
		return "", errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if w == nil || w.native == nil {
		return "", ErrClosed
	}
	if err := validateDialog(options); err != nil {
		return "", err
	}
	return w.native.dialog(ctx, options, 0)
}

// OpenFiles 选择多个已有文件
// 入参: ctx 调用上下文, options 对话框选项
// 返回: []string 文件路径, error 错误信息，用户取消返回ErrCanceled，并发调用返回ErrDialogBusy
func (w *Window) OpenFiles(ctx context.Context, options DialogOptions) ([]string, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w == nil || w.native == nil {
		return nil, ErrClosed
	}
	if err := validateDialog(options); err != nil {
		return nil, err
	}
	return w.native.dialogs(ctx, options, 3)
}

// SaveFile 选择保存路径并提示覆盖已有文件
// 入参: ctx 调用上下文, options 对话框选项
// 返回: string 保存路径, error 错误信息，用户取消返回ErrCanceled，并发调用返回ErrDialogBusy
func (w *Window) SaveFile(ctx context.Context, options DialogOptions) (string, error) {
	if ctx == nil {
		return "", errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if w == nil || w.native == nil {
		return "", ErrClosed
	}
	if err := validateDialog(options); err != nil {
		return "", err
	}
	return w.native.dialog(ctx, options, 1)
}

// OpenDirectory 选择已有目录
// 入参: ctx 调用上下文, options 对话框选项
// 返回: string 目录路径, error 错误信息，用户取消返回ErrCanceled，并发调用返回ErrDialogBusy
func (w *Window) OpenDirectory(ctx context.Context, options DialogOptions) (string, error) {
	if ctx == nil {
		return "", errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if w == nil || w.native == nil {
		return "", ErrClosed
	}
	if err := validateDialog(options); err != nil {
		return "", err
	}
	return w.native.dialog(ctx, options, 2)
}
