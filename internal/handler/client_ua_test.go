package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// ?t=meowx(官方客户端 MeowX)按 clash 输出;其它取值照旧。
func TestResolveClientTypeMeowX(t *testing.T) {
	cases := map[string]string{"meowx": "clash", "MeowX": "clash", " MEOWX ": "clash", "clash": "clash", "surge": "surge", "": ""}
	for raw, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/clash/subscribe?t="+url.QueryEscape(raw), nil)
		if got := resolveClientType(r); got != want {
			t.Errorf("t=%q: got %q, want %q", raw, got, want)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/clash/subscribe?t=auto", nil)
	r.Header.Set("User-Agent", "Stash/2.5.0 Clash/1.9.0")
	if got := resolveClientType(r); got != "stash" {
		t.Errorf("t=auto 仍按 UA 推断: got %q", got)
	}
}

// UA → 客户端类型。顺序敏感的几条(Stash 含 clash、Surge Mac 含 surge)和后补的 v2ray 兜底、Happ 都钉在这里。
func TestDetectClientTypeFromUAFamilies(t *testing.T) {
	cases := map[string]string{
		"Stash/2.5.0 Clash/1.9.0":             "stash",
		"Surge Mac/2985":                      "surgemac",
		"Surge iOS/3272":                      "surge",
		"Shadowrocket/2070 CFNetwork/1410":    "shadowrocket",
		"Loon/762 CFNetwork/1410":             "loon",
		"Quantumult%20X/1.4.1 (iPhone; iOS)":  "qx",
		"SFA/1.9.3 (sing-box 1.9.3)":          "sing-box",
		"v2rayN/6.42":                         "v2ray",
		"v2rayNG/1.8.19":                      "v2ray",
		"V2rayU/4.2.6":                        "v2ray",
		"v2rayA/2.2.5":                        "v2ray",
		"v2ray/5.16.1":                        "v2ray",
		"Happ/3.26.1":                         "v2ray",
		"clash-verge/v2.0.0":                  "clash",
		"mihomo/1.19.0 MeowX/0.4.2 (Android)": "clash",
		"curl/8.0":                            "",
		"":                                    "",
	}
	for ua, want := range cases {
		if got := detectClientTypeFromUA(ua); got != want {
			t.Errorf("UA %q: got %q, want %q", ua, got, want)
		}
	}
}
