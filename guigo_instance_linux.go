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
	"net"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// unixInstance 持有不落盘的Linux实例入口
type unixInstance struct {
	*net.UnixListener
	once sync.Once
	err  error
}

// instanceAddress 生成当前用户及图形会话的抽象套接字地址
// 入参: key 调用方实例键
// 返回: string 地址, error 标识错误
func instanceAddress(key string) (string, error) {
	session := os.Getenv("XDG_RUNTIME_DIR") + ":" + os.Getenv("WAYLAND_DISPLAY") + ":" + os.Getenv("DISPLAY")
	return "@guigo." + instanceHash(fmt.Sprintf("%d:%s", os.Geteuid(), session), key), nil
}

// listenInstance 原子取得抽象套接字入口，已有实例时返回nil
// 入参: address 入口地址
// 返回: instanceListener 接收端或nil, error 创建错误
func listenInstance(address string) (instanceListener, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if errors.Is(err, unix.EADDRINUSE) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &unixInstance{UnixListener: listener}, nil
}

// dialInstance 连接并验证已有实例的用户身份
// 入参: ctx 连接上下文, address 入口地址
// 返回: instanceConn 连接, error 连接错误
func dialInstance(ctx context.Context, address string) (instanceConn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return nil, err
	}
	if err := checkInstancePeer(conn.(*net.UnixConn)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn.(*net.UnixConn), nil
}

// checkInstancePeer 校验抽象套接字对端的有效用户
// 入参: conn 本机连接
// 返回: error 身份错误
func checkInstancePeer(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		peer, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		peerErr = err
		if err == nil && peer.Uid != uint32(os.Geteuid()) {
			peerErr = os.ErrPermission
		}
	})
	return errors.Join(err, peerErr)
}

// instanceUnavailable 判断是否可以重新竞争实例入口
// 入参: err 连接错误
// 返回: bool 是否可重试
func instanceUnavailable(err error) bool {
	return errors.Is(err, unix.ECONNREFUSED) || errors.Is(err, unix.ENOENT)
}

// Close 释放抽象套接字入口，可重复调用
// 返回: error 关闭错误
func (l *unixInstance) Close() error {
	l.once.Do(func() { l.err = l.UnixListener.Close() })
	return l.err
}

// Accept 接收并校验同一用户的本机连接
// 返回: instanceConn 连接, error 接收错误
func (l *unixInstance) Accept() (instanceConn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		if err := checkInstancePeer(conn); err == nil {
			return conn, nil
		}
		conn.Close()
	}
}
