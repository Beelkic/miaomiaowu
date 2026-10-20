package handler

import "github.com/MMWOrg/mmwX-plugins/proxyparser"

// 节点 URI 解析已统一抽取到共享 module github.com/MMWOrg/mmwX-plugins/proxyparser,
// 供 miaomiaowu 与 miaomiaowux 共用。本文件仅保留薄包装,维持原有调用方签名不变。
//
// 以前这里有一层 mierus:// → mieru:// 的兼容改写(#115),只保留 protocol / multiplexing / mtu
// 三个参数,会把 handshake-mode、traffic-pattern、端口范围、udp 全部丢掉(#130)。
// proxyparser 已原生支持 mierus://(端口在 query、端口范围、profile 作名字),兼容层已移除。

// ParseProxyURL 解析单个代理 URI,返回 clash 风格字段的 map。
func ParseProxyURL(uri string) (map[string]any, error) {
	return proxyparser.Parse(uri)
}

// ParseV2raySubscription 解析 v2ray 订阅内容(base64 或明文多行 URI)。
func ParseV2raySubscription(content string) ([]map[string]any, error) {
	return proxyparser.ParseSubscription(content)
}
