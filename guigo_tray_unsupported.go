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

//go:build !(windows && amd64) && !(linux && !android && (amd64 || arm64))

package guigo

// newTray 返回未适配平台错误，托盘与窗口宿主的支持范围分别维护
// 入参: options 托盘配置
// 返回: *Tray 空句柄, error 平台错误
func (n *nativeWindow) newTray(options TrayOptions) (*Tray, error) {
	return nil, ErrUnsupportedPlatform
}
