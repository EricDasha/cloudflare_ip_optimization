package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cfTrace 是 Cloudflare `/cdn-cgi/trace` 的结构化结果。
//
// 这个端点由 CF 边缘直接应答，不经过 Worker，因此它回答的是「这个 IP 是不是
// 一个真正能终止 TLS 的 CF 边缘」——正是 WS 101 失败时缺失的那块信息。
// 2026-11 的四仓库调研里，CloudFlareScan 与 cfnb 都用它做独立诊断；我们此前
// 只在 WS 成功时从 CF-RAY 拿 colo，失败候选完全没有机房归属。
type cfTrace struct {
	IP          string
	Colo        string
	VisitScheme string
	TLS         string
	Kex         string
	UAG         string
	CFRay       string
	Server      string
}

// traceProbeUserAgent 是 trace 探测固定使用的 UA。
//
// `/cdn-cgi/trace` 会把请求 UA 原样回显到 `uag=`。cfnb 的 L7 验活把
// 「回显 UA 必须等于请求 UA」列为硬条件：一个真正的 CF 边缘会忠实回显，
// 中间层改写或伪造的应答则不会。固定常量让这条检查可复现。
const traceProbeUserAgent = "cloudflare-tools-trace-probe"

func parseCFTrace(body string) cfTrace {
	var trace cfTrace
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ip":
			trace.IP = value
		case "colo":
			trace.Colo = value
		case "visit_scheme":
			trace.VisitScheme = value
		case "tls":
			trace.TLS = value
		case "kex":
			trace.Kex = value
		case "uag":
			trace.UAG = value
		}
	}
	return trace
}

func (t cfTrace) summary() string {
	parts := make([]string, 0, 5)
	if t.Colo != "" {
		parts = append(parts, "colo="+t.Colo)
	}
	if t.VisitScheme != "" {
		parts = append(parts, "visit_scheme="+t.VisitScheme)
	}
	if t.TLS != "" {
		parts = append(parts, "tls="+t.TLS)
	}
	if t.Kex != "" {
		parts = append(parts, "kex="+t.Kex)
	}
	if t.IP != "" {
		parts = append(parts, "client_ip="+t.IP)
	}
	if t.UAG != "" {
		parts = append(parts, "uag="+t.UAG)
	}
	return strings.Join(parts, " ")
}

// traceL7RejectReason 只对「拿到了 trace 应答」的候选做硬判定。
//
// 三条硬条件来自 cfnb 的 L7 验活：TLS 必须真正终止、密钥交换不能是 none、
// 请求必须走 https。缺 colo 说明应答不是 CF 边缘生成的，同样不合格。
//
// 注意：trace 里的 `ip=` 是客户端出口 IP，不是边缘 IP。我们直连拨号时它
// 必然等于本机出口，拿它判回环会把所有候选误杀，因此这里不比较 trace.IP
// 与本机公网 IP。
func traceL7RejectReason(trace cfTrace) string {
	if trace.VisitScheme != "" && trace.VisitScheme != "https" {
		return "trace visit_scheme=" + trace.VisitScheme
	}
	if trace.TLS == "off" {
		return "trace tls=off"
	}
	if trace.Kex == "none" {
		return "trace kex=none"
	}
	if trace.Colo == "" {
		return "trace 未返回 colo"
	}
	// 以下四条来自 cfnb 的 L7 硬条件。它们回答的是「这个应答是不是 CF 边缘
	// 亲自生成的」：colo/ip 缺一不可，CF-RAY 是边缘签发的射线 ID，Server
	// 必须是 cloudflare；uag 则验证边缘忠实回显了我们发出的 UA。
	//
	// 注意仍然不比较 trace.IP 与本机公网 IP：直连 CF 边缘时它本来就等于
	// 本机出口，那条检查只适用于「反代 IP」场景（cfnb 的用途），照搬会
	// 把所有正常候选判死。
	if trace.IP == "" {
		return "trace 未返回 ip"
	}
	if trace.UAG != traceProbeUserAgent {
		return "trace uag 不匹配"
	}
	if trace.CFRay == "" {
		return "trace 缺少 CF-RAY"
	}
	if !strings.Contains(strings.ToLower(trace.Server), "cloudflare") {
		return "trace server 不是 cloudflare"
	}
	return ""
}

// probeCFTrace 用真实域名做 SNI、把 TCP 拨号钉到候选 IP，请求
// `https://<host>/cdn-cgi/trace`。等价于 curl --resolve，不会出现
// 「用 IP 当 SNI」导致的证书错误。
func probeCFTrace(parent context.Context, ip string, port int, host string) (cfTrace, error) {
	var trace cfTrace
	host = strings.TrimSpace(host)
	if host == "" {
		return trace, fmt.Errorf("缺少 trace 探测域名")
	}
	if port <= 0 || port > 65535 {
		port = 443
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip, strconv.Itoa(port)))
		},
		TLSClientConfig: &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2:   false,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 2 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	url := "https://" + host + "/cdn-cgi/trace"
	if port != 443 {
		url = "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/cdn-cgi/trace"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return trace, err
	}
	req.Host = host
	req.Header.Set("User-Agent", traceProbeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return trace, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return trace, err
	}
	if resp.StatusCode != http.StatusOK {
		return trace, fmt.Errorf("trace 状态码 %d", resp.StatusCode)
	}
	trace = parseCFTrace(string(body))
	trace.CFRay = strings.TrimSpace(resp.Header.Get("CF-RAY"))
	trace.Server = strings.TrimSpace(resp.Header.Get("Server"))
	if trace.Colo == "" {
		trace.Colo = coloFromCFRay(trace.CFRay)
	}
	return trace, nil
}

// attachTraceDiagnostics 只给 WS 失败候选补 trace 诊断，不参与准入。
//
// 上限 30 个、并发 4、每个 2 秒：一轮最多 30 个额外请求，远小于初筛的
// 990 个，符合「不扩大探测压力」的约束。结果写 Colo 与 Note，方便在
// 状态页区分「TLS 都没通」和「TLS 通了但 WS 路径不对」。
func attachTraceDiagnostics(parent context.Context, results []proxyScanResult, cfg proxyAutoConfig) {
	const maxDiagnostics = 30
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	launched := 0
	for i := range results {
		if results[i].Stage != "WS_FAIL" || results[i].Error == "" {
			continue
		}
		if launched >= maxDiagnostics || parent.Err() != nil {
			break
		}
		launched++
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if parent.Err() != nil {
				return
			}
			trace, err := probeCFTrace(parent, results[i].IP, cfg.Port, cfg.Host)
			if err != nil {
				results[i].Note = "trace 诊断失败: " + err.Error()
				return
			}
			if trace.Colo != "" && results[i].Colo == "" {
				results[i].Colo = trace.Colo
			}
			note := "trace: " + trace.summary()
			if reason := traceL7RejectReason(trace); reason != "" {
				note += "；" + reason
			}
			results[i].Note = note
		}(i)
	}
	wg.Wait()
	if launched > 0 {
		log.Printf("trace diagnostics: probed %d WS-failed candidates", launched)
	}
}

// filterTraceL7 对终审通过的候选做一次 L7 复验。
//
// 失败（超时、连接被拒）一律放行：未知不等于差。只有拿到 trace 应答且
// 明确不满足硬条件时才拒绝。若全部候选都被拒，则整批放行并记日志——
// 宁可保留一批可疑成员，也不能因为诊断端点不可用把池子清空。
func filterTraceL7(parent context.Context, passed []string, results []proxyScanResult, cfg proxyAutoConfig) []string {
	if len(passed) == 0 || parent.Err() != nil {
		return passed
	}
	if !envBool("PROXY_TRACE_L7", true) {
		return passed
	}
	type outcome struct {
		ip    string
		trace cfTrace
		err   error
	}
	outcomes := make([]outcome, len(passed))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, ip := range passed {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if parent.Err() != nil {
				outcomes[i] = outcome{ip: ip, err: parent.Err()}
				return
			}
			trace, err := probeCFTrace(parent, ip, cfg.Port, cfg.Host)
			outcomes[i] = outcome{ip: ip, trace: trace, err: err}
		}(i, ip)
	}
	wg.Wait()

	kept := make([]string, 0, len(passed))
	rejected := make([]string, 0)
	unverified := 0
	for _, outcome := range outcomes {
		if outcome.err != nil {
			unverified++
			kept = append(kept, outcome.ip)
			continue
		}
		if reason := traceL7RejectReason(outcome.trace); reason != "" {
			rejected = append(rejected, fmt.Sprintf("%s(%s)", outcome.ip, reason))
			for i := range results {
				if results[i].IP == outcome.ip {
					if results[i].Colo == "" {
						results[i].Colo = outcome.trace.Colo
					}
					results[i].Note = "trace L7 拒绝: " + reason
					break
				}
			}
			continue
		}
		kept = append(kept, outcome.ip)
		for i := range results {
			if results[i].IP == outcome.ip && results[i].Colo == "" {
				results[i].Colo = outcome.trace.Colo
				break
			}
		}
	}
	if len(kept) == 0 && len(rejected) > 0 {
		log.Printf("proxy auto apply: trace L7 rejected all %d finalists, keeping them (diagnostic only): %v", len(rejected), rejected)
		return passed
	}
	if len(rejected) > 0 {
		log.Printf("proxy auto apply: trace L7 rejected %d finalists: %v", len(rejected), rejected)
	}
	// 即使零拒绝也留一行：否则「L7 跑了但全过」和「L7 根本没跑」在日志里
	// 长得一模一样，线上无法判断闸门是否生效。
	log.Printf("proxy auto apply: trace L7 checked %d finalists (%d rejected, %d unverified)", len(passed), len(rejected), unverified)
	return kept
}
