package main

import (
	"context"
	"log"
	"net"
	"strings"
	"time"
)

// ============================================================================
// 在位验活（登基成员定时复检 / Throne Liveness Probe）
// ============================================================================
//
// 由来：扶持登基信用户所给必为可用，入池不体检；考生供给虽体检过，入池后也可能
// 悄悄死掉。两者同坐皇位（cfnat -fixed），却没有一条「定时复检、坏了就请下台」的
// 链。既有的 active 被动健康（recordActiveHealth）只喂换池闸，从不主动剔人；而后台
// 优化器那条主动探测又被 optimizerEnabled / PROXY_AUTO_APPLY 两道闸锁死——沙盒关闸
// 时，死 IP 会一直占着皇位，被哈希粘性锁死的设备就一路超时。
//
// 本链独立于后台优化器：无论优选开关如何，准点上来摸一遍在位池的每个成员，
// 连续失败达阈值者请下台并重启转发。只碰在位池，不碰候选池。
//
// 2026-11-06 扩版：判据从「死活」扩到「死活 + 快慢」，并补上三处会漏人的口子。
//  1. 慢成员以前无人管——握手只要通着就一路留着，1600ms/1800ms 的成员长期占位。
//  2. 死成员要等满 3 个 10 分钟周期才动手（≈30 分钟）；现改为「一有异常就 2 分钟后
//     单独复验」，死/慢的处置时间压到 ≈12–14 分钟。
//  3. 域名从来不被这条链管——现在一起摸，死了下线并进 30 分钟观察期。
//
// 三条不可让的底线：
//   - 绝不踢空：全部判死时留失败数最少的一个（否则 -fixed 变空串，cfnat 起不来）。
//   - 无替补不删：候选池里没有合格替补时宁可暂留病号并明说，也不让池子缩水。
//   - 固定成员只对「死」负责：用户亲手固定的成员判慢不动，判死才摘，且必须解固定
//     并留下可被页面看见的失效记录。

const (
	throneProbeDefaultIntervalSeconds = 600  // 10 分钟
	throneProbeMinIntervalSeconds     = 300  // 5 分钟
	throneProbeMaxIntervalSeconds     = 1800 // 30 分钟
	throneProbeDefaultFailThreshold   = 3
	throneProbeMaxFailThreshold       = 10

	// throneProbeSlowStrikeLimit 连续慢几轮才动手。慢一轮多半是瞬时抖动，
	// 且每轮之间还有一次 2 分钟的快速复验兜底，两轮足够说明问题。
	throneProbeSlowStrikeLimit = 2

	// throneProbeFastRecheck 嫌疑成员的快速复检间隔：本轮出现「连续失败」或
	// 「连续慢」但还没到动手阈值时，不等常态 10 分钟，2 分钟后再摸一次。
	throneProbeFastRecheck = 120 * time.Second

	// throneProbeMaxPinLoss 失效固定的记账条数上限，够页面提示即可，不无限膨胀。
	throneProbeMaxPinLoss = 8
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

// throneProbeStat 单个在位成员的复检账：连续失败/连续慢计数 + 最近观测。
type throneProbeStat struct {
	ConsecutiveFailures int `json:"consecutiveFailures"`
	// SlowStrikes 连续「握手成功但超过慢阈值」的轮数。与连败分开记：
	// 一个能连上但很慢的成员和一个连不上的成员，处置方式不同。
	SlowStrikes   int       `json:"slowStrikes,omitempty"`
	LastProbe     time.Time `json:"lastProbe,omitempty"`
	LastOK        time.Time `json:"lastOk,omitempty"`
	LastLatencyMs int64     `json:"lastLatencyMs,omitempty"`
	LastError     string    `json:"lastError,omitempty"`
	// RecheckAt 非零表示该成员被列入快速复检（默认 2 分钟后单独再摸一次）。
	// 只有「已出现异常、还没到动手阈值」的成员才带这个标记；一旦处置过或暂留，
	// 立刻清掉，免得系统长期锁死在 2 分钟一次的探测节拍上。
	RecheckAt time.Time `json:"recheckAt,omitempty"`
	// Held 表示判死或判慢、但候选池里没有合格替补，本轮暂留原位。
	Held       bool      `json:"held,omitempty"`
	HeldReason string    `json:"heldReason,omitempty"`
	HeldAt     time.Time `json:"heldAt,omitempty"`
}

// throneLostPin 记录「原固定成员因判死被解除固定并下线」。
//
// 用户亲手固定的成员被系统摘掉，是必须留痕的事件：页面上如果还显示「固定」，
// 而它早就不在 -fixed 里，那就是报喜不报忧。解固定与记账必须同时发生。
type throneLostPin struct {
	Member string    `json:"member"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

const (
	throneStateOK   = "ok"
	throneStateSlow = "slow"
	throneStateDead = "dead"
)

// throneMemberState 纯函数：按验活账本给成员分档。
// 死优先于慢——连不通的成员谈不上「慢」。
func throneMemberState(st *throneProbeStat, failThreshold, slowLimit int) string {
	if st == nil {
		return throneStateOK
	}
	if st.ConsecutiveFailures >= failThreshold {
		return throneStateDead
	}
	if st.SlowStrikes >= slowLimit {
		return throneStateSlow
	}
	return throneStateOK
}

// planThroneAction 纯函数：给出本轮该处置的成员。
//
//   - dead：连续失败达阈值，必须换掉或暂留；
//   - slow：握手通但连续慢达阈值，优先换掉，换不掉就暂留；
//   - pinned 里的成员不列入 slow——固定是运营者的担保，慢由他自己决定，
//     系统不该背着他把成员换掉。判死仍然列入 dead（死了留着也没用）。
//   - **慢只处置 IP**。域名没有「就地换人」这条路：换域名等于换入口身份，是另一
//     回事；而域名的 DNS 本来就会自己换边缘，一次慢多半是瞬时的。对域名只保留
//     「死了下线」这一条硬判据，否则一轮整体变慢就能把域名池削掉一层。
//
// 铁律「绝不踢空」：全部判死时保留失败数最少的一个（并列取原序先者），
// 否则一次上游全灭就会把 -fixed 清成空串，cfnat 起不来，用户看到的是彻底断网
// 而不是「还剩一个待观察」。这不改变「整组已死」的判定——activePoolAllDead
// 读的仍是原始账本，救援快车道照常放行。
func planThroneAction(members []string, pinned map[string]bool, stats map[string]*throneProbeStat, failThreshold, slowLimit int) (dead, slow []string) {
	if len(members) == 0 {
		return nil, nil
	}
	dead = make([]string, 0, len(members))
	slow = make([]string, 0, len(members))
	for _, member := range members {
		switch throneMemberState(stats[member], failThreshold, slowLimit) {
		case throneStateDead:
			dead = append(dead, member)
		case throneStateSlow:
			if pinned[member] || net.ParseIP(member) == nil {
				continue
			}
			slow = append(slow, member)
		}
	}
	if len(dead) == len(members) {
		best := dead[0]
		for _, member := range dead[1:] {
			if stats[member].ConsecutiveFailures < stats[best].ConsecutiveFailures {
				best = member
			}
		}
		trimmed := make([]string, 0, len(dead)-1)
		for _, member := range dead {
			if member != best {
				trimmed = append(trimmed, member)
			}
		}
		dead = trimmed
	}
	return dead, slow
}

// pickThroneReplacements 纯函数：给每个待换成员挑一个替补 IP。
//
// candidates 由调用方按「延迟合格 + 质量优先」排好序。挑选偏好「换完不减少片区
// 多样性」，但那是偏好不是铁律：一个同片区的好 IP 仍胜过一个死 IP，结构美观不能
// 拿可用性去换。降级会打日志，便于事后看池子是怎么被摊平的。
func pickThroneReplacements(victims, candidates, current []string) (picked map[string]string, unresolved []string) {
	picked = make(map[string]string, len(victims))
	used := make(map[string]bool, len(victims))
	unresolved = make([]string, 0)
	for _, victim := range victims {
		choice := ""
		for _, candidate := range candidates {
			if used[candidate] || containsString(current, candidate) {
				continue
			}
			if degradedSegmentDiversity(current, replacePoolMember(current, victim, candidate)) {
				continue
			}
			choice = candidate
			break
		}
		if choice == "" {
			for _, candidate := range candidates {
				if used[candidate] || containsString(current, candidate) {
					continue
				}
				choice = candidate
				break
			}
		}
		if choice == "" {
			unresolved = append(unresolved, victim)
			continue
		}
		used[choice] = true
		picked[victim] = choice
	}
	return picked, unresolved
}

// runThroneProbeLoop 在位验活主循环：启动后 2 分钟首探，随后按需定节拍。
func (a *app) runThroneProbeLoop() {
	if !throneProbeEnabled() {
		return
	}
	initial := time.NewTimer(2 * time.Minute)
	defer initial.Stop()
	<-initial.C
	for {
		a.probeThroneOnce(context.Background())
		timer := time.NewTimer(a.throneNextDelay())
		<-timer.C
		timer.Stop()
	}
}

// throneNextDelay 下一轮间隔：还有嫌疑成员待复验就 2 分钟后单独再摸一次，
// 否则回常态 10 分钟。
//
// 这个「退避」是与快速复检配套的保险：判慢但换不掉的成员如果一直挂着复检标记，
// 系统会长期锁在 2 分钟一次的探测节拍上，白白烧负载且毫无进展。处置过的、
// 暂留的一律清标记（见 reapThroneMembers），这里才回得到常态。
func (a *app) throneNextDelay() time.Duration {
	a.throneProbeMu.Lock()
	due := false
	for _, st := range a.throneProbe {
		if st != nil && !st.RecheckAt.IsZero() {
			due = true
			break
		}
	}
	a.throneProbeMu.Unlock()
	if due {
		return throneProbeFastRecheck
	}
	return throneProbeInterval()
}

// probeThroneOnce 摸一遍在位成员：WS 握手验活 → 记账 → 处置该下台的。
//
// 常态轮次摸全池；存在「快速复检」待办时只摸那几个嫌疑成员——整池每 2 分钟
// 扫一遍既没必要，也会让高频节拍变成真实负载。
func (a *app) probeThroneOnce(parent context.Context) {
	current := a.proxyActivePoolSnapshot()
	// 只摸真正参与转发的成员。被用户关闭的成员不进 -fixed，它们由
	// disabled_bench.go 单独体检（延迟 + 下载速度）并决定去留；让这条链
	// 也去摸它们，会出现「用户关掉的成员被验活判慢、换上一个替补并开始
	// 转发」——用户的操作被系统悄悄撤销。
	members := forwardingMembers(current)
	// 顺手把账本裁到当前转发集合：旧成员不该在页面上继续挂着历史成绩。
	a.pruneThroneLedger(members)
	if len(members) == 0 {
		return
	}
	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		// 缺 WS 握手参数，无从验活——宁可不动，也不误杀。
		a.setThroneProbeError("缺 PROXY_AUTO_HOST/PATH，跳过在位验活")
		return
	}
	cfg.Concurrency = 4
	// 单次探测超时必须高于「慢」阈值，否则慢成员会先被超时打断、记成失败，
	// 于是「慢」这一档永远测不出来——那正是我们要抓的那批人。
	cfg.MaxLatency = int(throneProbeSlowMs()) + 500
	if cfg.MaxLatency < 1500 {
		cfg.MaxLatency = 1500
	}
	if cfg.MaxLatency > 3000 {
		cfg.MaxLatency = 3000
	}
	targets := a.throneProbeTargets(members)
	// 整轮预算要盖得住「每个目标都摸到慢阈值」的最坏情况，否则排在尾部的那批
	// 目标会在 ctx 已经到期后才开跑，连一个字节都没发出去就被记成失败——这和
	// 2026-11-11 域名成批误判是同一个病：预算不够被当成了「成员死了」。
	budget := 40 * time.Second
	if need := time.Duration(len(targets)/cfg.Concurrency+1) * time.Duration(cfg.MaxLatency) * time.Millisecond; need > budget {
		budget = need
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	results := scanProxyWebSockets(ctx, targets, cfg)
	now := time.Now()
	a.recordThroneProbeResults(results, now)
	a.reapThroneMembers(current, now)
}

// throneProbeTargets 有快速复检待办时只返回嫌疑成员，否则返回全池。
func (a *app) throneProbeTargets(members []string) []string {
	a.throneProbeMu.Lock()
	defer a.throneProbeMu.Unlock()
	suspects := make([]string, 0, len(members))
	for _, member := range members {
		if st := a.throneProbe[member]; st != nil && !st.RecheckAt.IsZero() {
			suspects = append(suspects, member)
		}
	}
	if len(suspects) == 0 {
		return members
	}
	return suspects
}

// throneResultIsTimeout 判断一条探测失败是否属于「只是慢了」——超时/截止时间到。
//
// 用 ErrorClass 为首要判据（分类逻辑集中在 classifyProbeError），文本匹配只兜底：
// 有些超时来自 context 包装，分类未必落到 timeout 上。硬失败（403/拒绝/重置/
// 证书不对）一律返回 false，它们该走正常的连败计数。
func throneResultIsTimeout(r proxyScanResult) bool {
	if r.ErrorClass == failureTimeout {
		return true
	}
	msg := strings.ToLower(r.Error)
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded")
}

// recordThroneProbeResults 把本轮成绩记入验活账本，并给嫌疑成员排上快速复检。
func (a *app) recordThroneProbeResults(results []proxyScanResult, now time.Time) {
	slowMs := throneProbeSlowMs()
	failThreshold := throneProbeFailThreshold()
	a.throneProbeMu.Lock()
	defer a.throneProbeMu.Unlock()
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
			if r.Latency > slowMs {
				st.SlowStrikes++
			} else {
				st.SlowStrikes = 0
			}
		} else if net.ParseIP(r.IP) == nil && throneResultIsTimeout(r) {
			// 域名只认硬失败（拒绝/重置/状态码非 101/证书不对）。
			//
			// 2026-11-11 线上事故：29 个域名在同一轮被判死全部摘除，池子从 31 掉到 7，
			// cfnat 随即重启。事后在 NAS 上逐个手工握手，那 29 个里 24 个正常返回 101，
			// 只是耗时 1.5–2.1s；而本轮的握手预算 = slowMs+500 = 2000ms。域名走
			// SNI 复用到同一个 Worker，天然比直连 IP 慢一档，被这 2 秒卡在了门槛外。
			//
			// 判死一个域名要重启一次 cfnat、断掉全部在途连接，代价与「一个慢域名多花
			// 几百毫秒拨号」完全不对等。真死的域名由 30 分钟一轮的域名体检负责摘除
			// （domain-quality.json，TCP + VLESS 三轮取样，dead 状态进闸门过滤），
			// 那条链有足够样本量，不会因为一次 2 秒超时就把人赶走。
			//
			// 超时仍按「慢」记账，页面看得见，只是不触发处置。
			st.SlowStrikes++
			st.LastError = r.Error
			st.LastLatencyMs = r.Latency
		} else {
			st.ConsecutiveFailures++
			st.SlowStrikes = 0
			st.LastError = r.Error
		}
		// 恢复正常即摘掉「暂留」标记：下一轮若再出问题，账从零开始。
		if st.ConsecutiveFailures == 0 && st.SlowStrikes == 0 {
			st.Held, st.HeldReason, st.HeldAt = false, "", time.Time{}
		}
		// 快速复检只排给「已出现异常、但还没到动手阈值」的成员。
		// 到了阈值的由本轮处置负责；处置不了的（无替补）会被 reap 清掉标记。
		//
		// 域名只按死活处置，判慢不会有人动手——给慢域名排快速复检等于每 2 分钟
		// 白摸一次、白把主节拍拽到高频档。所以域名只在连败时才排。
		suspect := st.ConsecutiveFailures > 0
		if net.ParseIP(r.IP) != nil {
			suspect = suspect || st.SlowStrikes > 0
		}
		if suspect && throneMemberState(st, failThreshold, throneProbeSlowStrikeLimit) == throneStateOK {
			st.RecheckAt = now.Add(throneProbeFastRecheck)
		} else {
			st.RecheckAt = time.Time{}
		}
	}
	a.throneLastProbe = now
	a.throneLastError = ""
}

// reapThroneMembers 处置本轮该下台的成员：能换就换，换不掉就暂留并明说。
//
// 锁序：替补考试要拿 proxyScanMu（做网络扫描），而换池要拿 cfnatCtlMu。
// 全局锁序是 refresh → proxyScan → cfnatCtl，先拿 cfnatCtl 再拿 proxyScan 就是
// 死锁。所以替补考试放在拿换池锁之前完成。
func (a *app) reapThroneMembers(current proxyActivePool, now time.Time) {
	dead, slow := a.throneActionPlan(current)
	victims := append(append([]string(nil), dead...), slow...)
	if len(victims) == 0 {
		return
	}
	ipVictims := make([]string, 0, len(victims))
	for _, member := range victims {
		if net.ParseIP(member) != nil {
			ipVictims = append(ipVictims, member)
		}
	}
	// 域名没有「就地换人」这一说：域名死了就是下线进观察期，不找替身。
	replacements, scanned := a.findThroneReplacements(ipVictims, current)
	var deferred []string
	if len(ipVictims) > 0 && !scanned {
		// 「没考成」不等于「没替补」。这一轮扫描锁被别人占着（全量重挑、手动扫描
		// 都可能占着几十秒到几分钟），我们根本没去看候选池。若按「暂留」记账，
		// 会给用户一个错误结论，还会把复检标记清掉，让判死的成员按 10 分钟节拍
		// 干等——线上 14:25 那次就是这样。资源没轮到，就排快速复检，2 分钟后再来。
		//
		// 但域名不受影响：它下线不找替身，没必要跟着 IP 一起等。把 IP 从本轮
		// 受害者里摘出去，剩下的域名照常处置。
		log.Printf("throne probe: replacement scan unavailable, deferring %v", ipVictims)
		a.scheduleThroneRecheck(ipVictims, now)
		deferred = ipVictims
		victims = dropDeferred(victims, deferred)
		if len(victims) == 0 {
			return
		}
	}

	// 换池需独占换池锁；被占就本轮作罢（账已记下，下轮再剔，绝不误杀）。
	if !lockTimeout(&a.cfnatCtlMu, 10*time.Second) {
		log.Printf("throne probe: reap skipped, pool control busy")
		// 锁被占不该让一个已判死的成员干等 10 分钟：给它排一次快速复检，
		// 2 分钟后重来。这与「无替补暂留」不同——那是结论，这里只是没轮到。
		a.scheduleThroneRecheck(victims, now)
		return
	}
	defer a.cfnatCtlMu.Unlock()

	// 锁内重取快照并按最新账本重算：探测期间池可能已被别的路径换过，
	// 只处置仍在位的成员。新出现的受害者本轮没有替补，先暂留，下轮再说。
	fresh := a.proxyActivePoolSnapshot()
	dead, slow = a.throneActionPlan(fresh)
	victims = append(append([]string(nil), dead...), slow...)
	if len(deferred) > 0 {
		// 本轮没考成替补的 IP 不参与处置：它们没有结论，只是没轮到。
		victims = dropDeferred(victims, deferred)
	}
	if len(victims) == 0 {
		return
	}

	next := fresh
	next.UpdatedAt = now
	next.IPs = make([]string, 0, len(fresh.IPs))
	next.Domains = make([]string, 0, len(fresh.Domains))
	removed := make([]string, 0, len(victims))
	swapped := make([]string, 0, len(victims))
	held := make([]string, 0, len(victims))
	for _, ip := range fresh.IPs {
		if !containsString(victims, ip) {
			next.IPs = append(next.IPs, ip)
			continue
		}
		if replacement, ok := replacements[ip]; ok {
			next.IPs = append(next.IPs, replacement)
			removed = append(removed, ip)
			swapped = append(swapped, ip+"→"+replacement)
			continue
		}
		// 无合格替补：不删。池子缩水（5→4→3）比留一个病号更糟，而且越缩越
		// 容易触发「整组已死」。留下来并在账上标清理由，页面能看见。
		next.IPs = append(next.IPs, ip)
		held = append(held, ip)
	}
	for _, domain := range fresh.Domains {
		if !containsString(victims, domain) {
			next.Domains = append(next.Domains, domain)
			continue
		}
		removed = append(removed, domain)
	}
	// 固定成员被摘掉时必须解固定并留痕：页面上还标着「固定」而脚下已经没它，
	// 等于让用户对着一份过期的配置做判断。
	lostPins := make([]throneLostPin, 0, len(removed))
	priority := make([]string, 0, len(fresh.Priority))
	for _, member := range fresh.Priority {
		if containsString(removed, member) {
			lostPins = append(lostPins, throneLostPin{Member: member, At: now, Reason: "验活判死，已解除固定并下线"})
			continue
		}
		priority = append(priority, member)
	}
	next.Priority = priority
	// 成绩单跟着成员走：下线的成员不该在页面上继续挂着旧成绩。
	if len(next.Results) > 0 {
		keptResults := make([]proxyScanResult, 0, len(next.Results))
		for _, result := range next.Results {
			if containsString(removed, result.IP) {
				continue
			}
			keptResults = append(keptResults, result)
		}
		next.Results = keptResults
	}

	if len(removed) == 0 {
		// 全员暂留：不动池子、不重启 cfnat，只把「暂留」写进账本并清掉复检标记，
		// 让主节拍退回常态。
		a.markThroneHeld(held, now)
		return
	}
	// 兜底：锁位模式下若把固定成员摘光，-fixed 会变空串。宁可整轮不动。
	if _, _, err := planFixedMembers(next); err != nil {
		log.Printf("throne probe reap aborted, resulting pool unusable: %v", err)
		a.setThroneProbeError("验活剔除被拦住：" + err.Error())
		a.markThroneHeld(held, now)
		return
	}
	for _, domain := range removed {
		if net.ParseIP(domain) == nil {
			a.quarantineDomain(domain, now.Add(domainQuarantine))
		}
	}
	if err := a.applyPoolLocked(next, fresh); err != nil {
		a.setThroneProbeError("在位验活换池失败: " + err.Error())
		a.markThroneHeld(held, now)
		return
	}
	a.markPoolSwitched(next.UpdatedAt)
	// 已下线的成员不再占账本；暂留的照旧留着，下一轮还会被重新评估。
	a.throneProbeMu.Lock()
	for _, member := range removed {
		delete(a.throneProbe, member)
	}
	if len(lostPins) > 0 {
		a.thronePinLoss = append(append([]throneLostPin(nil), lostPins...), a.thronePinLoss...)
		if len(a.thronePinLoss) > throneProbeMaxPinLoss {
			a.thronePinLoss = a.thronePinLoss[:throneProbeMaxPinLoss]
		}
	}
	for _, ip := range held {
		if st := a.throneProbe[ip]; st != nil {
			st.Held = true
			st.HeldReason = "判死/判慢但候选池无合格替补，暂留原位"
			st.HeldAt = now
			st.RecheckAt = time.Time{}
		}
	}
	a.throneProbeMu.Unlock()
	log.Printf("throne probe reaped %v (swapped %v, held %v): %d IPs + %d domains remain",
		removed, swapped, held, len(next.IPs), len(next.Domains))
}

// dropDeferred 从候选集合里去掉「本轮暂缓处置」的成员，保持原有顺序。
func dropDeferred(members, deferred []string) []string {
	if len(deferred) == 0 || len(members) == 0 {
		return members
	}
	kept := make([]string, 0, len(members))
	for _, member := range members {
		if !containsString(deferred, member) {
			kept = append(kept, member)
		}
	}
	return kept
}

// throneActionPlan 按当前账本算出本轮该处置的成员。
func (a *app) throneActionPlan(current proxyActivePool) (dead, slow []string) {
	pinned := make(map[string]bool, len(current.Priority))
	for _, member := range current.Priority {
		pinned[member] = true
	}
	members := forwardingMembers(current)
	// PROXY_DOMAIN_FORWARD 显式配置是运营者担保：与质量闸同口径，系统不动它。
	// 否则会出现「验活剔掉 → 下一轮重算加回 → 再剔掉」的拉锯，每 30 分钟
	// 白重启一次 cfnat。担保失败由人负责，不由验证器负责。
	explicit := envForwardDomains("PROXY_DOMAIN_FORWARD")
	if len(explicit) > 0 {
		kept := members[:0]
		for _, member := range members {
			if !containsString(explicit, member) {
				kept = append(kept, member)
			}
		}
		members = kept
	}
	a.throneProbeMu.Lock()
	defer a.throneProbeMu.Unlock()
	return planThroneAction(members, pinned, a.throneProbe, throneProbeFailThreshold(), throneProbeSlowStrikeLimit)
}

// scheduleThroneRecheck 给「本轮没轮到处置」的成员排一次快速复检。
// 与「暂留」不同：暂留是结论（没有替补），这里只是资源被占、下轮再来。
func (a *app) scheduleThroneRecheck(members []string, now time.Time) {
	if len(members) == 0 {
		return
	}
	a.throneProbeMu.Lock()
	defer a.throneProbeMu.Unlock()
	for _, member := range members {
		if st := a.throneProbe[member]; st != nil {
			st.RecheckAt = now.Add(throneProbeFastRecheck)
		}
	}
}

// markThroneHeld 给暂留成员写标记，并清掉快速复检，让主节拍退回常态。
func (a *app) markThroneHeld(members []string, now time.Time) {
	if len(members) == 0 {
		return
	}
	a.throneProbeMu.Lock()
	defer a.throneProbeMu.Unlock()
	for _, member := range members {
		st := a.throneProbe[member]
		if st == nil {
			continue
		}
		st.Held = true
		st.HeldReason = "判死/判慢但候选池无合格替补，暂留原位"
		st.HeldAt = now
		st.RecheckAt = time.Time{}
	}
}

// findThroneReplacements 从候选池里考一批替补 IP。
//
// 只收 WS 通、且延迟低于「进池上限」（默认 800ms）的候选——换人的目的就是换掉
// 慢的和死的，塞一个同样慢的进来毫无意义。考不出来就返回空，调用方据此暂留。
//
// 第二个返回值表示「这次到底考没考」：
//   - true  = 看过候选池了，返回空就是结论——候选池里确实没有合格替补；
//   - false = 扫描锁被别人占着，根本没看。这是「没轮到」，不是「没有」，
//     调用方必须区别对待：排快速复检，而不是记成暂留。
//
// 配置缺失（Host/Path 空）算 true：那是配置问题，不是资源竞争，排复检只会
// 让系统每 2 分钟白摸一次。
func (a *app) findThroneReplacements(victims []string, current proxyActivePool) (map[string]string, bool) {
	if len(victims) == 0 {
		return nil, true
	}
	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		return nil, true
	}
	// 替补考试是网络活，必须在拿换池锁之前完成（锁序见函数头注释）。
	if !lockTimeout(&a.proxyScanMu, 30*time.Second) {
		log.Printf("throne probe: replacement scan skipped, proxy scan busy")
		return nil, false
	}
	defer a.proxyScanMu.Unlock()
	snapshot := a.proxyCandidateSnapshot()
	inPool := make(map[string]bool, len(current.IPs))
	for _, ip := range current.IPs {
		inPool[ip] = true
	}
	need := len(victims)
	pending := make([]string, 0, need*6)
	for _, ip := range snapshot.IPs {
		if len(pending) >= need*6 {
			break
		}
		if inPool[ip] {
			continue
		}
		pending = append(pending, ip)
	}
	if len(pending) == 0 {
		return nil, true
	}
	ceiling := poolEntryMaxLatencyMs()
	scanCfg := cfg
	scanCfg.Concurrency = 8
	scanCfg.MaxLatency = int(ceiling)
	if scanCfg.MaxLatency < 500 {
		scanCfg.MaxLatency = 500
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	results := scanProxyWebSockets(ctx, pending, scanCfg)
	applyCandidateSourcePriority(results, snapshot.SourceByIP)
	fast := make([]string, 0, need)
	for _, result := range results {
		if result.Error != "" || result.Latency > ceiling {
			continue
		}
		fast = append(fast, result.IP)
	}
	if len(fast) == 0 {
		log.Printf("throne probe: %d candidates scanned, none under the %dms entry ceiling", len(pending), ceiling)
		return nil, true
	}
	fast = orderPassedByColoPreference(fast, results, poolColoPreferenceSet())
	fast = orderPassedBySegmentHealth(fast, results)
	picked, unresolved := pickThroneReplacements(victims, fast, current.IPs)
	if len(unresolved) > 0 {
		log.Printf("throne probe: no qualified replacement for %v (%d candidates passed the gate)", unresolved, len(fast))
	}
	return picked, true
}

// pruneThroneLedger 把验活账本收窄到当前转发成员。
//
// 账本以前只增不减：池子换过几十轮后，一百多条历史记录一直挂着，页面显示
// 一堆早就不在池里的 IP，throneNextDelay 还可能被历史成员的复检标记拖进
// 高频档。每轮按当前转发集合裁剪一次，账本规模就与池规模同阶。
func (a *app) pruneThroneLedger(keep []string) {
	keepSet := make(map[string]bool, len(keep))
	for _, member := range keep {
		keepSet[member] = true
	}
	a.throneProbeMu.Lock()
	for member := range a.throneProbe {
		if !keepSet[member] {
			delete(a.throneProbe, member)
		}
	}
	a.throneProbeMu.Unlock()
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
	lost := append([]throneLostPin(nil), a.thronePinLoss...)
	return map[string]any{
		"enabled":            throneProbeEnabled(),
		"intervalSeconds":    int(throneProbeInterval().Seconds()),
		"failThreshold":      throneProbeFailThreshold(),
		"slowMs":             throneProbeSlowMs(),
		"slowStrikeLimit":    throneProbeSlowStrikeLimit,
		"fastRecheckSeconds": int(throneProbeFastRecheck.Seconds()),
		"lastProbe":          a.throneLastProbe,
		"lastError":          a.throneLastError,
		"members":            members,
		"pinnedLost":         lost,
	}
}
