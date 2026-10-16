package speedtest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultTestURL      = "https://dl.google.com/dl/android/studio/install/3.4.1.0/android-studio-ide-183.5522156-windows.exe"
	defaultTestDuration = 8 * time.Second
	latencyProbeURL     = "https://www.gstatic.com/generate_204"
	cfLatencyProbeURL   = "https://cp.cloudflare.com/generate_204" // 真连接延迟用 Cloudflare 204(全球边缘 + CDN 边)
	egressIPProbeURL    = "https://api.ipify.org"
	mixedPort           = 17900
	cfLatencySamples    = 3 // 真延迟采样次数,取最快 2 个平均(去掉首包冷启动)
)

var runMu sync.Mutex

type Result struct {
	DownMbps  float64
	LatencyMs int64
	Bytes     int64
	Duration  time.Duration
	EgressIP  string
}

type Options struct {
	TestURL      string
	TestDuration time.Duration
	TestBytes    int64
	Timeout      time.Duration
	Threads      int
	LatencyOnly  bool // true 仅测真连接延迟(Cloudflare 204 多采样)不跑大文件下载
	// 下面三项给「节点探测」用:探测只关心通不通,不需要测速那套完整流程。
	LatencyTimeout time.Duration // 单次 204 请求的超时;0 = 默认 8 秒
	SkipEgressIP   bool          // 不查出口 IP(探测用不上,省一次经节点的外网请求)
	FailFast       bool          // 首个采样就失败即判不通,不再重试剩余采样(挂掉的节点只花一次超时)
}

// RunNodeTest 用 mihomo 起单节点代理，测延迟 + 下行吞吐。
func RunNodeTest(ctx context.Context, mihomoBin, clashConfigJSON string, opts Options) (Result, error) {
	runMu.Lock()
	defer runMu.Unlock()

	if opts.TestDuration <= 0 {
		opts.TestDuration = defaultTestDuration
	}
	testURL := opts.TestURL
	if testURL == "" {
		testURL = defaultTestURL
	}
	if opts.Timeout <= 0 {
		opts.Timeout = opts.TestDuration + 30*time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var proxy map[string]any
	if err := json.Unmarshal([]byte(clashConfigJSON), &proxy); err != nil {
		return Result{}, fmt.Errorf("解析节点 clash 配置失败: %w", err)
	}
	name, _ := proxy["name"].(string)
	if name == "" {
		name = "node"
		proxy["name"] = name
	}

	mini := map[string]any{
		"mixed-port":          mixedPort,
		"allow-lan":           false,
		"mode":                "rule",
		"log-level":           "warning",
		"external-controller": "127.0.0.1:0",
		"proxies":             []map[string]any{proxy},
		"proxy-groups": []map[string]any{
			{"name": "PROXY", "type": "select", "proxies": []string{name}},
		},
		"rules": []string{"MATCH,PROXY"},
	}
	cfg, err := yaml.Marshal(mini)
	if err != nil {
		return Result{}, err
	}

	workdir := filepath.Join("data", "speedtest-tmp", fmt.Sprintf("%d", time.Now().UnixNano()))
	stop, err := startMihomo(mihomoBin, workdir, cfg)
	if err != nil {
		return Result{}, err
	}
	defer func() { stop(); os.RemoveAll(workdir) }()

	var egressIP string
	if !opts.SkipEgressIP {
		egressIP = measureEgressIP(ctx)
	}

	// LatencyOnly:只测真连接延迟(Cloudflare 204 多采样),不跑下载
	if opts.LatencyOnly {
		latency := measureLatencyCloudflare(ctx, cfLatencySamples, opts.LatencyTimeout, opts.FailFast)
		return Result{LatencyMs: latency, EgressIP: egressIP}, nil
	}

	latency := measureLatency(ctx)

	threads := opts.Threads
	if threads <= 1 {
		threads = 1
	}
	n, dur, err := downloadTimed(ctx, testURL, opts.TestDuration, opts.TestBytes, threads)
	if err != nil {
		return Result{LatencyMs: latency, EgressIP: egressIP}, fmt.Errorf("下载测速失败: %w", err)
	}
	mbps := 0.0
	if dur > 0 {
		mbps = float64(n) * 8 / dur.Seconds() / 1e6
	}
	return Result{DownMbps: mbps, LatencyMs: latency, Bytes: n, Duration: dur, EgressIP: egressIP}, nil
}

func startMihomo(bin, workdir string, cfg []byte) (func(), error) {
	if err := os.MkdirAll(workdir, 0755); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(workdir, "config.yaml")
	if err := os.WriteFile(cfgPath, cfg, 0644); err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "-d", workdir, "-f", cfgPath)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", mixedPort)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, derr := (&net.Dialer{Timeout: 500 * time.Millisecond}).Dial("tcp", addr); derr == nil {
			c.Close()
			var once sync.Once
			return func() {
				once.Do(func() {
					done := make(chan error, 1)
					go func() { done <- cmd.Wait() }()
					if runtime.GOOS == "windows" {
						_ = cmd.Process.Kill()
					} else {
						_ = cmd.Process.Signal(syscall.SIGTERM)
					}
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						_ = cmd.Process.Kill()
						<-done
					}
				})
			}, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil, fmt.Errorf("mihomo 启动超时(端口 %d 15s 内未就绪)", mixedPort)
}

// proxyClient 经 mihomo mixed-port 走代理的 HTTP 客户端。
// 单流测速调优:1MB ReadBufferSize / 禁 HTTP/2(单流被流控限速)/ 禁 Compression / 复用 Transport
func proxyClient() *http.Client {
	return &http.Client{Transport: sharedProxyTransport()}
}

var (
	sharedTransportOnce sync.Once
	sharedTransport     *http.Transport
)

func sharedProxyTransport() *http.Transport {
	sharedTransportOnce.Do(func() {
		proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", mixedPort))
		sharedTransport = &http.Transport{
			Proxy:              http.ProxyURL(proxyURL),
			ReadBufferSize:     1 << 20, // 1MB,降低 loopback->mihomo read syscall 频率
			WriteBufferSize:    64 << 10,
			DisableCompression: true,
			ForceAttemptHTTP2:  false,
			TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{}, // 显式禁 HTTP/2
			MaxIdleConns:       64,
			IdleConnTimeout:    90 * time.Second,
		}
	})
	return sharedTransport
}

// 1MB 测速 io.Copy 缓冲池(默认 32KB 在 >100Mbps 时 syscall 太密)
var bigCopyBufPool = sync.Pool{
	New: func() any { b := make([]byte, 1<<20); return &b },
}

func measureLatency(ctx context.Context) int64 {
	client := proxyClient()
	client.Timeout = 10 * time.Second
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latencyProbeURL, nil)
	if err != nil {
		return -1
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return -1
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return time.Since(start).Milliseconds()
}

// measureLatencyCloudflare 用 Cloudflare 204(全球边缘 + CDN 边)多次采样,取最快 2 个均值;
// 首包受 TLS 握手 / mihomo cold-start 影响,平均后更接近"真连接延迟"。全部失败返回 -1。
// timeout 为 0 用默认 8 秒;failFast 为 true 时首个采样失败就直接返回 -1(节点探测用)。
func measureLatencyCloudflare(ctx context.Context, samples int, timeout time.Duration, failFast bool) int64 {
	if samples <= 0 {
		samples = cfLatencySamples
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	client := proxyClient()
	client.Timeout = timeout
	probes := make([]int64, 0, samples)
	for i := 0; i < samples; i++ {
		if ctx.Err() != nil {
			break
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfLatencyProbeURL, nil)
		if err != nil {
			continue
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			if failFast && len(probes) == 0 {
				return -1
			}
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		probes = append(probes, time.Since(start).Milliseconds())
	}
	if len(probes) == 0 {
		return -1
	}
	// 取最快 2 个均值(去掉首包冷启动的最慢);不足 2 个全取
	sortInt64Asc(probes)
	keep := 2
	if len(probes) < keep {
		keep = len(probes)
	}
	var sum int64
	for i := 0; i < keep; i++ {
		sum += probes[i]
	}
	return sum / int64(keep)
}

func sortInt64Asc(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func measureEgressIP(ctx context.Context) string {
	client := proxyClient()
	client.Timeout = 8 * time.Second
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, egressIPProbeURL, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(buf))
	if len(ip) < 3 || len(ip) > 45 || (!strings.Contains(ip, ".") && !strings.Contains(ip, ":")) {
		return ""
	}
	return ip
}

func downloadTimed(ctx context.Context, dlURL string, dur time.Duration, maxBytes int64, threads int) (int64, time.Duration, error) {
	dlCtx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	if threads <= 1 {
		return downloadSingle(dlCtx, dlURL, maxBytes)
	}

	var wg sync.WaitGroup
	results := make([]int64, threads)
	errs := make([]error, threads)
	start := time.Now()
	for i := range threads {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			n, _, e := downloadSingle(dlCtx, dlURL, maxBytes)
			results[idx] = n
			errs[idx] = e
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	var total int64
	var firstErr error
	for i := range threads {
		total += results[i]
		if errs[i] != nil && firstErr == nil {
			firstErr = errs[i]
		}
	}
	if total > 0 {
		return total, elapsed, nil
	}
	return 0, elapsed, firstErr
}

func downloadSingle(ctx context.Context, dlURL string, maxBytes int64) (int64, time.Duration, error) {
	client := proxyClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 mmw-speedtest/1.0")
	req.Header.Set("Accept-Encoding", "identity")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var reader io.Reader = resp.Body
	if maxBytes > 0 {
		reader = io.LimitReader(resp.Body, maxBytes)
	}
	buf := bigCopyBufPool.Get().(*[]byte)
	defer bigCopyBufPool.Put(buf)
	n, cerr := io.CopyBuffer(io.Discard, reader, *buf)
	elapsed := time.Since(start)
	if ctx.Err() == context.DeadlineExceeded || cerr == nil {
		return n, elapsed, nil
	}
	return n, elapsed, cerr
}
