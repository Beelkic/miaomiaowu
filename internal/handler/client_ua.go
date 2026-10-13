package handler

import (
	"net/http"
	"strings"
)

// 订阅 auto 模式:`?t=auto` 时按 User-Agent 推断客户端,下发对应格式。推断不出来时返回空串,
// 调用方按「不带 t」处理 → Clash YAML。这张表同时是「拦截未知订阅 UA」的白名单
// (subscription_ua_guard.go),漏掉一个客户端 = 开了拦截就把它的用户整个 403 挡在门外。
type clientUARule struct {
	// keywords 任一命中即算匹配(全部转小写后比对)
	keywords []string
	client   string
}

// clientUARules 的顺序即优先级,靠前的先匹配:
//   - Stash 的 UA 形如 `Stash/2.5.0 Clash/1.9.0`,含 "clash" 子串 → 必须排在 Clash 之前
//   - Surge Mac 的 UA 含 "surge" → 必须排在 Surge 之前
var clientUARules = []clientUARule{
	{[]string{"stash"}, "stash"},
	{[]string{"shadowrocket"}, "shadowrocket"},
	{[]string{"surge mac", "surgemac"}, "surgemac"},
	{[]string{"surge"}, "surge"},
	{[]string{"loon"}, "loon"},
	{[]string{"quantumult%20x", "quantumult x", "quantumultx"}, "qx"},
	{[]string{"egern"}, "egern"},
	{[]string{"surfboard"}, "surfboard"},
	{[]string{"sing-box", "sfi/", "sfa/", "sfm/", "sft/"}, "sing-box"},
	// 裸 "v2ray" 是这一族的兜底:子串匹配,同时覆盖 v2rayN / v2rayNG / v2rayU / v2rayA / v2rayW,
	// 以及只报 `v2ray/x.y.z` 的客户端。以前只列了 v2rayn / v2rayng,列表之外的推断不出格式,
	// 落到最后的 clash 兜底,v2ray 客户端拿到一份 clash YAML。v2box 不含 "v2ray",单独留着。
	{[]string{"v2ray", "v2box"}, "v2ray"},
	// Happ(Android / 桌面),UA 形如 `Happ/3.26.1`:归到 base64 URI 列表,那是兼容面最广的格式
	{[]string{"happ/"}, "v2ray"},
	// Clash 系放最后兜底:mihomo / Clash.Meta / clash-verge / ClashforWindows 都归到这里
	{[]string{"mihomo", "clash"}, "clash"},
}

func detectClientTypeFromUA(ua string) string {
	ua = strings.ToLower(strings.TrimSpace(ua))
	for _, rule := range clientUARules {
		for _, keyword := range rule.keywords {
			if strings.Contains(ua, keyword) {
				return rule.client
			}
		}
	}
	return ""
}

// resolveClientType translates ?t=auto to the format implied by User-Agent.
// Unknown clients intentionally fall back to the default Clash YAML output.
func resolveClientType(r *http.Request) string {
	clientType := strings.TrimSpace(r.URL.Query().Get("t"))
	// MeowX(官方客户端)吃的就是 Clash YAML,后面一律按 clash 走
	if strings.EqualFold(clientType, meowXClientType) {
		return "clash"
	}
	if !strings.EqualFold(clientType, "auto") {
		return clientType
	}
	return detectClientTypeFromUA(r.Header.Get("User-Agent"))
}

// meowXClientType 订阅地址里显式选的客户端类型「MeowX」(?t=meowx),产出与 ?t=clash 相同。
const meowXClientType = "meowx"
