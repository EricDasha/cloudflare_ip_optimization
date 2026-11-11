package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 优选域名体检（Domain Probe）
//
// 由来：优选域名是 cfnat -fixed 的一等公民，拨号时按当前 DNS 活解析。IP 会随机
// 切换、域名对谁解析成哪台边缘并不固定，所以「测某个 IP 有多快」意义有限——
// 真正要回答的是「这个域名此刻当入口，跑我这条隧道有多快、稳不稳」。
//
// 因此本链不再只做 TCP:443 握手，而是把域名喂进 VLESS 探针的 server 字段：
// TLS 的 server_name 与 WS 的 Host 仍指向我们自己的 Worker 域名，只有入口换成
// 被考域名。测到的就是「该域名此刻的入口质量」，与 cfnat 的真实拨号路径一致。
//
// 只读体检：绝不动池、绝不把域名解析成 IP 快照，域名仍是活解析成员。

const (
	domainQualityFile = "domain-quality.json"

	// domainQualitySampleCap 保留最近 N 轮样本：30 分钟一轮 ≈ 6 小时窗口，
	// 足以分辨「偶发慢」与「持续抖」。
	domainQualitySampleCap  = 12
	domainQualityMinSamples = 3

	domainProbeDefaultIntervalSeconds = 1800 // 30 分钟
	domainProbeMinIntervalSeconds     = 300
	domainProbeMaxIntervalSeconds     = 7200

	domainProbeDefaultTimeoutMs = 3000
	domainProbeMinTimeoutMs     = 500
	domainProbeMaxTimeoutMs     = 10000

	// domainProbeConcurrency 同时测几个域名。压低到 3 是为了让每个域名拿到
	// 公平的带宽份额——并发太高会互相抢吞吐，测出来的数字失去可比性。
	domainProbeConcurrency = 3

	// domainSpeedDefaultURL 是域名测速的下载端点。
	//
	// 必须是 **非 Cloudflare 目标**。2026-10-20 在 NAS 上实测：这条
	// Worker+ProxyIP 链路对 gstatic/generate_204（非 CF）0.54 秒即可完成，
	// 而 speed.cloudflare.com 与 cp.cloudflare.com（CF 目标）即便只取 64 KB，
	// 也是 20–60 秒零字节。默认端点原为 speed.cloudflare.com，于是每一轮
	// 每一条都超时，账本里全是 0 —— 测速功能形同虚设。
	// 换成 proof.ovh.net 后同一条隧道 1 MB 用 2.88 秒（约 2.9 Mbps）。
	//
	// 代价：测到的是「入口 + Worker + ProxyIP + 目标」的端到端质量，而非纯入口
	// 质量。但这正是用户真实感受到的那条路，故仍是更诚实的指标。
	// 若上游以后修好 CF 目标，可用 PROXY_DOMAIN_SPEED_URL 换回。
	domainSpeedDefaultURL = "https://proof.ovh.net/files/10Mb.dat"

	// 只读到 domainSpeedDefaultBytes 即止：文件本身是 10 MB，靠 io.LimitReader
	// 截断，实际流量仍只有这么多。
	domainSpeedDefaultBytes          = 256 * 1024
	domainSpeedMinBytes              = 64 * 1024
	domainSpeedMaxBytes              = 8 * 1024 * 1024
	domainSpeedDefaultTimeoutSeconds = 20
	domainSpeedMinTimeoutSeconds     = 3
	domainSpeedMaxTimeoutSeconds     = 60

	// domainQualityMinBytes 低于此值不足以判断速率（可能只是握手回包）。
	domainQualityMinBytes = 64 * 1024

	domainStateHealthy  = "healthy"
	domainStateUnstable = "unstable"
	domainStateDead     = "dead"
	domainStateTesting  = "testing"

	// domainJitterThreshold 抖动阈值：P95/P50 超过此值即判不稳。
	domainJitterThreshold = 1.8
	// domainSuccessFloor 成功率下限：低于此值即判不稳。
	domainSuccessFloor = 0.6
	// domainDeadFailures 连续失败达几次判死。
	domainDeadFailures = 3
	// domainQuarantine 域名「下线观察期」。
	//
	// 在位验活判死的域名会被剔出 -fixed，但 reconcilePoolDomains 每轮体检后都会
	// 按 poolDomainMembers() 把域名集合整个重算一遍——没有这道闸，被判死的域名
	// 会在下一轮被原样加回来，剔除等于白做。观察期内禁止自动回池，到期自动放行
	// 再考一次；再死再关。与质量闸不同，它不依赖体检账本，而是验活的直接结论。
	domainQuarantine = 30 * time.Minute
	// domainRecoveryStreak 连续成功达几次即认定「已经恢复」，并丢弃之前的旧账。
	//
	// 2026-10-21 生产事故：上游链路中断约 3 小时，账本把这 12 个域名全打成
	// unstable(0.25)。链路恢复后按 12 样本窗口回算，需要连续 8 轮成功
	// （≈4 小时）才能越过 0.6 下限——一场 3 小时的故障要用 4 小时的旧账继续
	// 惩罚已经健康的域名，这是错的。连续 3 轮成功（≈90 分钟）已是充分的
	// 现时证据，故达到即清空历史，从当下重新记账。
	domainRecoveryStreak = 3
)

func domainProbeEnabled() bool { return envBool("PROXY_DOMAIN_PROBE_ENABLED", true) }

// domainProbeInterval 探测间隔，钳到 [5, 120] 分钟，越界回落 30 分钟。
func domainProbeInterval() time.Duration {
	seconds := envInt("PROXY_DOMAIN_PROBE_INTERVAL_SECONDS", domainProbeDefaultIntervalSeconds)
	if seconds < domainProbeMinIntervalSeconds || seconds > domainProbeMaxIntervalSeconds {
		seconds = domainProbeDefaultIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

// domainProbeTimeout 单次 TCP 拨号超时，钳到 [500, 10000] 毫秒，越界回落 3000。
func domainProbeTimeout() time.Duration {
	ms := envInt("PROXY_DOMAIN_PROBE_TIMEOUT_MS", domainProbeDefaultTimeoutMs)
	if ms < domainProbeMinTimeoutMs || ms > domainProbeMaxTimeoutMs {
		ms = domainProbeDefaultTimeoutMs
	}
	return time.Duration(ms) * time.Millisecond
}

// domainProbeSample 一轮体检的成绩：先 TCP 快检，再走 VLESS 短时下载。
type domainProbeSample struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Stage string    `json:"stage,omitempty"` // tcp / tcp-only / speed
	TCPMs int64     `json:"tcpMs,omitempty"`
	Mbps  float64   `json:"mbps,omitempty"`
	Bytes int64     `json:"bytes,omitempty"`
	Error string    `json:"error,omitempty"`
}

// domainQuality 单个优选域名的稳定性账。
// 与 qualityRecord（IP 用）分开存：域名不带端口、按 DNS 活解析，生命周期与 IP 不同。
type domainQuality struct {
	Domain               string              `json:"domain"`
	Samples              []domainProbeSample `json:"samples,omitempty"`
	SuccessRate          float64             `json:"successRate"`
	AverageMbps          float64             `json:"averageMbps"`
	P50Mbps              float64             `json:"p50Mbps"`
	P95Mbps              float64             `json:"p95Mbps"`
	JitterRatio          float64             `json:"jitterRatio"`
	ConsecutiveFailures  int                 `json:"consecutiveFailures"`
	ConsecutiveSuccesses int                 `json:"consecutiveSuccesses,omitempty"`
	LastProbeAt          time.Time           `json:"lastProbeAt,omitempty"`
	LastOKAt             time.Time           `json:"lastOkAt,omitempty"`
	State                string              `json:"state"`
}

type domainQualityStore struct {
	Version   int                       `json:"version"`
	UpdatedAt time.Time                 `json:"updatedAt"`
	Records   map[string]*domainQuality `json:"records"`
}

// domainLatency 单个优选域名的当前视图（UI 直接消费）。
// Ms 仍是 TCP:443 握手耗时；Mbps / SuccessRate / JitterRatio / State 来自稳定性账。
type domainLatency struct {
	Ms                  int64     `json:"ms"`
	OK                  bool      `json:"ok"`
	At                  time.Time `json:"at"`
	Error               string    `json:"error,omitempty"`
	Mbps                float64   `json:"mbps,omitempty"`
	SuccessRate         float64   `json:"successRate,omitempty"`
	JitterRatio         float64   `json:"jitterRatio,omitempty"`
	State               string    `json:"state,omitempty"`
	ConsecutiveFailures int       `json:"consecutiveFailures,omitempty"`
	Stage               string    `json:"stage,omitempty"`
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (a *app) loadDomainQuality() {
	data, err := os.ReadFile(filepath.Join(a.dataDir, domainQualityFile))
	if err != nil {
		return
	}
	var stored domainQualityStore
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	if stored.Records == nil {
		return
	}
	a.domainQualityMu.Lock()
	a.domainQuality = stored.Records
	a.domainQualityMu.Unlock()
	// 老账立即回填 UI 视图，重启后不必空等一轮。
	a.refreshDomainLatencyViewLocked()
}

func (a *app) saveDomainQualityLocked() error {
	store := domainQualityStore{Version: 1, UpdatedAt: time.Now(), Records: a.domainQuality}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, domainQualityFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// refreshDomainLatencyViewLocked 把稳定性账折叠成 UI 视图。调用方须持 domainQualityMu。
func (a *app) refreshDomainLatencyViewLocked() {
	view := make(map[string]domainLatency, len(a.domainQuality))
	for domain, rec := range a.domainQuality {
		if rec == nil {
			continue
		}
		last, hasLast := rec.lastSample()
		view[domain] = domainLatency{
			Ms:                  rec.lastTCPMs(),
			OK:                  hasLast && last.OK,
			At:                  rec.LastProbeAt,
			Error:               last.Error,
			Stage:               last.Stage,
			Mbps:                rec.AverageMbps,
			SuccessRate:         rec.SuccessRate,
			JitterRatio:         rec.JitterRatio,
			State:               rec.State,
			ConsecutiveFailures: rec.ConsecutiveFailures,
		}
	}
	a.domainLatencyMu.Lock()
	a.domainLatency = view
	a.domainLatencyMu.Unlock()
}

// lastSample 返回最近一轮样本（可能为空）。
func (q *domainQuality) lastSample() (domainProbeSample, bool) {
	if len(q.Samples) == 0 {
		return domainProbeSample{}, false
	}
	return q.Samples[len(q.Samples)-1], true
}

// lastTCPMs 取最近一次 TCP 握手耗时（样本里可能混有纯测速轮次）。
func (q *domainQuality) lastTCPMs() int64 {
	for i := len(q.Samples) - 1; i >= 0; i-- {
		if q.Samples[i].TCPMs > 0 {
			return q.Samples[i].TCPMs
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// 探测
// ---------------------------------------------------------------------------

// domainSpeedProbeURL 决定域名测速的下载地址。支持 {bytes} 占位符：
// 若所选端点能按字节数供数，占位符会被替换；否则原样使用，靠 ReadLimit 截断。
func domainSpeedProbeURL(size int) string {
	candidate := strings.TrimSpace(env("PROXY_DOMAIN_SPEED_URL", domainSpeedDefaultURL))
	if candidate == "" {
		candidate = domainSpeedDefaultURL
	}
	if !strings.Contains(candidate, "{bytes}") {
		return candidate
	}
	return strings.ReplaceAll(candidate, "{bytes}", strconv.Itoa(size))
}

// domainSpeedProbeConfig 组装域名测速用的 VLESS 配置。
//
// 与 IP 探针的差别只在「测什么」：这里拉的是非 CF 定长文件的头部若干字节，
// 字节数由 PROXY_DOMAIN_SPEED_BYTES 控制（默认 256 KiB）。返回模板错误时
// 调用方降级为纯 TCP 体检——宁可只报可达性，也不冤枉好域名。
func (a *app) domainSpeedProbeConfig() (vlessProbeConfig, map[string]any, error) {
	base := defaultVLESSProbeConfig()
	bytes := envInt("PROXY_DOMAIN_SPEED_BYTES", domainSpeedDefaultBytes)
	if bytes < domainSpeedMinBytes || bytes > domainSpeedMaxBytes {
		bytes = domainSpeedDefaultBytes
	}
	timeoutSeconds := envInt("PROXY_DOMAIN_SPEED_TIMEOUT_SECONDS", domainSpeedDefaultTimeoutSeconds)
	if timeoutSeconds < domainSpeedMinTimeoutSeconds || timeoutSeconds > domainSpeedMaxTimeoutSeconds {
		timeoutSeconds = domainSpeedDefaultTimeoutSeconds
	}
	cfg := vlessProbeConfig{
		Enabled:        true,
		Binary:         base.Binary,
		TemplatePath:   base.TemplatePath,
		TestURL:        domainSpeedProbeURL(bytes),
		ExpectedStatus: http.StatusOK,
		Timeout:        time.Duration(timeoutSeconds) * time.Second,
		MaxCandidates:  1,
		ReadLimit:      int64(bytes),
		MinBytes:       domainQualityMinBytes,
	}
	template, err := loadVLESSOutboundTemplate(cfg.TemplatePath)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, template, nil
}

// domainProbeTargets 决定每轮体检覆盖哪些域名。
//
// 不能只取生效池：质量闸把判死的域名挡在池外后，池里就再也没有它，
// 于是它永远得不到新样本、永远无法翻身。故取「池内 + 全部自动候选」的并集，
// 让被拦下的域名仍在考试名单上，成绩好转即可自动回池。
func (a *app) domainProbeTargets() []string {
	seen := map[string]struct{}{}
	domains := make([]string, 0, maxForwardDomains*2)
	add := func(domain string) {
		if !validCandidateHostname(domain) {
			return
		}
		if _, ok := seen[domain]; ok {
			return
		}
		seen[domain] = struct{}{}
		domains = append(domains, domain)
	}
	for _, domain := range a.proxyActivePoolSnapshot().Domains {
		add(domain)
	}
	snapshot := a.proxyCandidateSnapshot()
	for _, domain := range snapshot.ForwardDomains {
		add(domain)
	}
	return domains
}

// domainProbePathBrokenThreshold 体检通路故障的判定阈值：
// 一轮里 DNS 解析失败 + TCP 建连失败合计占比达到该值、且无一例测速成功，
// 即判定为体检通路故障而非域名本身死亡（见 classifyDomainProbeRound）。
const domainProbePathBrokenThreshold = 0.7

// domainProbeRoundStats 是一轮域名体检的失败分账：DNS 解析失败、TCP 建连
// 失败、测速失败三者分开记——只有分开，才能区分「通路坏了」与「域名死了」。
// 旧实现只看「测速成功数是否为 0」，DNS/TCP 全灭时仍把失败记进账本，
// 于是新域名永远停在 state=""，质量闸一律放行，坏域名被整批塞回池。
type domainProbeRoundStats struct {
	Total        int
	DNSFailed    int
	TCPFailed    int
	SpeedFailed  int
	SpeedOK      int
	TCPReachable int // Stage=speed 的样本数：真正走到了测速阶段
}

// classifyDomainProbeRound 按样本的失败阶段分账。Stage 由 probeOneDomain
// 在失败点如实填写：dns=解析不到，tcp=解析到了但建连失败，speed=建连成功
// 但测速失败。纯函数，便于单测钉死阈值边界。
func classifyDomainProbeRound(samples map[string]domainProbeSample) domainProbeRoundStats {
	stats := domainProbeRoundStats{Total: len(samples)}
	for _, sample := range samples {
		switch {
		case sample.OK && sample.Stage == "speed":
			stats.SpeedOK++
			stats.TCPReachable++
		case sample.OK:
			// tcp-only 降级样本：只报可达性，没进测速阶段，不计入任一失败账。
		case sample.Stage == "speed":
			stats.SpeedFailed++
			stats.TCPReachable++
		case sample.Stage == "dns":
			stats.DNSFailed++
		default:
			// Stage=tcp 或未知：解析到了（或无法区分）但建连失败。
			stats.TCPFailed++
		}
	}
	return stats
}

// pathBroken 判定本轮是否为体检通路故障：解析/建连失败占大头且无一例
// 测速成功。测速失败（Stage=speed）不计入分子——解析成功、建连成功、
// 测速失败的才是域名本身的问题。
func (s domainProbeRoundStats) pathBroken() bool {
	if s.Total == 0 || s.SpeedOK > 0 {
		return false
	}
	unreachable := s.DNSFailed + s.TCPFailed
	return float64(unreachable) >= domainProbePathBrokenThreshold*float64(s.Total)
}

// probeActiveDomainLatencies 给生效池里的每个优选域名做一轮体检：
// TCP:443 快检 → VLESS 短时下载 → 写稳定性账。
// 返回本轮是否为体检通路故障：是则调用方应跳过 reconcilePoolDomains，
// 保持生效池域名集合不变——通路坏时体检结论不可信，不能据此换池。
func (a *app) probeActiveDomainLatencies(ctx context.Context) bool {
	domains := a.domainProbeTargets()
	if len(domains) == 0 {
		return false
	}

	speedCfg, template, speedErr := a.domainSpeedProbeConfig()
	if speedErr != nil {
		log.Printf("domain speed probe degraded to TCP-only: %v", speedErr)
	}

	timeout := domainProbeTimeout()
	samples := make(map[string]domainProbeSample, len(domains))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, domainProbeConcurrency)
	for _, domain := range domains {
		wg.Add(1)
		sem <- struct{}{}
		go func(domain string) {
			defer wg.Done()
			defer func() { <-sem }()
			_, sample := a.probeOneDomain(ctx, domain, timeout, speedCfg, template, speedErr == nil)
			mu.Lock()
			samples[domain] = sample
			mu.Unlock()
		}(domain)
	}
	wg.Wait()
	return a.recordDomainProbeResults(samples)
}

// probeOneDomain 单域名体检。TCP 不通即失败；TCP 通但测速失败仍记失败，
// 但保留 TCP 延迟，便于区分「解析不到」与「能连上但跑不动」。
//
// TODO(改动 C，已评估，暂不做)：本函数解析域名后直连得到的 CF 前置 IP，
// 走的正是可能已坏的出口，体检必然零样本。曾评估改为经 PROXY_AUTO_HOST
// 的 Worker 路径做健康判定，但那会把「域名入口质量」换成「Worker 链路
// 质量」，且要重排 probeVLESSCandidateMetrics 的 server/Host 拼装与测速
// 端点选择，改动面远超 30 行——按交接单要求放弃，留此 TODO，不重构。
func (a *app) probeOneDomain(ctx context.Context, domain string, timeout time.Duration, cfg vlessProbeConfig, template map[string]any, speedEnabled bool) (domainLatency, domainProbeSample) {
	started := time.Now()
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(domain, "443"))
	tcpMs := time.Since(started).Milliseconds()
	view := domainLatency{Ms: tcpMs, At: time.Now(), Stage: "tcp"}
	sample := domainProbeSample{At: view.At, Stage: "tcp", TCPMs: tcpMs}
	if err != nil {
		view.Error = err.Error()
		sample.Error = err.Error()
		// DNS 解析失败与 TCP 建连失败必须分开记：前者是解析通路问题，
		// 后者是出口/目标问题；混在一起，recordDomainProbeResults
		// 就无法判定「体检通路故障」。
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			view.Stage = "dns"
			sample.Stage = "dns"
		}
		return view, sample
	}
	_ = conn.Close()

	if !speedEnabled {
		// 降级：模板/二进制不可用，只报可达性，不判死。
		view.OK = true
		view.State = domainStateTesting
		view.Stage = "tcp-only"
		sample.OK = true
		sample.Stage = "tcp-only"
		return view, sample
	}

	probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	metrics, err := probeVLESSCandidateMetrics(probeCtx, domain, 443, cfg, template)
	if err != nil {
		view.Error = err.Error()
		view.Stage = "speed"
		sample.Stage = "speed"
		sample.Error = err.Error()
		return view, sample
	}
	view.OK = true
	view.Mbps = metrics.Mbps
	view.Stage = "speed"
	sample.OK = true
	sample.Stage = "speed"
	sample.Mbps = metrics.Mbps
	sample.Bytes = metrics.Bytes
	return view, sample
}

// speedPathWarnRounds 连续多少轮「TCP 可达、测速全零」就判定测速通路可疑。
//
// 这不是新机制，只是把 recordDomainProbeResults 里已经存在的那道保险
// （整轮零成绩不记连败）从「悄悄跳过」升级为「能被看见」：以前端点挂了，
// 系统默默不再更新速度，页面上一切正常；现在会明确报出「测速通路坏了」。
const speedPathWarnRounds = 3

// speedPathStat 是测速通路自检账：记的是「测速这件事本身还能不能做」，
// 不是某个域名的好坏。数据来自每轮体检的实际结果，不做额外探测——
// 端点只在隧道里可达与否，脱离隧道单独 curl 得出的「可用」是假象。
type speedPathStat struct {
	LastRound time.Time `json:"lastRound,omitempty"`
	// Domains 是本轮体检的域名数；SpeedOK 是真正测出速度的数量。
	Domains int `json:"domains"`
	SpeedOK int `json:"speedOk"`
	// TCPOK 是「本轮真正走到测速阶段」的数量：只有 Stage=speed 才计。
	// 用它区分「域名都连不上」与「能连上但测速通路跑不动」——
	// 只有后者才说明测速端点或本地探针坏了；模板降级（tcp-only）不算。
	TCPOK int `json:"tcpOk"`
	// ZeroRounds 是连续「TCPOK>0 且 SpeedOK==0」的轮数；中间有一轮
	// TCP 全灭或未进测速阶段，计数归零，保证「连续 N 轮」说法成立。
	ZeroRounds int       `json:"zeroRounds"`
	LastOK     time.Time `json:"lastOk,omitempty"`
	Suspect    bool      `json:"suspect"`
	// Reason 只在 Suspect 为真时非空，直接给人看。
	Reason string `json:"reason,omitempty"`
}

func (a *app) recordSpeedPathRound(now time.Time, domains, speedOK, tcpOK int) {
	a.speedPathMu.Lock()
	defer a.speedPathMu.Unlock()
	s := &a.speedPath
	s.LastRound = now
	s.Domains = domains
	s.SpeedOK = speedOK
	s.TCPOK = tcpOK
	if speedOK > 0 {
		s.LastOK = now
		s.ZeroRounds = 0
		if s.Suspect {
			log.Printf("domain speed path recovered: %d/%d domains produced speed samples", speedOK, domains)
		}
		s.Suspect = false
		s.Reason = ""
		return
	}
	if tcpOK == 0 {
		// 本轮要么域名全不可达，要么模板降级根本没进测速阶段。
		// 这时的零成绩不能算到端点头上；同时清零连续计数，
		// 否则跨一轮断档后再累计，页面上的「连续 N 轮」就是失实的。
		s.ZeroRounds = 0
		return
	}
	s.ZeroRounds++
	if s.ZeroRounds >= speedPathWarnRounds {
		if !s.Suspect {
			log.Printf("domain speed path suspect: %d consecutive rounds TCP-reachable but zero speed samples (endpoint %s)", s.ZeroRounds, domainSpeedProbeURL(domainSpeedDefaultBytes))
		}
		s.Suspect = true
		// 每轮重写，避免停在阈值那一轮的数字上。
		s.Reason = "连续 " + strconv.Itoa(s.ZeroRounds) + " 轮 TCP 可达但测速全零：测速端点或隧道疑似失效"
	}
}

func (a *app) speedPathStatus() speedPathStat {
	a.speedPathMu.Lock()
	defer a.speedPathMu.Unlock()
	return a.speedPath
}

// recordDomainProbeResults 把本轮样本记入稳定性账，重算统计与状态，并回填 UI 视图。
// 返回本轮是否为体检通路故障：是则调用方应保持生效池不变（见 probeActiveDomainLatencies）。
//
// 一道刻意的保险：只有当本轮至少有一个域名测速成功时，失败才计入连败。
// 若整轮无一人测出速度，那更可能是测速通路本身坏了（模板失效、sing-box 缺位、
// 上游测速端点不可达），而不是十二个域名同时死。此时累加连败会把整个域名池
// 打成 dead，「只开活跃」随即清空——那是数据丢失级的方向性错误。
// 单域名真死仍能被识别：只要同轮里还有同伴成功，它的失败就照常计数。
//
// 另一道保险（改动 A）：DNS 解析失败 + TCP 建连失败合计占比达到阈值
// （domainProbePathBrokenThreshold）且无一例测速成功时，同样判定为通路
// 故障——本轮失败样本不入账、不累计连败、不重算状态。旧实现只认「测速零
// 样本 + TCP 可达」，DNS/TCP 全灭时失败照样入账，新域名永远停在 state=""
// （质量闸一律放行），坏域名在下一轮 reconcile 时被整批塞回池。
func (a *app) recordDomainProbeResults(samples map[string]domainProbeSample) bool {
	now := time.Now()
	stats := classifyDomainProbeRound(samples)
	speedSuccess := stats.SpeedOK
	tcpReachable := stats.TCPReachable
	a.recordSpeedPathRound(now, len(samples), speedSuccess, tcpReachable)
	probeHealthy := speedSuccess > 0
	// 「域名能连上、但没有一个测出速度」才是测速通路本身的可疑形态。
	// 若连 TCP 都不通，不能把责任推给端点，仍按旧口径记失败，
	// 否则域名真的整组死掉时反而永远清不掉。
	probePathBroken := !probeHealthy && tcpReachable > 0
	// DNS/TCP 失败占大头的通路故障：与测速通路故障同等对待。
	roundPathBroken := stats.pathBroken()
	if roundPathBroken {
		log.Printf("domain probe path broken: %d/%d unreachable (dns %d, tcp %d), pool unchanged", stats.DNSFailed+stats.TCPFailed, len(samples), stats.DNSFailed, stats.TCPFailed)
	}
	pathBroken := probePathBroken || roundPathBroken
	if !probeHealthy && len(samples) > 0 && !roundPathBroken {
		log.Printf("domain probe: no domain produced a speed sample this round (%d domains), failures not counted toward dead", len(samples))
	}

	a.domainQualityMu.Lock()
	for domain, sample := range samples {
		// 整轮无成绩（pathBroken）时，失败的样本连账都不入。
		// 只挡住 ConsecutiveFailures 不够：SuccessRate 是按时段样本算的，
		// 端点坏三轮就会把好域名拖成 unstable，随后被移出转发——
		// 测速端点坏了，反倒把域名池清空。
		// 没入账的样本也不建记录、不刷新 LastProbeAt：否则账本凭空多出
		// 一批 state="" 的空记录（闸门对空状态一律放行，下轮照样回池），
		// 已有记录的新鲜度还会被没测过的轮次刷新。
		if !sample.OK && pathBroken {
			continue
		}
		rec := a.domainQuality[domain]
		if rec == nil {
			rec = &domainQuality{Domain: domain}
			a.domainQuality[domain] = rec
		}
		rec.Samples = append(rec.Samples, sample)
		if len(rec.Samples) > domainQualitySampleCap {
			rec.Samples = rec.Samples[len(rec.Samples)-domainQualitySampleCap:]
		}
		rec.LastProbeAt = now
		if sample.OK {
			rec.ConsecutiveFailures = 0
			rec.ConsecutiveSuccesses++
			rec.LastOKAt = now
		} else if probeHealthy {
			rec.ConsecutiveFailures++
			rec.ConsecutiveSuccesses = 0
		}
		rec.recalculate()
	}
	// 已移出池的域名不再占据成绩册。
	for domain := range a.domainQuality {
		if _, ok := samples[domain]; !ok {
			delete(a.domainQuality, domain)
		}
	}
	a.refreshDomainLatencyViewLocked()
	_ = a.saveDomainQualityLocked()
	a.domainQualityMu.Unlock()

	a.domainLatencyMu.Lock()
	a.domainLatencyLast = now
	a.domainLatencyMu.Unlock()
	return pathBroken
}

// recalculate 从最近样本重算成功率、速度分位与抖动系数。
//
// 抖动用 P95/P50（相对倍数），比绝对差值更能横向比较不同速率的域名：
// 10 Mbps 的线在 15 Mbps 波动，与 100 Mbps 的线在 150 Mbps 波动，体感同样是「不稳」。
func (q *domainQuality) recalculate() {
	// 连续成功已足够多 → 之前的失败属于已过去的事件，先清空再按现时证据重算。
	// 不清空的话，一场 3 小时的上游中断会让域名在恢复后仍被判不稳约 4 小时。
	if q.ConsecutiveSuccesses >= domainRecoveryStreak && len(q.Samples) > domainRecoveryStreak {
		q.Samples = q.Samples[len(q.Samples)-domainRecoveryStreak:]
	}
	if len(q.Samples) == 0 {
		q.State = domainStateTesting
		return
	}
	speeds := make([]float64, 0, len(q.Samples))
	successes := 0
	for _, sample := range q.Samples {
		if sample.OK {
			successes++
		}
		if sample.OK && sample.Mbps > 0 {
			speeds = append(speeds, sample.Mbps)
		}
	}
	q.SuccessRate = float64(successes) / float64(len(q.Samples))

	if len(speeds) == 0 {
		q.AverageMbps, q.P50Mbps, q.P95Mbps, q.JitterRatio = 0, 0, 0, 0
	} else {
		sort.Float64s(speeds)
		var total float64
		for _, value := range speeds {
			total += value
		}
		q.AverageMbps = total / float64(len(speeds))
		q.P50Mbps = speeds[len(speeds)/2]
		q.P95Mbps = speeds[int(float64(len(speeds)-1)*0.95)]
		if q.P50Mbps > 0 {
			q.JitterRatio = q.P95Mbps / q.P50Mbps
		}
	}

	if len(q.Samples) < domainQualityMinSamples {
		q.State = domainStateTesting
		return
	}
	// 没有一次真正测过速度（全是 tcp-only 降级样本）时，状态封顶在
	// testing：只凭 TCP 通就判 healthy，会让从未测速的域名通过质量闸。
	// probeOneDomain 的注释也只说 tcp-only「只报可达性，不判死」。
	if len(speeds) == 0 {
		q.State = domainStateTesting
		return
	}
	switch {
	case q.ConsecutiveFailures >= domainDeadFailures:
		q.State = domainStateDead
	case q.SuccessRate < domainSuccessFloor:
		q.State = domainStateUnstable
	case q.JitterRatio > domainJitterThreshold:
		q.State = domainStateUnstable
	default:
		q.State = domainStateHealthy
	}
}

// runDomainLatencyProbeLoop 域名体检主循环：启动后 45 秒首探（避开 cfnat 自启），
// 随后每 interval 一轮。
func (a *app) runDomainLatencyProbeLoop() {
	if !domainProbeEnabled() {
		return
	}
	initial := time.NewTimer(45 * time.Second)
	defer initial.Stop()
	<-initial.C
	for {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), domainProbeInterval())
			defer cancel()
			// 体检通路故障时不 reconcile：体检结论不可信，本轮保持
			// 生效池域名集合不变（改动 A），否则未验证域名会被整批塞回池。
			if broken := a.probeActiveDomainLatencies(ctx); !broken {
				// 体检结论必须能改变生效池，否则闸门只剪不补。
				a.reconcilePoolDomains()
			}
		}()
		timer := time.NewTimer(domainProbeInterval())
		<-timer.C
		timer.Stop()
	}
}

// reconcilePoolDomains 让生效池的域名集合跟上体检结论。
//
// 只在集合真的变化时才重启 cfnat，且 IP 一个不动，因此刻意绕过换池冷却与
// 「现池健康就别顶」两道闸——它们防的是 IP 抖动，与域名准入无关。
//
// 没有这条回路，启动时被闸门剪掉的坏域名会永久缺席：现池 IP 健康 → 自动换池
// 被拦 → poolDomainMembers() 再也不被求值 → 体检恢复的域名永远回不来。
// 域名集合在生产线上一律由 poolDomainMembers() 派生（PROXY_DOMAIN_FORWARD +
// bestcf 优选域名表 + 已启用优选域名），此处重算与建池口径一致。
func (a *app) reconcilePoolDomains() {
	current := a.proxyActivePoolSnapshot()
	want := a.poolDomainMembers()
	// 用户固定的域名必须留在池里。
	//
	// poolDomainMembers() 只认「显式配置 + 自动候选 + 已启用优选域名」这三个来源，
	// 而用户在页面上固定一个域名走的是扶持登基通道，它只写进 active.Domains。
	// 这个函数每轮体检后都会把 Domains 整个重算一遍，于是用户固定的域名会在
	// 下一轮（默认 30 分钟后）被静默抹掉——页面上还显示着「已固定」，转发却
	// 已经不用它了。这属于报喜不报忧，必须在源头堵住。
	for _, member := range current.Priority {
		if net.ParseIP(member) != nil || !validCandidateHostname(member) {
			continue
		}
		if !containsString(want, member) {
			want = append(want, member)
		}
	}
	if sameStringSet(current.Domains, want) {
		return
	}
	// 观察期内的域名不许自动回池：验活刚判死它，下一轮体检重算就把它加回来，
	// 等于白剔一次。PROXY_DOMAIN_FORWARD 显式配置是运营者担保，不受此限。
	explicit := envForwardDomains("PROXY_DOMAIN_FORWARD")
	now := time.Now()
	admitted := make([]string, 0, len(want))
	for _, domain := range want {
		if !containsString(explicit, domain) && a.domainQuarantined(domain, now) {
			log.Printf("domain pool reconcile: %s 在观察期内，暂不回池", domain)
			continue
		}
		admitted = append(admitted, domain)
	}
	want = admitted
	// 口径必须与 applyPoolLocked 完全一致，否则这里每轮都会「重算出一个池子→
	// 交给 apply 被闸门剪掉→落盘的还是旧的」：日志里 0 -> 2 domains 之后紧跟
	// delivered 0 个域名的 applied，就是这个死循环，每轮体检都白重启一次 cfnat。
	// 先在本地过一遍同一道闸，算出的 want 就是 apply 会真正落盘的那一份。
	pool := current
	pool.Domains = want
	want = a.gatePoolDomains(pool)
	if sameStringSet(current.Domains, want) {
		return
	}
	pool.Domains = want
	pool.UpdatedAt = time.Now()
	if err := a.applyProxyPool(pool, current); err != nil {
		log.Printf("domain pool reconcile failed: %v", err)
		return
	}
	log.Printf("domain pool reconciled: %d -> %d domains (%s)",
		len(current.Domains), len(want), strings.Join(want, ","))
}

// quarantineDomain 把域名的自动回池时间推迟到 until。已在观察期内则取更晚的那个。
func (a *app) quarantineDomain(domain string, until time.Time) {
	a.domainQuarantineMu.Lock()
	defer a.domainQuarantineMu.Unlock()
	if a.domainQuarantine == nil {
		a.domainQuarantine = make(map[string]time.Time)
	}
	if current, ok := a.domainQuarantine[domain]; ok && current.After(until) {
		until = current
	}
	a.domainQuarantine[domain] = until
}

// domainQuarantined 报告域名是否仍在观察期内；过期条目顺手清掉。
func (a *app) domainQuarantined(domain string, now time.Time) bool {
	a.domainQuarantineMu.Lock()
	defer a.domainQuarantineMu.Unlock()
	until, ok := a.domainQuarantine[domain]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(a.domainQuarantine, domain)
		return false
	}
	return true
}

// domainLatencySnapshot 返回域名延迟快照的深拷贝。
func (a *app) domainLatencySnapshot() map[string]domainLatency {
	a.domainLatencyMu.Lock()
	defer a.domainLatencyMu.Unlock()
	out := make(map[string]domainLatency, len(a.domainLatency))
	for k, v := range a.domainLatency {
		out[k] = v
	}
	return out
}
