package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fetchSubscriptionBody 拉取单个维护者订阅内容：HTTPS 同 host 跳转、≤512KiB。
func fetchSubscriptionBody(parent context.Context, converter, maintainer, token string) ([]byte, error) {
	if converter == "" || maintainer == "" || token == "" {
		return nil, errors.New("订阅转换器、维护者或 token 为空")
	}
	target := converter + "?token=" + url.QueryEscape(token) + "&sub=" + url.QueryEscape(maintainer)
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, errors.New("订阅转换器地址必须是 HTTPS")
	}
	subFetchTimeout := envInt("PROXY_SUBSCRIPTION_FETCH_TIMEOUT", 45)
	if subFetchTimeout < 15 || subFetchTimeout > 180 {
		subFetchTimeout = 45
	}
	client := &http.Client{
		Timeout: time.Duration(subFetchTimeout) * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !strings.EqualFold(req.URL.Hostname(), parsed.Hostname()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(parent, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("订阅 HTTP 状态码 %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCandidateSourceBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxCandidateSourceBody {
		return nil, errors.New("订阅内容超过 512 KiB")
	}
	return body, nil
}

// loadSubscriptionPool 启动加载持久化的订阅 IP 池。
func (a *app) loadSubscriptionPool() {
	data, err := os.ReadFile(filepath.Join(a.dataDir, subscriptionPoolFile))
	if err != nil {
		return
	}
	var stored struct {
		UpdatedAt time.Time         `json:"updatedAt"`
		IPs       []string          `json:"ips"`
		Sources   map[string]string `json:"sources"`
	}
	if json.Unmarshal(data, &stored) != nil || len(stored.IPs) == 0 {
		return
	}
	valid := make([]string, 0, len(stored.IPs))
	sources := make(map[string]string, len(stored.IPs))
	for _, raw := range stored.IPs {
		if ip := net.ParseIP(raw); isPublicIPv4(ip) {
			key := ip.To4().String()
			valid = append(valid, key)
			if src := stored.Sources[key]; src != "" {
				sources[key] = src
			} else {
				sources[key] = "subscription"
			}
		}
	}
	a.subMu.Lock()
	a.subIPs = valid
	a.subSources = sources
	a.subMu.Unlock()
}

func (a *app) saveSubscriptionPoolLocked() error {
	data, err := json.MarshalIndent(map[string]any{
		"updatedAt": time.Now(),
		"ips":       a.subIPs,
		"sources":   a.subSources,
	}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, subscriptionPoolFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// subscriptionPoolIPs 返回订阅池 IP 快照（按入库顺序，已过 Cloudflare 段闸），
// 供候选汇合以 subscription 层合入。
//
// 闸门理由：订阅原文里会被 extractPublicIPv4 抓出所有公网 IPv4，其中大量是
// 机场自有 VPS（不是 Cloudflare 边缘）。不过在源头过滤，这些地址就会挤占
// 配额、再被下游 VLESS 终审一个个淘汰，白烧探针。
func (a *app) subscriptionPoolIPs(remaining int) []string {
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	if remaining <= 0 || len(a.subIPs) == 0 {
		return nil
	}
	out := make([]string, 0, min(remaining, len(a.subIPs)))
	for _, raw := range a.subIPs {
		ip := net.ParseIP(raw)
		if !isCloudflareIPv4(ip) {
			continue
		}
		out = append(out, ip.To4().String())
		if len(out) >= remaining {
			break
		}
	}
	return out
}

func (a *app) subscriptionStatus() map[string]any {
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	maintainers := subscriptionMaintainers()
	byMaintainer := make(map[string]int, len(maintainers))
	for _, src := range a.subSources {
		byMaintainer[src]++
	}
	return map[string]any{
		"enabled":      subscriptionRefreshEnabled(),
		"maintainers":  maintainers,
		"totalIPs":     len(a.subIPs),
		"byMaintainer": byMaintainer,
		"lastRun":      a.subLastRun,
		"lastError":    a.subLastError,
		"lastCount":    a.subLastCount,
	}
}

// sweepSubscriptions 执行一轮订阅拉取 → IP 提取 → WS 慢筛 → 入池。
// 防重入：同一时刻只跑一轮，避免手动触发与定时轮重叠造成“人挤人”式重复拉取。
func (a *app) sweepSubscriptions(parent context.Context) {
	if !a.subSweepMu.TryLock() {
		log.Printf("subscription sweep skipped: another sweep is running")
		return
	}
	defer a.subSweepMu.Unlock()
	token := subscriptionToken()
	converter := defaultSubscriptionConverter()
	maintainers := subscriptionMaintainers()
	if token == "" || len(maintainers) == 0 {
		a.subMu.Lock()
		a.subLastError = "订阅 token 未配置（PROXY_SUBSCRIPTION_TOKEN）"
		a.subMu.Unlock()
		log.Printf("subscription sweep skipped: token unset")
		return
	}
	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		a.subMu.Lock()
		a.subLastError = "PROXY_AUTO_HOST 未配置，跳过订阅验活"
		a.subMu.Unlock()
		log.Printf("subscription sweep skipped: PROXY_AUTO_HOST unset")
		return
	}
	cfg.Concurrency = 4
	cfg.MaxLatency = 2000
	perMaintainer := envInt("PROXY_SUBSCRIPTION_PER_MAINTAINER", 30)
	if perMaintainer < 1 || perMaintainer > 200 {
		perMaintainer = 30
	}
	var fresh []string
	freshSources := make(map[string]string)
	sourceErrors := []string{}
	ctx, cancel := context.WithTimeout(parent, 8*time.Minute)
	defer cancel()
	// 并行拉取各维护者订阅（并发 3、单源失败重试 1 次）：
	// 订阅转换器偶发冷启动慢（实测单源 14~21s），串行会被慢源拖垮整轮预算，
	// 后续维护者还没轮到就撞上总超时。
	type subFetch struct {
		maintainer string
		body       []byte
		err        error
	}
	fetches := make([]subFetch, len(maintainers))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, maintainer := range maintainers {
		wg.Add(1)
		go func(i int, maintainer string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var (
				body []byte
				err  error
			)
			for attempt := 0; attempt < 2; attempt++ {
				body, err = fetchSubscriptionBody(ctx, converter, maintainer, token)
				if err == nil {
					break
				}
				select {
				case <-ctx.Done():
					fetches[i] = subFetch{maintainer, nil, err}
					return
				case <-time.After(3 * time.Second):
				}
			}
			fetches[i] = subFetch{maintainer, body, err}
		}(i, maintainer)
	}
	wg.Wait()
	for _, f := range fetches {
		if f.err != nil {
			sourceErrors = append(sourceErrors, f.maintainer+": "+f.err.Error())
			// 逐维护者记健康账：哪个订阅源死了，审计表与健康账一眼可辨。
			a.recordSourceHealth("sub:"+f.maintainer, 0, f.err)
			continue
		}
		maintainer := f.maintainer
		text := string(f.body)
		extracted := 0
		for _, ip := range extractPublicIPv4(text) {
			if _, ok := freshSources[ip]; !ok {
				freshSources[ip] = maintainer
				fresh = append(fresh, ip)
				extracted++
			}
		}
		for _, ip := range resolveSubscriptionHostnames(ctx, extractSubscriptionHostnames(text), perMaintainer) {
			if _, ok := freshSources[ip]; !ok {
				freshSources[ip] = maintainer
				fresh = append(fresh, ip)
				extracted++
			}
		}
		a.recordSourceHealth("sub:"+maintainer, extracted, nil)
	}
	if len(fresh) == 0 {
		a.subMu.Lock()
		a.subLastRun = time.Now()
		a.subLastError = strings.Join(sourceErrors, " | ")
		if a.subLastError == "" {
			a.subLastError = "订阅未提取到公网 IPv4"
		}
		a.subMu.Unlock()
		log.Printf("subscription sweep: no IPs extracted: %s", strings.Join(sourceErrors, " | "))
		return
	}
	// 慢筛：只把 WS 通过的留下来，避免脏 IP 进池。
	// fresh 按维护者顺序排列；为避免单维护者独占验活预算，轮转打散后再验。
	fresh = interleaveByMaintainer(fresh, freshSources)
	wsTimeout := envInt("PROXY_SUBSCRIPTION_VERIFY_TIMEOUT", 240)
	if wsTimeout < 60 || wsTimeout > 900 {
		wsTimeout = 240
	}
	wsCtx, wsCancel := context.WithTimeout(ctx, time.Duration(wsTimeout)*time.Second)
	defer wsCancel()
	results := scanProxyWebSockets(wsCtx, fresh, cfg)
	verified := make([]string, 0, len(results))
	for _, result := range results {
		if result.Error == "" {
			verified = append(verified, result.IP)
		}
	}
	a.subMu.Lock()
	merged := make(map[string]struct{}, len(a.subIPs))
	combined := make([]string, 0, len(a.subIPs)+len(verified))
	for _, ip := range a.subIPs {
		if _, ok := merged[ip]; !ok {
			merged[ip] = struct{}{}
			combined = append(combined, ip)
		}
	}
	for _, ip := range verified {
		if _, ok := merged[ip]; !ok {
			merged[ip] = struct{}{}
			combined = append(combined, ip)
		}
	}
	// 订阅池上限 1000，老 IP 在前（先到先得，慢扫天然轮换）。
	if len(combined) > 1000 {
		combined = combined[len(combined)-1000:]
	}
	sources := make(map[string]string, len(combined))
	for _, ip := range combined {
		if src, ok := a.subSources[ip]; ok {
			sources[ip] = src
		} else if src, ok := freshSources[ip]; ok {
			sources[ip] = src
		} else {
			sources[ip] = "subscription"
		}
	}
	a.subIPs = combined
	a.subSources = sources
	a.subLastRun = time.Now()
	a.subLastError = strings.Join(sourceErrors, " | ")
	a.subLastCount = len(verified)
	saveErr := a.saveSubscriptionPoolLocked()
	a.subMu.Unlock()
	if saveErr != nil {
		log.Printf("subscription pool save failed: %v", saveErr)
	}
	log.Printf("subscription sweep: %d verified of %d extracted (pool=%d)", len(verified), len(fresh), len(combined))
	// The subscription pool is a candidate source. Refresh its aggregate
	// snapshot immediately so the next auto-apply round cannot use stale input.
	go a.refreshAndApplyProxyPool(context.Background())
}

// interleaveByMaintainer 按维护者轮转打散 IP，保证多维护者公平分享验活预算。
func interleaveByMaintainer(ips []string, sources map[string]string) []string {
	bySource := make(map[string][]string)
	order := make([]string, 0)
	for _, ip := range ips {
		src := sources[ip]
		if _, ok := bySource[src]; !ok {
			order = append(order, src)
		}
		bySource[src] = append(bySource[src], ip)
	}
	out := make([]string, 0, len(ips))
	for i := 0; len(out) < len(ips); i++ {
		progress := false
		for _, src := range order {
			if i < len(bySource[src]) {
				out = append(out, bySource[src][i])
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return out
}
