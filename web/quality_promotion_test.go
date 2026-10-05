package main

import (
	"testing"
	"time"
)

// --- 刀1：superiorTo 双轨判据 ---

func TestSuperiorToThroughputTrackWhenBothHaveSpeed(t *testing.T) {
	policy := defaultSchedulerPolicy()
	// 双方有速度：+25% 且 +80Mbps 才算优越
	barely := lineQuality{AverageMbps: 400, SuccessRate: 1}
	active := lineQuality{AverageMbps: 300, SuccessRate: 1}
	// 400 >= 300*1.25=375 ✓ 但 400 >= 380 ✓ → 勉强双过 → 优越
	if !superiorTo(barely, active, policy) {
		t.Fatal("300→400 (+33%, +100Mbps) should be superior on throughput track")
	}
	// 350: >=375? 否 → 不优越（相对门槛拦下）
	mild := lineQuality{AverageMbps: 350, SuccessRate: 1}
	if superiorTo(mild, active, policy) {
		t.Fatal("300→350 (+16%) must not be superior")
	}
	// 相对过但绝对不过：300→370 (+23%)? 相对不过；构造 300→378(+26%, +78Mbps)
	abs := lineQuality{AverageMbps: 378, SuccessRate: 1}
	if superiorTo(abs, active, policy) {
		t.Fatal("+26% but +78Mbps < 80 must not be superior (absolute gate)")
	}
}

func TestSuperiorToFallsBackToWSWhenSpeedMissing(t *testing.T) {
	policy := defaultSchedulerPolicy()
	// 生产死锁场景：Active avg=0（seedActive 未测速），候选也无速度成绩
	active := lineQuality{AverageMbps: 0, AverageLatencyMs: 120, SuccessRate: 1}
	// WS 轨：延迟 120→80（改善 40ms ≥ max(120*0.25=30, 15)）→ 优越
	fast := lineQuality{AverageMbps: 0, AverageLatencyMs: 80, SuccessRate: 1}
	if !superiorTo(fast, active, policy) {
		t.Fatal("zero-speed active vs 40ms-latency-improved candidate should be superior on WS track")
	}
	// 改善不足：120→100（20ms < 30ms 相对门槛）→ 不优越
	slow := lineQuality{AverageMbps: 0, AverageLatencyMs: 100, SuccessRate: 1}
	if superiorTo(slow, active, policy) {
		t.Fatal("20ms improvement below 25% relative must not be superior")
	}
	// 绝对门槛：极低延迟时 25% 未必 ≥15ms；active 20ms → candidate 16ms（-4ms <15ms）
	tight := lineQuality{AverageMbps: 0, AverageLatencyMs: 20, SuccessRate: 1}
	near := lineQuality{AverageMbps: 0, AverageLatencyMs: 16, SuccessRate: 1}
	if superiorTo(near, tight, policy) {
		t.Fatal("4ms improvement below 15ms absolute must not be superior")
	}
	// 成功率倒退：再快也不优越
	regress := lineQuality{AverageMbps: 0, AverageLatencyMs: 80, SuccessRate: 0.5}
	if superiorTo(regress, active, policy) {
		t.Fatal("candidate with lower success rate must not be superior")
	}
}

func TestSuperiorToNoBaselineNeverSuperior(t *testing.T) {
	policy := defaultSchedulerPolicy()
	// 无延迟基线（Active 刚 seedActive 且未做健康探测）→ 宁可不判优
	noBaseline := lineQuality{AverageMbps: 0, AverageLatencyMs: 0, SuccessRate: 1}
	candidate := lineQuality{AverageMbps: 0, AverageLatencyMs: 50, SuccessRate: 1}
	if superiorTo(candidate, noBaseline, policy) {
		t.Fatal("candidate without active latency baseline must not be judged superior")
	}
	noBaselineActive := lineQuality{AverageMbps: 0, AverageLatencyMs: 0, SuccessRate: 1}
	if superiorTo(candidate, noBaselineActive, policy) {
		t.Fatal("zero active baseline must not fire WS track")
	}
}

func TestSchedulerPromotesViaWSLatencyTrackAfterThreeRounds(t *testing.T) {
	policy := defaultSchedulerPolicy()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	// 旧世界：双零吞吐 → 永不晋升；新世界：WS 轨三轮后晋升
	active := lineQuality{IP: "A", State: lineActive, AverageMbps: 0, AverageLatencyMs: 150, SuccessRate: 1}
	candidate := lineQuality{IP: "B", State: lineStandby, AverageMbps: 0, AverageLatencyMs: 100, SuccessRate: 1, ConsecutiveSuperior: 2}
	decision, err := decideLineSwitch(schedulerInput{Now: now, Active: &active, Standby: []lineQuality{candidate}}, policy)
	if err != nil || decision.Event != switchNone {
		t.Fatalf("two rounds must not promote: %#v, %v", decision, err)
	}
	candidate.ConsecutiveSuperior = 3
	decision, _ = decideLineSwitch(schedulerInput{Now: now, Active: &active, Standby: []lineQuality{candidate}}, policy)
	if decision.Event != switchPromotion || decision.ToIP != "B" {
		t.Fatalf("WS-track candidate with 3 rounds should promote: %#v", decision)
	}
}

// --- 刀2：Active 建立 WS 基线 ---

func TestActiveHealthProbeBuildsLatencyBaseline(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	r.seedActive("1.1.1.1", now)
	// 健康探测前：无基线
	if got := r.data.Records["1.1.1.1"].AverageLatencyMs; got != 0 {
		t.Fatalf("baseline before probe = %v, want 0", got)
	}
	r.observeActiveHealth(true, 90, now.Add(5*time.Minute))
	rec := r.data.Records["1.1.1.1"]
	if rec.AverageLatencyMs != 90 {
		t.Fatalf("active latency baseline = %v, want 90", rec.AverageLatencyMs)
	}
	// 健康样本 Mbps=0 不得稀释历史吞吐（Active 曾有速度成绩的场景）
	r.observe("1.1.1.1", "active_pool", 500, 0, true, now.Add(-time.Hour))
	r.observeActiveHealth(true, 85, now.Add(10*time.Minute))
	rec = r.data.Records["1.1.1.1"]
	if rec.AverageMbps != 500 {
		t.Fatalf("health sample diluted throughput: avg = %v, want 500", rec.AverageMbps)
	}
	if rec.AverageLatencyMs != 87.5 {
		t.Fatalf("latency baseline after 2 samples = %v, want 87.5", rec.AverageLatencyMs)
	}
}

// --- 刀4：lastDecision 保真 + 成绩册墓碑 ---

func TestDecideKeepsLastRealDecisionAgainstNonSwitchOverwrites(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	r.seedActive("1.1.1.1", now)
	// 无候选 → decide 返回 NONE，不应产生 decision 记录
	if got := r.decide(now); got.Event != switchNone {
		t.Fatalf("expected NONE, got %#v", got)
	}
	if r.data.LastDecision.Event != "" {
		t.Fatalf("empty NONE should not write lastDecision: %#v", r.data.LastDecision)
	}
	// 制造一次真实 failover 决策
	r.observe("8.8.8.8", "official", 600, 40, true, now)
	r.observeActiveHealth(false, 80, now.Add(time.Minute))
	r.observeActiveHealth(false, 80, now.Add(2*time.Minute))
	r.observeActiveHealth(false, 80, now.Add(3*time.Minute))
	got := r.decide(now.Add(3 * time.Minute))
	if got.Event != switchFailover {
		t.Fatalf("expected failover, got %#v", got)
	}
	saved := r.data.LastDecision
	// 后续空决策轮不得覆盖真实换池记录
	for i := 0; i < 5; i++ {
		r.decide(now.Add(time.Duration(4+i) * time.Minute))
	}
	if r.data.LastDecision != saved {
		t.Fatalf("NONE overwrote last real decision: before %#v after %#v", saved, r.data.LastDecision)
	}
}

func TestQualityRecordsPruneKeepsActiveAndNewest(t *testing.T) {
	r := testQualityRuntime(t)
	base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	r.seedActive("active.ip", base)
	// 灌入超过墓碑上限的记录，Active 最老但必须幸存
	for i := 0; i < qualityRecordsCap+200; i++ {
		ip := "10.0." + itoa(i/256) + "." + itoa(i%256)
		r.ensureRecordLocked(ip, "subscription", base.Add(time.Duration(i)*time.Minute))
	}
	r.pruneRecordsLocked()
	if len(r.data.Records) > qualityRecordsCap {
		t.Fatalf("records after prune = %d, want <= %d", len(r.data.Records), qualityRecordsCap)
	}
	if _, ok := r.data.Records["active.ip"]; !ok {
		t.Fatal("active record was pruned")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
