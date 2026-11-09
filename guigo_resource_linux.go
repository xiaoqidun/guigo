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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var linuxHeaderID atomic.Uint64
var linuxHeaderTargets sync.Map

// linuxInput 持有原生请求流及取消标识
type linuxInput struct {
	mu     sync.Mutex
	closed bool
	stream uintptr
	cancel uintptr
	stop   func() bool
}

// linuxWriter 把HTTP响应直接写入原生管道
type linuxWriter struct {
	window  *nativeWindow
	request uintptr
	ctx     context.Context
	header  http.Header
	status  int
	pipe    *os.File
	err     error
	stop    func() bool
}

// linuxHeaders 复制请求头到Go对象
// 入参: key 字段名, value 字段值, data 注册标识
func linuxHeaders(key, value *byte, data uintptr) {
	if target, ok := linuxHeaderTargets.Load(uint64(data)); ok {
		target.(http.Header).Add(nativeString(key), nativeString(value))
	}
}

// Read 读取原生流并传递取消或读取错误
// 入参: data 输出缓冲区
// 返回: int 读取长度, error 读取错误
func (r *linuxInput) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	stream, cancel := gtk.Ref(r.stream), gtk.Ref(r.cancel)
	r.mu.Unlock()
	defer gtk.Unref(stream)
	defer gtk.Unref(cancel)
	var nativeErr *linuxError
	count := gtk.InputRead(stream, unsafe.Pointer(&data[0]), uintptr(len(data)), cancel, &nativeErr)
	if count < 0 {
		return 0, nativeError(nativeErr)
	}
	if count == 0 {
		return 0, io.EOF
	}
	return int(count), nil
}

// Close 释放请求流及取消标识
// 返回: error 关闭错误
func (r *linuxInput) Close() error {
	if r.stop != nil {
		r.stop()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	gtk.Cancel(r.cancel)
	gtk.Unref(r.stream)
	gtk.Unref(r.cancel)
	return nil
}

// cancelRead 同步取消读取并避免访问已释放的原生标识
func (r *linuxInput) cancelRead() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		gtk.Cancel(r.cancel)
	}
}

// linuxResource 获取原生请求并交给后台协程
// 入参: request 请求对象, data 用户数据
func linuxResource(request, data uintptr) {
	n := active.Load()
	if n == nil || gtk.RequestView(request) != n.view {
		return
	}
	uri := gtk.RequestURI(request)
	req, err := http.NewRequestWithContext(n.pageCtx, gtk.RequestMethod(request), uri, nil)
	if err != nil {
		n.fail(err)
		return
	}
	req.RequestURI = req.URL.RequestURI()
	headers := gtk.RequestHeaders(request)
	id := linuxHeaderID.Add(1)
	linuxHeaderTargets.Store(id, req.Header)
	gtk.HeadersEach(headers, linuxCallbacks.headers, uintptr(id))
	linuxHeaderTargets.Delete(id)
	body := gtk.RequestBody(request)
	if body != 0 {
		input := &linuxInput{stream: body, cancel: gtk.CancelNew()}
		input.stop = context.AfterFunc(req.Context(), input.cancelRead)
		req.Body = input
		req.ContentLength = -1
		if value := req.Header.Get("Content-Length"); value != "" {
			req.ContentLength, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	gtk.Ref(request)
	n.resources[request] = true
	calls := n.calls
	go func() {
		if req.Body != nil {
			defer req.Body.Close()
		}
		w := &linuxWriter{window: n, request: request, ctx: req.Context(), header: make(http.Header)}
		defer func() {
			if v := recover(); v != nil {
				w.err = fmt.Errorf("resource handler panic: %v", v)
				if w.status == 0 {
					http.Error(w, "Internal Server Error", 500)
				}
			}
			if w.status == 0 {
				w.WriteHeader(200)
			}
			if w.pipe != nil {
				_ = w.pipe.Close()
			}
			if w.stop != nil {
				w.stop()
			}
			if w.err != nil && req.Context().Err() == nil {
				n.fail(w.err)
			}
		}()
		if !localURL(uri) {
			http.Error(w, "Forbidden", 403)
			return
		}
		if req.URL.Path == "/__guigo/call" {
			if req.Method != http.MethodPost {
				http.Error(w, "Method Not Allowed", 405)
				return
			}
			call := calls.take(req.URL.Query().Get("id"))
			if call == nil {
				http.Error(w, "Call closed", 410)
				return
			}
			if input, ok := req.Body.(*linuxInput); ok {
				stop := context.AfterFunc(call.ctx, input.cancelRead)
				defer stop()
			}
			size, err := strconv.ParseInt(req.Header.Get("X-Guigo-Length"), 10, 64)
			if err != nil || size < 4 || req.Body == nil {
				calls.finish(call)
				http.Error(w, "Invalid binary size", 400)
				return
			}
			reply := calls.run(call, n.options.Bindings, req.Body, size)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(marshalReply(reply))
			return
		}
		if n.options.ResourceHandler != nil {
			resource, err := n.options.ResourceHandler(req)
			if resource != nil && resource.Body != nil {
				defer resource.Body.Close()
			}
			if err != nil {
				w.err = err
				http.Error(w, "Internal Server Error", 500)
				return
			}
			if resource != nil {
				if resource.Body == nil {
					http.Error(w, "Missing resource body", 500)
					return
				}
				size, err := resource.Body.Seek(0, io.SeekEnd)
				if err == nil {
					_, err = resource.Body.Seek(0, io.SeekStart)
				}
				if err != nil || size < 0 {
					w.err = err
					http.Error(w, "Invalid resource stream", 500)
					return
				}
				w.header = resource.Header.Clone()
				if w.header == nil {
					w.header = make(http.Header)
				}
				w.header.Set("Content-Length", strconv.FormatInt(size, 10))
				status := resource.StatusCode
				if status == 0 {
					status = 200
				}
				w.WriteHeader(status)
				if req.Method != http.MethodHead {
					_, w.err = io.Copy(w, resource.Body)
				}
				return
			}
		}
		if _, ok := n.options.Handler.(fileHandler); ok && strings.HasSuffix(req.URL.Path, "/index.html") {
			req.URL.Path = strings.TrimSuffix(req.URL.Path, "index.html")
			req.URL.RawPath = ""
		}
		n.options.Handler.ServeHTTP(w, req)
	}()
}

// Header 返回响应头集合
// 返回: http.Header 响应头
func (w *linuxWriter) Header() http.Header { return w.header }

// WriteHeader 创建管道并发布原生响应
// 入参: status HTTP状态码
func (w *linuxWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status < 200 || status > 599 {
		status = 500
	}
	w.status = status
	reader, writer, err := os.Pipe()
	if err != nil {
		w.err = err
		w.window.fail(err)
		_ = w.window.enqueue(func() { w.window.finishResponse(w.request, 500, nil) })
		return
	}
	fd, err := syscall.Dup(int(reader.Fd()))
	_ = reader.Close()
	if err != nil {
		_ = writer.Close()
		w.err = err
		w.window.fail(err)
		_ = w.window.enqueue(func() { w.window.finishResponse(w.request, 500, nil) })
		return
	}
	stream := gtk.UnixInput(int32(fd), true)
	w.pipe = writer
	stop := context.AfterFunc(w.ctx, func() { _ = writer.Close() })
	w.stop = stop
	headers := w.header.Clone()
	size := int64(-1)
	if value := headers.Get("Content-Length"); value != "" {
		size, _ = strconv.ParseInt(value, 10, 64)
	}
	discard := func() { gtk.Unref(stream); _ = writer.Close(); stop() }
	if err = w.window.enqueueCleanup(func() {
		defer gtk.Unref(stream)
		if !w.window.resources[w.request] {
			_ = writer.Close()
			stop()
			return
		}
		w.window.respondStream(w.request, stream, size, status, headers)
	}, discard); err != nil {
		discard()
		w.err = err
	}
}

// Write 发送HTTP响应内容
// 入参: data 响应数据
// 返回: int 写入长度, error 写入错误
func (w *linuxWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		if w.header.Get("Content-Type") == "" {
			w.header.Set("Content-Type", http.DetectContentType(data))
		}
		w.WriteHeader(200)
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.pipe == nil {
		return 0, errors.New("response stream unavailable")
	}
	return w.pipe.Write(data)
}

// respondStream 将原生流交给WebKit并释放请求引用
// 入参: request 请求对象, stream 原生流, size 响应长度, status 状态码, headers 响应头
func (n *nativeWindow) respondStream(request, stream uintptr, size int64, status int, headers http.Header) {
	if !n.resources[request] {
		return
	}
	delete(n.resources, request)
	defer gtk.Unref(request)
	response := gtk.ResponseNew(stream, size)
	defer gtk.Unref(response)
	gtk.ResponseStatus(response, uint32(status), http.StatusText(status))
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	gtk.ResponseType(response, contentType)
	nativeHeaders := gtk.HeadersNew(1)
	for key, values := range headers {
		for _, value := range values {
			if !strings.ContainsAny(key+value, "\r\n\x00") {
				gtk.HeadersAppend(nativeHeaders, key, value)
			}
		}
	}
	gtk.ResponseHeaders(response, nativeHeaders)
	gtk.FinishResponse(request, response)
}

// finishResponse 关闭尚未完成的请求
// 入参: request 请求对象, status 状态码, headers 响应头
func (n *nativeWindow) finishResponse(request uintptr, status int, headers http.Header) {
	if !n.resources[request] {
		return
	}
	file := gtk.FileNew("/dev/null")
	defer gtk.Unref(file)
	var nativeErr *linuxError
	stream := gtk.FileRead(file, 0, &nativeErr)
	if stream == 0 {
		n.fail(nativeError(nativeErr))
		delete(n.resources, request)
		gtk.Unref(request)
		return
	}
	defer gtk.Unref(stream)
	n.respondStream(request, stream, 0, status, headers)
}
