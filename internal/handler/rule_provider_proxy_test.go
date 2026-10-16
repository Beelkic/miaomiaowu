package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRuleProviderProxiedURL(t *testing.T) {
	raw := "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/telegram.mrs"
	cases := []struct{ server, target, want string }{
		{"https://cdn.jsdelivr.net/", raw, "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geoip/telegram.mrs"},
		{"fastly.jsdelivr.net", raw, "https://fastly.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geoip/telegram.mrs"},
		{"https://gh-proxy.com", raw, "https://gh-proxy.com/" + raw},
		{"https://ghfast.top/", "https://github.com/a/b/raw/main/x/y.yaml", "https://ghfast.top/https://github.com/a/b/raw/main/x/y.yaml"},
		// jsDelivr 认 github.com 的 raw / blob 写法,也认 refs/heads/<分支>
		{"https://cdn.jsdelivr.net/", "https://github.com/a/b/blob/main/x/y.yaml", "https://cdn.jsdelivr.net/gh/a/b@main/x/y.yaml"},
		{"https://cdn.jsdelivr.net/", "https://raw.githubusercontent.com/a/b/refs/heads/dev/x.list", "https://cdn.jsdelivr.net/gh/a/b@dev/x.list"},
		// 不是 GitHub 的地址不改写
		{"https://gh-proxy.com/", "https://example.com/rules.yaml", ""},
		// jsDelivr 取不了仓库文件以外的东西
		{"https://cdn.jsdelivr.net/", "https://gist.githubusercontent.com/u/abc/raw/x.yaml", ""},
		{"not a url ://", raw, ""},
	}
	for _, c := range cases {
		if got := ruleProviderProxiedURL(c.server, c.target); got != c.want {
			t.Errorf("%s + %s\n  got  %q\n  want %q", c.server, c.target, got, c.want)
		}
	}
	if normalizeProxyServer("ftp://x.com") != "" || normalizeProxyServer(" https://a.b/c/ ") != "https://a.b/c/" {
		t.Fatal("加速站地址的归一化不对")
	}
}

func TestFetchRuleProviderViaProxy(t *testing.T) {
	ruleProviderProxyLastGood.Store("")
	gh := "https://raw.githubusercontent.com/a/b/main/r.yaml"
	var calls []string
	fetch := func(_ context.Context, u string, _ time.Duration) (string, error) {
		calls = append(calls, u)
		switch {
		case strings.HasPrefix(u, "https://fastly.jsdelivr.net/"):
			return "", errors.New("timeout")
		case strings.HasPrefix(u, "https://testingcf.jsdelivr.net/"):
			return "<!DOCTYPE html><html>blocked</html>", nil // 200 但回的是网页
		case strings.HasPrefix(u, "https://cdn.jsdelivr.net/"):
			return "payload:\n  - DOMAIN,a.com\n", nil
		case u == gh:
			return "direct", nil
		}
		return "", errors.New("unexpected " + u)
	}
	// 没开:直连
	if got, err := fetchRuleProviderVia(context.Background(), RuleProviderProxyConfig{}, gh, fetch); err != nil || got != "direct" || len(calls) != 1 {
		t.Fatalf("没开代理应直连: %q %v %v", got, err, calls)
	}
	// 自动:依次试,跳过超时的与回网页的,用第三个,并记住它
	calls = nil
	got, err := fetchRuleProviderVia(context.Background(), RuleProviderProxyConfig{Enabled: true}, gh, fetch)
	if err != nil || !strings.HasPrefix(got, "payload:") || len(calls) != 3 {
		t.Fatalf("自动模式: %q %v %v", got, err, calls)
	}
	calls = nil
	if _, err := fetchRuleProviderVia(context.Background(), RuleProviderProxyConfig{Enabled: true}, gh, fetch); err != nil || len(calls) != 1 || !strings.HasPrefix(calls[0], "https://cdn.jsdelivr.net/") {
		t.Fatalf("下一次应先试上次成功的那个: %v %v", err, calls)
	}
	// 指定的加速站不通:退回直连
	calls = nil
	got, err = fetchRuleProviderVia(context.Background(), RuleProviderProxyConfig{Enabled: true, Server: "https://fastly.jsdelivr.net/"}, gh, fetch)
	if err != nil || got != "direct" || len(calls) != 2 {
		t.Fatalf("加速站不通应退回直连: %q %v %v", got, err, calls)
	}
	// 不是 GitHub 的地址:开了也直连,不碰加速站
	calls = nil
	other := func(_ context.Context, u string, _ time.Duration) (string, error) {
		calls = append(calls, u)
		return "x", nil
	}
	if _, err := fetchRuleProviderVia(context.Background(), RuleProviderProxyConfig{Enabled: true}, "https://example.com/r.yaml", other); err != nil || len(calls) != 1 || calls[0] != "https://example.com/r.yaml" {
		t.Fatalf("非 GitHub 地址应直连: %v %v", err, calls)
	}
	ruleProviderProxyLastGood.Store("")
}
