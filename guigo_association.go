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
	"path/filepath"
	"strings"
)

const (
	// AssociationUser 仅登记当前用户的文件关联
	AssociationUser AssociationScope = "user"
	// AssociationMachine 登记所有用户可用的文件关联，需要管理员权限
	AssociationMachine AssociationScope = "machine"
)

// ErrAssociationElevationRequired 表示所有用户范围需要以管理员权限运行
var ErrAssociationElevationRequired = errors.New("administrator privileges required")

// AssociationScope 指定独立管理的文件关联范围
type AssociationScope string

// FileAssociation 定义应用支持的文件类型，ID在应用内唯一，Extensions为不重复的小写带点扩展名
type FileAssociation struct {
	ID         string
	Name       string
	Extensions []string
}

// AssociationOptions 定义应用专用文件关联资源，由调用方保持标识和类型集合稳定
// Executable为程序绝对路径，Arguments为文件路径前的固定参数，撤销会整树删除专用键
type AssociationOptions struct {
	AppID       string
	Name        string
	Description string
	Executable  string
	Arguments   []string
	IconIndex   int
	Types       []FileAssociation
}

// AssociationStatus 描述指定范围内的登记状态，不代表用户已选择为默认应用
// Supported表示平台支持，Registered表示存在登记，Complete表示与当前配置一致，Writable表示当前权限可写
type AssociationStatus struct {
	Supported  bool `json:"supported"`
	Registered bool `json:"registered"`
	Complete   bool `json:"complete"`
	Writable   bool `json:"writable"`
}

// QueryFileAssociations 查询指定范围的登记状态，不修改注册表
// 入参: o 应用配置, scope 作用范围
// 返回: AssociationStatus 登记状态, error 查询错误
func QueryFileAssociations(o AssociationOptions, scope AssociationScope) (AssociationStatus, error) {
	if err := validateAssociation(o, scope); err != nil {
		return AssociationStatus{}, err
	}
	return queryFileAssociations(o, scope)
}

// RegisterFileAssociations 登记指定范围的打开方式，不修改默认应用或自动提权
// 入参: o 应用配置, scope 作用范围
// 返回: error 登记错误
func RegisterFileAssociations(o AssociationOptions, scope AssociationScope) error {
	if err := validateAssociation(o, scope); err != nil {
		return err
	}
	return registerFileAssociations(o, scope)
}

// UnregisterFileAssociations 删除指定范围的专用键及公共键中的应用值，不自动提权
// 入参: o 应用配置, scope 作用范围
// 返回: error 撤销错误
func UnregisterFileAssociations(o AssociationOptions, scope AssociationScope) error {
	if err := validateAssociation(o, scope); err != nil {
		return err
	}
	return unregisterFileAssociations(o, scope)
}

// validateAssociation 校验关联标识、扩展名和启动参数
// 入参: o 应用配置, scope 作用范围
// 返回: error 配置错误
func validateAssociation(o AssociationOptions, scope AssociationScope) error {
	if scope != AssociationUser && scope != AssociationMachine {
		return errors.New("invalid association scope")
	}
	if validateAppID(o.AppID) != nil || !filepath.IsAbs(o.Executable) || strings.ContainsAny(o.Executable, "\x00\r\n\"%") || strings.HasSuffix(o.Executable, `\`) || o.Name == "" || o.Description == "" || len(o.Types) == 0 {
		return errors.New("invalid association options")
	}
	for _, value := range []string{o.Name, o.Description} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("association text contains invalid control characters")
		}
	}
	for _, value := range o.Arguments {
		if strings.ContainsAny(value, "\x00\r\n%") {
			return errors.New("association argument contains invalid characters")
		}
	}
	ids, extensions := make(map[string]bool), make(map[string]bool)
	for _, kind := range o.Types {
		id := strings.ToLower(kind.ID)
		if validateAppID(kind.ID) != nil || strings.Contains(kind.ID, ".") || kind.Name == "" || strings.ContainsAny(kind.Name, "\x00\r\n") || len(kind.Extensions) == 0 || ids[id] {
			return errors.New("invalid association file type")
		}
		ids[id] = true
		for _, extension := range kind.Extensions {
			if len(extension) < 2 || extension[0] != '.' {
				return errors.New("invalid association extension")
			}
			for _, c := range extension[1:] {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
					return errors.New("invalid association extension")
				}
			}
			if extensions[extension] {
				return errors.New("duplicate association extension")
			}
			extensions[extension] = true
		}
	}
	return nil
}
