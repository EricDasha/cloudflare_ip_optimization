package main

import (
	"context"
	"log"
	"time"
)

// ============================================================================
// 在位验活（登基 IP 定时复检 / Throne Liveness Probe）
// ============================================================================
//
// 由来：扶持登基信用户所给必为可用，入池不体检；考生供给虽体检过，入池后也可能
// 悄悄死掉。两者同坐皇位（cfnat -fixed），却没有一条「定时复检、坏了就请下台」的
// 链。既有的 active 被动健康（recordActiveHealth）只喂换池闸，从不主动剔人；而后台
// 优化器那条主动探测又被 optimizerEnabled / PROXY_AUTO_APPLY 两道闸锁死——沙盒关闸
// 时，死 IP 会一直占着皇位，被哈希粘性锁死的设备就一路超时。
//
// 本链独立于后台优化器：无论优选开关如何，准点上来摸一遍在位池的每个 IP，连续失败
// 达阈值者请下台并重启转发。只碰在位池，不碰候选池；绝不踢空（铁律，见 reap 函数）。
//
// 只管 IP：优选域名是 cfnat -fixed 的一等公民，拨号时活解析，无需 IP 级探活。

const (
	throneProbeDefaultIntervalSeconds = 600  // 10 分钟
	throneProbeMinIntervalSeconds     = 300  // 5 分钟
	throneProbeMaxIntervalSeconds     = 1800 // 30 分钟
	throneProbeDefaultFailThreshold   = 3
	throneProbeMaxFailThreshold       = 10
)

// throneProbeEnabled 在位验活总闸，默认开。
func throneProbeEnabled() bool {
	return envBool("PROXY_ACTIVE_PROBE_ENABLED", true)
}

// throneProbeInterval 复检间隔，钳到 [5, 30] 分钟。
func throneProbeInterval() time.Duration {
	seconds := envInt("PROXY_ACTIVE_PROBE_INTERVAL_SECONDS", throneProbeDefaultIntervalSeconds)
	if seconds < throneProbeMinIntervalSeconds || seconds > throneProbeMaxIntervalSeconds {
		seconds = throneProbeDefaultIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

// throneProbeFailThreshold 连续失败达几次才请下台（单次抖动不换人，防误杀）。
func throneProbeFailThreshold() int {
	n := envInt("PROXY_ACTIVE_PROBE_FAIL_THRESHOLD", throneProbeDefaultFailThreshold)
	if n < 1 || n > throneProbeMaxFailThreshold {
		n = throneProbeDefaultFailThreshold
	}
	return n
}

// throneProbeStat 单个在位 IP 的复检账：连续失败计数 + 最近观测。
type throneProbeStat struct {
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	LastProbe           time.Time `json:"lastProbe,omitempty"`
	LastOK              time.Time `json:"lastOk,omitempty"`
	LastLatencyMs       int64     `json:"lastLatencyMs,omitempty"`
	LastError           string    `json:"lastError,omitempty"`
}

// reapDeadThroneMembers 纯函数：从在位 IP 列表剔除连续失败达阈值者，返回 (保留, 剔除)。
// 铁律「绝不踢空」——若全部达阈值，留下失败数最少的一个（并列取原序先者），
// 免得 cfnat -fixed 变空串、用户看到"无目标"而非"还剩一个待观察"。不改动入参，便于测试。
func reapDeadThroneMembers(ips []string, stats map[string]*throneProbeStat, threshold int) (kept, removed []string) {
	kept = make([]string, 0, len(ips))
	for _, ip := range ips {
		st := stats[ip]
		if st != nil && st.ConsecutiveFailures >= threshold {
			removed = append(removed, ip)
			continue
		}
		kept = append(kept, ip)
	}
	if len(kept) == 0 && len(removed) > 0 {
		best := removed[0]
		for _, ip := range removed[1:] {
			if stats[ip].ConsecutiveFailures < stats[best].ConsecutiveFailures {
				best = ip
			}
		}
		removed, _ = withoutPoolIP(removed, best)
		kept = append(kept, best)
	}
	return kept, removed
}

// runThroneProbeLoop 在位验活主循环：启动后 2 分钟首探，随后每 interval 一轮。
func (a *app) runThroneProbeLoop() {
	if !throneProbeEnabled() {
		return
	}
	initial := time.NewTimer(2 * time.Minute)
	defer initial.Stop()
	<-initial.C
	for {
		a.probeThroneOnce(context.Background())
		timer := time.NewTimer(throneProbeInterval())
		<-timer.C
		timer.Stop()
	}
}

// probeThroneOnce 摸一遍在位 IP：WS 握手验活 → 记账 → 达阈值者请下台并重启转发。
func (a *app) probeThroneOnce(parent context.Context) {
	current := a.proxyActivePoolSnapshot()
	if len(current.IPs) == 0 {
		return
	}
	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		// 缺 WS 握手参数，无从验活——宁可不动，也不误杀。
		a.setThroneProbeError("缺 PROXY_AUTO_HOST/PATH，跳过在位验活")
		return
	}
	cfg.Concurrency = 4
	if cfg.MaxLatency <= 0 || cfg.MaxLatency > 3000 {
		cfg.MaxLatency = 1500
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	results := scanProxyWebSockets(ctx, current.IPs, cfg)
	now := time.Now()

	a.throneProbeMu.Lock()
	if a.throneProbe == nil {
		a.throneProbe = make(map[string]*throneProbeStat)
	}
	for _, r := range results {
		st := a.throneProbe[r.IP]
		if st == nil {
			st = &throneProbeStat{}
			a.throneProbe[r.IP] = st
		}
		st.LastProbe = now
		if r.Error == "" {
			st.ConsecutiveFailures = 0
			st.LastOK = now
			st.LastLatencyMs = r.Latency
			st.LastError = ""
		} else {
			st.ConsecutiveFailures++
			st.LastError = r.Error
		}
	}
	a.throneLastProbe = now
	a.throneLastError = ""
	a.throneProbeMu.Unlock()

	// 换池需独占换池锁；被占就本轮作罢（账已记下，下轮再剔，绝不误杀）。
	if !lockTimeout(&a.cfnatCtlMu, 10*time.Second) {
		log.Printf("throne probe: reap skipped, pool control busy")
		return
	}
	defer a.cfnatCtlMu.Unlock()

	// 锁内重取快照：探测期间池可能已被别的路径换过，只剔仍在位的死 IP。
	fresh := a.proxyActivePoolSnapshot()
	a.throneProbeMu.Lock()
	kept, removed := reapDeadThroneMembers(fresh.IPs, a.throneProbe, throneProbeFailThreshold())
	if len(removed) == 0 {
		a.throneProbeMu.Unlock()
		return
	}
	for _, ip := range removed {
		delete(a.throneProbe, ip)
	}
	a.throneProbeMu.Unlock()

	keptResults := make([]proxyScanResult, 0, len(fresh.Results))
	for _, result := range fresh.Results {
		if containsString(removed, result.IP) {
			continue
		}
		keptResults = append(keptResults, result)
	}
	pool := fresh
	pool.UpdatedAt = time.Now()
	pool.IPs = kept
	pool.Results = keptResults
	if err := a.applyPoolLocked(pool, fresh); err != nil {
		a.setThroneProbeError("在位验活换池失败: " + err.Error())
		return
	}
	a.markPoolSwitched(pool.UpdatedAt)
	log.Printf("throne probe reaped %v: %d IPs + %d domains remain", removed, len(kept), len(pool.Domains))
}

func (a *app) setThroneProbeError(message string) {
	a.throneProbeMu.Lock()
	a.throneLastError = message
	a.throneProbeMu.Unlock()
}

// activeProbeStatus 在位验活快照，供 /api/status 观测。
func (a *app) activeProbeStatus() map[string]any {
	a.throneProbeMu.Lock()
	defer a.throneProbeMu.Unlock()
	members := make(map[string]throneProbeStat, len(a.throneProbe))
	for ip, st := range a.throneProbe {
		if st != nil {
			members[ip] = *st
		}
	}
	return map[string]any{
		"enabled":         throneProbeEnabled(),
		"intervalSeconds": int(throneProbeInterval().Seconds()),
		"failThreshold":   throneProbeFailThreshold(),
		"lastProbe":       a.throneLastProbe,
		"lastError":       a.throneLastError,
		"members":         members,
	}
}
