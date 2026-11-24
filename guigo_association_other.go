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

// queryFileAssociations 返回未适配平台的关联能力
// 入参: o 应用配置, scope 作用范围
// 返回: AssociationStatus 不支持的状态, error 空错误
func queryFileAssociations(o AssociationOptions, scope AssociationScope) (AssociationStatus, error) {
	return AssociationStatus{}, nil
}

// registerFileAssociations 返回未适配平台的登记错误
// 入参: o 应用配置, scope 作用范围
// 返回: error 平台错误
func registerFileAssociations(o AssociationOptions, scope AssociationScope) error {
	return ErrUnsupportedPlatform
}

// unregisterFileAssociations 返回未适配平台的撤销错误
// 入参: o 应用配置, scope 作用范围
// 返回: error 平台错误
func unregisterFileAssociations(o AssociationOptions, scope AssociationScope) error {
	return ErrUnsupportedPlatform
}
