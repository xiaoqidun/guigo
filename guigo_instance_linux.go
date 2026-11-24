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

//go:build linux && !android

package guigo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// unixInstance 持有Linux本机套接字实例入口与排他锁文件
type unixInstance struct {
	*net.UnixListener
	lockFile *os.File
	sockPath string
	once     sync.Once
	err      error
}

// linuxRuntimeDir 校验XDG_RUNTIME_DIR存在、非符号链接、权限为0700且属主为当前有效用户
// 返回: string 校验通过的运行时目录路径，不合法时返回空字符串
func linuxRuntimeDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" || !filepath.IsAbs(dir) || strings.ContainsRune(dir, 0) {
		return ""
	}
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		return ""
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o777 != 0o700 || st.Uid != uint32(os.Geteuid()) {
		return ""
	}
	return filepath.Clean(dir)
}

// instanceAddress 生成当前用户及图形会话的本机套接字地址，优先使用私有XDG_RUNTIME_DIR路径
// 入参: key 调用方实例键
// 返回: string 入口地址, error 标识错误
func instanceAddress(key string) (string, error) {
	rawRuntimeDir := os.Getenv("XDG_RUNTIME_DIR")
	wayland := os.Getenv("WAYLAND_DISPLAY")
	var session string
	if wayland != "" {
		session = "wayland\x00" + rawRuntimeDir + "\x00" + wayland
	} else {
		session = "x11\x00" + rawRuntimeDir + "\x00" + os.Getenv("DISPLAY")
	}
	hash := instanceHash(fmt.Sprintf("%d\x00%s", os.Geteuid(), session), key)
	if dir := linuxRuntimeDir(); dir != "" {
		for _, tag := range []string{hash, hash[:32]} {
			sockPath := filepath.Join(dir, "guigo-"+tag+".sock")
			if len(sockPath) <= 107 {
				return sockPath, nil
			}
		}
	}
	return "@guigo." + hash, nil
}

// listenInstance 原子取得单实例监听入口，已有活跃实例时返回nil
// 入参: address 入口地址
// 返回: instanceListener 接收端或nil, error 创建错误
func listenInstance(address string) (instanceListener, error) {
	if strings.HasPrefix(address, "@") {
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
		if errors.Is(err, unix.EADDRINUSE) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &unixInstance{UnixListener: listener}, nil
	}
	lockPath := strings.TrimSuffix(address, ".sock") + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = lockFile.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, nil
		}
		return nil, err
	}
	if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
		_ = lockFile.Close()
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		_ = lockFile.Close()
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	_ = os.Chmod(address, 0600)
	return &unixInstance{UnixListener: listener, lockFile: lockFile, sockPath: address}, nil
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

// checkInstancePeer 校验本机套接字对端的有效用户
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

// Close 停止监听、在持锁状态下清理套接字文件并释放锁，可重复调用
// 返回: error 关闭错误
func (l *unixInstance) Close() error {
	l.once.Do(func() {
		l.err = l.UnixListener.Close()
		if l.sockPath != "" {
			_ = os.Remove(l.sockPath)
		}
		if l.lockFile != nil {
			if closeErr := l.lockFile.Close(); l.err == nil {
				l.err = closeErr
			}
		}
	})
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
