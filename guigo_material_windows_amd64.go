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
	"unsafe"

	"golang.org/x/sys/windows"
)

// SupportsWindowMaterial 查询当前平台是否提供指定窗口材质，实际效果由系统外观设置决定
// 入参: material 窗口材质
// 返回: bool 是否支持
func SupportsWindowMaterial(material WindowMaterial) bool {
	return material == WindowSolid || (material == WindowMicaAlt || material == WindowAcrylic) && windows.RtlGetVersion().BuildNumber >= 22621
}

// setWindowMaterial 在宿主线程切换材质，失败时恢复当前窗口外观
// 入参: material 已校验的窗口材质
// 返回: error 系统材质或WebView错误
func (n *nativeWindow) setWindowMaterial(material WindowMaterial) error {
	return n.invoke(func() error {
		if n.options.WindowMaterial == material {
			return nil
		}
		previous := n.options.WindowMaterial
		n.options.WindowMaterial = material
		if err := n.applyWindowMaterial(); err != nil {
			n.options.WindowMaterial = previous
			return errors.Join(err, n.applyWindowMaterial())
		}
		n.publishTitleBarChange(false)
		return nil
	})
}

// applyWindowBackdrop 设置当前窗口的DWM背景材质
// 返回: error 系统材质设置错误
func (n *nativeWindow) applyWindowBackdrop() error {
	material := uint32(1)
	switch n.options.WindowMaterial {
	case WindowMicaAlt:
		material = 4
	case WindowAcrylic:
		material = 3
	}
	hr, _, _ := dwmapi.NewProc("DwmSetWindowAttribute").Call(n.hwnd, 38, uintptr(unsafe.Pointer(&material)), unsafe.Sizeof(material))
	return hresult(hr)
}

// applyWindowMaterial 同步窗口材质、系统配色及WebView底色
// 返回: error 系统或WebView设置错误
func (n *nativeWindow) applyWindowMaterial() error {
	if err := n.applyWindowBackdrop(); err != nil {
		return err
	}
	if err := n.extendWindowFrame(); err != nil {
		return err
	}
	if err := n.applyWindowBackground(); err != nil {
		return err
	}
	return n.applyTitleBarColors(n.currentTitleBarInfo().Active)
}

// initializeWindowMaterial 设置WebView透明底色，使页面透明区域透出宿主的系统材质
// 返回: error WebView接口或背景设置错误
func (n *nativeWindow) initializeWindowMaterial() error {
	if n.options.WindowMaterial == WindowSolid {
		return nil
	}
	return n.applyWindowMaterial()
}

// applyWindowBackground 根据当前材质设置WebView底色
// 返回: error WebView接口或背景设置错误
func (n *nativeWindow) applyWindowBackground() error {
	iid := guid("{C979903E-D4CA-4228-92EB-47EE3FA96EAB}")
	var controller *comObject
	if err := hresult(com(n.controller, 0, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&controller)))); err != nil {
		return err
	}
	defer release(controller)
	color := uintptr(0)
	if n.options.WindowMaterial == WindowSolid {
		color = 0xffffffff
	}
	return hresult(com(controller, 27, color))
}

// extendWindowFrame 将系统材质延伸到客户区，纯色模式恢复原有标题栏范围
// 返回: error 系统边框设置错误
func (n *nativeWindow) extendWindowFrame() error {
	var margins [4]int32
	if n.options.WindowMaterial != WindowSolid {
		margins = [4]int32{-1, -1, -1, -1}
	} else if n.options.TitleBar == TitleBarContent {
		margins[2] = n.titleBarMetric(4) + n.titleBarBorder() + n.titleBarOffset()
	}
	hr, _, _ := dwmapi.NewProc("DwmExtendFrameIntoClientArea").Call(n.hwnd, uintptr(unsafe.Pointer(&margins)))
	return hresult(hr)
}
