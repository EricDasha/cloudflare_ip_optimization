package main

import (
	"context"
	"encoding/json"
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
	Domain              string              `json:"domain"`
	Samples             []domainProbeSample `json:"samples,omitempty"`
	SuccessRate         float64             `json:"successRate"`
	AverageMbps         float64             `json:"averageMbps"`
	P50Mbps             float64             `json:"p50Mbps"`
	P95Mbps             float64             `json:"p95Mbps"`
	JitterRatio         float64             `json:"jitterRatio"`
	ConsecutiveFailures int                 `json:"consecutiveFailures"`
	LastProbeAt         time.Time           `json:"lastProbeAt,omitempty"`
	LastOKAt            time.Time           `json:"lastOkAt,omitempty"`
	State               string              `json:"state"`
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

// probeActiveDomainLatencies 给生效池里的每个优选域名做一轮体检：
// TCP:443 快检 → VLESS 短时下载 → 写稳定性账。
func (a *app) probeActiveDomainLatencies(ctx context.Context) {
	domains := a.proxyActivePoolSnapshot().Domains
	if len(domains) == 0 {
		return
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
	a.recordDomainProbeResults(samples)
}

// probeOneDomain 单域名体检。TCP 不通即失败；TCP 通但测速失败仍记失败，
// 但保留 TCP 延迟，便于区分「解析不到」与「能连上但跑不动」。
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

// recordDomainProbeResults 把本轮样本记入稳定性账，重算统计与状态，并回填 UI 视图。
//
// 一道刻意的保险：只有当本轮至少有一个域名测速成功时，失败才计入连败。
// 若整轮无一人测出速度，那更可能是测速通路本身坏了（模板失效、sing-box 缺位、
// 上游测速端点不可达），而不是十二个域名同时死。此时累加连败会把整个域名池
// 打成 dead，「只开活跃」随即清空——那是数据丢失级的方向性错误。
// 单域名真死仍能被识别：只要同轮里还有同伴成功，它的失败就照常计数。
func (a *app) recordDomainProbeResults(samples map[string]domainProbeSample) {
	now := time.Now()
	speedSuccess := 0
	for _, sample := range samples {
		if sample.OK && sample.Stage == "speed" {
			speedSuccess++
		}
	}
	probeHealthy := speedSuccess > 0
	if !probeHealthy && len(samples) > 0 {
		log.Printf("domain probe: no domain produced a speed sample this round (%d domains), failures not counted toward dead", len(samples))
	}

	a.domainQualityMu.Lock()
	for domain, sample := range samples {
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
			rec.LastOKAt = now
		} else if probeHealthy {
			rec.ConsecutiveFailures++
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
}

// recalculate 从最近样本重算成功率、速度分位与抖动系数。
//
// 抖动用 P95/P50（相对倍数），比绝对差值更能横向比较不同速率的域名：
// 10 Mbps 的线在 15 Mbps 波动，与 100 Mbps 的线在 150 Mbps 波动，体感同样是「不稳」。
func (q *domainQuality) recalculate() {
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
			a.probeActiveDomainLatencies(ctx)
		}()
		timer := time.NewTimer(domainProbeInterval())
		<-timer.C
		timer.Stop()
	}
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
