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
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
)

// callQueue 管理当前页面的调用及执行上限
type callQueue struct {
	mu    sync.Mutex
	ctx   context.Context
	items map[string]*pendingCall
	slots chan struct{}
}

// pendingCall 保存调用参数和取消状态
type pendingCall struct {
	ctx     context.Context
	cancel  context.CancelFunc
	message envelope
	started bool
	discard func()
}

// binaryCall 描述二进制参数的位置及长度
type binaryCall struct {
	Args  any          `json:"args"`
	Parts []binaryPart `json:"parts"`
}

// binaryPart 定位参数中的字节数据
type binaryPart struct {
	Path []string `json:"path"`
	Size int64    `json:"size"`
}

// pageContextKey 标识调用所属页面的生命周期
type pageContextKey struct{}

// PageContext 返回调用所属页面的上下文，导航或关闭窗口时取消
// 入参: ctx 处理器收到的调用上下文
// 返回: context.Context 页面上下文，非宿主上下文原样返回
func PageContext(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if page, ok := ctx.Value(pageContextKey{}).(context.Context); ok {
		return page
	}
	return ctx
}

// newCallQueue 创建页面调用队列
// 入参: ctx 页面上下文
// 返回: *callQueue 调用队列
func newCallQueue(ctx context.Context) *callQueue {
	return &callQueue{ctx: ctx, items: make(map[string]*pendingCall), slots: make(chan struct{}, 8)}
}

// readBinaryCall 读取元信息与原始字节并还原参数
// 入参: ctx 调用上下文, body 请求流, size 请求长度
// 返回: any 参数, error 解码错误
func readBinaryCall(ctx context.Context, body io.Reader, size int64) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var prefix [4]byte
	if _, err := io.ReadFull(body, prefix[:]); err != nil {
		return nil, err
	}
	length := int64(binary.LittleEndian.Uint32(prefix[:]))
	if length > 4<<20 || size < length+4 {
		return nil, errors.New("invalid binary header")
	}
	header := make([]byte, int(length))
	if _, err := io.ReadFull(body, header); err != nil {
		return nil, err
	}
	var packet binaryCall
	if err := json.Unmarshal(header, &packet); err != nil {
		return nil, err
	}
	remaining := size - length - 4
	for _, part := range packet.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if part.Size < 0 || part.Size > remaining || part.Size > int64(int(^uint(0)>>1)) {
			return nil, errors.New("invalid binary size")
		}
		data := make([]byte, int(part.Size))
		for offset := 0; offset < len(data); {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := min(offset+(256<<10), len(data))
			if _, err := io.ReadFull(body, data[offset:end]); err != nil {
				return nil, err
			}
			offset = end
		}
		if err := assignBinary(&packet.Args, part.Path, data); err != nil {
			return nil, err
		}
		remaining -= part.Size
	}
	if remaining != 0 {
		return nil, errors.New("unexpected binary data")
	}
	return packet.Args, nil
}

// assignBinary 将原始字节写入元信息指定位置
// 入参: value 参数位置, path 字段路径, data 原始字节
// 返回: error 路径错误
func assignBinary(value *any, path []string, data []byte) error {
	if len(path) == 0 {
		if *value != nil {
			return errors.New("invalid binary target")
		}
		*value = data
		return nil
	}
	switch parent := (*value).(type) {
	case map[string]any:
		child, ok := parent[path[0]]
		if !ok {
			break
		}
		if err := assignBinary(&child, path[1:], data); err != nil {
			return err
		}
		parent[path[0]] = child
		return nil
	case []any:
		i, err := strconv.Atoi(path[0])
		if err != nil || i < 0 || i >= len(parent) {
			break
		}
		return assignBinary(&parent[i], path[1:], data)
	}
	return errors.New("invalid binary path")
}

// add 登记调用并限制待处理数量
// 入参: message 请求消息
// 返回: *pendingCall 已登记调用, error 登记错误
func (q *callQueue) add(message envelope) (*pendingCall, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ctx.Err(); err != nil {
		return nil, err
	}
	if message.ID == "" || message.Name == "" || q.items[message.ID] != nil {
		return nil, errors.New("invalid call")
	}
	if len(q.items) >= 128 {
		return nil, errors.New("too many pending calls")
	}
	ctx, cancel := context.WithCancel(context.WithValue(q.ctx, pageContextKey{}, q.ctx))
	call := &pendingCall{ctx: ctx, cancel: cancel, message: message}
	q.items[message.ID] = call
	return call, nil
}

// take 取得尚未开始的二进制调用
// 入参: id 请求标识
// 返回: *pendingCall 待执行调用
func (q *callQueue) take(id string) *pendingCall {
	q.mu.Lock()
	defer q.mu.Unlock()
	call := q.items[id]
	if call == nil || !call.message.Binary || call.started {
		return nil
	}
	call.started = true
	return call
}

// remove 取消调用并移除未执行请求
// 入参: id 请求标识
func (q *callQueue) remove(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if call := q.items[id]; call != nil {
		call.cancel()
		if !call.started {
			delete(q.items, id)
		}
	}
}

// accept 确认页面已接收结果并移交资源所有权
// 入参: id 请求标识
func (q *callQueue) accept(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if call := q.items[id]; call != nil && call.discard != nil && call.ctx.Err() == nil {
		call.discard = nil
		call.cancel()
		delete(q.items, id)
	}
}

// finish 移除调用并释放未交付结果
// 入参: call 已执行调用
func (q *callQueue) finish(call *pendingCall) {
	q.mu.Lock()
	call.cancel()
	discard := call.discard
	call.discard = nil
	if q.items[call.message.ID] == call {
		delete(q.items, call.message.ID)
	}
	q.mu.Unlock()
	if discard != nil {
		defer func() { _ = recover() }()
		discard()
	}
}

// run 等待执行额度并调用业务处理器
// 入参: call 待执行调用, bindings 方法集合, body 二进制请求流, size 请求长度
// 返回: envelope 调用结果
func (q *callQueue) run(call *pendingCall, bindings map[string]HandlerFunc, body io.Reader, size int64) envelope {
	delivered := false
	defer func() {
		if !delivered {
			q.finish(call)
		}
	}()
	q.mu.Lock()
	call.started = true
	q.mu.Unlock()
	select {
	case q.slots <- struct{}{}:
		defer func() { <-q.slots }()
	case <-call.ctx.Done():
		return envelope{ID: call.message.ID, Type: "reply", Error: call.ctx.Err().Error(), ErrorName: "AbortError"}
	}
	if body != nil {
		args, err := readBinaryCall(call.ctx, body, size)
		if err != nil {
			return envelope{ID: call.message.ID, Type: "reply", Error: err.Error()}
		}
		call.message.Args = args
	}
	reply := dispatch(call.ctx, bindings, call.message)
	var result Result
	var hasResult bool
	switch value := reply.Value.(type) {
	case Result:
		result, hasResult = value, true
	case *Result:
		if value != nil {
			result, hasResult = *value, true
		} else {
			reply.Value = nil
		}
	}
	if hasResult {
		reply.Value = result.Value
		if result.Discard != nil {
			q.mu.Lock()
			call.discard = result.Discard
			if call.ctx.Err() == nil {
				delivered = true
			}
			q.mu.Unlock()
			if delivered {
				context.AfterFunc(call.ctx, func() { q.finish(call) })
			}
		}
	}
	if err := call.ctx.Err(); err != nil {
		reply.Value = nil
		reply.Error = err.Error()
		reply.ErrorName = "AbortError"
	}
	return reply
}
