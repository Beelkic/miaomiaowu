package handler

import "testing"

// mierus:// 官方分享链接直接交给 proxyparser 解析(#115 的兼容层已移除),
// 端口 / 凭据 / 传输协议之外,handshake-mode 和 udp 也要保留(#130)。
func TestParseMieruShareURI(t *testing.T) {
	raw := "mierus://r2RYIiZSxS:GuoNTOfndr@116.126.122.66?handshake-mode=HANDSHAKE_NO_WAIT&mtu=1400&multiplexing=MULTIPLEXING_OFF&port=11211&profile=default&protocol=TCP"

	node, err := ParseProxyURL(raw)
	if err != nil {
		t.Fatalf("解析 mierus:// 失败: %v", err)
	}
	for key, want := range map[string]any{
		"type":           "mieru",
		"server":         "116.126.122.66",
		"port":           11211,
		"username":       "r2RYIiZSxS",
		"password":       "GuoNTOfndr",
		"transport":      "TCP",
		"multiplexing":   "MULTIPLEXING_OFF",
		"mtu":            1400,
		"handshake-mode": "HANDSHAKE_NO_WAIT",
		"udp":            true,
	} {
		if got := node[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
}

// 多行明文订阅里的 mierus:// 同样走原生解析。
func TestParseV2raySubscriptionWithMierus(t *testing.T) {
	content := "mierus://alice:s3cret@example.com?port=2999&profile=home&protocol=UDP\nss://YWVzLTEyOC1nY206dGVzdA@1.2.3.4:8388#ss\n"
	nodes, err := ParseV2raySubscription(content)
	if err != nil {
		t.Fatalf("解析订阅失败: %v", err)
	}
	var mieru map[string]any
	for _, n := range nodes {
		if n["type"] == "mieru" {
			mieru = n
		}
	}
	if mieru == nil {
		t.Fatalf("订阅里的 mierus:// 没有解析出来: %v", nodes)
	}
	if mieru["port"] != 2999 || mieru["transport"] != "UDP" || mieru["name"] != "home" {
		t.Errorf("mieru 节点字段不对: %#v", mieru)
	}
}
