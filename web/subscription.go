package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	errSubscriptionMaintainerLimit = fmt.Errorf("维护者数量已达上限 %d 个，请先删除不用的", maxSubscriptionMaintainers)
	errSubscriptionConverterScheme = errors.New("订阅转换器地址必须是 https://")
)

// fetchSubscriptionBody 拉取单个维护者订阅内容：HTTPS 同 host 跳转、≤512KiB。
// 返回值附带 HTTP 状态码，供留痕使用——「404」和「超时」是两种完全不同的故障。
// allowHTTPSRedirect 判定是否跟进重定向：只认同 host 且仍为 HTTPS。
// 只比 host 的旧写法会放行同 host https→http：订阅 URL 里带 token
// 查询串（及候选源内容）会被明文发出。
func allowHTTPSRedirect(req *http.Request, base *url.URL) bool {
	return req.URL.Scheme == "https" && strings.EqualFold(req.URL.Hostname(), base.Hostname())
}

// subscriptionRedirectPolicy 是订阅与候选源共用的重定向策略：只跟同 host
// 的 HTTPS 跳转，且最多 10 次。Go 的 10 次重定向上限只在默认
// checkRedirect 里（CheckRedirect == nil 时才用）：一旦自定义
// CheckRedirect，上限就不存在——被攻陷的订阅转换器返回自指 Location
// 时，一轮采集会被拖成请求风暴（订阅 URL 里还带 token 查询串，
// 反复发往同一 host）。
func subscriptionRedirectPolicy(base *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !allowHTTPSRedirect(req, base) {
			return http.ErrUseLastResponse
		}
		return nil
	}
}

func fetchSubscriptionBody(parent context.Context, converter, maintainer, token string) ([]byte, int, error) {
	if converter == "" || maintainer == "" || token == "" {
		return nil, 0, errors.New("订阅转换器、维护者或 token 为空")
	}
	target := converter + "?token=" + url.QueryEscape(token) + "&sub=" + url.QueryEscape(maintainer)
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, 0, errors.New("订阅转换器地址必须是 HTTPS")
	}
	subFetchTimeout := envInt("PROXY_SUBSCRIPTION_FETCH_TIMEOUT", 45)
	if subFetchTimeout < 15 || subFetchTimeout > 180 {
		subFetchTimeout = 45
	}
	client := &http.Client{
		Timeout:       time.Duration(subFetchTimeout) * time.Second,
		CheckRedirect: subscriptionRedirectPolicy(parsed),
	}
	request, err := http.NewRequestWithContext(parent, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, response.StatusCode, fmt.Errorf("订阅 HTTP 状态码 %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCandidateSourceBody+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(body) > maxCandidateSourceBody {
		return nil, response.StatusCode, errors.New("订阅内容超过 512 KiB")
	}
	return body, response.StatusCode, nil
}

// subscriptionPoolIPs 返回本子里考过的 IP（按成绩降序），供候选汇合以
// subscription 层合入。
//
// 与旧版的关键差别：不再过 CF 段闸。理由在线上被数据证伪——2026-11-05 实测
// 在位池 45 个成员里 30 个不在官方 15 段内，它们此刻正在正常转发，延迟与官方段
// 相差不到 30ms。CF 段闸问的是「像不像 Cloudflare 自己的地址」，而真正该问的是
// 「能不能把流量送到 Cloudflare」，这两件事不等价，判据应该交给真实考试。
func (a *app) subscriptionPoolIPs(remaining int) []string {
	return a.subscriptionPassedIPs(remaining)
}

func (a *app) subscriptionStatus() map[string]any {
	summary := a.subscriptionInventorySummary()
	settings := a.subscriptionSettingsSnapshot()
	maintainers := make([]string, 0)
	if items, ok := settings["maintainers"].([]map[string]any); ok {
		for _, item := range items {
			if host, _ := item["host"].(string); host != "" {
				maintainers = append(maintainers, host)
			}
		}
	}
	a.subMu.RLock()
	lastRun := a.subInv.LastHarvestAt
	lastError := a.subInv.LastHarvestErr
	lastCount := a.subInv.LastPassed
	a.subMu.RUnlock()
	out := map[string]any{
		"enabled":     subscriptionRefreshEnabled(),
		"maintainers": maintainers,
		"lastRun":     lastRun,
		"lastError":   lastError,
		"lastCount":   lastCount,
	}
	for key, value := range summary {
		out[key] = value
	}
	return out
}

// harvestSubscriptions 只做一件事：把各维护者的订阅内容拉下来、提取、记进本子。
//
// 这里刻意不做任何探测。旧实现是「拉取→提取→立刻握手→只存通过的」一次跑完，
// 结果是采集量被考试预算绑死（240 秒握手预算决定这一轮最多能收多少），
// 而且没考过的候选连记录都不留——「收了 800 个、只过 141 个」这件事在系统里
// 完全不可见。拆开之后，收就是收，收多少记多少。
func (a *app) harvestSubscriptions(parent context.Context) (int, error) {
	if !a.subHarvestMu.TryLock() {
		return 0, errors.New("另一轮采集仍在进行")
	}
	defer a.subHarvestMu.Unlock()

	token := subscriptionToken()
	converter := a.subscriptionConverterURL()
	maintainers := a.activeSubscriptionMaintainers()
	if token == "" {
		return 0, errors.New("订阅 token 未配置（PROXY_SUBSCRIPTION_TOKEN）")
	}
	if len(maintainers) == 0 {
		return 0, errors.New("没有启用的订阅维护者")
	}

	perMaintainer := envInt("PROXY_SUBSCRIPTION_PER_MAINTAINER", 30)
	if perMaintainer < 1 || perMaintainer > 200 {
		perMaintainer = 30
	}
	// 退避：连续拉不动的维护者暂时跳过，别让一个死域名每轮都白等 45 秒。
	//
	// 但全部都在退避时必须整体放行——否则「退避」就变成了「永久拉黑」，
	// 那比不做退避更糟。这与 bestcf 那条路的规矩一致。
	skipped := make([]string, 0)
	active := make([]string, 0, len(maintainers))
	for _, host := range maintainers {
		if a.sourceInBackoff("sub:" + host) {
			skipped = append(skipped, host)
			continue
		}
		active = append(active, host)
	}
	if len(active) == 0 {
		active = maintainers
		skipped = nil
	}
	if len(skipped) > 0 {
		log.Printf("subscription harvest: %d maintainer(s) in backoff, skipped: %v", len(skipped), skipped)
	}
	maintainers = active

	ctx, cancel := context.WithTimeout(parent, 8*time.Minute)
	defer cancel()

	type subFetch struct {
		maintainer string
		body       []byte
		status     int
		started    time.Time
		elapsed    time.Duration
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
			started := time.Now()
			var (
				body   []byte
				status int
				err    error
			)
			for attempt := 0; attempt < 2; attempt++ {
				body, status, err = fetchSubscriptionBody(ctx, converter, maintainer, token)
				if err == nil {
					break
				}
				select {
				case <-ctx.Done():
					fetches[i] = subFetch{maintainer: maintainer, status: status, started: started, elapsed: time.Since(started), err: err}
					return
				case <-time.After(3 * time.Second):
				}
			}
			fetches[i] = subFetch{maintainer: maintainer, body: body, status: status, started: started, elapsed: time.Since(started), err: err}
		}(i, maintainer)
	}
	wg.Wait()

	sources := make(map[string]string)
	via := make(map[string]string)
	records := make(map[string]subFetchRecord, len(fetches))
	sourceErrors := make([]string, 0)
	totalExtracted := 0

	for _, f := range fetches {
		record := subFetchRecord{At: f.started, DurationMs: f.elapsed.Milliseconds(), Status: f.status}
		if f.err != nil {
			record.Error = f.err.Error()
			records[f.maintainer] = record
			sourceErrors = append(sourceErrors, f.maintainer+": "+f.err.Error())
			a.recordSourceHealth("sub:"+f.maintainer, 0, f.err)
			continue
		}
		text := string(f.body)
		extracted := 0
		// 直接 IP 与域名解析共用同一 perMaintainer 上限：旧代码只在解析
		// 分支计数，一个维护者塞几千个直连 IP 能一次灌满整个本子。
		for _, ip := range extractPublicIPv4(text) {
			if extracted >= perMaintainer {
				break
			}
			if _, ok := sources[ip]; !ok {
				sources[ip] = f.maintainer
				extracted++
			}
		}
		hosts := extractSubscriptionHostnames(text)
		byHost := resolveSubscriptionHostnameMap(ctx, hosts)
		// 溯源按主机名逐个回填：哪个域名解析到哪个 IP，就记哪个域名。
		// 旧写法把平坦结果配给每个域名，同一维护者的多个域名全记成第一个，
		// 排障时看到的来源是错的——比没有来源更坏。
		for i, host := range hosts {
			if i >= len(byHost) || extracted >= perMaintainer {
				break
			}
			for _, ip := range byHost[i] {
				if extracted >= perMaintainer {
					break
				}
				if _, ok := sources[ip]; !ok {
					sources[ip] = f.maintainer
					extracted++
					if via[ip] == "" {
						via[ip] = host
					}
					continue
				}
				if via[ip] == "" {
					via[ip] = host
				}
			}
		}
		record.OK = true
		record.Extracted = extracted
		records[f.maintainer] = record
		totalExtracted += extracted
		a.recordSourceHealth("sub:"+f.maintainer, extracted, nil)
	}

	added, refreshed := a.mergeSubscriptionHarvest(sources, via, records)
	a.subMu.Lock()
	a.subInv.LastHarvestErr = strings.Join(sourceErrors, " | ")
	a.subInv.LastHarvested = totalExtracted
	now := time.Now()
	pruned := a.pruneSubscriptionInventoryLocked(now)
	total := len(a.subInv.Entries)
	saveErr := a.saveSubscriptionInventoryLocked()
	a.subMu.Unlock()
	if saveErr != nil {
		log.Printf("subscription inventory save failed: %v", saveErr)
	}
	log.Printf("subscription harvest: %d maintainers, %d extracted (%d new, %d refreshed), pruned %d, inventory=%d",
		len(maintainers), totalExtracted, added, refreshed, pruned, total)

	if totalExtracted == 0 {
		if len(sourceErrors) == 0 {
			sourceErrors = append(sourceErrors, "订阅未提取到公网 IPv4")
		}
		return 0, errors.New(strings.Join(sourceErrors, " | "))
	}
	return totalExtracted, nil
}

// probeSubscriptions 只做一件事：从本子里挑候选，真实考试，把结果写回去。
//
// 考试分两段，不是二选一：
//
//	① WS 101 粗筛——便宜（并发 4、单条 2 秒），只剔「连门都敲不开」的，
//	   单次失败不判死，但失败照样记入连败：3 次连败照样降为 fail。
//	   若 WS 失败永远不计数，连门都敲不开的条目就永远赖在本子里——
//	   淘汰要求 State==fail 且连败≥6，WS 不计数它永远达不到。
//	② sing-box 真实数据面——真正取回内容，这才是裁判。
//
// 为什么不能全省成 ②：实测成本差七倍以上（15 秒/条 vs 2 秒/条），
// 全量真跑会让一轮考试从几分钟变成一小时，供给直接跟不上。
func (a *app) probeSubscriptions(parent context.Context) (int, error) {
	if !a.subProbeMu.TryLock() {
		return 0, errors.New("另一轮考试仍在进行")
	}
	defer a.subProbeMu.Unlock()

	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		return 0, errors.New("PROXY_AUTO_HOST/PATH 未配置，无法考试")
	}

	batch := envInt("PROXY_SUBSCRIPTION_PROBE_BATCH", 400)
	if batch < 20 || batch > 3000 {
		batch = 400
	}
	recheck := time.Duration(envInt("PROXY_SUBSCRIPTION_RECHECK_MINUTES", 360)) * time.Minute
	if recheck < 30*time.Minute || recheck > 30*24*time.Hour {
		recheck = 6 * time.Hour
	}
	retry := time.Duration(envInt("PROXY_SUBSCRIPTION_RETRY_MINUTES", 120)) * time.Minute
	if retry < 15*time.Minute || retry > 24*time.Hour {
		retry = 2 * time.Hour
	}
	candidates := a.subscriptionProbeCandidates(batch, recheck, retry)
	if len(candidates) == 0 {
		a.subMu.Lock()
		a.subInv.LastProbeAt = time.Now()
		a.subInv.LastProbeErr = ""
		a.subInv.LastProbed = 0
		a.subInv.LastPassed = 0
		if saveErr := a.saveSubscriptionInventoryLocked(); saveErr != nil {
			log.Printf("subscription inventory save failed: %v", saveErr)
		}
		a.subMu.Unlock()
		return 0, nil
	}

	prescreenCfg := cfg
	prescreenCfg.Concurrency = 4
	prescreenCfg.MaxLatency = 2000
	wsTimeout := envInt("PROXY_SUBSCRIPTION_VERIFY_TIMEOUT", 240)
	if wsTimeout < 60 || wsTimeout > 1800 {
		wsTimeout = 240
	}
	wsCtx, wsCancel := context.WithTimeout(parent, time.Duration(wsTimeout)*time.Second)
	wsResults := scanProxyWebSockets(wsCtx, candidates, prescreenCfg)
	wsCancel()

	wsPass := make([]string, 0, len(wsResults))
	survivor := make(map[string]proxyScanResult, len(wsResults))
	for _, result := range wsResults {
		survivor[result.IP] = result
		if result.Error == "" {
			wsPass = append(wsPass, result.IP)
		}
	}

	// 真数据面是否启用：由 PROXY_VLESS_PROBE 决定，与漏斗保持同一开关。
	// 未启用时，WS 通过即算通过，并在 Stage 里注明是握手级结论。
	var (
		vlessCfg      vlessProbeConfig
		vlessTemplate map[string]any
		vlessReady    bool
	)
	if defaultProxyAutoConfig().VLESS.Enabled {
		vlessCfg = defaultProxyAutoConfig().VLESS
		template, err := loadVLESSOutboundTemplate(vlessCfg.TemplatePath)
		if err != nil {
			log.Printf("subscription probe: VLESS 模板不可用，本轮降级为握手级: %v", err)
			a.subMu.Lock()
			a.subInv.LastProbeErr = "VLESS 模板不可用，本轮仅握手级: " + err.Error()
			a.subMu.Unlock()
		} else {
			vlessTemplate = template
			vlessReady = true
			// 上一轮如果因为模板缺失降级过，这一轮恢复了就得把话收回，
			// 否则页面会一直挂着一条早就过期的异常。
			a.subMu.Lock()
			a.subInv.LastProbeErr = ""
			a.subMu.Unlock()
		}
	}

	now := time.Now()
	passed := 0
	a.subMu.Lock()
	for ip, result := range survivor {
		if result.Stage == "skipped" {
			// 没排上 WS 槽位、根本没测：不得记 ConsecutiveFails。
			continue
		}
		if vlessReady && result.Error == "" {
			// 真实数据面启用时，握手通过不算结论——真正取不回内容的地址
			// 不该先被记成 pass。等 VLESS 结果出来再写，避免「先判能用、
			// 再判不能用」这种自相矛盾的中间态进池。
			continue
		}
		outcome := subscriptionProbeOutcome{
			At:         now,
			OK:         result.Error == "",
			Stage:      "ws",
			LatencyMs:  result.Latency,
			Colo:       result.Colo,
			Error:      result.Error,
			ErrorClass: result.ErrorClass,
		}
		a.recordSubscriptionProbeLocked(ip, outcome)
	}
	a.subMu.Unlock()

	if vlessReady {
		probeCfg := vlessCfg
		probeCfg.Timeout = time.Duration(envInt("PROXY_SUBSCRIPTION_VLESS_TIMEOUT", 12)) * time.Second
		probePort := cfg.Port
		maxProbes := envInt("PROXY_SUBSCRIPTION_VLESS_MAX", 120)
		if maxProbes < 1 || maxProbes > 1000 {
			maxProbes = 120
		}
		if len(wsPass) > maxProbes {
			// 超出的这部分已经敲开过门，只是这一轮预算不够。必须把 LastProbe
			// 记上（状态仍是 pending），否则它们每轮都会排在最前重新敲一遍 WS，
			// 预算永远花在重复劳动上，后面新来的候选反而轮不到。
			for _, ip := range wsPass[maxProbes:] {
				a.subMu.Lock()
				if entry := a.subInv.Entries[ip]; entry != nil && entry.State == subStatePending {
					entry.LastProbe = now
				}
				a.subMu.Unlock()
			}
			wsPass = wsPass[:maxProbes]
		}
		// 并发跑 sing-box：每条约 12 秒，串行会拖成一条长龙。
		// 并发度刻意压到 2——每个都是一个真实进程，开太猛会把 NAS 打满。
		concurrency := envInt("PROXY_SUBSCRIPTION_VLESS_CONCURRENCY", 2)
		if concurrency < 1 || concurrency > 8 {
			concurrency = 2
		}
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		var mu sync.Mutex
		vlessCtx, vlessCancel := context.WithTimeout(parent, time.Duration(envInt("PROXY_SUBSCRIPTION_VLESS_BUDGET", 900))*time.Second)
		for _, ip := range wsPass {
			wg.Add(1)
			go func(ip string) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-vlessCtx.Done():
					return
				}
				ctx, cancel := context.WithTimeout(vlessCtx, probeCfg.Timeout)
				metrics, err := probeVLESSCandidateMetrics(ctx, ip, probePort, probeCfg, vlessTemplate)
				cancel()
				if err != nil && isLocalProbeError(err) {
					// 探针自己坏了（端口冲突耗尽、sing-box 提前退出等）：
					// 不是候选的问题，不记失败账；否则整批候选各背一次连败。
					return
				}
				mu.Lock()
				defer mu.Unlock()
				outcome := subscriptionProbeOutcome{
					At:        time.Now(),
					OK:        err == nil,
					Stage:     "vless",
					LatencyMs: survivor[ip].Latency,
					Colo:      survivor[ip].Colo,
				}
				if err != nil {
					outcome.Error = err.Error()
					outcome.ErrorClass = classifyProbeError(err)
				} else {
					outcome.Mbps = metrics.Mbps
					passed++
				}
				a.subMu.Lock()
				a.recordSubscriptionProbeLocked(ip, outcome)
				a.subMu.Unlock()
			}(ip)
		}
		wg.Wait()
		vlessCancel()
	} else {
		passed = len(wsPass)
	}

	_, stateCounts := a.subscriptionProbeSummary()
	a.subMu.Lock()
	a.subInv.LastProbeAt = time.Now()
	a.subInv.LastProbed = len(survivor)
	a.subInv.LastPassed = passed
	if err := a.saveSubscriptionInventoryLocked(); err != nil {
		log.Printf("subscription inventory save failed: %v", err)
	}
	lastErr := a.subInv.LastProbeErr
	a.subMu.Unlock()
	log.Printf("subscription probe: %d candidates -> %d ws-pass -> %d real-pass (%s); inventory pass=%d pending=%d fail=%d",
		len(candidates), len(wsPass), passed, lastErr, stateCounts[subStatePass], stateCounts[subStatePending], stateCounts[subStateFail])
	return passed, nil
}

// subscriptionProbeSummary 返回本子总数与各状态计数（供日志与接口）。
func (a *app) subscriptionProbeSummary() (int, map[string]int) {
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	counts := map[string]int{subStatePending: 0, subStatePass: 0, subStateFail: 0}
	for _, entry := range a.subInv.Entries {
		if entry == nil {
			continue
		}
		state := entry.State
		if state == "" {
			state = subStatePending
		}
		counts[state]++
	}
	return len(a.subInv.Entries), counts
}

// sweepSubscriptions 保留旧入口：一轮采集 + 一轮考试。
// 供「立即刷新」按钮与后台定时共用，内部仍是两个独立阶段。
func (a *app) sweepSubscriptions(parent context.Context) {
	if _, err := a.harvestSubscriptions(parent); err != nil {
		log.Printf("subscription harvest failed: %v", err)
	}
	if _, err := a.probeSubscriptions(parent); err != nil {
		log.Printf("subscription probe skipped: %v", err)
	}
	// 本子是候选来源，考完立刻刷新聚合快照，避免下一轮自动换池读到旧输入。
	go a.refreshAndApplyProxyPool(context.Background())
}
