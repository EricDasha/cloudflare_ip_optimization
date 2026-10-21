package main

import (
	"reflect"
	"testing"
	"time"
)

// freshRecord 造一条「刚刚体检过」的记录，避免新鲜度闸把测试样本当旧账放行。
func freshRecord(state string) *domainQuality {
	return &domainQuality{Domain: "x", State: state, LastProbeAt: time.Now()}
}

// 质量闸的核心职责：体检判死的域名不得再进 cfnat -fixed。
// 生产事故形态：12 个 successRate≈0.2 的域名留在轮转池，每 4 次拨号 3 次撞死。
func TestPoolDomainMembersBlocksDeadAndUnstable(t *testing.T) {
	a := newProbeTestApp(t)
	a.candidates = proxyCandidateSnapshot{
		ForwardDomains: []string{"good.example.com", "unstable.example.com", "dead.example.com", "fresh.example.com"},
	}
	a.domainQuality["good.example.com"] = freshRecord(domainStateHealthy)
	a.domainQuality["unstable.example.com"] = freshRecord(domainStateUnstable)
	a.domainQuality["dead.example.com"] = freshRecord(domainStateDead)
	// fresh.example.com 无体检记录，应放行（没有证据证明它坏）。

	got := a.poolDomainMembers()
	want := []string{"good.example.com", "fresh.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("poolDomainMembers() = %v, want %v", got, want)
	}
	// 池里还有合格域名，不该触发兜底去把坏域名拉进来凑数。
	if fb := a.domainFallbackSnapshot(); len(fb) != 0 {
		t.Fatalf("有合格候选时不得兜底, got %v", fb)
	}
}

// testing 状态（样本不足）必须放行：新域名不能因为「还没测够」而永远进不了池。
func TestPoolDomainMembersAllowsTestingState(t *testing.T) {
	a := newProbeTestApp(t)
	a.candidates = proxyCandidateSnapshot{ForwardDomains: []string{"new.example.com"}}
	a.domainQuality["new.example.com"] = freshRecord(domainStateTesting)

	if got := a.poolDomainMembers(); !reflect.DeepEqual(got, []string{"new.example.com"}) {
		t.Fatalf("poolDomainMembers() = %v, want [new.example.com]", got)
	}
}

// 闸门可关：运营者需要临时放行时不必改代码。
func TestPoolDomainMembersGateCanBeDisabled(t *testing.T) {
	a := newProbeTestApp(t)
	a.candidates = proxyCandidateSnapshot{ForwardDomains: []string{"dead.example.com"}}
	a.domainQuality["dead.example.com"] = freshRecord(domainStateDead)
	t.Setenv("PROXY_DOMAIN_QUALITY_GATE", "false")

	if got := a.poolDomainMembers(); !reflect.DeepEqual(got, []string{"dead.example.com"}) {
		t.Fatalf("gate disabled: poolDomainMembers() = %v, want [dead.example.com]", got)
	}
}

// 关掉体检就没有证据源，闸门必须放行，否则等于拿旧账永久封杀。
func TestPoolDomainMembersGateIgnoresStateWhenProbeDisabled(t *testing.T) {
	a := newProbeTestApp(t)
	a.candidates = proxyCandidateSnapshot{ForwardDomains: []string{"dead.example.com"}}
	a.domainQuality["dead.example.com"] = freshRecord(domainStateDead)
	t.Setenv("PROXY_DOMAIN_PROBE_ENABLED", "false")

	if got := a.poolDomainMembers(); !reflect.DeepEqual(got, []string{"dead.example.com"}) {
		t.Fatalf("probe disabled: poolDomainMembers() = %v, want [dead.example.com]", got)
	}
}

// 账本久未更新（进程停机数天）时，旧状态不再是现时证据。
func TestPoolDomainMembersGateIgnoresStaleRecord(t *testing.T) {
	a := newProbeTestApp(t)
	a.candidates = proxyCandidateSnapshot{ForwardDomains: []string{"dead.example.com"}}
	a.domainQuality["dead.example.com"] = &domainQuality{
		Domain:      "dead.example.com",
		State:       domainStateDead,
		LastProbeAt: time.Now().Add(-24 * time.Hour),
	}

	if got := a.poolDomainMembers(); !reflect.DeepEqual(got, []string{"dead.example.com"}) {
		t.Fatalf("stale record: poolDomainMembers() = %v, want [dead.example.com]", got)
	}
}

// 被闸门拦下的域名必须仍在体检名单上，否则永远拿不到新样本、永远翻不了身。
func TestDomainProbeTargetsIncludeGatedOutDomains(t *testing.T) {
	a := newProbeTestApp(t)
	a.candidates = proxyCandidateSnapshot{
		ForwardDomains: []string{"inpool.example.com", "gated.example.com"},
	}
	a.activePool = proxyActivePool{Domains: []string{"inpool.example.com"}}
	a.domainQuality["gated.example.com"] = freshRecord(domainStateDead)

	got := a.domainProbeTargets()
	seen := map[string]bool{}
	for _, d := range got {
		seen[d] = true
	}
	if !seen["gated.example.com"] {
		t.Fatalf("domainProbeTargets() = %v, must include gated-out domain so it can recover", got)
	}
	if !seen["inpool.example.com"] {
		t.Fatalf("domainProbeTargets() = %v, must include in-pool domain", got)
	}
}

// 成绩好转后应能自动回池：闸门只读状态，不写任何粘滞标记。
//
// 2026-10-22 决策变更：唯一候选被判 unstable 时，闸门拦下去池子就空了，
// 而空池 = 服务完全不可用（比"用 30% 成功率的域名"严重得多）。
// 故改为「拦到空就兜底放行 + 标记降级」，账本恢复后兜底自动收回。
func TestPoolDomainMembersRecoversAfterQualityImproves(t *testing.T) {
	a := newProbeTestApp(t)
	a.candidates = proxyCandidateSnapshot{ForwardDomains: []string{"flappy.example.com"}}
	a.domainQuality["flappy.example.com"] = freshRecord(domainStateUnstable)

	got := a.poolDomainMembers()
	if !reflect.DeepEqual(got, []string{"flappy.example.com"}) {
		t.Fatalf("唯一候选被判坏时应兜底放行, got %v", got)
	}
	if fb := a.domainFallbackSnapshot(); !reflect.DeepEqual(fb, []string{"flappy.example.com"}) {
		t.Fatalf("兜底项必须标记为降级, got %v", fb)
	}

	a.domainQuality["flappy.example.com"].State = domainStateHealthy
	got = a.poolDomainMembers()
	if !reflect.DeepEqual(got, []string{"flappy.example.com"}) {
		t.Fatalf("recovered domain must return to pool, got %v", got)
	}
	if fb := a.domainFallbackSnapshot(); len(fb) != 0 {
		t.Fatalf("账本恢复后不应再有兜底标记, got %v", fb)
	}
}

// 一场上游中断不该用几小时的旧账继续惩罚已经恢复的域名：
// 连续 3 轮成功即清空历史，按现时证据重新判定。
func TestRecalculateClearsHistoryAfterRecoveryStreak(t *testing.T) {
	q := &domainQuality{Domain: "flappy.example.com"}
	// 模拟一次持续中断：12 轮全失败。
	for i := 0; i < domainQualitySampleCap; i++ {
		q.Samples = append(q.Samples, domainProbeSample{OK: false, Stage: "speed"})
	}
	q.ConsecutiveFailures = domainQualitySampleCap
	q.recalculate()
	if q.State != domainStateUnstable && q.State != domainStateDead {
		t.Fatalf("after outage state = %q, want unstable/dead", q.State)
	}

	// 链路恢复：连续成功达到阈值后应立刻回到 healthy，而不是等 8 轮。
	for i := 0; i < domainRecoveryStreak; i++ {
		q.Samples = append(q.Samples, domainProbeSample{OK: true, Stage: "speed", Mbps: 3})
		if len(q.Samples) > domainQualitySampleCap {
			q.Samples = q.Samples[len(q.Samples)-domainQualitySampleCap:]
		}
		q.ConsecutiveFailures = 0
		q.ConsecutiveSuccesses++
		q.recalculate()
	}
	if q.State != domainStateHealthy {
		t.Fatalf("after %d consecutive successes state = %q (successRate=%.2f), want healthy",
			domainRecoveryStreak, q.State, q.SuccessRate)
	}
	if len(q.Samples) > domainRecoveryStreak {
		t.Fatalf("recovered record kept %d samples, want <= %d (old outage evidence must be dropped)",
			len(q.Samples), domainRecoveryStreak)
	}
}

// 启动 / 手动换池也必须过闸：否则闸门只在建池时生效，重启一次坏域名就全回来了。
func TestAdmittedPoolDomainsFiltersPersistedPool(t *testing.T) {
	a := newProbeTestApp(t)
	a.domainQuality["good.example.com"] = freshRecord(domainStateHealthy)
	a.domainQuality["bad.example.com"] = freshRecord(domainStateDead)

	got := a.admittedPoolDomains([]string{"good.example.com", "bad.example.com", "unknown.example.com"})
	want := []string{"good.example.com", "unknown.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("admittedPoolDomains() = %v, want %v", got, want)
	}
}

// 未达阈值的偶发成功不得提前清账，否则抖动会被「洗白」。
func TestRecalculateKeepsHistoryBelowRecoveryStreak(t *testing.T) {
	q := &domainQuality{Domain: "jittery.example.com"}
	for i := 0; i < domainQualitySampleCap; i++ {
		q.Samples = append(q.Samples, domainProbeSample{OK: false, Stage: "speed"})
	}
	q.ConsecutiveSuccesses = domainRecoveryStreak - 1
	for i := 0; i < domainRecoveryStreak-1; i++ {
		q.Samples = append(q.Samples, domainProbeSample{OK: true, Stage: "speed", Mbps: 3})
		if len(q.Samples) > domainQualitySampleCap {
			q.Samples = q.Samples[len(q.Samples)-domainQualitySampleCap:]
		}
	}
	q.recalculate()
	if len(q.Samples) != domainQualitySampleCap {
		t.Fatalf("kept %d samples below recovery streak, want %d", len(q.Samples), domainQualitySampleCap)
	}
	if q.State == domainStateHealthy {
		t.Fatalf("state = healthy below recovery streak, want unstable")
	}
}
