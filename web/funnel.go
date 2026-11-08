package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type proxyScanConfig struct {
	IPs          string   `json:"ips"`
	Subscription string   `json:"subscription"`
	Sources      []string `json:"sources"`
	Host         string   `json:"host"`
	Port         int      `json:"port"`
	Concurrency  int      `json:"concurrency"`
	MaxLatency   int      `json:"maxLatency"`
	Limit        int      `json:"limit"`
	TLS          bool     `json:"tls"`
}

func rankVLESSPassesBySpeed(parent context.Context, passed []string, results []proxyScanResult, port int, base vlessProbeConfig, template map[string]any) []string {
	return rankVLESSPassesBySpeedWithProbe(parent, passed, results, port, base, template, probeVLESSCandidateMetrics)
}

func rankVLESSPassesBySpeedWithProbe(
	parent context.Context,
	passed []string,
	results []proxyScanResult,
	port int,
	base vlessProbeConfig,
	template map[string]any,
	probe func(context.Context, string, int, vlessProbeConfig, map[string]any) (vlessProbeMetrics, error),
) []string {
	if len(passed) == 0 {
		return passed
	}
	speedCfg := base
	speedBytes := envInt("PROXY_VLESS_SPEED_BYTES", 2097152)
	if speedBytes < 1048576 || speedBytes > 33554432 {
		speedBytes = 2097152
	}
	speedTimeout := envInt("PROXY_VLESS_SPEED_TIMEOUT", 20)
	if speedTimeout < 5 || speedTimeout > 120 {
		speedTimeout = 20
	}
	speedCfg.TestURL = vlessSpeedTestURL(speedBytes)
	speedCfg.ExpectedStatus = http.StatusOK
	speedCfg.ReadLimit = int64(speedBytes)
	speedCfg.MinBytes = int64(speedBytes)
	speedCfg.Timeout = time.Duration(speedTimeout) * time.Second
	times := make(map[string]int64, len(passed))
	speeds := make(map[string]float64, len(passed))
	minMbps := envFloat("PROXY_VLESS_MIN_DOWNLOAD_MBPS", 3)
	if minMbps < 0 || minMbps > 10000 {
		minMbps = 3
	}
	// 测速只对前 maxSpeedProbes 个候选执行：终审池上限本来就只有个位数，
	// 没必要为排序把每个候选都跑一遍 2MB 下载。
	maxSpeedProbes := len(passed)
	if maxSpeedProbes > maxVLESSSpeedProbes {
		maxSpeedProbes = maxVLESSSpeedProbes
	}
	verified := make([]string, 0, len(passed))
	for idx, ip := range passed {
		if idx < maxSpeedProbes {
			ctx, cancel := context.WithTimeout(parent, speedCfg.Timeout)
			metrics, err := probe(ctx, ip, port, speedCfg, template)
			cancel()
			if err == nil {
				for i := range results {
					if results[i].IP == ip {
						results[i].DownloadMbps = metrics.Mbps
						break
					}
				}
			}
			// 软门槛：测速只用于排序与展示，不再一票否决。
			// 数据面（204）已通过的 IP 一律保留——否则国内网络很难稳定跑到
			// 20Mbps 门槛，终审会恒为 0 通过，池子永远换不出去。
			if err == nil && metrics.Mbps >= minMbps {
				times[ip] = metrics.Duration.Milliseconds()
				speeds[ip] = metrics.Mbps
			}
			for i := range results {
				if results[i].IP == ip {
					results[i].Stage = "VLESS_PASS"
					if err != nil {
						results[i].Note = "测速未完成（软门槛，仅影响排序）：" + err.Error()
					} else if metrics.Mbps < minMbps {
						results[i].Note = fmt.Sprintf("测速 %.2f Mbps 低于参考值 %.2f Mbps（软门槛，仅影响排序）", metrics.Mbps, minMbps)
					}
					break
				}
			}
		}
		verified = append(verified, ip)
	}
	passed = verified
	sort.SliceStable(passed, func(i, j int) bool {
		leftRank := proxyResultSourceRank(results, passed[i])
		rightRank := proxyResultSourceRank(results, passed[j])
		leftSpeed, rightSpeed := speeds[passed[i]], speeds[passed[j]]
		if leftSpeed != rightSpeed {
			return leftSpeed > rightSpeed
		}
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		left, lok := times[passed[i]]
		right, rok := times[passed[j]]
		if lok && rok && left != right {
			return left < right
		}
		return proxyResultLatency(results, passed[i]) < proxyResultLatency(results, passed[j])
	})
	return passed
}

func proxyResultLatency(results []proxyScanResult, ip string) int64 {
	for _, result := range results {
		if result.IP == ip {
			return result.Latency
		}
	}
	return int64(^uint64(0) >> 1)
}

func proxyResultSourceRank(results []proxyScanResult, ip string) int {
	for _, result := range results {
		if result.IP == ip {
			return result.SourceRank
		}
	}
	return 4
}

// orderPassedByColoPreference 把过考名单按机房偏好稳定分区重排：偏好机房在前，
// 区内保持原序（VLESS 路径的 Mbps 降序、WS 路径的来源+延迟序皆不破坏）。
// 只重排、不淘汰——名单长度永不因偏好而缩短；调用方截断时「充裕取偏好、
// 匮乏放行其余」的软语义自然成立。无 colo 情报的成员归非偏好区，绝不因此被拒。
// 偏好集为空（未配置）时恒等返回——默认行为与历史版本零差异。
func orderPassedByColoPreference(passed []string, results []proxyScanResult, prefer map[string]struct{}) []string {
	if len(prefer) == 0 || len(passed) < 2 {
		return passed
	}
	coloByIP := make(map[string]string, len(results))
	for _, r := range results {
		if r.Colo != "" {
			if _, ok := coloByIP[r.IP]; !ok {
				coloByIP[r.IP] = r.Colo
			}
		}
	}
	head := make([]string, 0, len(passed))
	tail := make([]string, 0, len(passed))
	for _, ip := range passed {
		if _, ok := prefer[coloByIP[ip]]; ok {
			head = append(head, ip)
		} else {
			tail = append(tail, ip)
		}
	}
	return append(head, tail...)
}

// spreadBySegment 在保持输入顺序（延迟/机房偏好已排好）的前提下限制单段占用，
// 提升段多样性。软配额：候选不足时按原顺序补回，宁可同段也不留空位。
//
// 2026-10-22 事故：池里只有 1 个 IP、1 个段，该段一挂全灭。段多样性是抗单点
// 故障的第一道防线，优先级高于"挑延迟最低的 3 个同段 IP"。
func spreadBySegment(ips []string, poolSize int) []string {
	if poolSize <= 0 {
		return ips
	}
	if len(ips) <= 1 {
		if len(ips) > poolSize {
			return ips[:poolSize]
		}
		return ips
	}
	// 按段分组，组内保持原顺序（延迟序），组顺序按各段首次出现位置。
	order := make([]string, 0, 4)
	groups := make(map[string][]string, 4)
	pos := make(map[string]int, len(ips))
	for i, ip := range ips {
		if _, ok := pos[ip]; !ok {
			pos[ip] = i
		}
		seg := segmentOf(net.ParseIP(ip))
		if seg == "" {
			// 只有非公网/非法地址会走到这里——它们本就进不了池，此处兜底防止
			// 空串把不同来源的无效条目混成一组。
			seg = "\x00invalid"
		}
		if _, ok := groups[seg]; !ok {
			order = append(order, seg)
		}
		groups[seg] = append(groups[seg], ip)
	}
	// 轮转取：每轮每组各取一个。顺序贪心会让排在前面的段吃满名额，
	// 后面的段永远进不来——那就退化成「没有段多样性」。
	out := make([]string, 0, poolSize)
	for round := 0; len(out) < poolSize; round++ {
		progressed := false
		for _, seg := range order {
			if len(out) >= poolSize {
				break
			}
			if round < len(groups[seg]) {
				out = append(out, groups[seg][round])
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	// 取完再按原序列位置排回：池成员的顺序就是 cfnat 的尝试顺序，
	// 延迟低的仍要排在前面。
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i]] < pos[out[j]] })
	return out
}

// admitPassedUnderCeiling 把已通过终审的名单按「入池延迟上限」再裁一遍。
// 顺序不变；results 里找不到成绩的成员保守保留（调用方给的 passed 本就来自 results）。
//
// 为什么要单独抽出来：手动供给、踢人补位、在位替补、自动全量重挑是四条入口，
// 旧实现只有前三条查 800ms，自动换池按 5000ms 放行——闸门各说各话，慢 IP
// 总能从最宽的那条缝里进来。
func admitPassedUnderCeiling(passed []string, results []proxyScanResult, ceiling int64) []string {
	if ceiling <= 0 || len(passed) == 0 {
		return passed
	}
	latency := make(map[string]int64, len(results))
	for _, result := range results {
		latency[result.IP] = result.Latency
	}
	out := make([]string, 0, len(passed))
	for _, ip := range passed {
		if ms, ok := latency[ip]; ok && ms > ceiling {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// degradedFromWSResults 从 WS 扫描结果里挑出「WS 101 已通过、但没进 VLESS
// 终审通过名单」的候选，用于池子将要空掉时的降级兜底。
//
// 语义边界：只补到 want 为止，绝不越界扩池——降级是为了不空池，
// 不是为了用未验证的候选把池子填满。
func degradedFromWSResults(results []proxyScanResult, existing []string, want int) []string {
	if want <= len(existing) {
		return nil
	}
	seen := make(map[string]struct{}, len(existing))
	for _, ip := range existing {
		seen[ip] = struct{}{}
	}
	out := make([]string, 0, want-len(existing))
	for _, result := range results {
		if len(existing)+len(out) >= want {
			break
		}
		if result.Error != "" {
			continue
		}
		if _, ok := seen[result.IP]; ok {
			continue
		}
		seen[result.IP] = struct{}{}
		out = append(out, result.IP)
	}
	return out
}

func (a *app) autoApplyProxyPool(parent context.Context) {
	// 手动「立即优选」会经此路径：排队等待正在跑的考试，而不是拒绝。
	if !lockTimeout(&a.proxyScanMu, 60*time.Second) {
		log.Printf("proxy auto apply skipped: another proxy scan still running after 60s")
		a.setProxyPoolError("已有优选在运行（等待 60 秒未结束），请稍后再试", nil)
		return
	}
	defer a.proxyScanMu.Unlock()
	cfg := defaultProxyAutoConfig()
	if !cfg.Enabled || cfg.Host == "" || cfg.Path == "" {
		return
	}
	if strings.ContainsAny(cfg.Host, "/\\") || !strings.HasPrefix(cfg.Path, "/") {
		log.Printf("proxy auto apply disabled: invalid host/path")
		return
	}
	if cfg.Port < 1 || cfg.Port > 65535 || cfg.Concurrency < 1 || cfg.Concurrency > 100 || cfg.MaxLatency < 1 || cfg.MaxLatency > 30000 || cfg.PoolSize < 1 || cfg.PoolSize > 50 || cfg.MinPool < 1 || cfg.MinPool > cfg.PoolSize {
		log.Printf("proxy auto apply disabled: invalid configuration")
		return
	}
	var vlessTemplate map[string]any
	if cfg.VLESS.Enabled {
		if err := cfg.VLESS.validate(); err != nil {
			a.setProxyPoolError("VLESS 终审配置无效；保留旧池: "+err.Error(), nil)
			return
		}
		var err error
		vlessTemplate, err = loadVLESSOutboundTemplate(cfg.VLESS.TemplatePath)
		if err != nil {
			a.setProxyPoolError("VLESS 终审不可用；保留旧池: "+err.Error(), nil)
			return
		}
	}
	snapshot := a.proxyCandidateSnapshot()
	candidates := snapshot.IPs
	if len(candidates) == 0 {
		return
	}
	// ---- 三层漏斗 ----
	// 3层·初筛：全量候选做大规模 TCP/TLS RTT 测试（参考原项目 cfip/scan.go
	// 的 runRTTTest 分层思想），高并发一次过，按延迟排序取前 N。
	// 2层·精筛：仅对初筛短名单做 WS 101 精筛 + 可选 VLESS 数据面终审。
	// 1层·在位：active pool 粘性分发（cfnat -sticky）。
	// 初筛并发默认 100：CFST 实证经验——高并发探测会被运营商/CF 判为网络
	// 扫描而触发临时限制（症状：连续几轮通过数递减，停一阵自愈）。
	// 需要提速可调高，但出现递减症状时应回调（README 诊断指引）。
	prescreenConcurrency := envInt("PROXY_AUTO_PRESCREEN_CONCURRENCY", 100)
	if prescreenConcurrency < 10 || prescreenConcurrency > 1000 {
		prescreenConcurrency = 100
	}
	shortlistSize := envInt("PROXY_AUTO_SHORTLIST", 60)
	if shortlistSize < cfg.PoolSize || shortlistSize > 500 {
		shortlistSize = 60
	}
	shortlist := candidates
	if len(candidates) > shortlistSize {
		prescreenCfg := proxyScanConfig{
			Host:        cfg.Host,
			Port:        cfg.Port,
			Concurrency: prescreenConcurrency,
			MaxLatency:  cfg.MaxLatency,
			TLS:         true,
		}
		// scanProxyIPs 返回按 (通过, 延迟升序) 排列；直接取延迟达标的前 N。
		prescreen := scanProxyIPs(parent, candidates, prescreenCfg)
		shortlist = make([]string, 0, shortlistSize)
		for _, result := range prescreen {
			if result.Error != "" {
				continue
			}
			shortlist = append(shortlist, result.IP)
			if len(shortlist) >= shortlistSize {
				break
			}
		}
		log.Printf("tier3 prescreen: %d candidates -> %d shortlist (rtt<=%dms)", len(candidates), len(shortlist), cfg.MaxLatency)
	}
	results := scanProxyWebSockets(parent, shortlist, cfg)
	applyCandidateSourcePriority(results, snapshot.SourceByIP)
	passed := make([]string, 0, cfg.PoolSize)
	finalStage := "WS"
	if cfg.VLESS.Enabled {
		finalStage = "VLESS"
		passed = probeVLESSPool(parent, results, cfg.Port, min(cfg.VLESS.MaxCandidates, len(results)), cfg.VLESS, vlessTemplate)
		passed = rankVLESSPassesBySpeed(parent, passed, results, cfg.Port, cfg.VLESS, vlessTemplate)
		// 机房软偏好：Mbps 序排定后分区重排，截断时充裕取偏好、匮乏放行。
		passed = orderPassedByColoPreference(passed, results, poolColoPreferenceSet())
		// 片区降权：整片高失败率的段排到末尾（不淘汰，候选不够照样用）。
		passed = orderPassedBySegmentHealth(passed, results)
		passed = spreadBySegment(passed, cfg.PoolSize)
	} else {
		for _, result := range results {
			if result.Error == "" {
				passed = append(passed, result.IP)
			}
		}
		passed = orderPassedByColoPreference(passed, results, poolColoPreferenceSet())
		passed = orderPassedBySegmentHealth(passed, results)
		passed = spreadBySegment(passed, cfg.PoolSize)
	}
	// 入池延迟上限对「全量重挑」同样适用。旧实现只有踢人补位和在位替补查
	// 800ms，自动换池却按 PROXY_AUTO_MAX_LATENCY（现场 5000ms）放行，于是
	// 1.5~2.5 秒的 IP 也能以「WS_PASS」进池，粘性设备随即卡死。所有换池
	// 入口必须同一口径，否则闸门等于各自解释。
	if ceiling := poolEntryMaxLatencyMs(); len(passed) > 0 {
		before := len(passed)
		passed = admitPassedUnderCeiling(passed, results, ceiling)
		if dropped := before - len(passed); dropped > 0 {
			log.Printf("auto apply rejected %d candidates over the %dms entry ceiling", dropped, ceiling)
		}
	}
	// 考试结果的有效性就地复验：进池的必须是公网 IPv4 且属于 CF 边缘段。
	// 不用「候选快照有没有变」作判据——候选池是轮转采样队列，每轮重新排序，
	// 拿它的抖动去否定一个端到端验过的结论属于本末倒置（详见 filterPoolAdmissibleIPs）。
	if admitted, rejected := filterPoolAdmissibleIPs(passed); len(rejected) > 0 {
		log.Printf("proxy auto apply: dropped %d passed candidates failing pool gate: %v", len(rejected), rejected)
		passed = admitted
	}
	if len(passed) < cfg.MinPool {
		message := fmt.Sprintf("%s 终审仅 %d 个通过，低于最小池 %d", finalStage, len(passed), cfg.MinPool)
		// L3 降级：VLESS 数据面没验过，但 WS 101 已经通过 —— 说明 TCP/TLS/WS
		// 三层都通，失败点大概率在探测目标而非链路本身。池子空掉比用
		// 「未完成终审」的候选严重得多，故降级采用并明确标记。
		if cfg.VLESS.Enabled {
			if extra := degradedFromWSResults(results, passed, cfg.MinPool); len(extra) > 0 {
				passed = append(passed, extra...)
				message = fmt.Sprintf("%s；已降级采用 %d 个 WS 已通过但 VLESS 未验的候选", message, len(extra))
				log.Printf("proxy pool DEGRADED: %s", message)
			}
		}
		if len(passed) < cfg.MinPool {
			// 保留旧池之前先证明它活着，否则等于把一具尸体留在岗位上。
			// 2026-10-22 事故：建池 0 通过，旧池唯一 IP 早已 dial timeout，
			// 系统照样报「保留旧池」，对外看起来一切正常。
			alive, total := a.verifyActivePoolAlive()
			if alive > 0 {
				a.setProxyPoolError(fmt.Sprintf("%s；旧池 %d/%d 存活，继续沿用", message, alive, total), results)
				return
			}
			a.setProxyPoolError(fmt.Sprintf("%s；旧池 %d 个成员全部不可达，已无法沿用", message, total), results)
			log.Printf("proxy pool CRITICAL: 建池失败且旧池全灭（0/%d 可达），需要人工介入", total)
			return
		}
	}
	current := a.proxyActivePoolSnapshot()
	// 锁位模式是用户亲手点名「只转发这些」，自动重挑无权改动。
	if current.Lock {
		log.Printf("proxy auto apply skipped: pool lock enabled, user-pinned members only")
		return
	}
	domains := a.poolDomainMembers()
	passed, domains = carryPinnedIntoRebuild(passed, domains, current)
	now := time.Now()
	pool := proxyActivePool{
		UpdatedAt: now, Host: cfg.Host, Path: cfg.Path,
		IPs: passed, Domains: domains, Results: results,
		Priority: append([]string(nil), current.Priority...),
		Disabled: append([]string(nil), current.Disabled...),
	}
	if sameIPSet(current.IPs, passed) && sameStringSet(current.Domains, domains) {
		if err := a.saveProxyActivePool(pool); err != nil {
			log.Printf("save reverified proxy active pool: %v", err)
			return
		}
		a.activeMu.Lock()
		a.activePool = pool
		a.activeMu.Unlock()
		log.Printf("proxy active pool reverified: %d IPs", len(passed))
		return
	}
	// 全量终审换池同样受全局冷却保护，避免与 scheduler 互相顶池。
	// （残员豁免：池缩到最小数之下时 allowPoolSwitch 放行补员，见函数注释。）
	// 拦截必须写回 pool.Error——用户点的立即优选若被拦，前端靠它报信；
	// 只打容器日志等于静默吞单（生产 17:41/17:53 两次"优选完成"假捷报即此）。
	verdict := a.allowPoolSwitch(now, "auto-apply")
	if !verdict.Allowed {
		a.recordPoolBlock(verdict, "auto-apply", now)
		var reason string
		if !verdict.NextAllowedAt.IsZero() {
			reason = fmt.Sprintf("自动换池被换池闸拦截（%s），下轮放行约 %s；保留旧池", verdict.Reason, verdict.NextAllowedAt.Format(time.RFC3339))
		} else {
			reason = fmt.Sprintf("自动换池被换池闸拦截（%s）；保留旧池", verdict.Reason)
		}
		a.setProxyPoolError(reason, results)
		return
	}
	if err := a.applyProxyPool(pool, current); err != nil {
		a.setProxyPoolError("自动换池失败，保留旧池: "+err.Error(), results)
		return
	}
	a.markAutoSwitch(now)
}

func scanProxyWebSockets(parent context.Context, ips []string, cfg proxyAutoConfig) []proxyScanResult {
	results := make([]proxyScanResult, len(ips))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			started := time.Now()
			ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.MaxLatency)*time.Millisecond)
			defer cancel()
			colo, err := probeProxyWebSocket(ctx, ip, cfg)
			latency := time.Since(started).Milliseconds()
			results[i] = proxyScanResult{IP: ip, Latency: latency, Colo: colo, Stage: "WS_PASS"}
			if err != nil {
				results[i].Stage = "WS_FAIL"
				results[i].Error = err.Error()
				results[i].ErrorClass = classifyProbeError(err)
			}
		}(i, ip)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if (results[i].Error == "") != (results[j].Error == "") {
			return results[i].Error == ""
		}
		return results[i].Latency < results[j].Latency
	})
	return results
}

// measurePoolEntryLatency 三次 WS 握手取最优延迟（毫秒）、机房码与失败次数。
// 股神/CFST 多发采样思想：丢手即质量——3 测 2 失者不该与 3 测全通同等待遇，
// 失败数交由调用方记录与展示（入池闸只认「全败」与「超上限」两道硬伤）。
// 三次全败时 best=0（调用方以此判「不通」）；colo 取任一次成功握手的机房码。
func measurePoolEntryLatency(ctx context.Context, ip string, cfg proxyAutoConfig) (int64, string, int) {
	best := int64(0)
	colo := ""
	fails := 0
	for range 3 {
		started := time.Now()
		probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		c, err := probeProxyWebSocket(probeCtx, ip, cfg)
		cancel()
		if err != nil {
			fails++
			continue
		}
		if c != "" && colo == "" {
			colo = c
		}
		if ms := time.Since(started).Milliseconds(); best == 0 || ms < best {
			best = ms
		}
	}
	return best, colo, fails
}

// coloPattern 三字 IATA 机房码。CFST 同源手法（XIU2/CloudflareSpeedTest
// task/httping.go）：CF-RAY 的射线 ID 段恒为小写十六进制，全串里唯一的
// 三连大写字母即机房码（如 8f3b2c1d4e5f6a7b-HKG）。
var coloPattern = regexp.MustCompile(`[A-Z]{3}`)

// coloFromCFRay 从 CF-RAY 响应头值提取机房码；空头或无匹配返回空串。
// 空串语义为「无机房情报」——软偏好闸把它当非偏好处理，永不阻塞入池。
func coloFromCFRay(ray string) string {
	return coloPattern.FindString(strings.TrimSpace(ray))
}

func probeProxyWebSocket(ctx context.Context, ip string, cfg proxyAutoConfig) (string, error) {
	dialer := &net.Dialer{}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
	if err != nil {
		return "", err
	}
	defer raw.Close()
	tlsConn := tls.Client(raw, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return "", err
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+cfg.Host+cfg.Path, nil)
	if err != nil {
		return "", err
	}
	req.Host = cfg.Host
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("User-Agent", "cloudflare-tools-auto-probe")
	if err := req.Write(tlsConn); err != nil {
		return "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return "", fmt.Errorf("WS 状态码 %d", resp.StatusCode)
	}
	return coloFromCFRay(resp.Header.Get("CF-RAY")), nil
}

type proxyScanResult struct {
	IP           string  `json:"ip"`
	Latency      int64   `json:"latency"`
	Colo         string  `json:"colo,omitempty"`
	DataLatency  int64   `json:"dataLatency,omitempty"`
	DownloadMbps float64 `json:"downloadMbps,omitempty"`
	SourceRank   int     `json:"sourceRank,omitempty"`
	Stage        string  `json:"stage,omitempty"`
	Error        string  `json:"error,omitempty"`
	// ErrorClass 是 Error 的归类（timeout/refused/unreachable/handshake/protocol/other）。
	// 原始报错文本随版本平台变化，直接统计没法用；归类后才能看出「整片被掐」
	// 与「个别地址没了」的区别。
	ErrorClass string `json:"errorClass,omitempty"`
	Note       string `json:"note,omitempty"`
}

func normalizeProxyScan(c proxyScanConfig) (proxyScanConfig, error) {
	auto := defaultProxyAutoConfig()
	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" {
		// 全自动化：手动优选未填 SNI 时直接用 PROXY_AUTO_HOST，用户无需理解协议细节。
		c.Host = auto.Host
	}
	if c.Host == "" {
		return c, errors.New("SNI/Host 未配置（请在 .env 设置 PROXY_AUTO_HOST）")
	}
	if c.Port == 0 {
		c.Port = auto.Port
	}
	if c.Port < 1 || c.Port > 65535 {
		return c, errors.New("端口必须在 1-65535")
	}
	if c.Concurrency == 0 {
		c.Concurrency = 50
	}
	if c.Concurrency < 1 || c.Concurrency > 500 {
		return c, errors.New("并发必须在 1-500")
	}
	if c.MaxLatency == 0 {
		c.MaxLatency = 300
	}
	if c.MaxLatency < 1 || c.MaxLatency > 10000 {
		return c, errors.New("延迟上限必须在 1-10000 ms")
	}
	if c.Limit == 0 {
		c.Limit = 100
	}
	if c.Limit < 1 || c.Limit > 10000 {
		return c, errors.New("扫描数量必须在 1-10000")
	}
	return c, nil
}

func scanProxyIPs(parent context.Context, ips []string, cfg proxyScanConfig) []proxyScanResult {
	results := make([]proxyScanResult, len(ips))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			started := time.Now()
			timeout := time.Duration(cfg.MaxLatency+1000) * time.Millisecond
			if timeout < 2*time.Second {
				timeout = 2 * time.Second
			}
			dialer := &net.Dialer{Timeout: timeout}
			ctx, cancel := context.WithTimeout(parent, timeout)
			defer cancel()
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
			if err == nil && cfg.TLS {
				tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
				err = tlsConn.HandshakeContext(ctx)
			}
			if conn != nil {
				_ = conn.Close()
			}
			latency := time.Since(started).Milliseconds()
			results[i] = proxyScanResult{IP: ip, Latency: latency, Stage: "TLS_PASS"}
			if err != nil {
				results[i].Stage = "TLS_FAIL"
				results[i].Error = err.Error()
				results[i].ErrorClass = classifyProbeError(err)
				return
			}
			if latency > int64(cfg.MaxLatency) {
				results[i].Error = fmt.Sprintf("超过延迟上限 %d ms", cfg.MaxLatency)
			}
		}(i, ip)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if (results[i].Error == "") != (results[j].Error == "") {
			return results[i].Error == ""
		}
		return results[i].Latency < results[j].Latency
	})
	return results
}
