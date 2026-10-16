package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"miaomiaowu/internal/storage"
)

// 规则集抓取的 GitHub 加速(iluobei/miaomiaowu#125)。
//
// 主控在境内时,规则集最常见的来源 raw.githubusercontent.com 经常连不上或超时。这里给规则集抓取
// 加一个可选的「代理」:开了之后,GitHub 上的地址改从加速站取。加速站有两种写法 ——
//
//	jsDelivr 系:  https://cdn.jsdelivr.net/gh/<用户>/<仓库>@<分支>/<路径>
//	前缀转发系:   https://gh-proxy.com/<原地址>
//
// 「自动」= 按清单逐个试,用第一个取得到的,并记住它(下次先试它)。也可以固定用某一个,或自己填。
// 只对 GitHub 的地址生效;别的来源照常直连。全都取不到时再直连试一次。
//
// 加速站是第三方:内容经它们转一手。这是使用者自己选的取舍,所以默认关闭。

const ruleProviderProxyKey = "rule_provider_proxy"

// ruleProviderProxyPresets 内置的加速站。顺序就是「自动」时的尝试顺序。
var ruleProviderProxyPresets = []string{
	"https://fastly.jsdelivr.net/",
	"https://testingcf.jsdelivr.net/",
	"https://cdn.jsdelivr.net/",
	"https://ghfast.top/",
	"https://git.yylx.win/",
	"https://cdn.akaere.online/",
	"https://gh-proxy.com/",
	"https://gh.jasonzeng.dev/",
	"https://ghproxy.monkeyray.net/",
	"https://down.mxw.xx.kg/",
	"https://github.tbap.top/",
	"https://ghm.078465.xyz/",
	"https://gh.monlor.com/",
	"https://gh.idayer.com/",
}

// ruleProviderProxyTestURL 检测加速站可用性时取的文件:小、稳定、内容是纯文本。
const ruleProviderProxyTestURL = "https://raw.githubusercontent.com/github/gitignore/main/Go.gitignore"

// RuleProviderProxyConfig Server 为空 = 自动。
type RuleProviderProxyConfig struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
}

// ruleProviderProxyLastGood 「自动」模式下上一次取成功的加速站。
var ruleProviderProxyLastGood atomic.Value // string

func loadRuleProviderProxyConfig(ctx context.Context, repo *storage.TrafficRepository) RuleProviderProxyConfig {
	var cfg RuleProviderProxyConfig
	if repo == nil {
		return cfg
	}
	if raw, _ := repo.GetSystemSetting(ctx, ruleProviderProxyKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	cfg.Server = normalizeProxyServer(cfg.Server)
	return cfg
}

// normalizeProxyServer 统一成「https://host/」的形状;不合法的返回空。
func normalizeProxyServer(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	path := strings.TrimRight(u.Path, "/")
	return u.Scheme + "://" + u.Host + path + "/"
}

// githubFileRef 从 GitHub 的文件地址里拆出 用户 / 仓库 / 分支 / 路径。认两种写法:
//
//	https://raw.githubusercontent.com/<用户>/<仓库>/<分支>/<路径>      (分支可写成 refs/heads/<分支>)
//	https://github.com/<用户>/<仓库>/raw/<分支>/<路径>                 (也认 /blob/)
func githubFileRef(target string) (user, repo, branch, path string, ok bool) {
	u, err := url.Parse(target)
	if err != nil {
		return
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch strings.ToLower(u.Host) {
	case "raw.githubusercontent.com":
		if len(parts) >= 3 && parts[2] == "refs" && len(parts) >= 6 && parts[3] == "heads" {
			return parts[0], parts[1], parts[4], strings.Join(parts[5:], "/"), true
		}
		if len(parts) >= 4 {
			return parts[0], parts[1], parts[2], strings.Join(parts[3:], "/"), true
		}
	case "github.com":
		if len(parts) >= 5 && (parts[2] == "raw" || parts[2] == "blob") {
			if parts[3] == "refs" && len(parts) >= 7 && parts[4] == "heads" {
				return parts[0], parts[1], parts[5], strings.Join(parts[6:], "/"), true
			}
			return parts[0], parts[1], parts[3], strings.Join(parts[4:], "/"), true
		}
	}
	return
}

// isGitHubURL 加速只对 GitHub 的地址有意义。
func isGitHubURL(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Host) {
	case "raw.githubusercontent.com", "github.com", "gist.githubusercontent.com", "objects.githubusercontent.com":
		return true
	}
	return false
}

// ruleProviderProxiedURL 把 target 改写成经 server 取的地址;这个加速站取不了这种地址时返回空。
func ruleProviderProxiedURL(server, target string) string {
	server = normalizeProxyServer(server)
	if server == "" || !isGitHubURL(target) {
		return ""
	}
	su, _ := url.Parse(server)
	if strings.HasSuffix(strings.ToLower(su.Host), "jsdelivr.net") {
		user, repo, branch, path, ok := githubFileRef(target)
		if !ok {
			return "" // jsDelivr 只能取仓库里的文件
		}
		return fmt.Sprintf("%s://%s/gh/%s/%s@%s/%s", su.Scheme, su.Host, user, repo, branch, path)
	}
	return server + target
}

// ruleProviderProxyCandidates 这次抓取要依次尝试的加速站。
func ruleProviderProxyCandidates(cfg RuleProviderProxyConfig) []string {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Server != "" {
		return []string{cfg.Server}
	}
	out := make([]string, 0, len(ruleProviderProxyPresets)+1)
	if last, _ := ruleProviderProxyLastGood.Load().(string); last != "" {
		out = append(out, last)
	}
	for _, s := range ruleProviderProxyPresets {
		if len(out) == 0 || s != out[0] {
			out = append(out, s)
		}
	}
	return out
}

// fetchRuleProviderVia 抓规则集:开了代理且地址是 GitHub 的,先经加速站;都不行(或没开)就直连。
// fetch 是实际发请求的函数(直连用同一个),测试可注入。
func fetchRuleProviderVia(ctx context.Context, cfg RuleProviderProxyConfig, target string,
	fetch func(ctx context.Context, url string, timeout time.Duration) (string, error)) (string, error) {
	var tried []string
	auto := cfg.Server == ""
	for _, server := range ruleProviderProxyCandidates(cfg) {
		proxied := ruleProviderProxiedURL(server, target)
		if proxied == "" {
			continue
		}
		// 加速站一个不通就换下一个,别在一个上面等满 30 秒
		content, err := fetch(ctx, proxied, 12*time.Second)
		if err == nil && !looksLikeHTMLPage(content) {
			if auto {
				ruleProviderProxyLastGood.Store(server)
			}
			return content, nil
		}
		tried = append(tried, server)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	content, err := fetch(ctx, target, 30*time.Second)
	if err != nil && len(tried) > 0 {
		return "", fmt.Errorf("%w(已试过 %d 个加速站,均未取到)", err, len(tried))
	}
	return content, err
}

// looksLikeHTMLPage 加速站挂了或限流时常回一张 200 的网页;把它当规则集存下来,订阅就坏了。
func looksLikeHTMLPage(content string) bool {
	head := strings.ToLower(strings.TrimSpace(content))
	if len(head) > 256 {
		head = head[:256]
	}
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

// ---- 管理接口 ----

type ruleProviderProxyProbe struct {
	Server string `json:"server"`
	OK     bool   `json:"ok"`
	Ms     int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
}

func (h *RuleProvidersHandler) handleProxyGet(w http.ResponseWriter, r *http.Request) {
	cfg := loadRuleProviderProxyConfig(r.Context(), h.repo)
	last, _ := ruleProviderProxyLastGood.Load().(string)
	respondJSON(w, http.StatusOK, map[string]any{
		"success": true, "config": cfg, "presets": ruleProviderProxyPresets, "last_good": last,
	})
}

func (h *RuleProvidersHandler) handleProxyPut(w http.ResponseWriter, r *http.Request) {
	var req RuleProviderProxyConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeBadRequest(w, "请求格式不正确")
		return
	}
	server := normalizeProxyServer(req.Server)
	if strings.TrimSpace(req.Server) != "" && server == "" {
		writeBadRequest(w, "代理服务器地址不正确,应形如 https://gh-proxy.com/")
		return
	}
	cfg := RuleProviderProxyConfig{Enabled: req.Enabled, Server: server}
	blob, _ := json.Marshal(cfg)
	if err := h.repo.SetSystemSetting(r.Context(), ruleProviderProxyKey, string(blob)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"success": true, "config": cfg})
}

// handleProxyTest 把内置清单(加上请求里带的自定义地址)各取一次测试文件,按可用与耗时排序返回。
func (h *RuleProvidersHandler) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Server string `json:"server"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)
	servers := append([]string{}, ruleProviderProxyPresets...)
	if custom := normalizeProxyServer(req.Server); custom != "" {
		known := false
		for _, s := range servers {
			known = known || s == custom
		}
		if !known {
			servers = append(servers, custom)
		}
	}
	results := make([]ruleProviderProxyProbe, len(servers))
	var wg sync.WaitGroup
	for i, server := range servers {
		wg.Add(1)
		go func(i int, server string) {
			defer wg.Done()
			res := ruleProviderProxyProbe{Server: server}
			start := time.Now()
			content, err := fetchRuleProviderRaw(r.Context(), ruleProviderProxiedURL(server, ruleProviderProxyTestURL), 10*time.Second)
			res.Ms = time.Since(start).Milliseconds()
			switch {
			case err != nil:
				res.Error = err.Error()
			case looksLikeHTMLPage(content):
				res.Error = "返回的是网页,不是文件内容"
			default:
				res.OK = true
			}
			results[i] = res
		}(i, server)
	}
	wg.Wait()
	sort.SliceStable(results, func(a, b int) bool {
		if results[a].OK != results[b].OK {
			return results[a].OK
		}
		return results[a].Ms < results[b].Ms
	})
	respondJSON(w, http.StatusOK, map[string]any{"success": true, "results": results})
}

// fetchRuleProviderRaw 取一个地址的全部内容(SSRF 安全客户端,带大小上限)。
func fetchRuleProviderRaw(ctx context.Context, target string, timeout time.Duration) (string, error) {
	if target == "" {
		return "", errors.New("地址为空")
	}
	client := newSSRFSafeHTTPClient(timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mmwx-rule-provider/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ruleProviderMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > ruleProviderMaxBytes {
		return "", fmt.Errorf("内容过大(上限 %d MB)", ruleProviderMaxBytes>>20)
	}
	if len(body) == 0 {
		return "", errors.New("远程返回空内容")
	}
	return string(body), nil
}
