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
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// 文件关联操作锁与系统注册表和外壳刷新接口
var (
	associationMu        sync.Mutex
	associationSetValue  = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegSetValueExW")
	associationDeleteKey = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegDeleteKeyExW")
	associationNotify    = windows.NewLazySystemDLL("shell32.dll").NewProc("SHChangeNotify")
)

// associationValue 保存一个注册表值及其位置
type associationValue struct {
	path, name string
	kind       uint32
	data       []byte
	exists     bool
}

// associationRoot 返回指定范围的独立注册表根
// 入参: scope 作用范围
// 返回: registry.Key 注册表根
func associationRoot(scope AssociationScope) registry.Key {
	if scope == AssociationMachine {
		return registry.LOCAL_MACHINE
	}
	return registry.CURRENT_USER
}

// associationPath 返回应用的专用登记根
// 入参: o 应用配置
// 返回: string 专用键路径
func associationPath(o AssociationOptions) string {
	return `Software\` + o.AppID + `.FileAssociations`
}

// associationValues 构造应用能力、类型和公共打开方式值
// 入参: o 应用配置
// 返回: []associationValue 登记内容
func associationValues(o AssociationOptions) []associationValue {
	capability := associationPath(o) + `\Capabilities`
	command := windows.EscapeArg(o.Executable)
	if !strings.HasPrefix(command, "\"") {
		command = "\"" + command + "\""
	}
	for _, argument := range o.Arguments {
		command += " " + windows.EscapeArg(argument)
	}
	command += ` "%1"`
	icon := fmt.Sprintf("\"%s\",%d", o.Executable, o.IconIndex)
	values := []associationValue{
		associationString(capability, "ApplicationName", o.Name),
		associationString(capability, "ApplicationDescription", o.Description),
		associationString(capability, "ApplicationIcon", icon),
		associationString(`Software\RegisteredApplications`, o.AppID, capability),
	}
	for _, kind := range o.Types {
		id := o.AppID + "." + kind.ID
		path := `Software\Classes\` + id
		values = append(values, associationString(path, "", kind.Name),
			associationString(path+`\Application`, "ApplicationName", o.Name),
			associationString(path+`\Application`, "ApplicationIcon", icon),
			associationString(path+`\DefaultIcon`, "", icon),
			associationString(path+`\shell\open\command`, "", command))
		for _, extension := range kind.Extensions {
			values = append(values, associationString(capability+`\FileAssociations`, extension, id),
				associationValue{path: `Software\Classes\` + extension + `\OpenWithProgids`, name: id, kind: registry.NONE, exists: true})
		}
	}
	return values
}

// associationString 编码字符串注册表值
// 入参: path 键路径, name 值名, value 字符串
// 返回: associationValue 字符串值
func associationString(path, name, value string) associationValue {
	text := utf16.Encode([]rune(value + "\x00"))
	data := make([]byte, len(text)*2)
	for i, c := range text {
		binary.LittleEndian.PutUint16(data[i*2:], c)
	}
	return associationValue{path: path, name: name, kind: registry.SZ, data: data, exists: true}
}

// readAssociationValue 读取值并区分尚未登记与访问失败
// 入参: root 注册表根, path 键路径, name 值名
// 返回: associationValue 当前值, error 查询错误
func readAssociationValue(root registry.Key, path, name string) (associationValue, error) {
	value := associationValue{path: path, name: name}
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	defer key.Close()
	size, _, err := key.GetValue(name, nil)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	for range 5 {
		buf := make([]byte, size)
		n, kind, err := key.GetValue(name, buf)
		if errors.Is(err, registry.ErrShortBuffer) || errors.Is(err, windows.ERROR_MORE_DATA) {
			size = n
			continue
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return value, nil
		}
		if err != nil {
			return value, err
		}
		if n > len(buf) {
			size = n
			continue
		}
		value.kind, value.data, value.exists = kind, buf[:n], true
		return value, nil
	}
	return value, windows.ERROR_MORE_DATA
}

// writeAssociationValue 写入或删除指定键中的单个值
// 入参: root 注册表根, value 值内容
// 返回: error 写入错误
func writeAssociationValue(root registry.Key, value associationValue) error {
	key, err := registry.OpenKey(root, value.path, registry.SET_VALUE|registry.WOW64_64KEY)
	if !value.exists && errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	defer key.Close()
	if !value.exists {
		err = key.DeleteValue(value.name)
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		return err
	}
	name, err := windows.UTF16PtrFromString(value.name)
	if err != nil {
		return err
	}
	var data *byte
	if len(value.data) != 0 {
		data = &value.data[0]
	}
	result, _, _ := associationSetValue.Call(uintptr(key), uintptr(unsafe.Pointer(name)), 0, uintptr(value.kind), uintptr(unsafe.Pointer(data)), uintptr(len(value.data)))
	if result != 0 {
		return windows.Errno(result)
	}
	return nil
}

// associationKeyPaths 列出登记涉及的键，排除系统公共根
// 入参: values 登记内容
// 返回: []string 由浅至深的键路径
func associationKeyPaths(values []associationValue) []string {
	found := make(map[string]bool)
	for _, value := range values {
		parts := strings.Split(value.path, `\`)
		for n := 2; n <= len(parts); n++ {
			found[strings.Join(parts[:n], `\`)] = true
		}
	}
	paths := make([]string, 0, len(found))
	for path := range found {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		a, b := strings.Count(paths[i], `\`), strings.Count(paths[j], `\`)
		if a == b {
			return paths[i] < paths[j]
		}
		return a < b
	})
	return paths
}

// removeAssociationEmptyKey 删除已经没有值和子键的父键
// 入参: root 注册表根, path 键路径
// 返回: error 删除错误
func removeAssociationEmptyKey(root registry.Key, path string) error {
	key, err := registry.OpenKey(root, path, registry.READ|registry.WOW64_64KEY)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := key.Stat()
	key.Close()
	if err != nil {
		return err
	}
	if info.SubKeyCount != 0 || info.ValueCount != 0 {
		return nil
	}
	err = deleteAssociationKey(root, path)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	return err
}

// removeAssociationTree 整树删除应用专用键及其全部内容
// 入参: root 注册表根, path 专用键路径
// 返回: error 删除错误
func removeAssociationTree(root registry.Key, path string) error {
	key, err := registry.OpenKey(root, path, registry.READ|registry.WOW64_64KEY)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := key.ReadSubKeyNames(-1)
	key.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := removeAssociationTree(root, path+`\`+name); err != nil {
			return err
		}
	}
	err = deleteAssociationKey(root, path)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	return err
}

// deleteAssociationKey 按统一视图删除一个没有子键的注册表键
// 入参: root 注册表根, path 键路径
// 返回: error 删除错误
func deleteAssociationKey(root registry.Key, path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	result, _, _ := associationDeleteKey.Call(uintptr(root), uintptr(unsafe.Pointer(name)), registry.WOW64_64KEY, 0)
	if result != 0 {
		return windows.Errno(result)
	}
	return nil
}

// readStoredAssociations 读取当前应用在Capabilities下已登记的扩展名与ProgID映射
// 入参: root 注册表根, o 应用配置
// 返回: map[string]string 已登记的扩展名到ProgID映射, error 查询错误
func readStoredAssociations(root registry.Key, o AssociationOptions) (map[string]string, error) {
	stored := make(map[string]string)
	key, err := registry.OpenKey(root, associationPath(o)+`\Capabilities\FileAssociations`, registry.READ|registry.WOW64_64KEY)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return stored, nil
	}
	if err != nil {
		return nil, err
	}
	defer key.Close()
	names, err := key.ReadValueNames(-1)
	if err != nil {
		return nil, err
	}
	prefix := o.AppID + "."
	for _, ext := range names {
		if !strings.HasPrefix(ext, ".") || strings.ContainsAny(ext, "\\\x00") {
			continue
		}
		progID, _, err := key.GetStringValue(ext)
		if err != nil {
			continue
		}
		if len(progID) > len(prefix) && strings.EqualFold(progID[:len(prefix)], prefix) && !strings.ContainsAny(progID, "\\\x00") {
			stored[ext] = progID
		}
	}
	return stored, nil
}

// queryFileAssociations 查询独立范围并报告当前进程是否允许写入
// 入参: o 应用配置, scope 作用范围
// 返回: AssociationStatus 登记状态, error 查询错误
func queryFileAssociations(o AssociationOptions, scope AssociationScope) (AssociationStatus, error) {
	associationMu.Lock()
	defer associationMu.Unlock()
	status, err := queryAssociation(associationRoot(scope), o)
	status.Writable = scope == AssociationUser || windows.GetCurrentProcessToken().IsElevated()
	return status, err
}

// queryAssociation 检查类型和能力登记是否完整且指向当前程序
// 入参: root 注册表根, o 应用配置
// 返回: AssociationStatus 登记状态, error 查询错误
func queryAssociation(root registry.Key, o AssociationOptions) (AssociationStatus, error) {
	status := AssociationStatus{Supported: true}
	complete := true
	for _, expected := range associationValues(o) {
		got, err := readAssociationValue(root, expected.path, expected.name)
		if err != nil {
			return status, err
		}
		status.Registered = status.Registered || got.exists
		complete = complete && got.exists && got.kind == expected.kind && bytes.Equal(got.data, expected.data)
	}
	expectedExt := make(map[string]string)
	for _, kind := range o.Types {
		id := o.AppID + "." + kind.ID
		for _, ext := range kind.Extensions {
			expectedExt[strings.ToLower(ext)] = id
		}
	}
	stored, err := readStoredAssociations(root, o)
	if err != nil {
		return status, err
	}
	for ext, progID := range stored {
		status.Registered = true
		if expected, ok := expectedExt[strings.ToLower(ext)]; !ok || !strings.EqualFold(expected, progID) {
			complete = false
		}
	}
	paths := []string{associationPath(o)}
	for _, kind := range o.Types {
		paths = append(paths, `Software\Classes\`+o.AppID+"."+kind.ID)
	}
	for _, path := range paths {
		key, err := registry.OpenKey(root, path, registry.READ|registry.WOW64_64KEY)
		if err == nil {
			status.Registered = true
			key.Close()
		} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return status, err
		}
	}
	status.Complete = complete
	return status, nil
}

// registerFileAssociations 登记当前范围，仅成功后通知系统刷新
// 入参: o 应用配置, scope 作用范围
// 返回: error 登记错误
func registerFileAssociations(o AssociationOptions, scope AssociationScope) error {
	associationMu.Lock()
	defer associationMu.Unlock()
	if scope == AssociationMachine && !windows.GetCurrentProcessToken().IsElevated() {
		return ErrAssociationElevationRequired
	}
	file, err := os.Stat(o.Executable)
	if err != nil {
		return err
	}
	if file.IsDir() {
		return errors.New("association executable is a directory")
	}
	if err := registerAssociation(associationRoot(scope), o); err != nil {
		return err
	}
	associationNotify.Call(0x08000000, 0, 0, 0)
	return nil
}

// registerAssociation 写入关联并在失败时恢复本次修改，成功后清理旧登记残留
// 入参: root 注册表根, o 应用配置
// 返回: error 登记或回滚错误
func registerAssociation(root registry.Key, o AssociationOptions) (result error) {
	oldStored, err := readStoredAssociations(root, o)
	if err != nil {
		return err
	}
	values := associationValues(o)
	var created []string
	var undo []associationValue
	defer func() {
		if result == nil {
			return
		}
		for i := len(undo) - 1; i >= 0; i-- {
			result = errors.Join(result, writeAssociationValue(root, undo[i]))
		}
		for i := len(created) - 1; i >= 0; i-- {
			result = errors.Join(result, removeAssociationEmptyKey(root, created[i]))
		}
	}()
	for _, path := range associationKeyPaths(values) {
		key, existing, err := registry.CreateKey(root, path, registry.READ|registry.WRITE|registry.WOW64_64KEY)
		if err != nil {
			return err
		}
		key.Close()
		if !existing {
			created = append(created, path)
		}
	}
	for _, value := range values {
		old, err := readAssociationValue(root, value.path, value.name)
		if err != nil {
			return err
		}
		if err = writeAssociationValue(root, value); err != nil {
			return err
		}
		undo = append(undo, old)
	}
	newExt := make(map[string]string)
	newProgIDs := make(map[string]bool)
	for _, kind := range o.Types {
		id := o.AppID + "." + kind.ID
		newProgIDs[strings.ToLower(id)] = true
		for _, ext := range kind.Extensions {
			newExt[strings.ToLower(ext)] = id
		}
	}
	capabilityExt := associationPath(o) + `\Capabilities\FileAssociations`
	staleProgIDs := make(map[string]string)
	for ext, oldProgID := range oldStored {
		lowerExt := strings.ToLower(ext)
		if newProgID, ok := newExt[lowerExt]; !ok {
			_ = writeAssociationValue(root, associationValue{path: capabilityExt, name: ext})
			openWith := `Software\Classes\` + ext + `\OpenWithProgids`
			_ = writeAssociationValue(root, associationValue{path: openWith, name: oldProgID})
			_ = removeAssociationEmptyKey(root, openWith)
		} else if !strings.EqualFold(newProgID, oldProgID) {
			openWith := `Software\Classes\` + ext + `\OpenWithProgids`
			_ = writeAssociationValue(root, associationValue{path: openWith, name: oldProgID})
		}
		if lowerOld := strings.ToLower(oldProgID); !newProgIDs[lowerOld] {
			staleProgIDs[lowerOld] = oldProgID
		}
	}
	for _, oldProgID := range staleProgIDs {
		_ = removeAssociationTree(root, `Software\Classes\`+oldProgID)
	}
	return nil
}

// unregisterFileAssociations 撤销当前范围，仅成功后通知系统刷新
// 入参: o 应用配置, scope 作用范围
// 返回: error 撤销错误
func unregisterFileAssociations(o AssociationOptions, scope AssociationScope) error {
	associationMu.Lock()
	defer associationMu.Unlock()
	if scope == AssociationMachine && !windows.GetCurrentProcessToken().IsElevated() {
		return ErrAssociationElevationRequired
	}
	if err := unregisterAssociation(associationRoot(scope), o); err != nil {
		return err
	}
	associationNotify.Call(0x08000000, 0, 0, 0)
	return nil
}

// unregisterAssociation 整树删除专用键及旧历史ProgID，公共键仅删除本应用的值并清理空的OpenWithProgids
// 入参: root 注册表根, o 应用配置
// 返回: error 撤销错误
func unregisterAssociation(root registry.Key, o AssociationOptions) error {
	var result error
	oldStored, err := readStoredAssociations(root, o)
	if err != nil {
		result = errors.Join(result, err)
	}
	result = errors.Join(result, writeAssociationValue(root, associationValue{path: `Software\RegisteredApplications`, name: o.AppID}))
	progIDs := make(map[string]string)
	for ext, oldProgID := range oldStored {
		openWith := `Software\Classes\` + ext + `\OpenWithProgids`
		result = errors.Join(result, writeAssociationValue(root, associationValue{path: openWith, name: oldProgID}))
		result = errors.Join(result, removeAssociationEmptyKey(root, openWith))
		progIDs[strings.ToLower(oldProgID)] = oldProgID
	}
	for _, kind := range o.Types {
		id := o.AppID + "." + kind.ID
		progIDs[strings.ToLower(id)] = id
		for _, extension := range kind.Extensions {
			openWith := `Software\Classes\` + extension + `\OpenWithProgids`
			result = errors.Join(result, writeAssociationValue(root, associationValue{path: openWith, name: id}))
			result = errors.Join(result, removeAssociationEmptyKey(root, openWith))
		}
	}
	for _, id := range progIDs {
		result = errors.Join(result, removeAssociationTree(root, `Software\Classes\`+id))
	}
	return errors.Join(result, removeAssociationTree(root, associationPath(o)))
}
