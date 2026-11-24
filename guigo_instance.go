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
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// 单实例消息体积、错误文本与默认超时上限
const (
	defaultInstanceLimit   = 1 << 20
	maxInstanceLimit       = 16 << 20
	defaultInstanceTimeout = 10 * time.Second
	maxInstanceErrorBytes  = 4096
)

// errInstanceWindow 表示窗口已不属于当前桌面，需要重新选择接收端
var errInstanceWindow = errors.New("instance window is not on current desktop")

// InstanceOptions 定义单实例入口的标识、转交消息及匹配策略
// Key为非空隔离键；Payload长度不得超过MaxPayloadBytes(默认1MiB，最大16MiB)
// Window在windows/amd64上按当前虚拟桌面最近激活的同组窗口优先转交，其他平台沿用会话单实例
// Timeout非正值默认10秒
type InstanceOptions struct {
	Key             string
	Payload         []byte
	Window          bool
	Timeout         time.Duration
	MaxPayloadBytes int
}

// instanceConn 提供支持截止时间的本机通信连接
type instanceConn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// instanceListener 提供实例入口的接收及释放能力
type instanceListener interface {
	Accept() (instanceConn, error)
	Close() error
}

// Instance 持有当前用户及会话内的实例入口，关闭后允许后续进程接管
type Instance struct {
	listener instanceListener
	cancel   context.CancelFunc
	done     chan struct{}
	timeout  time.Duration
	limit    int
	closed   atomic.Bool
	once     sync.Once
	err      error
}

// normalizeInstanceOptions 校验并补全单实例默认配置
// 入参: options 单实例选项
// 返回: InstanceOptions 归一化后的选项, error 参数错误
func normalizeInstanceOptions(options InstanceOptions) (InstanceOptions, error) {
	if options.Key == "" || strings.ContainsRune(options.Key, 0) {
		return InstanceOptions{}, errors.New("invalid instance options")
	}
	if options.MaxPayloadBytes < 0 || options.MaxPayloadBytes > maxInstanceLimit {
		return InstanceOptions{}, errors.New("invalid instance payload limit")
	}
	if options.MaxPayloadBytes == 0 {
		options.MaxPayloadBytes = defaultInstanceLimit
	}
	if len(options.Payload) > options.MaxPayloadBytes {
		return InstanceOptions{}, errors.New("instance payload exceeds limit")
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultInstanceTimeout
	}
	return options, nil
}

// OpenInstance 创建单实例入口，已有同组实例时转交消息并返回nil
// 首个实例不调用receive处理自己的消息，调用方自行处理初始请求
// receive串行执行，应快速接收或入队；返回nil表示已接受，不表示业务已完成
// receive应响应上下文取消，不得同步调用当前实例的Close
// receive返回ErrClosed时，发送方等待入口释放后重新竞争
// 不同用户、登录会话及Windows提升权限分别隔离，不修改窗口应用标识
// 入参: ctx 生命周期上下文, options 实例配置, receive 接收回调
// 返回: *Instance 新实例入口或nil, error 创建或转交错误
func OpenInstance(ctx context.Context, options InstanceOptions, receive func(context.Context, []byte) error) (*Instance, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if receive == nil {
		return nil, errors.New("nil instance receiver")
	}
	normalized, err := normalizeInstanceOptions(options)
	if err != nil {
		return nil, err
	}
	if normalized.Window {
		return openWindowInstance(ctx, normalized, receive)
	}
	return openSessionInstance(ctx, normalized, receive)
}

// openSessionInstance 在当前用户和会话范围内竞争或转交单实例入口
// 入参: ctx 生命周期上下文, options 归一化实例配置, receive 接收回调
// 返回: *Instance 新实例入口或nil, error 创建或转交错误
func openSessionInstance(ctx context.Context, options InstanceOptions, receive func(context.Context, []byte) error) (*Instance, error) {
	address, err := instanceAddress(options.Key)
	if err != nil {
		return nil, err
	}
	request, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	for {
		if err := request.Err(); err != nil {
			return nil, err
		}
		listener, err := listenInstance(address)
		if err != nil {
			return nil, err
		}
		if listener != nil {
			return startInstance(ctx, listener, options.Timeout, options.MaxPayloadBytes, receive), nil
		}
		conn, err := dialInstance(request, address)
		if err == nil {
			err = forwardInstance(request, conn, options.Payload, options.MaxPayloadBytes)
			if !errors.Is(err, ErrClosed) {
				return nil, err
			}
		} else if !instanceUnavailable(err) {
			return nil, err
		}
		select {
		case <-request.Done():
			return nil, request.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// startInstance 启动入口接收循环
// 入参: ctx 生命周期上下文, listener 接收端, timeout 单次接收超时, limit 消息字节上限, receive 接收回调
// 返回: *Instance 实例入口
func startInstance(ctx context.Context, listener instanceListener, timeout time.Duration, limit int, receive func(context.Context, []byte) error) *Instance {
	life, stop := context.WithCancel(ctx)
	instance := &Instance{listener: listener, cancel: stop, done: make(chan struct{}), timeout: timeout, limit: limit}
	go instance.serve(life, receive)
	return instance
}

// forwardInstance 转交消息并读取接收结果，不重发已确认或结果不明的请求
// 入参: ctx 请求上下文, conn 本机连接, data 消息, limit 消息字节上限
// 返回: error 转交错误
func forwardInstance(ctx context.Context, conn instanceConn, data []byte, limit int) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	var ready [1]byte
	if _, err := io.ReadFull(conn, ready[:]); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrClosed
	}
	if ready[0] != 1 {
		return errors.New("invalid instance readiness")
	}
	if err := writeInstanceMessage(conn, data, limit); err != nil {
		return err
	}
	reply, err := readInstanceMessage(conn, limit)
	if err != nil {
		return err
	}
	_, _ = conn.Write([]byte{1})
	if len(reply) == 0 {
		return io.ErrUnexpectedEOF
	}
	switch reply[0] {
	case 0:
		return nil
	case 1:
		return errors.New(string(reply[1:]))
	case 2:
		return ErrClosed
	case 3:
		return errInstanceWindow
	default:
		return errors.New("invalid instance reply")
	}
}

// instanceHash 生成固定长度的本机入口名称
// 入参: identity 用户及会话标识, key 调用方实例键
// 返回: string 入口摘要
func instanceHash(identity, key string) string {
	sum := sha256.Sum256([]byte(identity + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// readInstanceMessage 读取长度受限的实例消息
// 入参: reader 数据来源, limit 消息字节上限
// 返回: []byte 消息, error 读取错误
func readInstanceMessage(reader io.Reader, limit int) ([]byte, error) {
	var size uint32
	if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
		return nil, err
	}
	if int64(size) > int64(limit) {
		return nil, errors.New("instance message too large")
	}
	data := make([]byte, size)
	_, err := io.ReadFull(reader, data)
	return data, err
}

// writeInstanceMessage 写入长度受限的实例消息
// 入参: writer 数据目标, data 消息, limit 消息字节上限
// 返回: error 写入错误
func writeInstanceMessage(writer io.Writer, data []byte, limit int) error {
	if len(data) > limit {
		return errors.New("instance message too large")
	}
	if err := binary.Write(writer, binary.LittleEndian, uint32(len(data))); err != nil {
		return err
	}
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// truncateUTF8 按UTF-8字符边界截断超长字符串
// 入参: text 原始文本, maxBytes 最大字节数
// 返回: string 截断后的合法UTF-8字符串
func truncateUTF8(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// Close 停止接收并释放实例入口，可重复调用，nil及零值接收者均安全
// 返回: error 关闭错误
func (i *Instance) Close() error {
	if i == nil {
		return nil
	}
	i.once.Do(func() {
		i.closed.Store(true)
		if i.cancel != nil {
			i.cancel()
		}
		if i.listener != nil {
			i.err = i.listener.Close()
		}
		if i.done != nil {
			<-i.done
		}
	})
	return i.err
}

// serve 串行接收请求并在调用方确认后返回结果
// 入参: ctx 生命周期上下文, receive 接收回调
func (i *Instance) serve(ctx context.Context, receive func(context.Context, []byte) error) {
	defer close(i.done)
	defer i.listener.Close()
	stop := context.AfterFunc(ctx, func() { i.listener.Close() })
	defer stop()
	for {
		conn, err := i.listener.Accept()
		if err != nil {
			return
		}
		i.receive(ctx, conn, receive)
	}
}

// receive 处理单个连接并隔离回调异常，等待对方读取回执
// 入参: ctx 生命周期上下文, conn 本机连接, receive 接收回调
func (i *Instance) receive(ctx context.Context, conn instanceConn, receive func(context.Context, []byte) error) {
	defer conn.Close()
	request, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()
	stop := context.AfterFunc(request, func() { conn.Close() })
	defer stop()
	deadline, _ := request.Deadline()
	if conn.SetDeadline(deadline) != nil {
		return
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		return
	}
	data, err := readInstanceMessage(conn, i.limit)
	if err != nil || request.Err() != nil {
		return
	}
	var receiveErr error
	func() {
		defer func() {
			if v := recover(); v != nil {
				receiveErr = fmt.Errorf("instance receiver panic: %v", v)
			}
		}()
		receiveErr = receive(request, data)
	}()
	reply := []byte{0}
	if receiveErr != nil {
		if errors.Is(receiveErr, ErrClosed) {
			reply[0] = 2
		} else if errors.Is(receiveErr, errInstanceWindow) {
			reply[0] = 3
		} else {
			maxErr := min(maxInstanceErrorBytes, max(0, i.limit-1))
			reply = append([]byte{1}, []byte(truncateUTF8(receiveErr.Error(), maxErr))...)
		}
	}
	if writeInstanceMessage(conn, reply, i.limit) == nil {
		var accepted [1]byte
		_, _ = io.ReadFull(conn, accepted[:])
	}
}
