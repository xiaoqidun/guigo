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
	"fmt"
	"unsafe"
)

// fileDrop 读取WebView2提供的原生文件路径
// 入参: args 拖放消息参数
func (n *nativeWindow) fileDrop(args *comObject) {
	if n.options.OnDrop == nil {
		return
	}
	iid := guid("{06FC7AB7-C90C-4297-9389-33CA01CF6D5E}")
	var extended *comObject
	if hresult(com(args, 0, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&extended)))) != nil || extended == nil {
		return
	}
	defer release(extended)
	var objects *comObject
	if hresult(com(extended, 6, uintptr(unsafe.Pointer(&objects)))) != nil || objects == nil {
		return
	}
	defer release(objects)
	var count uint32
	if hresult(com(objects, 3, uintptr(unsafe.Pointer(&count)))) != nil {
		return
	}
	paths := make([]string, 0, count)
	iid = guid("{F2C19559-6BC1-4583-A757-90021BE9AFEC}")
	for i := uint32(0); i < count; i++ {
		var object, file *comObject
		if hresult(com(objects, 4, uintptr(i), uintptr(unsafe.Pointer(&object)))) != nil || object == nil {
			continue
		}
		if hresult(com(object, 0, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&file)))) == nil && file != nil {
			path, err := comString(file, 3)
			if err == nil && path != "" {
				paths = append(paths, path)
			}
			release(file)
		}
		release(object)
	}
	if len(paths) > 0 {
		go func() {
			defer func() {
				if v := recover(); v != nil {
					n.reportError(fmt.Errorf("OnDrop panic: %v", v))
				}
			}()
			n.options.OnDrop(paths)
		}()
	}
}
