package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"time"
)

// ============================================================================
// 关闭成员体检（Disabled Bench）
// ============================================================================
//
// 用户把一个成员关掉，本身就是一条质量信号：多半是「用着卡」。关闭之后它
// 不再进 cfnat -fixed，正好是零成本体检窗口——测握手延迟与下载速度：
//
//   - 反复连不上、或因链路坏掉测不出速度 → 扔掉；
//   - 能连上但体质差（下载低于下限） → 和候选池里的替补比，替补更好就换掉；
//   - 还能用 → 留在关闭席上，不自动恢复。关是用户的决定，系统不替他改。
//
// 这条链只碰 Disabled 成员，不碰正在转发的成员；转发成员的处置归
// throne_probe.go。两条链的职责必须分开：关闭成员本来就不转发，拿「判慢」
// 去换人等于把用户关掉的成员偷偷换成一根会转发的新线。
//
// 域名不在这条链里动刀：域名体检由 domain_latency.go 每 30 分钟照常做
// （关闭的域名仍在体检名单上），判死由域名闸门拦在池外。这里只在状态里
// 展示它们的成绩，不做「换域名」这种会改变入口身份的操作。

const (
	disabledBenchKeep   = "keep"
	disabledBenchRemove = "remove"
	disabledBenchWeak   = "weak"
)

type disabledBenchStat struct {
	LastProbe        time.Time `json:"lastProbe,omitempty"`
	LastLatencyMs    int64     `json:"lastLatencyMs,omitempty"`
	LastMbps         float64   `json:"lastMbps,omitempty"`
	SpeedTested      bool      `json:"speedTested,omitempty"`
	ConsecutiveFails int       `json:"consecutiveFails,omitempty"`
	LastError        string    `json:"lastError,omitempty"`
	Verdict          string    `json:"verdict,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	SwappedTo        string    `json:"swappedTo,omitempty"`
}

// disabledBenchEvent 是给人看的一行处置记录。关闭成员的消失必须能解释，
// 否则页面上只会看到「我关掉的那个 IP 不见了」，不知道是系统扔了还是换了。
type disabledBenchEvent struct {
	Member    string    `json:"member"`
	Action    string    `json:"action"`
	Candidate string    `json:"candidate,omitempty"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

const disabledBenchMaxEvents = 20

// runDisabledBenchLoop 启动后 3 分钟首轮（避开自启与首轮候选刷新），
// 之后按配置间隔跑。用户新关闭成员会通过 disabledBenchWake 立即插队。
func (a *app) runDisabledBenchLoop() {
	if !disabledBenchEnabled() {
		return
	}
	initial := time.NewTimer(3 * time.Minute)
	defer initial.Stop()
	select {
	case <-initial.C:
	case <-a.disabledBenchWake:
	}
	for {
		// 扫描锁被占时不要傻等满 15 分钟：用户刚关掉成员，正是指望立刻拿到
		// 成绩的时候。锁通常几秒内就会释放，2 分钟后重试即可。
		if a.probeDisabledBenchOnce(context.Background()) {
			timer := time.NewTimer(disabledBenchBusyRetry)
			select {
			case <-timer.C:
			case <-a.disabledBenchWake:
				timer.Stop()
			}
			continue
		}
		timer := time.NewTimer(disabledBenchInterval())
		select {
		case <-timer.C:
		case <-a.disabledBenchWake:
			timer.Stop()
		}
	}
}

// signalDisabledBench 叫醒体检循环。带缓冲的通道合并重复信号：
// 用户连续关三个人，只需要跑一轮，不必排三次队。
func (a *app) signalDisabledBench() {
	if a.disabledBenchWake == nil {
		return
	}
	select {
	case a.disabledBenchWake <- struct{}{}:
	default:
	}
}

func (a *app) setDisabledBenchError(message string) {
	a.disabledBenchMu.Lock()
	a.disabledBenchLastErr = message
	a.disabledBenchLast = time.Now()
	a.disabledBenchMu.Unlock()
}

// clearDisabledBenchError 在每轮开跑前清掉上一轮的错误。否则一次早已恢复的
// 锁占用会永远挂在状态里，页面只能看到「还在坏」，看不出「已经好了」。
func (a *app) clearDisabledBenchError() {
	a.disabledBenchMu.Lock()
	a.disabledBenchLastErr = ""
	a.disabledBenchMu.Unlock()
}

// disabledBenchTargets 从关闭成员里挑本轮要体检的 IP：最久没测的优先，
// 新关闭的（LastProbe 为零）排最前。一轮只取 batch 个，避免一次起太多次
// sing-box。域名不参与，理由见文件头。
func (a *app) disabledBenchTargets(pool proxyActivePool, now time.Time) []string {
	if len(pool.Disabled) == 0 {
		return nil
	}
	interval := disabledBenchInterval()
	a.disabledBenchMu.Lock()
	defer a.disabledBenchMu.Unlock()
	type pending struct {
		member string
		at     time.Time
	}
	items := make([]pending, 0, len(pool.Disabled))
	for _, member := range pool.Disabled {
		if net.ParseIP(member) == nil || !containsString(pool.IPs, member) {
			continue
		}
		st := a.disabledBench[member]
		due := st == nil || st.LastProbe.IsZero() || now.Sub(st.LastProbe) >= interval
		if !due {
			continue
		}
		at := time.Time{}
		if st != nil {
			at = st.LastProbe
		}
		items = append(items, pending{member: member, at: at})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	if len(items) > disabledBenchBatch {
		items = items[:disabledBenchBatch]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.member)
	}
	return out
}

// probeDisabledBenchOnce 是一轮完整体检：握手 → 测速 → 判档 → 需要时找替补。
// 返回 true 表示本轮被扫描锁挡下、值得很快重试（而不是等满一个周期）。
func (a *app) probeDisabledBenchOnce(parent context.Context) (deferred bool) {
	pool := a.proxyActivePoolSnapshot()
	now := time.Now()
	// 上一轮的错误不能一直挂着：本轮只要能开跑就先清掉，再按结果重写。
	// 粘滞的 lastError 会让页面把一次早已恢复的锁占用说成「现在还在坏」。
	a.clearDisabledBenchError()
	targets := a.disabledBenchTargets(pool, now)
	a.disabledBenchMu.Lock()
	a.disabledBenchLast = now
	a.disabledBenchMu.Unlock()
	if len(targets) == 0 {
		return false
	}
	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		a.setDisabledBenchError("缺 PROXY_AUTO_HOST/PATH，跳过关闭成员体检")
		return false
	}

	// 第一关：WS 握手。只在这一段持 proxyScanMu，测速会起 sing-box，
	// 不该把整条候选扫描锁占住几十秒。
	wsResults, unlocked := a.probeDisabledWebSockets(parent, targets, cfg)
	if !unlocked {
		a.setDisabledBenchError("候选扫描锁被占用，本轮延后")
		return true
	}
	// 握手失败先原地复测一次：关闭成员本来就没有转发流量，单次失败更可能
	// 是瞬时抖动。复测仍失败才记一笔。
	retry := make([]string, 0)
	for _, ip := range targets {
		if r, ok := wsResults[ip]; !ok || r.Error != "" {
			retry = append(retry, ip)
		}
	}
	if len(retry) > 0 {
		if second, ok := a.probeDisabledWebSockets(parent, retry, cfg); ok {
			for ip, r := range second {
				wsResults[ip] = r
			}
		}
	}

	// 第二关：下载测速。只有握手通过的才值得起 sing-box。
	speedCfg, template, speedErr := disabledBenchSpeedConfig()
	speedAttempts, speedSuccesses := 0, 0
	mbpsByIP := make(map[string]float64, len(targets))
	speedErrByIP := make(map[string]string, len(targets))
	if speedErr == nil {
		for _, ip := range targets {
			r, ok := wsResults[ip]
			if !ok || r.Error != "" {
				continue
			}
			speedAttempts++
			ctx, cancel := context.WithTimeout(parent, speedCfg.Timeout)
			metrics, err := probeVLESSCandidateMetrics(ctx, ip, cfg.Port, speedCfg, template)
			cancel()
			if err == nil {
				speedSuccesses++
				mbpsByIP[ip] = metrics.Mbps
			} else {
				speedErrByIP[ip] = err.Error()
			}
		}
	}
	// 整轮零成功 = 测速通路本身有问题（端点、sing-box、模板），不是这批
	// 成员都坏了。此时不记连败，只把原因写进账，避免把好成员误扔。
	speedPathBroken := speedAttempts > 0 && speedSuccesses == 0

	minMbps := disabledBenchMinMbps()
	slowMs := throneProbeSlowMs()
	failThreshold := disabledBenchFailThreshold

	type weakTarget struct {
		member string
		stat   disabledBenchStat
	}
	weak := make([]weakTarget, 0)
	removals := make([]string, 0)

	a.disabledBenchMu.Lock()
	for _, ip := range targets {
		r, ok := wsResults[ip]
		wsOK := ok && r.Error == ""
		st := a.disabledBench[ip]
		if st == nil {
			st = &disabledBenchStat{}
			a.disabledBench[ip] = st
		}
		st.LastProbe = now
		st.LastLatencyMs = r.Latency
		if wsOK {
			st.ConsecutiveFails = 0
			st.LastError = ""
			if mbps, ok := mbpsByIP[ip]; ok {
				st.SpeedTested = true
				st.LastMbps = mbps
			} else {
				st.SpeedTested = false
				if msg := speedErrByIP[ip]; msg != "" {
					st.LastError = "测速未完成: " + msg
					// 别人测得出、只有它测不出：更像这条线自己的数据面坏了，
					// 记一笔。整轮零成功的情况已由 speedPathBroken 排除。
					if !speedPathBroken {
						st.ConsecutiveFails++
					}
				} else if speedErr != nil {
					st.LastError = "测速未启用: " + speedErr.Error()
				}
			}
		} else {
			st.ConsecutiveFails++
			st.SpeedTested = false
			st.LastMbps = 0
			if ok {
				st.LastError = r.Error
			} else {
				st.LastError = "未取得握手结果"
			}
		}
		if speedPathBroken {
			// 通路问题不记到成员头上；保留握手结果，下一轮再测。
			st.SpeedTested = false
			st.LastMbps = 0
			st.LastError = "测速通路本轮整体失败，未计入质量结论"
		}
		action, reason := decideDisabledBench(wsOK, st.ConsecutiveFails, failThreshold, st.SpeedTested, st.LastMbps, minMbps, st.LastLatencyMs, slowMs)
		// 握手通、数据面连续测不通：和连续握手失败同属「不能用」，
		// 但 decide 的输入只看握手，所以在这里补上这一档。
		if action == disabledBenchKeep && wsOK && !st.SpeedTested && !speedPathBroken && speedErrByIP[ip] != "" && st.ConsecutiveFails >= failThreshold {
			action = disabledBenchRemove
			reason = fmt.Sprintf("连续 %d 轮数据面测速失败", st.ConsecutiveFails)
		}
		st.Verdict, st.Reason = action, reason
		snapshot := *st
		switch action {
		case disabledBenchRemove:
			removals = append(removals, ip)
		case disabledBenchWeak:
			weak = append(weak, weakTarget{member: ip, stat: snapshot})
		}
	}
	a.disabledBenchMu.Unlock()

	// 第三关：弱成员和替补比。只有弱成员才触发，正常轮次没有额外开销。
	swaps := make(map[string]string)
	swapReasons := make(map[string]string)
	// 锁位模式只转置顶成员，换进来的普通候选根本不会转发。此时找替补
	// 只是把成员从池里删掉，起不到「换人承载流量」的作用，跳过。
	if len(weak) > 0 && !pool.Lock {
		victims := make([]string, 0, len(weak))
		statByIP := make(map[string]disabledBenchStat, len(weak))
		for _, item := range weak {
			victims = append(victims, item.member)
			statByIP[item.member] = item.stat
		}
		swaps, swapReasons = a.findDisabledBenchReplacements(victims, statByIP, minMbps)
	}

	if len(removals) == 0 && len(swaps) == 0 {
		return false
	}
	a.applyDisabledBenchChanges(removals, swaps, swapReasons, now)
	return false
}

// probeDisabledWebSockets 对一组 IP 做 WS 101 握手验活。返回结果表与
// 「是否真的跑成了」——扫描锁被占时返回 false，调用方据此延后而不是误判。
func (a *app) probeDisabledWebSockets(parent context.Context, targets []string, cfg proxyAutoConfig) (map[string]proxyScanResult, bool) {
	out := make(map[string]proxyScanResult, len(targets))
	if len(targets) == 0 {
		return out, true
	}
	if !lockTimeout(&a.proxyScanMu, 20*time.Second) {
		log.Printf("disabled bench: proxy scan busy, deferring %d members", len(targets))
		return out, false
	}
	defer a.proxyScanMu.Unlock()
	scanCfg := cfg
	scanCfg.Concurrency = 2
	// 关闭成员要能测出「慢」：超时给到 3 秒，低于它的慢成员才有机会
	// 留下延迟样本，而不是一律记成握手失败。
	scanCfg.MaxLatency = 3000
	if slow := int(throneProbeSlowMs()) + 1500; slow > scanCfg.MaxLatency {
		scanCfg.MaxLatency = slow
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	for _, r := range scanProxyWebSockets(ctx, targets, scanCfg) {
		out[r.IP] = r
	}
	return out, true
}

// disabledBenchSpeedConfig 组装关闭成员测速用的 VLESS 配置。口径与
// rankVLESSPassesBySpeed 一致：同一组环境变量、同一个下载端点。
func disabledBenchSpeedConfig() (vlessProbeConfig, map[string]any, error) {
	base := defaultVLESSProbeConfig()
	if !base.Enabled {
		return base, nil, fmt.Errorf("PROXY_VLESS_PROBE 未启用")
	}
	bytes := envInt("PROXY_VLESS_SPEED_BYTES", 2097152)
	if bytes < 1048576 || bytes > 33554432 {
		bytes = 2097152
	}
	timeoutSeconds := envInt("PROXY_VLESS_SPEED_TIMEOUT", 20)
	if timeoutSeconds < 5 || timeoutSeconds > 120 {
		timeoutSeconds = 20
	}
	cfg := base
	cfg.TestURL = vlessSpeedTestURL(bytes)
	cfg.ExpectedStatus = http.StatusOK
	cfg.ReadLimit = int64(bytes)
	cfg.MinBytes = int64(bytes)
	cfg.Timeout = time.Duration(timeoutSeconds) * time.Second
	template, err := loadVLESSOutboundTemplate(cfg.TemplatePath)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, template, nil
}

// decideDisabledBench 纯函数：给一次体检样本分档。
//
// 死优先于弱：连不上就没有「慢」可言。测速失败但握手正常时，只有在延迟
// 也超过慢阈值的情况下才按弱处理——否则一次测速端点抖动就会把好成员推去
// 和替补比较，平白换线。
func decideDisabledBench(wsOK bool, fails, failThreshold int, speedOK bool, mbps, minMbps float64, latencyMs, slowMs int64) (action, reason string) {
	if !wsOK {
		if fails >= failThreshold {
			return disabledBenchRemove, fmt.Sprintf("连续 %d 轮握手失败", fails)
		}
		return disabledBenchKeep, "本轮握手失败，下轮复测"
	}
	if speedOK {
		if mbps < minMbps {
			return disabledBenchWeak, fmt.Sprintf("下载 %.2f Mbps，低于下限 %.2f Mbps", mbps, minMbps)
		}
		return disabledBenchKeep, fmt.Sprintf("握手 %dms / 下载 %.2f Mbps", latencyMs, mbps)
	}
	if latencyMs > slowMs {
		return disabledBenchWeak, fmt.Sprintf("握手 %dms 超过慢阈值 %dms，且测速未完成", latencyMs, slowMs)
	}
	return disabledBenchKeep, fmt.Sprintf("握手 %dms，测速未完成", latencyMs)
}

// disabledCandidateBeats 纯函数：替补是否明显强于弱成员。
//
// 双轨与调度器 superiorTo 同口径，避免同一个系统里出现两套「更好」的定义：
//   - 双方都有测速成绩：替补 ≥ 弱成员 1.25 倍，且不低于下限；
//   - 只有替补有成绩：替补达到下限即可（弱成员连速度都测不出来）；
//   - 替补没成绩：改比握手延迟，改善 ≥25% 且 ≥15ms。
func disabledCandidateBeats(victim disabledBenchStat, candLatencyMs int64, candMbps float64, candSpeedOK bool, minMbps float64) bool {
	if candSpeedOK && victim.SpeedTested && victim.LastMbps > 0 {
		return candMbps >= minMbps && candMbps >= victim.LastMbps*1.25
	}
	if candSpeedOK && (!victim.SpeedTested || victim.LastMbps <= 0) {
		return candMbps >= minMbps
	}
	if victim.LastLatencyMs <= 0 || candLatencyMs <= 0 {
		return false
	}
	improvement := victim.LastLatencyMs - candLatencyMs
	return candLatencyMs*4 <= victim.LastLatencyMs*3 && improvement >= 15
}

// findDisabledBenchReplacements 给弱成员找替补：先 WS 过进池闸，再按需测速。
// 一次最多摸 disabledBenchCandidateBatch 个候选，给几个候选测速，够用即可。
func (a *app) findDisabledBenchReplacements(victims []string, victimStats map[string]disabledBenchStat, minMbps float64) (map[string]string, map[string]string) {
	swaps := make(map[string]string)
	reasons := make(map[string]string)
	if len(victims) == 0 {
		return swaps, reasons
	}
	pool := a.proxyActivePoolSnapshot()
	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		return swaps, reasons
	}
	snapshot := a.proxyCandidateSnapshot()
	disabled := disabledMemberSet(pool)
	pending := make([]string, 0, disabledBenchCandidateBatch)
	for _, ip := range snapshot.IPs {
		if len(pending) >= disabledBenchCandidateBatch {
			break
		}
		if containsString(pool.IPs, ip) || disabled[ip] {
			continue
		}
		pending = append(pending, ip)
	}
	if len(pending) == 0 {
		return swaps, reasons
	}
	if !lockTimeout(&a.proxyScanMu, 20*time.Second) {
		log.Printf("disabled bench: replacement scan busy, keeping %d weak members", len(victims))
		return swaps, reasons
	}
	ceiling := poolEntryMaxLatencyMs()
	scanCfg := cfg
	scanCfg.Concurrency = 6
	scanCfg.MaxLatency = int(ceiling)
	if scanCfg.MaxLatency < 500 {
		scanCfg.MaxLatency = 500
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	results := scanProxyWebSockets(ctx, pending, scanCfg)
	cancel()
	a.proxyScanMu.Unlock()
	applyCandidateSourcePriority(results, snapshot.SourceByIP)

	type candidate struct {
		ip      string
		latency int64
	}
	passed := make([]candidate, 0, len(results))
	for _, r := range results {
		if r.Error != "" || r.Latency > ceiling {
			continue
		}
		passed = append(passed, candidate{ip: r.IP, latency: r.Latency})
	}
	sort.SliceStable(passed, func(i, j int) bool { return passed[i].latency < passed[j].latency })
	if len(passed) == 0 {
		return swaps, reasons
	}

	speedCfg, template, speedErr := disabledBenchSpeedConfig()
	speedProbes := 0
	speedByIP := make(map[string]float64)
	speedOK := make(map[string]bool)
	speedTestCandidate := func(ip string) {
		if speedErr != nil || speedProbes >= disabledBenchMaxSpeedProbes {
			return
		}
		if _, done := speedOK[ip]; done {
			return
		}
		speedProbes++
		probeCtx, probeCancel := context.WithTimeout(context.Background(), speedCfg.Timeout)
		metrics, err := probeVLESSCandidateMetrics(probeCtx, ip, cfg.Port, speedCfg, template)
		probeCancel()
		if err != nil {
			speedOK[ip] = false
			return
		}
		speedOK[ip] = true
		speedByIP[ip] = metrics.Mbps
	}

	used := make(map[string]bool)
	tryPick := func(victim string, st disabledBenchStat, strictDiversity bool) (string, string, bool) {
		for _, cand := range passed {
			if used[cand.ip] || containsString(pool.IPs, cand.ip) {
				continue
			}
			// 片区多样性是偏好不是铁律：先在不减少片区的候选里找，找不到
			// 再退回普通顺序。绝不因为「结构不好看」留下一个坏成员。
			if strictDiversity && degradedSegmentDiversity(pool.IPs, replacePoolMember(pool.IPs, victim, cand.ip)) {
				continue
			}
			// 延迟已经明显更好就不必再花一次 sing-box 做测速。
			if disabledCandidateBeats(st, cand.latency, 0, false, minMbps) {
				return cand.ip, fmt.Sprintf("握手 %dms→%dms 明显更快", st.LastLatencyMs, cand.latency), true
			}
			speedTestCandidate(cand.ip)
			if speedOK[cand.ip] && disabledCandidateBeats(st, cand.latency, speedByIP[cand.ip], true, minMbps) {
				return cand.ip, fmt.Sprintf("替补下载 %.2f Mbps 优于原成员 %.2f Mbps", speedByIP[cand.ip], st.LastMbps), true
			}
		}
		return "", "", false
	}
	for _, victim := range victims {
		st := victimStats[victim]
		candidate, reason, ok := tryPick(victim, st, true)
		if !ok {
			candidate, reason, ok = tryPick(victim, st, false)
		}
		if !ok {
			continue
		}
		swaps[victim] = candidate
		reasons[victim] = reason
		used[candidate] = true
	}
	return swaps, reasons
}

// applyDisabledBenchChanges 把体检结论落成池变更。
//
// 纯剔除（没有替补）不改变 -fixed：被关的成员本来就不转发，这种变更只落盘、
// 不重启 cfnat。换成替补则会让一根新线开始转发，必须走完整换池。
// 两条路必须分开：为了删掉一个不转发的成员重启 cfnat，是在拿真实用户的
// 连接给后台体检陪跑。
func (a *app) applyDisabledBenchChanges(removals []string, swaps map[string]string, reasons map[string]string, now time.Time) {
	if !lockTimeout(&a.cfnatCtlMu, 10*time.Second) {
		log.Printf("disabled bench: pool control busy, deferring %d removals / %d swaps", len(removals), len(swaps))
		return
	}
	defer a.cfnatCtlMu.Unlock()

	current := a.proxyActivePoolSnapshot()
	disabled := disabledMemberSet(current)
	victims := make(map[string]bool, len(removals)+len(swaps))
	for _, ip := range removals {
		// 用户可能在体检期间又把它打开了：那就不是关闭成员，不能按体检结论处理。
		if containsString(current.IPs, ip) && disabled[ip] {
			victims[ip] = true
		}
	}
	effectiveSwaps := make(map[string]string, len(swaps))
	for ip, candidate := range swaps {
		// 体检期间用户开了锁位：新候选不在 Priority 里，换进来也不转发。
		if current.Lock {
			break
		}
		if containsString(current.IPs, ip) && disabled[ip] {
			victims[ip] = true
			effectiveSwaps[ip] = candidate
		}
	}
	if len(victims) == 0 {
		return
	}

	next := current
	next.UpdatedAt = now
	next.IPs = make([]string, 0, len(current.IPs))
	for _, ip := range current.IPs {
		if !victims[ip] {
			next.IPs = append(next.IPs, ip)
			continue
		}
		if candidate, ok := effectiveSwaps[ip]; ok {
			next.IPs = append(next.IPs, candidate)
		}
	}
	removed := make([]string, 0, len(victims))
	for member := range victims {
		removed = append(removed, member)
	}
	// Disabled 的清理必须同时对照 IP 和域名：只留 next.IPs 会把同样被关闭的
	// 域名从 Disabled 里抹掉，下一轮它就会悄悄回到 -fixed 开始转发——用户的
	// 关闭决定不能被另一条链顺手撤销。
	next.Disabled = keepOnly(current.Disabled, append(append([]string(nil), next.IPs...), next.Domains...))
	next.Priority = keepOnly(current.Priority, append(append([]string(nil), next.IPs...), next.Domains...))
	if len(next.MemberChannels) > 0 {
		channels := make(map[string]string, len(next.MemberChannels))
		for member, channel := range next.MemberChannels {
			if containsString(removed, member) {
				continue
			}
			channels[member] = channel
		}
		for _, candidate := range effectiveSwaps {
			if _, ok := channels[candidate]; !ok {
				channels[candidate] = "disabled-bench"
			}
		}
		next.MemberChannels = channels
	}
	if len(next.Results) > 0 {
		kept := make([]proxyScanResult, 0, len(next.Results))
		for _, result := range next.Results {
			if !containsString(removed, result.IP) {
				kept = append(kept, result)
			}
		}
		next.Results = kept
	}
	if _, _, err := planFixedMembers(next); err != nil {
		log.Printf("disabled bench: resulting pool unusable, aborting: %v", err)
		return
	}

	beforeFixed, _, beforeErr := planFixedMembers(current)
	afterFixed, _, afterErr := planFixedMembers(next)
	if beforeErr == nil && afterErr == nil && sameMemberOrder(beforeFixed, afterFixed) {
		if err := a.commitPoolWithoutRestart(next); err != nil {
			log.Printf("disabled bench: save pool failed: %v", err)
			return
		}
	} else {
		if err := a.applyPoolLocked(next, current); err != nil {
			log.Printf("disabled bench: apply pool failed: %v", err)
			return
		}
		a.markPoolSwitched(next.UpdatedAt)
	}

	// 成功之后才记账：失败时保留原判，下一轮可以重试。
	a.disabledBenchMu.Lock()
	events := make([]disabledBenchEvent, 0, len(removed))
	for _, victim := range removed {
		candidate := effectiveSwaps[victim]
		action := "removed"
		reason := "体检不合格，已移出生效池"
		if candidate != "" {
			action = "swapped"
			reason = reasons[victim]
			if reason == "" {
				reason = "替补明显更好，已换入"
			}
		} else if st := a.disabledBench[victim]; st != nil && st.Reason != "" {
			reason = st.Reason
		}
		events = append(events, disabledBenchEvent{Member: victim, Action: action, Candidate: candidate, Reason: reason, At: now})
		delete(a.disabledBench, victim)
	}
	a.disabledBenchEvents = append(events, a.disabledBenchEvents...)
	if len(a.disabledBenchEvents) > disabledBenchMaxEvents {
		a.disabledBenchEvents = a.disabledBenchEvents[:disabledBenchMaxEvents]
	}
	a.disabledBenchMu.Unlock()

	log.Printf("disabled bench reaped %v, swaps %v", removed, effectiveSwaps)
}

// disabledBenchStatus 是给 /api/status 的只读快照。
func (a *app) disabledBenchStatus() map[string]any {
	a.disabledBenchMu.Lock()
	defer a.disabledBenchMu.Unlock()
	members := make(map[string]disabledBenchStat, len(a.disabledBench))
	for member, st := range a.disabledBench {
		if st != nil {
			members[member] = *st
		}
	}
	events := append([]disabledBenchEvent(nil), a.disabledBenchEvents...)
	return map[string]any{
		"enabled":         disabledBenchEnabled(),
		"intervalSeconds": int(disabledBenchInterval().Seconds()),
		"minMbps":         disabledBenchMinMbps(),
		"batch":           disabledBenchBatch,
		"failThreshold":   disabledBenchFailThreshold,
		"lastProbe":       a.disabledBenchLast,
		"lastError":       a.disabledBenchLastErr,
		"members":         members,
		"events":          events,
	}
}

// sameMemberOrder 比较两个成员序列是否完全一致（含顺序）。
// -fixed 的顺序影响 Priority 的先后，不能用集合比较糊弄过去。
func sameMemberOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
