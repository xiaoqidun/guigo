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
	"net/http"
	"net/url"
	"path"
	"strings"
)

// fileHandler 标识由宿主选项自动创建的静态文件处理器
type fileHandler struct{ http.Handler }

// isReservedPath 判断请求路径是否命中宿主保留前缀/__guigo/
// 入参: raw 原始路径
// 返回: bool 是否属于保留前缀
func isReservedPath(raw string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(raw, "/"))
	return clean == "/__guigo" || strings.HasPrefix(clean, "/__guigo/")
}

// normalizedPort 归一化常见协议的默认端口，便于精确比较同源地址
// 入参: scheme 协议名称, port 原始端口字符串
// 返回: string 归一化后的端口
func normalizedPort(scheme, port string) string {
	if port != "" {
		return port
	}
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	if strings.EqualFold(scheme, "http") {
		return "80"
	}
	return ""
}

// localResource 判断资源是否属于指定宿主来源，忽略协议和主机名大小写并归一化默认端口
// 入参: origin 宿主来源, raw 资源地址
// 返回: bool 是否属于宿主来源
func localResource(origin, raw string) bool {
	base, err := url.Parse(origin)
	if err != nil || base.Scheme == "" || base.Hostname() == "" || base.User != nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" {
		return false
	}
	return strings.EqualFold(u.Scheme, base.Scheme) &&
		strings.EqualFold(u.Hostname(), base.Hostname()) &&
		normalizedPort(u.Scheme, u.Port()) == normalizedPort(base.Scheme, base.Port())
}

// externalURL 限制外部浏览器可接受的地址，过滤空白、引号及控制字符
// 入参: raw 外部地址
// 返回: bool 是否允许打开
func externalURL(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\x00\"'<>`") {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] == 0x7f {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// resourceAddress 将资源路径限制在指定宿主来源
// 入参: origin 宿主来源, path 资源路径
// 返回: string 宿主资源地址
func resourceAddress(origin, path string) string {
	base, err := url.Parse(origin)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil {
		return ""
	}
	u, err := url.Parse(path)
	if err != nil {
		return origin + "/"
	}
	u.Scheme = base.Scheme
	u.Host = base.Host
	u.User = nil
	u.Opaque = ""
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	return u.String()
}
