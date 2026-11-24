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

//go:build !windows || !amd64

package guigo

import "context"

// openWindowInstance 在其他平台沿用当前会话的单实例复用范围
// 入参: ctx 生命周期上下文, options 归一化实例配置, receive 接收回调
// 返回: *Instance 实例入口或nil, error 创建或转交错误
func openWindowInstance(ctx context.Context, options InstanceOptions, receive func(context.Context, []byte) error) (*Instance, error) {
	return openSessionInstance(ctx, options, receive)
}
