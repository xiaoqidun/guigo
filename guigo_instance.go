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
	"io"
	"sync"
	"time"
)

// instanceLimit 限制单次实例消息的字节数
const instanceLimit = 1 << 20

// errInstanceWindow 表示窗口已不属于当前桌面，需要重新选择接收端
var errInstanceWindow = errors.New("instance window is not on current desktop")

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
	once     sync.Once
	err      error
}

// OpenInstance 创建实例入口，已有实例时转交消息并返回nil
// 首个实例不调用receive处理自己的消息，调用方自行处理初始请求
// receive串行执行，应快速接收或入队；返回nil表示已接受，不表示业务已完成
// receive应响应上下文取消，不得同步调用当前实例的Close
// receive返回ErrClosed时，发送方等待入口释放后重新竞争
// 不同用户、登录会话及Windows提升权限分别隔离，不修改窗口应用标识
// 入参: ctx 生命周期上下文, key 调用方实例键, data 转交消息，最大1MiB, receive 接收回调
// 返回: *Instance 首个实例入口或nil, error 创建或转交错误
func OpenInstance(ctx context.Context, key string, data []byte, receive func(context.Context, []byte) error) (*Instance, error) {
	if ctx == nil || key == "" || receive == nil || len(data) > instanceLimit {
		return nil, errors.New("invalid instance options")
	}
	address, err := instanceAddress(key)
	if err != nil {
		return nil, err
	}
	request, cancel := context.WithTimeout(ctx, 10*time.Second)
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
			return startInstance(ctx, listener, receive), nil
		}
		conn, err := dialInstance(request, address)
		if err == nil {
			err = forwardInstance(request, conn, data)
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

// OpenWindowInstance 优先转交到当前虚拟桌面最近激活的同组窗口，没有时创建入口
// Windows按窗口实时所属桌面选择，其他平台沿用OpenInstance的复用范围
// 返回入口后应传入Options.Instance启动窗口，并在运行结束后关闭入口
// 窗口登记前同组启动会等待；消息接收及用户隔离约定与OpenInstance相同
// 入参: ctx 生命周期上下文, key 调用方实例键, data 转交消息, receive 接收回调
// 返回: *Instance 新窗口入口或nil, error 创建或转交错误
func OpenWindowInstance(ctx context.Context, key string, data []byte, receive func(context.Context, []byte) error) (*Instance, error) {
	if ctx == nil || key == "" || receive == nil || len(data) > instanceLimit {
		return nil, errors.New("invalid instance options")
	}
	return openWindowInstance(ctx, key, data, receive)
}

// startInstance 启动入口接收循环
// 入参: ctx 生命周期上下文, listener 接收端, receive 接收回调
// 返回: *Instance 实例入口
func startInstance(ctx context.Context, listener instanceListener, receive func(context.Context, []byte) error) *Instance {
	life, stop := context.WithCancel(ctx)
	instance := &Instance{listener: listener, cancel: stop, done: make(chan struct{})}
	go instance.serve(life, receive)
	return instance
}

// forwardInstance 转交消息并读取接收结果，不重发已确认或结果不明的请求
// 入参: ctx 请求上下文, conn 本机连接, data 消息
// 返回: error 转交错误
func forwardInstance(ctx context.Context, conn instanceConn, data []byte) error {
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
	if err := writeInstanceMessage(conn, data); err != nil {
		return err
	}
	reply, err := readInstanceMessage(conn)
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
// 入参: reader 数据来源
// 返回: []byte 消息, error 读取错误
func readInstanceMessage(reader io.Reader) ([]byte, error) {
	var size uint32
	if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
		return nil, err
	}
	if size > instanceLimit {
		return nil, errors.New("instance message too large")
	}
	data := make([]byte, size)
	_, err := io.ReadFull(reader, data)
	return data, err
}

// writeInstanceMessage 写入长度受限的实例消息
// 入参: writer 数据目标, data 消息
// 返回: error 写入错误
func writeInstanceMessage(writer io.Writer, data []byte) error {
	if len(data) > instanceLimit {
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

// Close 停止接收并释放实例入口，可重复调用
// 返回: error 关闭错误
func (i *Instance) Close() error {
	if i == nil {
		return nil
	}
	i.once.Do(func() {
		i.err = i.listener.Close()
		<-i.done
		i.cancel()
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

// receive 处理单个连接并等待对方读取回执
// 入参: ctx 生命周期上下文, conn 本机连接, receive 接收回调
func (i *Instance) receive(ctx context.Context, conn instanceConn, receive func(context.Context, []byte) error) {
	defer conn.Close()
	request, cancel := context.WithTimeout(ctx, 10*time.Second)
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
	data, err := readInstanceMessage(conn)
	if err != nil || request.Err() != nil {
		return
	}
	reply := []byte{0}
	if err := receive(request, data); err != nil {
		if errors.Is(err, ErrClosed) {
			reply[0] = 2
		} else if errors.Is(err, errInstanceWindow) {
			reply[0] = 3
		} else {
			reply = append([]byte{1}, []byte(err.Error())...)
		}
	}
	if writeInstanceMessage(conn, reply) == nil {
		var accepted [1]byte
		_, _ = io.ReadFull(conn, accepted[:])
	}
}
