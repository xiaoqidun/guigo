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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

// resourceOrigin 为WebKitGTK拦截的内部资源来源
const resourceOrigin = "guigo://localhost"

var active atomic.Pointer[nativeWindow]
var linuxRunning atomic.Bool
var linuxThread sync.Once
var linuxRuns = make(chan linuxRun)
var linuxCallbacks = struct {
	tick, closing, closed, loaded, message, scheme, headers, policy, failed, dialog, drop, loadFailed, startup uintptr
	titleChanged, titlePosition, dragPosition                                                                  uintptr
}{}

// nativeWindow 保存GTK线程拥有的窗口及页面状态
type nativeWindow struct {
	options                                          Options
	mu                                               sync.Mutex
	queue                                            []linuxJob
	closed                                           bool
	ctx                                              context.Context
	cancel                                           context.CancelFunc
	done                                             chan struct{}
	window, view, manager, webContext, session, loop uintptr
	dropTarget                                       uintptr
	captionOverlay, dragOverlay                      uintptr
	captionControls                                  [2]uintptr
	captionStyle, captionDisplay                     uintptr
	dragHandles                                      []linuxDragHandle
	titleInfo                                        TitleBarInfo
	titleScale                                       int32
	titleCallbacksReady                              bool
	titleInfoSet, titleUpdatePending                 bool
	titleCallbackRunning, titleCallbackPending       bool
	pageCtx                                          context.Context
	pageCancel                                       context.CancelFunc
	calls                                            *callQueue
	generation                                       uint64
	ready, closing                                   bool
	closePending                                     bool
	pendingEvents                                    []string
	resources                                        map[uintptr]bool
	dialogsPending                                   map[uintptr]*linuxDialog
	nextDialog                                       uintptr
	iconDir                                          string
	iconData                                         []byte
	trays                                            map[uintptr]*linuxTray
	bridgeToken                                      string
	source, startupTimer                             uint32
	shown                                            bool
	hidden                                           bool
	err                                              error
}

// linuxJob 保存线程操作及关闭时的资源释放操作
type linuxJob struct {
	run     func()
	discard func()
}

// linuxRun 保存传给固定GTK线程的运行请求
type linuxRun struct {
	parent  context.Context
	options Options
	result  chan error
}

// linuxDragHandle 保存由GTK原生标题栏手势处理的内容区矩形
type linuxDragHandle struct {
	widget uintptr
	region Rect
}

// init 注册进程生命周期内复用的原生回调
func init() {
	linuxCallbacks.tick = purego.NewCallback(linuxTick)
	linuxCallbacks.closing = purego.NewCallback(linuxClosing)
	linuxCallbacks.closed = purego.NewCallback(linuxClosed)
	linuxCallbacks.loaded = purego.NewCallback(linuxLoadedPage)
	linuxCallbacks.message = purego.NewCallback(linuxMessage)
	linuxCallbacks.scheme = purego.NewCallback(linuxResource)
	linuxCallbacks.headers = purego.NewCallback(linuxHeaders)
	linuxCallbacks.policy = purego.NewCallback(linuxPolicy)
	linuxCallbacks.failed = purego.NewCallback(linuxProcessFailed)
	linuxCallbacks.dialog = purego.NewCallback(linuxDialogFinished)
	linuxCallbacks.drop = purego.NewCallback(linuxDrop)
	linuxCallbacks.loadFailed = purego.NewCallback(linuxLoadFailed)
	linuxCallbacks.startup = purego.NewCallback(linuxStartupTimeout)
	linuxCallbacks.titleChanged = purego.NewCallback(linuxTitleChanged)
	linuxCallbacks.titlePosition = purego.NewCallback(linuxTitlePosition)
	linuxCallbacks.dragPosition = purego.NewCallback(linuxDragPosition)
}

// SupportsWindowMaterial 查询当前平台是否提供指定窗口材质
// 入参: material 窗口材质
// 返回: bool 是否支持
func SupportsWindowMaterial(material WindowMaterial) bool {
	return material == WindowSolid
}

// run 在固定GTK线程创建窗口并等待退出
// 入参: parent 运行上下文, o 宿主选项
// 返回: error 启动或上下文错误
func run(parent context.Context, o Options) error {
	if !linuxRunning.CompareAndSwap(false, true) {
		return errors.New("another Run is active")
	}
	defer linuxRunning.Store(false)
	linuxThread.Do(func() {
		go func() {
			runtime.LockOSThread()
			for request := range linuxRuns {
				request.result <- runLinux(request.parent, request.options)
			}
		}()
	})
	result := make(chan error, 1)
	linuxRuns <- linuxRun{parent: parent, options: o, result: result}
	return <-result
}

// runLinux 创建GTK窗口并运行原生事件循环
// 入参: parent 运行上下文, o 宿主选项
// 返回: error 启动或上下文错误
func runLinux(parent context.Context, o Options) error {
	n := &nativeWindow{options: o, bridgeToken: rand.Text(), done: make(chan struct{}), resources: make(map[uintptr]bool), dialogsPending: make(map[uintptr]*linuxDialog)}
	if !active.CompareAndSwap(nil, n) {
		return errors.New("another Run is active")
	}
	defer active.Store(nil)
	if err := parent.Err(); err != nil {
		return err
	}
	if err := loadLinux(o.LibraryPath); err != nil {
		return err
	}
	if o.AppID != "" {
		defer gtk.Prgname(gtk.GetPrgname())
		name := append([]byte(o.AppID), 0)
		gtk.Prgname(&name[0])
	}
	if !gtk.Init() {
		return errors.New("GTK could not open display")
	}
	data, err := iconData(o)
	if err != nil {
		return err
	}
	dir := o.UserDataDir
	if dir == "" {
		dir, err = DefaultUserDataDir(o.AppID)
	} else {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	n.ctx, n.cancel = context.WithCancel(parent)
	defer n.cancel()
	n.webContext = gtk.ContextNew()
	n.session = gtk.SessionNew(filepath.Join(dir, "data"), filepath.Join(dir, "cache"))
	n.manager = gtk.ManagerNew()
	n.loop = gtk.LoopNew(0, false)
	defer gtk.Unref(n.webContext)
	defer gtk.Unref(n.session)
	defer gtk.Unref(n.manager)
	defer gtk.LoopUnref(n.loop)
	security := gtk.Security(n.webContext)
	gtk.SecureScheme(security, "guigo")
	gtk.CorsScheme(security, "guigo")
	gtk.RegisterScheme(n.webContext, "guigo", linuxCallbacks.scheme, 0, 0)
	gtk.Connect(n.manager, "script-message-received::guigo", linuxCallbacks.message, 0, 0, 0)
	if !gtk.RegisterMessage(n.manager, "guigo", 0) {
		return errors.New("register script message handler")
	}
	script := gtk.ScriptNew(linuxBridgeScript(n.bridgeToken), 1, 0, 0, 0)
	gtk.AddScript(n.manager, script)
	gtk.ScriptUnref(script)
	n.view = n.newView()
	if n.view == 0 {
		return errors.New("create WebKitGTK view")
	}
	gtk.Sink(n.view)
	defer gtk.Unref(n.view)
	n.window = gtk.WindowNew()
	gtk.Ref(n.window)
	defer gtk.Unref(n.window)
	defer func() {
		gtk.Destroy(n.window)
		if n.iconDir != "" {
			_ = os.RemoveAll(n.iconDir)
		}
		if o.OnClose != nil {
			o.OnClose()
		}
	}()
	gtk.Title(n.window, o.Title)
	gtk.Size(n.window, int32(o.Width), int32(o.Height))
	gtk.Minimum(n.view, int32(o.MinWidth), int32(o.MinHeight))
	gtk.Resizable(n.window, !o.FixedSize)
	n.initTitleBar()
	defer n.freeTitleBarColors()
	if len(data) != 0 {
		n.iconData = data
		n.iconDir, err = linuxIconDirectory()
		if err != nil {
			return err
		}
		iconName := filepath.Base(n.iconDir)
		if err = os.WriteFile(filepath.Join(n.iconDir, iconName+".png"), data, 0600); err != nil {
			return err
		}
		gtk.IconPath(gtk.IconTheme(gtk.Display(n.window)), n.iconDir)
		gtk.IconName(n.window, iconName)
	}
	gtk.Connect(n.window, "close-request", linuxCallbacks.closing, 0, 0, 0)
	gtk.Connect(n.view, "close", linuxCallbacks.closed, 0, 0, 0)
	gtk.Connect(n.view, "load-changed", linuxCallbacks.loaded, 0, 0, 0)
	gtk.Connect(n.view, "load-failed", linuxCallbacks.loadFailed, 0, 0, 0)
	gtk.Connect(n.view, "decide-policy", linuxCallbacks.policy, 0, 0, 0)
	gtk.Connect(n.view, "web-process-terminated", linuxCallbacks.failed, 0, 0, 0)
	if o.OnDrop != nil {
		target := gtk.DropNew(gtk.DropType(), 1)
		n.dropTarget = target
		gtk.ControllerPhase(target, 1)
		gtk.Connect(target, "drop", linuxCallbacks.drop, 0, 0, 0)
		gtk.AddController(n.view, target)
	}
	n.newPage()
	defer n.shutdown()
	stopCancel := context.AfterFunc(n.ctx, func() { _ = n.enqueue(n.shutdown) })
	defer stopCancel()
	defer func() {
		if n.startupTimer != 0 {
			gtk.SourceRemove(n.startupTimer)
		}
		n.mu.Lock()
		source := n.source
		n.source = 0
		n.mu.Unlock()
		if source != 0 {
			gtk.SourceRemove(source)
		}
	}()
	go func() {
		if o.OnReady != nil {
			func() {
				defer func() {
					if v := recover(); v != nil {
						n.fail(fmt.Errorf("OnReady panic: %v", v))
					}
				}()
				o.OnReady(&Window{native: n})
			}()
		}
		n.startTitleCallbacks()
		_ = n.enqueue(func() {
			n.startupTimer = gtk.Timeout(0, 30000, linuxCallbacks.startup, 0, 0)
			gtk.Load(n.view, resourceURL(o.EntryPath))
		})
	}()
	gtk.LoopRun(n.loop)
	if n.err != nil {
		return n.err
	}
	return parent.Err()
}

// newView 使用固定参数属性接口创建浏览器对象
// 返回: uintptr 浏览器对象
func (n *nativeWindow) newView() uintptr {
	names := [][]byte{[]byte("web-context\x00"), []byte("network-session\x00"), []byte("user-content-manager\x00")}
	pointers := [3]*byte{&names[0][0], &names[1][0], &names[2][0]}
	objects := [3]uintptr{n.webContext, n.session, n.manager}
	var values [3]nativeValue
	for i := range values {
		gtk.ValueInit(&values[i], 80)
		gtk.ValueObject(&values[i], objects[i])
		defer gtk.ValueUnset(&values[i])
	}
	return gtk.ObjectNew(gtk.ViewType(), 3, &pointers[0], &values[0])
}

// linuxIconDirectory 创建仅用于图标的私有临时目录
// 返回: string 目录路径, error 创建错误
func linuxIconDirectory() (string, error) {
	for range 32 {
		number, err := rand.Int(rand.Reader, big.NewInt(10000000))
		if err != nil {
			return "", err
		}
		path := filepath.Join(os.TempDir(), fmt.Sprintf("guigo-%07d", number.Int64()))
		if err = os.Mkdir(path, 0700); err == nil {
			return path, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", errors.New("could not allocate icon directory")
}

// linuxLoadFailed 报告导航错误并终止首次加载失败
// 入参: view 浏览器对象, event 加载阶段, uri 失败地址, nativeErr 原生错误, data 用户数据
// 返回: uintptr 是否已经处理
func linuxLoadFailed(view uintptr, event int32, uri *byte, nativeErr *linuxError, data uintptr) uintptr {
	n := active.Load()
	if n == nil || n.view != view {
		return 0
	}
	if n.ctx.Err() != nil {
		return 1
	}
	message := "navigation failed"
	if nativeErr != nil {
		message = nativeString(nativeErr.Message)
	}
	err := fmt.Errorf("load %s: %s", nativeString(uri), message)
	n.fail(err)
	if !n.shown {
		n.err = err
		n.shutdown()
	}
	return 1
}

// linuxStartupTimeout 终止超时的首次页面加载
// 入参: data 用户数据
// 返回: uintptr 是否继续计时
func linuxStartupTimeout(data uintptr) uintptr {
	if n := active.Load(); n != nil && !n.shown {
		n.startupTimer = 0
		n.err = errors.New("initial page load timed out")
		n.fail(n.err)
		n.shutdown()
	}
	return 0
}

// linuxBridgeScript 适配WebKit消息入口并保留公共桥接协议
// 入参: token 仅向受信顶层页面注入的消息凭据
// 返回: string 文档初始化脚本
func linuxBridgeScript(token string) string {
	encoded, _ := json.Marshal(token)
	script := strings.Replace(bridgeScript, `location.origin !== "`+resourceOrigin+`"`, `(location.protocol !== "guigo:" || location.host !== "localhost")`, 1)
	script = strings.ReplaceAll(script, "chrome.webview", "transport")
	script = strings.ReplaceAll(script, `method:"POST", body, signal`, `method:"POST", body:await body.arrayBuffer(), signal`)
	return `(() => {
 if (location.protocol !== "guigo:" || location.host !== "localhost" || window !== window.top) return;
 const nativeFetch = window.fetch.bind(window);
 window.fetch = async (input, options) => {
  const url = new URL(input instanceof Request ? input.url : input, location.href);
  if (url.protocol === "guigo:" && url.host === "localhost") {
   if (options?.body instanceof Blob) options = {...options, body:await options.body.arrayBuffer()};
   else if (input instanceof Request && options?.body === undefined && input.method !== "GET" && input.method !== "HEAD") {
    options = {...options, body:await input.clone().arrayBuffer()};
   }
  }
  return nativeFetch(input, options);
 };
 let receive;
 const transport = {
  postMessage: message => window.webkit.messageHandlers.guigo.postMessage(JSON.stringify({...message,token:` + string(encoded) + `})),
  addEventListener: (name, listener) => { receive = listener; }
 };
 Object.defineProperty(window,"__guigoReceive",{value:message => receive?.({data:message})});
 ` + script + `
 })();`
}

// enqueue 将操作交给GTK所属线程
// 入参: fn 原生操作
// 返回: error 关闭错误
func (n *nativeWindow) enqueue(fn func()) error {
	return n.enqueueCleanup(fn, nil)
}

// enqueueCleanup 将操作及资源释放函数交给GTK线程
// 入参: fn 原生操作, discard 关闭时的清理操作
// 返回: error 关闭错误
func (n *nativeWindow) enqueueCleanup(fn, discard func()) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return ErrClosed
	}
	n.queue = append(n.queue, linuxJob{run: fn, discard: discard})
	if n.source == 0 {
		n.source = gtk.Idle(0, linuxCallbacks.tick, 0, 0)
	}
	return nil
}

// linuxTick 处理跨线程操作及上下文取消
// 入参: data 原生用户数据
// 返回: uintptr 是否继续计时
func linuxTick(data uintptr) uintptr {
	n := active.Load()
	if n == nil {
		return 0
	}
	n.mu.Lock()
	work := n.queue
	n.queue = nil
	n.source = 0
	n.mu.Unlock()
	for _, job := range work {
		n.mu.Lock()
		closed := n.closed
		n.mu.Unlock()
		if !closed {
			job.run()
		} else if job.discard != nil {
			job.discard()
		}
	}
	if n.ctx.Err() != nil {
		n.shutdown()
	}
	return 0
}

// newPage 更新页面上下文及调用队列
func (n *nativeWindow) newPage() {
	if n.pageCancel != nil {
		n.pageCancel()
	}
	n.pageCtx, n.pageCancel = context.WithCancel(n.ctx)
	n.calls = newCallQueue(n.pageCtx)
	n.generation++
	n.ready = false
}

// linuxLoadedPage 跟踪导航并发送等待中的事件
// 入参: view 浏览器对象, event 加载状态, data 用户数据
func linuxLoadedPage(view uintptr, event int32, data uintptr) {
	n := active.Load()
	if n == nil || n.view != view {
		return
	}
	if event == 0 {
		n.newPage()
	}
	if event == 3 {
		if !n.shown && n.err == nil && n.ctx.Err() == nil {
			n.shown = true
			if n.startupTimer != 0 {
				gtk.SourceRemove(n.startupTimer)
				n.startupTimer = 0
			}
			if n.options.Maximized && !n.options.FixedSize {
				gtk.Maximize(n.window)
			}
			if !n.hidden {
				gtk.Present(n.window)
			}
		}
		n.ready = true
		events := n.pendingEvents
		n.pendingEvents = nil
		for _, raw := range events {
			n.evaluate(raw)
		}
	}
}

// evaluate 将消息投递到当前页面
// 入参: raw JSON消息
func (n *nativeWindow) evaluate(raw string) {
	gtk.Evaluate(n.view, "window.__guigoReceive?.("+raw+")", -1, 0, 0, 0, 0, 0)
}

// emit 在线程队列中发送事件
// 入参: raw JSON消息
// 返回: error 关闭错误
func (n *nativeWindow) emit(raw string) error {
	return n.enqueue(func() {
		if !n.ready {
			n.pendingEvents = append(n.pendingEvents, raw)
		} else {
			n.evaluate(raw)
		}
	})
}

// linuxMessage 解析页面消息并启动异步调用
// 入参: manager 内容管理器, value JavaScript值, data 用户数据
func linuxMessage(manager, value, data uintptr) {
	n := active.Load()
	if n == nil || n.manager != manager || !localURL(gtk.URI(n.view)) {
		return
	}
	p := gtk.JSText(value)
	raw := nativeString(p)
	gtk.Free(unsafe.Pointer(p))
	var received struct {
		envelope
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(raw), &received); err != nil {
		n.fail(err)
		return
	}
	if received.Token != n.bridgeToken {
		return
	}
	message := received.envelope
	calls := n.calls
	switch message.Type {
	case "cancel":
		calls.remove(message.ID)
	case "accept":
		calls.accept(message.ID)
	case "call":
		call, err := calls.add(message)
		if err != nil {
			n.evaluate(string(marshalReply(envelope{Type: "reply", ID: message.ID, Error: err.Error()})))
			return
		}
		if message.Binary {
			n.evaluate(string(marshalReply(envelope{Type: "upload", ID: message.ID})))
			return
		}
		generation := n.generation
		go func() {
			reply := calls.run(call, n.options.Bindings, nil, 0)
			encoded := string(marshalReply(reply))
			_ = n.enqueue(func() {
				if generation == n.generation && n.pageCtx.Err() == nil {
					n.evaluate(encoded)
				}
			})
		}()
	}
}

// activate 首次显示后请求桌面恢复最小化窗口并激活
// 返回: error 请求投递错误
func (n *nativeWindow) activate() error {
	return n.enqueue(func() {
		n.hidden = false
		if !n.shown {
			return
		}
		gtk.Unminimize(n.window)
		gtk.Present(n.window)
	})
}

// hide 隐藏窗口并保留首次加载前的显隐意图
// 返回: error 请求投递错误
func (n *nativeWindow) hide() error {
	return n.enqueue(func() {
		n.hidden = true
		gtk.Visible(n.window, false)
	})
}

// close 请求执行窗口关闭检查
// 返回: error 关闭错误
func (n *nativeWindow) close() error {
	return n.enqueue(n.requestClose)
}

// linuxClosing 截获用户关闭请求
// 入参: window 窗口对象, data 用户数据
// 返回: uintptr 是否阻止默认销毁
func linuxClosing(window, data uintptr) uintptr {
	if n := active.Load(); n != nil && n.window == window {
		n.requestClose()
	}
	return 1
}

// requestClose 异步检查业务关闭回调，保留校验期间的新请求并触发beforeunload
func (n *nativeWindow) requestClose() {
	if n.closing {
		n.closePending = true
		return
	}
	n.closing = true
	go func() {
		allow := true
		if n.options.OnClosing != nil {
			func() {
				defer func() {
					if v := recover(); v != nil {
						allow = false
						n.fail(fmt.Errorf("OnClosing panic: %v", v))
					}
				}()
				allow = n.options.OnClosing(n.ctx)
			}()
		}
		_ = n.enqueue(func() {
			n.closing = false
			pending := n.closePending
			n.closePending = false
			if !allow {
				if pending {
					n.requestClose()
				}
				return
			}
			gtk.TryClose(n.view)
		})
	}()
}

// linuxClosed 完成WebKit允许的关闭请求
// 入参: view 浏览器对象, data 用户数据
func linuxClosed(view, data uintptr) {
	if n := active.Load(); n != nil && n.view == view {
		n.shutdown()
	}
}

// shutdown 取消所有页面操作并退出主循环
func (n *nativeWindow) shutdown() {
	n.mu.Lock()
	first := !n.closed
	n.closed = true
	work := n.queue
	n.queue = nil
	source := n.source
	n.source = 0
	n.mu.Unlock()
	if source != 0 {
		gtk.SourceRemove(source)
	}
	for _, job := range work {
		if job.discard != nil {
			job.discard()
		}
	}
	if first {
		for _, tray := range n.trays {
			tray.release()
		}
		linuxPrintOperations.Range(func(key, value any) bool {
			if value.(*linuxPrintOperation).owner == n {
				linuxEndPrint(key.(uintptr), ErrClosed)
			}
			return true
		})
		n.cancel()
		n.pageCancel()
		close(n.done)
		for _, d := range n.dialogsPending {
			gtk.Cancel(d.cancel)
		}
		for request := range n.resources {
			n.finishResponse(request, 499, nil)
		}
	}
	if len(n.dialogsPending) == 0 {
		gtk.LoopQuit(n.loop)
	}
}

// setTitle 修改原生窗口标题
// 入参: title 标题
// 返回: error 关闭错误
func (n *nativeWindow) setTitle(title string) error {
	return n.enqueue(func() { gtk.Title(n.window, title) })
}

// setSize 校验并设置窗口尺寸
// 入参: width 宽度, height 高度
// 返回: error 尺寸或关闭错误
func (n *nativeWindow) setSize(width, height int) error {
	if width < 1 || height < 1 || width > 32767 || height > 32767 {
		return errors.New("invalid window dimensions")
	}
	o := n.options
	width = max(width, o.MinWidth)
	height = max(height, o.MinHeight)
	if o.MaxWidth > 0 {
		width = min(width, o.MaxWidth)
	}
	if o.MaxHeight > 0 {
		height = min(height, o.MaxHeight)
	}
	return n.enqueue(func() { gtk.Size(n.window, int32(width), int32(height)) })
}

// initTitleBar 在单个WebView上方叠放原生控件并保留GTK客户区装饰
func (n *nativeWindow) initTitleBar() {
	if n.options.TitleBar == TitleBarContent {
		titlebar := gtk.BoxNew(0, 0)
		gtk.Titlebar(n.window, titlebar)
		gtk.Visible(titlebar, false)
		n.dragOverlay = gtk.OverlayNew()
		gtk.OverlayChild(n.dragOverlay, n.view)
		n.captionOverlay = gtk.OverlayNew()
		gtk.OverlayChild(n.captionOverlay, n.dragOverlay)
		for side := range n.captionControls {
			controls := gtk.WindowControls(int32(side))
			n.captionControls[side] = controls
			gtk.OverlayAdd(n.captionOverlay, controls)
			gtk.Connect(controls, "notify::empty", linuxCallbacks.titleChanged, 0, 0, 0)
		}
		n.initTitleBarColors()
		gtk.Connect(n.captionOverlay, "get-child-position", linuxCallbacks.titlePosition, 0, 0, 0)
		gtk.Connect(n.dragOverlay, "get-child-position", linuxCallbacks.dragPosition, 0, 0, 0)
		gtk.Child(n.window, n.captionOverlay)
	} else {
		gtk.Child(n.window, n.view)
	}
	gtk.Connect(n.window, "notify::is-active", linuxCallbacks.titleChanged, 0, 0, 0)
	gtk.Connect(n.window, "notify::maximized", linuxCallbacks.titleChanged, 0, 0, 0)
	gtk.Connect(n.window, "notify::scale-factor", linuxCallbacks.titleChanged, 0, 0, 0)
	n.updateTitleInfo()
}

// initTitleBarColors 仅为当前窗口按钮附加配色，不改动主题按钮的尺寸和交互状态
func (n *nativeWindow) initTitleBarColors() {
	colors := n.options.TitleBarColors
	if colors == (TitleBarColors{}) {
		return
	}
	n.captionStyle, n.captionDisplay = gtk.CSSProviderNew(), gtk.Display(n.window)
	class := fmt.Sprintf("guigo-caption-%x", n.window)
	css := fmt.Sprintf(".%s { background-color: %s; color: %s; } .%s:backdrop { background-color: %s; color: %s; } .%s button:not(:hover):not(:active) { color: inherit; }", class, colors.Background, colors.Foreground, class, colors.InactiveBackground, colors.InactiveForeground, class)
	gtk.CSSLoad(n.captionStyle, css, int64(len(css)))
	gtk.CSSAdd(n.captionDisplay, n.captionStyle, 600)
	for _, controls := range n.captionControls {
		gtk.CSSClass(controls, class)
	}
}

// freeTitleBarColors 移除显示器上的临时样式并释放当前窗口持有的引用
func (n *nativeWindow) freeTitleBarColors() {
	if n.captionStyle != 0 {
		gtk.CSSRemove(n.captionDisplay, n.captionStyle)
		gtk.Unref(n.captionStyle)
		n.captionStyle, n.captionDisplay = 0, 0
	}
}

// titleBarInfo 读取最后一次GTK布局及窗口状态，不跨线程访问原生对象
// 返回: TitleBarInfo 标题栏信息, error 关闭错误
func (n *nativeWindow) titleBarInfo() (TitleBarInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return TitleBarInfo{}, ErrClosed
	}
	return n.titleInfo, nil
}

// setTitleBarColors 在GTK线程更新当前窗口配色并发布标题栏状态
// 入参: colors 已校验的标题栏配色
// 返回: error 模式或调度错误
func (n *nativeWindow) setTitleBarColors(colors TitleBarColors) error {
	if n.options.TitleBar != TitleBarContent {
		return errors.New("title bar colors require content title bar")
	}
	return n.enqueue(func() {
		if n.options.TitleBarColors == colors {
			return
		}
		n.freeTitleBarColors()
		n.options.TitleBarColors = colors
		n.initTitleBarColors()
		n.updateTitleInfo()
	})
}

// setWindowMaterial 保持GTK窗口的纯色背景
// 入参: material 已校验的平台可用材质
// 返回: error 调度错误
func (n *nativeWindow) setWindowMaterial(material WindowMaterial) error {
	return n.enqueue(func() {})
}

// setDragRegions 复制逻辑像素区域并交给GTK原生WindowHandle处理真实输入事件
// GTK手势使用实际GdkEvent的设备、时间戳和原生坐标，保留系统双击动作及窗口菜单
// 入参: regions 客户区拖动矩形，空切片清除全部区域
// 返回: error 模式、参数或关闭错误
func (n *nativeWindow) setDragRegions(regions []Rect) error {
	if n.options.TitleBar != TitleBarContent {
		return errors.New("content title bar is not enabled")
	}
	regions = append([]Rect(nil), regions...)
	for _, r := range regions {
		if math.IsNaN(r.X) || math.IsNaN(r.Y) || math.IsNaN(r.Width) || math.IsNaN(r.Height) ||
			math.IsInf(r.X, 0) || math.IsInf(r.Y, 0) || math.IsInf(r.Width, 0) || math.IsInf(r.Height, 0) ||
			r.Width < 0 || r.Height < 0 {
			return errors.New("invalid drag region")
		}
	}
	return n.enqueue(func() {
		for _, handle := range n.dragHandles {
			gtk.OverlayRemove(n.dragOverlay, handle.widget)
		}
		n.dragHandles = nil
		for _, region := range regions {
			if region.Width == 0 || region.Height == 0 {
				continue
			}
			handle := gtk.WindowHandle()
			n.dragHandles = append(n.dragHandles, linuxDragHandle{widget: handle, region: region})
			gtk.OverlayAdd(n.dragOverlay, handle)
		}
	})
}

// measureTitleControls 测量主题和系统按钮布局决定的标题栏逻辑尺寸
// 返回: int32 高度, [2]int32 左右原生控件宽度
func (n *nativeWindow) measureTitleControls() (int32, [2]int32) {
	height := int32(32)
	var widths [2]int32
	for side, controls := range n.captionControls {
		if controls == 0 || gtk.ControlsEmpty(controls) {
			continue
		}
		var minimum, natural int32
		gtk.Measure(controls, 0, -1, &minimum, &natural, nil, nil)
		widths[side] = max(minimum, natural)
		gtk.Measure(controls, 1, widths[side], &minimum, &natural, nil, nil)
		height = max(height, minimum, natural)
	}
	return height, widths
}

// updateTitleInfo 更新缓存并在后台发送变化通知，避免业务回调阻塞GTK线程
func (n *nativeWindow) updateTitleInfo() {
	n.titleUpdatePending = false
	scale := gtk.ScaleFactor(n.window)
	info := TitleBarInfo{Active: gtk.IsActive(n.window), Maximized: gtk.IsMaximized(n.window)}
	if n.options.TitleBar == TitleBarContent {
		info.Colors = n.options.TitleBarColors
		height, widths := n.measureTitleControls()
		info.Height, info.Left, info.Right = float64(height), float64(widths[0]), float64(widths[1])
		if n.captionOverlay != 0 {
			gtk.Minimum(n.captionOverlay, widths[0]+widths[1], height)
		}
	}
	n.mu.Lock()
	if n.closed || n.titleInfoSet && n.titleInfo == info && n.titleScale == scale {
		n.mu.Unlock()
		return
	}
	n.titleInfo, n.titleInfoSet, n.titleScale = info, true, scale
	start := false
	if n.options.OnTitleBarChanged != nil {
		n.titleCallbackPending = true
		start = n.titleCallbacksReady && !n.titleCallbackRunning
		if start {
			n.titleCallbackRunning = true
		}
	}
	n.mu.Unlock()
	if start {
		go n.deliverTitleInfo()
	}
}

// startTitleCallbacks 在OnReady返回后开放初始通知及后续状态的串行投递
func (n *nativeWindow) startTitleCallbacks() {
	n.mu.Lock()
	n.titleCallbacksReady = true
	start := !n.closed && n.titleCallbackPending && !n.titleCallbackRunning
	if start {
		n.titleCallbackRunning = true
	}
	n.mu.Unlock()
	if start {
		go n.deliverTitleInfo()
	}
}

// deliverTitleInfo 串行通知最新标题栏状态并隔离业务异常，关闭后丢弃未交付状态
func (n *nativeWindow) deliverTitleInfo() {
	for {
		n.mu.Lock()
		if n.closed || !n.titleCallbackPending || n.ctx.Err() != nil || active.Load() != n {
			n.titleCallbackRunning = false
			n.titleCallbackPending = false
			n.mu.Unlock()
			return
		}
		info := n.titleInfo
		n.titleCallbackPending = false
		n.mu.Unlock()
		func() {
			defer func() {
				if v := recover(); v != nil {
					n.fail(fmt.Errorf("OnTitleBarChanged panic: %v", v))
				}
			}()
			n.options.OnTitleBarChanged(info)
		}()
	}
}

// queueTitleInfo 合并同一轮原生状态及布局变化通知
func (n *nativeWindow) queueTitleInfo() {
	if !n.titleUpdatePending {
		n.titleUpdatePending = true
		_ = n.enqueue(n.updateTitleInfo)
	}
}

// linuxTitleChanged 跟踪当前窗口及原生按钮布局变化
// 入参: object 原生对象, property 属性定义, data 用户数据
func linuxTitleChanged(object, property, data uintptr) {
	n := active.Load()
	if n != nil && (object == n.window || object == n.captionControls[0] || object == n.captionControls[1]) {
		n.queueTitleInfo()
	}
}

// linuxTitlePosition 将真实GTK控件分配在内容区顶部的左右两侧
// 入参: overlay 叠放容器, child 原生控件, allocation 输出分配区域, data 用户数据
// 返回: uintptr 是否提供分配区域
func linuxTitlePosition(overlay, child uintptr, allocation *linuxRectangle, data uintptr) uintptr {
	n := active.Load()
	if n == nil || overlay != n.captionOverlay || allocation == nil {
		return 0
	}
	height, widths := n.measureTitleControls()
	for side, controls := range n.captionControls {
		if child == controls {
			*allocation = linuxRectangle{Width: widths[side], Height: height}
			if side == 1 {
				allocation.X = gtk.WidgetWidth(overlay) - allocation.Width
			}
			n.queueTitleInfo()
			return 1
		}
	}
	return 0
}

// linuxDragPosition 将拖动命中区裁剪到WebView逻辑坐标，不包含窗口阴影和原生边框
// 入参: overlay 叠放容器, child 拖动控件, allocation 输出分配区域, data 用户数据
// 返回: uintptr 是否提供分配区域
func linuxDragPosition(overlay, child uintptr, allocation *linuxRectangle, data uintptr) uintptr {
	n := active.Load()
	if n == nil || overlay != n.dragOverlay || allocation == nil {
		return 0
	}
	for _, handle := range n.dragHandles {
		if child == handle.widget {
			*allocation = linuxClipDragRegion(handle.region, gtk.WidgetWidth(n.view), gtk.WidgetHeight(n.view))
			return 1
		}
	}
	return 0
}

// linuxClipDragRegion 向内取整并裁剪区域，避免将相邻交互内容作为拖动命中区
// 入参: region 逻辑矩形, width 内容宽度, height 内容高度
// 返回: linuxRectangle GTK分配区域
func linuxClipDragRegion(region Rect, width, height int32) linuxRectangle {
	x := math.Ceil(max(0, min(float64(width), region.X)))
	y := math.Ceil(max(0, min(float64(height), region.Y)))
	right := math.Floor(max(x, min(float64(width), region.X+region.Width)))
	bottom := math.Floor(max(y, min(float64(height), region.Y+region.Height)))
	return linuxRectangle{X: int32(x), Y: int32(y), Width: int32(right - x), Height: int32(bottom - y)}
}

// openURL 使用系统浏览器打开外部链接
// 入参: raw 外部地址
// 返回: error 启动错误
func openURL(raw string) error {
	linuxLoadMu.Lock()
	path := linuxLoadedPath
	linuxLoadMu.Unlock()
	if err := loadLinux(path); err != nil {
		return err
	}
	var nativeErr *linuxError
	if !gtk.LaunchURI(raw, 0, &nativeErr) {
		if err := nativeError(nativeErr); err != nil {
			return err
		}
		return errors.New("could not launch default URI handler")
	}
	return nil
}

// linuxPolicy 限制页面导航到受信来源
// 入参: view 浏览器对象, decision 导航决策, kind 决策类型, data 用户数据
// 返回: uintptr 是否已经处理
func linuxPolicy(view, decision uintptr, kind int32, data uintptr) uintptr {
	if kind != 0 && kind != 1 {
		return 0
	}
	n := active.Load()
	if n == nil || n.view != view {
		return 0
	}
	action := gtk.NavigationAction(decision)
	uri := gtk.NavigationURI(gtk.ActionRequest(action))
	if kind == 0 && localURL(uri) {
		return 0
	}
	gtk.PolicyIgnore(decision)
	if externalURL(uri) {
		go func() {
			if err := openURL(uri); err != nil {
				n.fail(err)
			}
		}()
	}
	return 1
}

// linuxProcessFailed 报告浏览器进程异常并关闭窗口
// 入参: view 浏览器对象, reason 退出原因, data 用户数据
func linuxProcessFailed(view uintptr, reason int32, data uintptr) {
	if n := active.Load(); n != nil && n.view == view {
		n.err = fmt.Errorf("WebKit web process terminated: %d", reason)
		n.fail(n.err)
		n.shutdown()
	}
}

// fail 异步报告错误并隔离用户回调异常
// 入参: err 异步错误
func (n *nativeWindow) fail(err error) {
	if err != nil && n.options.OnError != nil {
		go func() { defer func() { _ = recover() }(); n.options.OnError(err) }()
	}
}

// linuxDrop 读取GdkFileList并异步通知业务
// 入参: target 拖放目标, value 文件列表, x 横坐标, y 纵坐标, data 用户数据
// 返回: uintptr 是否接收文件
func linuxDrop(target, value uintptr, x, y float64, data uintptr) uintptr {
	n := active.Load()
	if n == nil || target == 0 || n.dropTarget != target || n.options.OnDrop == nil {
		return 0
	}
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed || n.ctx.Err() != nil {
		return 0
	}
	list := gtk.DropFiles(gtk.ValueBoxed(value))
	defer gtk.ListFree(list)
	var paths []string
	for list != nil {
		p := gtk.FilePath(list.Data)
		if p != nil {
			paths = append(paths, nativeString(p))
			gtk.Free(unsafe.Pointer(p))
		}
		list = list.Next
	}
	if len(paths) == 0 {
		return 0
	}
	go func() {
		defer func() {
			if v := recover(); v != nil {
				n.fail(fmt.Errorf("OnDrop panic: %v", v))
			}
		}()
		n.mu.Lock()
		closed := n.closed
		n.mu.Unlock()
		if closed || n.ctx.Err() != nil || active.Load() != n {
			return
		}
		n.options.OnDrop(paths)
	}()
	return 1
}
