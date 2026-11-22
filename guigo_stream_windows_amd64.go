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

const (
	// temporaryFileAccess 允许临时文件读写和句柄关闭时删除
	temporaryFileAccess = syscall.GENERIC_READ | syscall.GENERIC_WRITE | 0x10000
	// temporaryFileFlags 标记临时文件并在最后一个句柄关闭时删除
	temporaryFileFlags = 0x100 | 0x04000000
)

var (
	// streamRoots 持有活动原生流对象防止被垃圾回收
	streamRoots sync.Map
	// streamTable 保存原生IStream虚函数表地址
	streamTable [14]uintptr
)

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

// responseSpool 为生产者和原生流提供有界内存的可寻址共享存储
type responseSpool struct {
	mu       sync.Mutex
	cond     *sync.Cond
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

// init 固定原生流回调入口
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
}

// newResponseSpool 创建可增量读取的临时存储
// 入参: ctx 运行上下文, directory 临时目录
// 返回: *responseSpool 响应存储, error 创建错误
func newResponseSpool(ctx context.Context, directory *temporaryDirectory) (*responseSpool, error) {
	file, err := directory.create("stream-")
	if err != nil {
		return nil, err
	}
	s := &responseSpool{file: file, refs: 1}
	s.cond = sync.NewCond(&s.mu)
	s.stop = context.AfterFunc(ctx, func() { s.finish(ctx.Err()) })
	return s, nil
}

// streamQuery 提供流接口和敏捷对象标记
// 入参: stream 原生流, iid 接口标识, out 接口输出
// 返回: uintptr HRESULT结果
func streamQuery(stream *nativeStream, iid *syscall.GUID, out **comObject) uintptr {
	if out == nil {
		return 0x80004003
	}
	*out = nil
	g := *iid
	if g == guid("{00000003-0000-0000-C000-000000000046}") && stream.marshal != nil {
		return com(stream.marshal, 0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(out)))
	}
	if g != guid("{00000000-0000-0000-C000-000000000046}") && g != guid("{0000000C-0000-0000-C000-000000000046}") && g != guid("{0C733A30-2A1C-11CE-ADE5-00AA0044773D}") && g != guid("{94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90}") {
		return 0x80004002
	}
	*out = (*comObject)(unsafe.Pointer(stream))
	streamAddRef(stream)
	return 0
}

// streamAddRef 增加原生流引用
// 入参: stream 原生流
// 返回: uintptr 引用计数
func streamAddRef(stream *nativeStream) uintptr {
	return uintptr(stream.refs.Add(1))
}

// streamRelease 释放原生流引用
// 入参: stream 原生流
// 返回: uintptr 引用计数
func streamRelease(stream *nativeStream) uintptr {
	refs := stream.refs.Add(-1)
	if refs == 0 {
		streamRoots.Delete(uintptr(unsafe.Pointer(stream)))
		release(stream.marshal)
		stream.spool.drop()
	}
	return uintptr(refs)
}

// streamRead 等待增量内容并读取最多指定字节
// 入参: stream 原生流, buffer 输出缓冲区, count 读取上限, read 已读字节数输出
// 返回: uintptr HRESULT结果
func streamRead(stream *nativeStream, buffer *byte, count uint32, read *uint32) uintptr {
	if read != nil {
		*read = 0
	}
	if count == 0 {
		return 0
	}
	if buffer == nil {
		return 0x80030009
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	s := stream.spool
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.size-stream.offset < int64(uint32(count)) && !s.complete {
		s.cond.Wait()
	}
	if stream.offset >= s.size {
		if s.err != nil {
			return 0x8003001e
		}
		return 1
	}
	available := s.size - stream.offset
	length := int64(uint32(count))
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
	if err != nil && err != io.EOF {
		return 0x8003001e
	}
	if uint32(n) < count {
		return 1
	}
	return 0
}

// streamSeek 设置独立流位置
// 入参: stream 原生流, offset 偏移量, origin 起始位置, position 最终位置输出
// 返回: uintptr HRESULT结果
func streamSeek(stream *nativeStream, offset int64, origin uint32, position *uint64) uintptr {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	next := int64(offset)
	switch origin {
	case 0:
	case 1:
		next += stream.offset
	case 2:
		s := stream.spool
		s.mu.Lock()
		for !s.complete {
			s.cond.Wait()
		}
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
// 入参: stream 原生流, out 状态输出, flags 查询标记
// 返回: uintptr HRESULT结果
func streamGetStat(stream *nativeStream, out *streamStat, flags uint32) uintptr {
	if out == nil {
		return 0x80030009
	}
	s := stream.spool
	s.mu.Lock()
	defer s.mu.Unlock()
	*out = streamStat{Type: 2, Size: uint64(s.size)}
	return 0
}

// streamClone 克隆共享内容但独立位置的流
// 入参: stream 原生流, out 克隆接口输出
// 返回: uintptr HRESULT结果
func streamClone(stream *nativeStream, out **comObject) uintptr {
	if out == nil {
		return 0x80030009
	}
	*out = nil
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
// 入参: stream 原生流, buffer 输入缓冲区, count 数据长度, written 写入长度输出
// 返回: uintptr HRESULT结果
func streamWrite(stream *nativeStream, buffer *byte, count uint32, written *uint32) uintptr {
	if written != nil {
		*written = 0
	}
	return 0x80030005
}

// streamSetSize 拒绝调整只读流长度
// 入参: stream 原生流, size 目标长度
// 返回: uintptr HRESULT结果
func streamSetSize(stream *nativeStream, size uint64) uintptr {
	return 0x80030005
}

// streamCopy 返回不支持流间复制
// 入参: stream 原生流, target 目标流, count 复制长度, read 读取长度输出, written 写入长度输出
// 返回: uintptr HRESULT结果
func streamCopy(stream *nativeStream, target *comObject, count uint64, read, written *uint64) uintptr {
	return 0x80004001
}

// streamCommit 完成只读流的空提交
// 入参: stream 原生流, flags 提交标记
// 返回: uintptr HRESULT结果
func streamCommit(stream *nativeStream, flags uint32) uintptr {
	return 0
}

// streamRevert 返回不支持回滚
// 入参: stream 原生流
// 返回: uintptr HRESULT结果
func streamRevert(stream *nativeStream) uintptr {
	return 0x80030001
}

// streamRegion 返回不支持区域锁定
// 入参: stream 原生流, offset 区域偏移, count 区域长度, kind 锁定类型
// 返回: uintptr HRESULT结果
func streamRegion(stream *nativeStream, offset, count uint64, kind uint32) uintptr {
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
		handle, err := syscall.CreateFile(wide(path), temporaryFileAccess, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.CREATE_NEW, temporaryFileFlags, 0)
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

// Write 增量追加内容并唤醒原生读取器
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
	s.cond.Broadcast()
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
	s.cond.Broadcast()
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

// native 创建独立位置的COM流
// 入参: offset 初始偏移
// 返回: *comObject 原生流, error 创建错误
func (s *responseSpool) native(offset int64) (*comObject, error) {
	s.mu.Lock()
	s.refs++
	s.mu.Unlock()
	stream := &nativeStream{table: &streamTable, spool: s, offset: offset}
	stream.refs.Store(1)
	p := uintptr(unsafe.Pointer(stream))
	streamRoots.Store(p, stream)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 0)
	if hresult(hr) == nil {
		defer ole32.NewProc("CoUninitialize").Call()
	} else if uint32(hr) != 0x80010106 {
		streamRelease(stream)
		return nil, hresult(hr)
	}
	hr, _, _ = ole32.NewProc("CoCreateFreeThreadedMarshaler").Call(p, uintptr(unsafe.Pointer(&stream.marshal)))
	if err := hresult(hr); err != nil {
		streamRelease(stream)
		return nil, err
	}
	return (*comObject)(unsafe.Pointer(stream)), nil
}
