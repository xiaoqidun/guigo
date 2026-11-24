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
	"os"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// firstPipeInstanceFlag 确保首实例独占创建首个命名管道实例
const firstPipeInstanceFlag = 0x00080000

// pipeInstance 持有当前会话的互斥入口和命名管道配置
type pipeInstance struct {
	address  string
	identity string
	security *windows.SECURITY_DESCRIPTOR
	mutex    windows.Handle
	first    windows.Handle
	ctx      context.Context
	cancel   context.CancelFunc
	acceptMu sync.Mutex
	once     sync.Once
	err      error
}

// instanceSecurityDescriptor 构造仅允许当前用户访问的安全描述符，提权进程附加高完整性标签
// 入参: identity 实例身份字符串
// 返回: *windows.SECURITY_DESCRIPTOR 安全描述符, error 构造错误
func instanceSecurityDescriptor(identity string) (*windows.SECURITY_DESCRIPTOR, error) {
	sid, _, _ := strings.Cut(identity, ":")
	sddl := "D:P(A;;GA;;;" + sid + ")"
	if strings.HasSuffix(identity, ":true") {
		sddl += "S:(ML;;NW;;;HI)"
	}
	return windows.SecurityDescriptorFromString(sddl)
}

// windowsInstanceIdentity 返回进程所属用户、登录会话及提升状态
// 入参: pid 进程编号
// 返回: string 实例身份, error 查询错误
func windowsInstanceIdentity(pid uint32) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(pid, &session); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%t", user.User.Sid.String(), session, token.IsElevated()), nil
}

// instanceAddress 生成当前用户、会话及权限范围的管道地址
// 入参: key 调用方实例键
// 返回: string 管道地址, error 标识错误
func instanceAddress(key string) (string, error) {
	identity, err := windowsInstanceIdentity(windows.GetCurrentProcessId())
	if err != nil {
		return "", err
	}
	return `\\.\pipe\guigo.` + instanceHash(identity, key), nil
}

// listenInstance 取得会话内唯一入口，已有实例时返回nil
// 入参: address 管道地址
// 返回: instanceListener 接收端或nil, error 创建错误
func listenInstance(address string) (instanceListener, error) {
	identity, err := windowsInstanceIdentity(windows.GetCurrentProcessId())
	if err != nil {
		return nil, err
	}
	security, err := instanceSecurityDescriptor(identity)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: security}
	name, err := windows.UTF16PtrFromString(`Local\` + strings.TrimPrefix(address, `\\.\pipe\`))
	if err != nil {
		return nil, err
	}
	mutex, err := windows.CreateMutex(&attributes, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(mutex)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pipeName, err := windows.UTF16PtrFromString(address)
	if err != nil {
		windows.CloseHandle(mutex)
		return nil, err
	}
	first, err := windows.CreateNamedPipe(pipeName, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED|firstPipeInstanceFlag, windows.PIPE_TYPE_BYTE|windows.PIPE_REJECT_REMOTE_CLIENTS, 255, 4096, 4096, 0, &attributes)
	if err != nil {
		windows.CloseHandle(mutex)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &pipeInstance{address: address, identity: identity, security: security, mutex: mutex, first: first, ctx: ctx, cancel: cancel}, nil
}

// connectInstancePipe 等待可取消的重叠管道连接
// 入参: ctx 等待上下文, pipe 管道句柄
// 返回: error 连接错误
func connectInstancePipe(ctx context.Context, pipe windows.Handle) error {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)
	overlap := windows.Overlapped{HEvent: event}
	err = windows.ConnectNamedPipe(pipe, &overlap)
	if err == nil || errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		return nil
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return err
	}
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		windows.CancelIoEx(pipe, &overlap)
		close(canceled)
	})
	var done uint32
	err = windows.GetOverlappedResult(pipe, &overlap, &done, true)
	if !stop() {
		<-canceled
	}
	return err
}

// dialInstance 连接同一用户和权限的实例，并转交窗口激活许可
// 入参: ctx 连接上下文, address 管道地址
// 返回: instanceConn 连接, error 连接错误
func dialInstance(ctx context.Context, address string) (instanceConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, _ := windows.UTF16PtrFromString(address)
	pipe, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return nil, err
	}
	var pid uint32
	err = windows.GetNamedPipeServerProcessId(pipe, &pid)
	if err == nil {
		var peer, own string
		peer, err = windowsInstanceIdentity(pid)
		if err == nil {
			own, err = windowsInstanceIdentity(windows.GetCurrentProcessId())
			if err == nil && peer != own {
				err = os.ErrPermission
			}
		}
	}
	if err != nil {
		windows.CloseHandle(pipe)
		return nil, err
	}
	windows.NewLazySystemDLL("user32.dll").NewProc("AllowSetForegroundWindow").Call(uintptr(pid))
	return os.NewFile(uintptr(pipe), address), nil
}

// instanceUnavailable 判断管道是否尚未就绪或正在接收其他请求
// 入参: err 连接错误
// 返回: bool 是否可重试
func instanceUnavailable(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PIPE_BUSY)
}

// Accept 等待管道连接并校验对端身份
// 返回: instanceConn 连接, error 接收错误
func (l *pipeInstance) Accept() (instanceConn, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()
	for {
		if err := l.ctx.Err(); err != nil {
			return nil, err
		}
		var pipe windows.Handle
		if l.first != 0 {
			pipe = l.first
			l.first = 0
		} else {
			name, _ := windows.UTF16PtrFromString(l.address)
			attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: l.security}
			var err error
			pipe, err = windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED, windows.PIPE_TYPE_BYTE|windows.PIPE_REJECT_REMOTE_CLIENTS, 255, 4096, 4096, 0, &attributes)
			if err != nil {
				return nil, err
			}
		}
		if err := connectInstancePipe(l.ctx, pipe); err != nil {
			windows.CloseHandle(pipe)
			return nil, err
		}
		var pid uint32
		err := windows.GetNamedPipeClientProcessId(pipe, &pid)
		if err == nil {
			var identity string
			identity, err = windowsInstanceIdentity(pid)
			if err == nil && identity != l.identity {
				err = os.ErrPermission
			}
		}
		if err == nil {
			return os.NewFile(uintptr(pipe), l.address), nil
		}
		windows.CloseHandle(pipe)
	}
}

// Close 停止等待并释放互斥入口与未使用的首管道句柄
// 返回: error 关闭错误
func (l *pipeInstance) Close() error {
	l.once.Do(func() {
		l.cancel()
		l.acceptMu.Lock()
		defer l.acceptMu.Unlock()
		if l.first != 0 {
			windows.CloseHandle(l.first)
			l.first = 0
		}
		l.err = windows.CloseHandle(l.mutex)
	})
	return l.err
}
