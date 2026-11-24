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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// WebView2最低支持版本与运行时加载类型标识
const (
	minimumRuntime               = "113.0.1774.30"
	installedRuntimeType uintptr = 0
	unknownRuntimeType   uintptr = 0xffffffff
)

// runtimeLibraries 保存尚未允许卸载的运行时模块
var runtimeLibraries = make(map[string]*syscall.DLL)

// runtimeClient 保存运行时位置与版本
type runtimeClient struct {
	path    string
	version [4]uint32
}

// runtimeVersion 解析四段浏览器版本
// 入参: value 版本字符串
// 返回: [4]uint32 版本分量, error 解析错误
func runtimeVersion(value string) ([4]uint32, error) {
	var version [4]uint32
	parts := strings.Split(value, ".")
	if len(parts) != len(version) {
		return version, fmt.Errorf("%w: invalid WebView2 Runtime version: %s", ErrRuntimeUnavailable, value)
	}
	for i, part := range parts {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return version, fmt.Errorf("%w: invalid WebView2 Runtime version: %s", ErrRuntimeUnavailable, value)
		}
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return version, fmt.Errorf("%w: invalid WebView2 Runtime version: %s", ErrRuntimeUnavailable, value)
		}
		version[i] = uint32(n)
	}
	return version, nil
}

// runtimeVersionLess 比较浏览器版本
// 入参: a 首个版本, b 次个版本
// 返回: bool 首个版本是否较旧
func runtimeVersionLess(a, b [4]uint32) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// checkRuntimeVersion 检查浏览器版本是否满足接口要求
// 入参: version 浏览器版本
// 返回: error 版本检查错误
func checkRuntimeVersion(version string) error {
	current, err := runtimeVersion(version)
	if err != nil {
		return err
	}
	minimum, _ := runtimeVersion(minimumRuntime)
	if runtimeVersionLess(current, minimum) {
		return fmt.Errorf("%w: WebView2 Runtime %s is older than required %s", ErrRuntimeUnavailable, version, minimumRuntime)
	}
	return nil
}

// runtimeRegistryString 读取运行时注册项中的字符串
// 入参: key 注册项, name 字段名称
// 返回: string 字段内容, error 读取错误
func runtimeRegistryString(key syscall.Handle, name string) (string, error) {
	var kind, size uint32
	if err := syscall.RegQueryValueEx(key, wide(name), nil, &kind, nil, &size); err != nil {
		return "", err
	}
	if kind != syscall.REG_SZ || size < 2 || size > 65536 || size%2 != 0 {
		return "", fmt.Errorf("invalid runtime registry value: %s", name)
	}
	data := make([]uint16, size/2)
	if err := syscall.RegQueryValueEx(key, wide(name), nil, &kind, (*byte)(unsafe.Pointer(&data[0])), &size); err != nil {
		return "", err
	}
	if kind != syscall.REG_SZ || size < 2 || size%2 != 0 || int(size/2) > len(data) || data[size/2-1] != 0 {
		return "", fmt.Errorf("invalid runtime registry value: %s", name)
	}
	for _, value := range data[:size/2-1] {
		if value == 0 {
			return "", fmt.Errorf("invalid runtime registry value: %s", name)
		}
	}
	return syscall.UTF16ToString(data[:size/2-1]), nil
}

// runtimeRegistryClient 读取并检查注册项中的运行时位置
// 入参: key 注册项, branch 注册分支
// 返回: runtimeClient 运行时信息, error 读取错误
func runtimeRegistryClient(key syscall.Handle, branch string) (runtimeClient, error) {
	var location, version string
	if branch == "ClientState" {
		value, err := runtimeRegistryString(key, "EBWebView")
		if err != nil {
			return runtimeClient{}, err
		}
		location = filepath.Clean(value)
		version = filepath.Base(location)
	} else {
		value, err := runtimeRegistryString(key, "location")
		if err != nil {
			return runtimeClient{}, err
		}
		version, err = runtimeRegistryString(key, "pv")
		if err != nil {
			return runtimeClient{}, err
		}
		location = filepath.Join(value, version)
	}
	if !filepath.IsAbs(location) || strings.HasPrefix(location, `\\`) {
		return runtimeClient{}, errors.New("runtime location must be local absolute path")
	}
	if err := checkRuntimeVersion(version); err != nil {
		return runtimeClient{}, err
	}
	path := filepath.Join(location, "EBWebView", "x64", "EmbeddedBrowserWebView.dll")
	for _, file := range []string{filepath.Join(location, "msedgewebview2.exe"), path} {
		info, err := os.Stat(file)
		if err != nil {
			return runtimeClient{}, err
		}
		if !info.Mode().IsRegular() {
			return runtimeClient{}, fmt.Errorf("runtime file is not regular: %s", file)
		}
	}
	parsed, _ := runtimeVersion(version)
	return runtimeClient{path: path, version: parsed}, nil
}

// installedRuntimes 按安装范围和版本排列可用运行时
// 返回: []string 原生模块路径, error 查找错误
func installedRuntimes() ([]string, error) {
	var paths []string
	seen := make(map[string]bool)
	for _, root := range []syscall.Handle{syscall.HKEY_LOCAL_MACHINE, syscall.HKEY_CURRENT_USER} {
		var clients []runtimeClient
		for _, view := range []uint32{syscall.KEY_WOW64_32KEY, syscall.KEY_WOW64_64KEY} {
			for _, branch := range []string{"ClientState", "Clients"} {
				keyPath := `Software\Microsoft\EdgeUpdate\` + branch + `\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
				var key syscall.Handle
				if syscall.RegOpenKeyEx(root, wide(keyPath), 0, syscall.KEY_QUERY_VALUE|view, &key) != nil {
					continue
				}
				client, err := runtimeRegistryClient(key, branch)
				syscall.RegCloseKey(key)
				if err == nil {
					clients = append(clients, client)
				}
			}
		}
		if len(clients) != 0 {
			sort.SliceStable(clients, func(i, j int) bool { return runtimeVersionLess(clients[j].version, clients[i].version) })
			for _, client := range clients {
				key := strings.ToLower(client.path)
				if !seen[key] {
					seen[key] = true
					paths = append(paths, client.path)
				}
			}
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: Microsoft WebView2 Runtime %s or later required", ErrRuntimeUnavailable, minimumRuntime)
	}
	return paths, nil
}

// loadRuntime 加载已安装或显式指定的原生运行时
// 入参: path 原生模块路径
// 返回: *syscall.DLL 运行时模块, uintptr 运行时类型, error 加载错误
func loadRuntime(path string) (*syscall.DLL, uintptr, error) {
	kind := installedRuntimeType
	var paths []string
	if path == "" {
		var err error
		paths, err = installedRuntimes()
		if err != nil {
			return nil, 0, err
		}
	} else {
		kind = unknownRuntimeType
		paths = []string{path}
	}
	dll, err := loadRuntimePaths(paths)
	return dll, kind, err
}

// loadRuntimePaths 依次加载候选运行时模块
// 入参: paths 原生模块路径
// 返回: *syscall.DLL 运行时模块, error 加载错误
func loadRuntimePaths(paths []string) (*syscall.DLL, error) {
	var failures []error
	for _, path := range paths {
		dll, err := loadRuntimeModule(path)
		if err == nil {
			return dll, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", path, err))
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("%w: no WebView2 Runtime candidates", ErrRuntimeUnavailable)
	}
	return nil, fmt.Errorf("%w: %w", ErrRuntimeUnavailable, errors.Join(failures...))
}

// loadRuntimeModule 加载并复用原生运行时模块
// 入参: path 原生模块路径
// 返回: *syscall.DLL 运行时模块, error 加载错误
func loadRuntimeModule(path string) (*syscall.DLL, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	key := strings.ToLower(path)
	if dll := runtimeLibraries[key]; dll != nil {
		return dll, nil
	}
	handle, _, err := kernel32.NewProc("LoadLibraryExW").Call(uintptr(unsafe.Pointer(wide(path))), 0, 0x1100)
	if handle == 0 {
		return nil, fmt.Errorf("%w: load WebView2 Runtime: %w", ErrRuntimeUnavailable, err)
	}
	dll := &syscall.DLL{Name: path, Handle: syscall.Handle(handle)}
	if _, err := dll.FindProc("CreateWebViewEnvironmentWithOptionsInternal"); err != nil {
		dll.Release()
		return nil, fmt.Errorf("%w: unsupported WebView2 Runtime: %w", ErrRuntimeUnavailable, err)
	}
	for _, existing := range runtimeLibraries {
		if existing.Handle == dll.Handle {
			dll.Release()
			return existing, nil
		}
	}
	runtimeLibraries[key] = dll
	return dll, nil
}

// releaseRuntimes 回收已不再使用的运行时模块
func releaseRuntimes() {
	for _, dll := range runtimeLibraries {
		releaseRuntime(dll)
	}
}

// releaseRuntime 卸载已释放全部对象的运行时模块
// 入参: dll 运行时模块
func releaseRuntime(dll *syscall.DLL) {
	proc, err := dll.FindProc("DllCanUnloadNow")
	if err != nil {
		return
	}
	result, _, _ := proc.Call()
	if result == 0 && dll.Release() == nil {
		delete(runtimeLibraries, strings.ToLower(dll.Name))
	}
}
