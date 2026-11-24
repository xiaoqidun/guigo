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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// 临时响应文件的独占访问权限与关闭自动删除标志
const (
	temporaryFileAccess = syscall.GENERIC_READ | syscall.GENERIC_WRITE | 0x10000
	temporaryFileFlags  = 0x100 | 0x04000000
)

// 原生IStream根表、虚函数表、缓存接口标识与常驻MTA调度状态
var (
	streamRoots         sync.Map
	streamTable         [14]uintptr
	iidUnknown          syscall.GUID
	iidMarshal          syscall.GUID
	iidStream           syscall.GUID
	iidSequentialStream syscall.GUID
	iidAgileObject      syscall.GUID
	mtaMu               sync.Mutex
	mtaQueue            chan mtaTask
)

// mtaTask 保存待常驻MTA线程执行的操作与结果Channel
type mtaTask struct {
	fn   func() error
	done chan error
}

// temporaryDirectory 管理当前窗口的临时文件目录
type temporaryDirectory struct {
	mu     sync.Mutex
	path   string
	closed bool
}

// temporaryFile 保存临时文件及所属目录
type temporaryFile struct {
	*os.File
	directory *temporaryDirectory
}

// responseSpool 为生产者和原生流提供基于可寻址流或临时文件的共享存储
type responseSpool struct {
	mu       sync.Mutex
	file     io.ReadSeekCloser
	size     int64
	complete bool
	err      error
	refs     int
	stop     func() bool
}

// nativeStream 实现可跨线程读取和克隆的只读IStream
type nativeStream struct {
	table   *[14]uintptr
	refs    atomic.Int32
	mu      sync.Mutex
	offset  int64
	spool   *responseSpool
	marshal *comObject
	pin     runtime.Pinner
}

// streamStat 对应STATSTG
type streamStat struct {
	Name     uintptr
	Type     uint32
	Size     uint64
	Modified uint64
	Created  uint64
	Accessed uint64
	Mode     uint32
	Locks    uint32
	Class    syscall.GUID
	State    uint32
	Reserved uint32
}

// init 固定原生流回调入口并缓存常用COM接口标识
func init() {
	streamTable = [14]uintptr{
		syscall.NewCallback(streamQuery),
		syscall.NewCallback(streamAddRef),
		syscall.NewCallback(streamRelease),
		syscall.NewCallback(streamRead),
		syscall.NewCallback(streamWrite),
		syscall.NewCallback(streamSeek),
		syscall.NewCallback(streamSetSize),
		syscall.NewCallback(streamCopy),
		syscall.NewCallback(streamCommit),
		syscall.NewCallback(streamRevert),
		syscall.NewCallback(streamRegion),
		syscall.NewCallback(streamRegion),
		syscall.NewCallback(streamGetStat),
		syscall.NewCallback(streamClone),
	}
	iidUnknown = guid("{00000000-0000-0000-C000-000000000046}")
	iidMarshal = guid("{00000003-0000-0000-C000-000000000046}")
	iidStream = guid("{0000000C-0000-0000-C000-000000000046}")
	iidSequentialStream = guid("{0C733A30-2A1C-11CE-ADE5-00AA0044773D}")
	iidAgileObject = guid("{94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90}")
}

// runOnMTA 在进程常驻MTA线程执行操作并隔离异常，遇到被污染的STA线程时销毁重试
// 入参: fn 待执行操作
// 返回: error 初始化或执行错误
func runOnMTA(fn func() error) error {
	mtaMu.Lock()
	queue := mtaQueue
	if queue == nil {
		var lastErr error
		for range 8 {
			ready := make(chan uintptr, 1)
			candidate := make(chan mtaTask)
			go func() {
				runtime.LockOSThread()
				hr, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 0)
				ready <- hr
				if hresult(hr) != nil {
					return
				}
				for task := range candidate {
					var err error
					func() {
						defer func() {
							if v := recover(); v != nil {
								err = fmt.Errorf("MTA task panic: %v", v)
							}
						}()
						err = task.fn()
					}()
					task.done <- err
				}
			}()
			hr := <-ready
			err := hresult(hr)
			if err == nil {
				queue = candidate
				mtaQueue = candidate
				break
			}
			lastErr = err
			if uint32(hr) != 0x80010106 {
				break
			}
		}
		if queue == nil {
			mtaMu.Unlock()
			return lastErr
		}
	}
	mtaMu.Unlock()
	done := make(chan error, 1)
	queue <- mtaTask{fn: fn, done: done}
	return <-done
}

// newResponseSpool 创建可寻址响应临时存储
// 入参: ctx 运行上下文, directory 临时目录
// 返回: *responseSpool 响应存储, error 创建错误
func newResponseSpool(ctx context.Context, directory *temporaryDirectory) (*responseSpool, error) {
	file, err := directory.create("stream-")
	if err != nil {
		return nil, err
	}
	s := &responseSpool{file: file, refs: 1}
	s.stop = context.AfterFunc(ctx, func() { s.finish(ctx.Err()) })
	return s, nil
}

// lookupStream 从活动根表安全检索原生流对象
// 入参: self 原生流地址
// 返回: *nativeStream 流对象, bool 是否存在
func lookupStream(self uintptr) (*nativeStream, bool) {
	value, ok := streamRoots.Load(self)
	if !ok {
		return nil, false
	}
	return value.(*nativeStream), true
}

// streamQuery 提供流接口和敏捷对象标记并隔离异常
// 入参: self 原生流地址, iid 接口标识, out 接口输出
// 返回: uintptr HRESULT结果
func streamQuery(self uintptr, iid *syscall.GUID, out *uintptr) (result uintptr) {
	defer func() {
		if recover() != nil {
			result = 0x80004005
		}
	}()
	if out == nil || iid == nil {
		return 0x80004003
	}
	*out = 0
	stream, ok := lookupStream(self)
	if !ok {
		return 0x80004005
	}
	g := *iid
	if g != iidMarshal && g != iidUnknown && g != iidStream && g != iidSequentialStream && g != iidAgileObject {
		return 0x80004002
	}
	if addRefPositive(&stream.refs) == 0 {
		return 0x80004005
	}
	if g == iidMarshal {
		marshal := stream.marshal
		if marshal == nil {
			streamRelease(self)
			return 0x80004002
		}
		hr := com(marshal, 0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(out)))
		streamRelease(self)
		return hr
	}
	*out = self
	return 0
}

// streamAddRef 增加原生流引用
// 入参: self 原生流地址
// 返回: uintptr 引用计数
func streamAddRef(self uintptr) (result uintptr) {
	defer func() {
		if recover() != nil {
			result = 0
		}
	}()
	stream, ok := lookupStream(self)
	if !ok {
		return 0
	}
	return uintptr(addRefPositive(&stream.refs))
}

// streamRelease 释放原生流引用并在归零命中时解除对象固定
// 入参: self 原生流地址
// 返回: uintptr 引用计数
func streamRelease(self uintptr) (result uintptr) {
	defer func() {
		if recover() != nil {
			result = 0
		}
	}()
	stream, ok := lookupStream(self)
	if !ok {
		return 0
	}
	refs, decremented := releaseRefPositive(&stream.refs)
	if !decremented {
		return 0
	}
	if refs == 0 {
		if _, loaded := streamRoots.LoadAndDelete(self); loaded {
			release(stream.marshal)
			stream.marshal = nil
			stream.spool.drop()
			stream.pin.Unpin()
		}
		return 0
	}
	return uintptr(refs)
}

// streamRead 读取最多指定字节的完整响应内容
// 入参: self 原生流地址, buffer 输出缓冲区, count 读取上限, read 已读字节数输出
// 返回: uintptr HRESULT结果
func streamRead(self uintptr, buffer *byte, count uint32, read *uint32) (result uintptr) {
	defer func() {
		if recover() != nil {
			result = 0x80004005
		}
	}()
	if read != nil {
		*read = 0
	}
	if count == 0 {
		return 0
	}
	if buffer == nil {
		return 0x80030009
	}
	stream, ok := lookupStream(self)
	if !ok {
		return 0x80004005
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	s := stream.spool
	s.mu.Lock()
	defer s.mu.Unlock()
	if stream.offset >= s.size {
		if s.err != nil {
			return 0x8003001e
		}
		return 1
	}
	available := s.size - stream.offset
	length := int64(count)
	if length > available {
		length = available
	}
	bytes := unsafe.Slice(buffer, int(length))
	if _, err := s.file.Seek(stream.offset, io.SeekStart); err != nil {
		return 0x80030019
	}
	n, err := io.ReadFull(s.file, bytes)
	stream.offset += int64(n)
	if read != nil {
		*read = uint32(n)
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0x8003001e
	}
	if uint32(n) < count {
		return 1
	}
	return 0
}

// streamSeek 设置独立流位置
// 入参: self 原生流地址, offset 偏移量, origin 起始位置, position 最终位置输出
// 返回: uintptr HRESULT结果
func streamSeek(self uintptr, offset int64, origin uint32, position *uint64) (result uintptr) {
	defer func() {
		if recover() != nil {
			result = 0x80004005
		}
	}()
	stream, ok := lookupStream(self)
	if !ok {
		return 0x80004005
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	next := offset
	switch origin {
	case 0:
	case 1:
		next += stream.offset
	case 2:
		s := stream.spool
		s.mu.Lock()
		next += s.size
		s.mu.Unlock()
	default:
		return 0x80030001
	}
	if next < 0 {
		return 0x80030001
	}
	stream.offset = next
	if position != nil {
		*position = uint64(next)
	}
	return 0
}

// streamGetStat 返回当前流类型及已知长度
// 入参: self 原生流地址, out 状态输出, flags 查询标记
// 返回: uintptr HRESULT结果
func streamGetStat(self uintptr, out *streamStat, flags uint32) (result uintptr) {
	defer func() {
		if recover() != nil {
			result = 0x80004005
		}
	}()
	if out == nil {
		return 0x80030009
	}
	stream, ok := lookupStream(self)
	if !ok {
		return 0x80004005
	}
	s := stream.spool
	s.mu.Lock()
	defer s.mu.Unlock()
	*out = streamStat{Type: 2, Size: uint64(s.size)}
	return 0
}

// streamClone 克隆共享内容但独立位置的流
// 入参: self 原生流地址, out 克隆接口输出
// 返回: uintptr HRESULT结果
func streamClone(self uintptr, out **comObject) (result uintptr) {
	defer func() {
		if recover() != nil {
			result = 0x80004005
		}
	}()
	if out == nil {
		return 0x80030009
	}
	*out = nil
	stream, ok := lookupStream(self)
	if !ok {
		return 0x80004005
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	cloned, err := stream.spool.native(stream.offset)
	if err != nil {
		return 0x80004005
	}
	*out = cloned
	return 0
}

// streamWrite 拒绝写入只读流
// 入参: self 原生流地址, buffer 输入缓冲区, count 数据长度, written 写入长度输出
// 返回: uintptr HRESULT结果
func streamWrite(self uintptr, buffer *byte, count uint32, written *uint32) uintptr {
	if written != nil {
		*written = 0
	}
	return 0x80030005
}

// streamSetSize 拒绝调整只读流长度
// 入参: self 原生流地址, size 目标长度
// 返回: uintptr HRESULT结果
func streamSetSize(self uintptr, size uint64) uintptr {
	return 0x80030005
}

// streamCopy 返回不支持流间复制
// 入参: self 原生流地址, target 目标流, count 复制长度, read 读取长度输出, written 写入长度输出
// 返回: uintptr HRESULT结果
func streamCopy(self uintptr, target *comObject, count uint64, read, written *uint64) uintptr {
	return 0x80004001
}

// streamCommit 完成只读流的空提交
// 入参: self 原生流地址, flags 提交标记
// 返回: uintptr HRESULT结果
func streamCommit(self uintptr, flags uint32) uintptr {
	return 0
}

// streamRevert 返回不支持回滚
// 入参: self 原生流地址
// 返回: uintptr HRESULT结果
func streamRevert(self uintptr) uintptr {
	return 0x80030001
}

// streamRegion 返回不支持区域锁定
// 入参: self 原生流地址, offset 区域偏移, count 区域长度, kind 锁定类型
// 返回: uintptr HRESULT结果
func streamRegion(self uintptr, offset, count uint64, kind uint32) uintptr {
	return 0x80030001
}

// Close 关闭临时文件并回收已停用的空目录
// 返回: error 关闭错误
func (f *temporaryFile) Close() error {
	err := f.File.Close()
	f.directory.mu.Lock()
	defer f.directory.mu.Unlock()
	if f.directory.closed {
		os.Remove(f.directory.path)
	}
	return err
}

// close 停止创建临时文件并回收空目录
func (d *temporaryDirectory) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.path != "" {
		os.Remove(d.path)
	}
}

// create 创建句柄关闭后由系统删除的临时文件
// 入参: prefix 文件名前缀
// 返回: *temporaryFile 临时文件, error 创建错误
func (d *temporaryDirectory) create(prefix string) (*temporaryFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, os.ErrClosed
	}
	for i := 0; d.path == "" && i < 100; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10000000))
		if err != nil {
			return nil, err
		}
		path := filepath.Join(os.TempDir(), fmt.Sprintf("guigo-%07d", n.Int64()))
		if err := os.Mkdir(path, 0700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return nil, err
		}
		d.path = path
	}
	if d.path == "" {
		return nil, os.ErrExist
	}
	for i := 0; i < 100; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10000000))
		if err != nil {
			return nil, err
		}
		path := filepath.Join(d.path, fmt.Sprintf("%s%07d.tmp", prefix, n.Int64()))
		handle, err := syscall.CreateFile(wide(path), temporaryFileAccess, 0, nil, syscall.CREATE_NEW, temporaryFileFlags, 0)
		if err == syscall.ERROR_FILE_EXISTS || err == syscall.ERROR_ALREADY_EXISTS {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &temporaryFile{File: os.NewFile(uintptr(handle), path), directory: d}, nil
	}
	return nil, os.ErrExist
}

// Write 增量追加内容至响应存储
// 入参: b 待写入数据
// 返回: int 写入字节数, error 写入错误
func (s *responseSpool) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.complete {
		if s.err != nil {
			return 0, s.err
		}
		return 0, io.ErrClosedPipe
	}
	if _, err := s.file.Seek(s.size, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := s.file.(io.Writer).Write(b)
	s.size += int64(n)
	if err != nil {
		s.err = err
		s.complete = true
	}
	return n, err
}

// finish 标记流结束或取消
// 入参: err 结束错误
func (s *responseSpool) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.complete {
		s.complete = true
		s.err = err
	}
}

// drop 释放生产者或流引用并回收临时文件
func (s *responseSpool) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs--
	if s.refs == 0 {
		if s.stop != nil {
			s.stop()
		}
		s.file.Close()
	}
}

// native 在常驻MTA线程创建支持跨线程封送的COM只读流
// 入参: offset 初始偏移
// 返回: *comObject 原生流, error 创建错误
func (s *responseSpool) native(offset int64) (*comObject, error) {
	s.mu.Lock()
	s.refs++
	s.mu.Unlock()
	stream := &nativeStream{table: &streamTable, spool: s, offset: offset}
	stream.refs.Store(1)
	stream.pin.Pin(stream)
	p := uintptr(unsafe.Pointer(stream))
	streamRoots.Store(p, stream)
	err := runOnMTA(func() error {
		hr, _, _ := ole32.NewProc("CoCreateFreeThreadedMarshaler").Call(p, uintptr(unsafe.Pointer(&stream.marshal)))
		return hresult(hr)
	})
	if err != nil {
		streamRelease(p)
		return nil, err
	}
	return (*comObject)(unsafe.Pointer(stream)), nil
}
