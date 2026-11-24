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

import "context"

// instanceAddress 返回未适配平台错误
// 入参: key 调用方实例键
// 返回: string 空地址, error 平台错误
func instanceAddress(key string) (string, error) {
	return "", ErrUnsupportedPlatform
}

// listenInstance 返回未适配平台错误
// 入参: address 入口地址
// 返回: instanceListener 空入口, error 平台错误
func listenInstance(address string) (instanceListener, error) {
	return nil, ErrUnsupportedPlatform
}

// dialInstance 返回未适配平台错误
// 入参: ctx 连接上下文, address 入口地址
// 返回: instanceConn 空连接, error 平台错误
func dialInstance(ctx context.Context, address string) (instanceConn, error) {
	return nil, ErrUnsupportedPlatform
}

// instanceUnavailable 表示平台错误不可重试
// 入参: err 连接错误
// 返回: bool 不可重试
func instanceUnavailable(err error) bool {
	return false
}
