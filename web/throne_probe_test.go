package main

import (
	"reflect"
	"testing"
	"time"
)

func stat(failures int) *throneProbeStat {
	return &throneProbeStat{ConsecutiveFailures: failures}
}

func slowStat(strikes int, latency int64) *throneProbeStat {
	return &throneProbeStat{SlowStrikes: strikes, LastLatencyMs: latency}
}

func noPins() map[string]bool { return map[string]bool{} }

// 死成员达阈值即入列；未达阈值的不动。
func TestPlanThroneActionDeadThreshold(t *testing.T) {
	members := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	stats := map[string]*throneProbeStat{
		"1.1.1.1": stat(0),
		"2.2.2.2": stat(3),
		"3.3.3.3": stat(5),
	}
	dead, slow := planThroneAction(members, noPins(), stats, 3, 2)
	if !reflect.DeepEqual(dead, []string{"2.2.2.2", "3.3.3.3"}) {
		t.Fatalf("dead = %v, want [2.2.2.2 3.3.3.3]", dead)
	}
	if len(slow) != 0 {
		t.Fatalf("slow = %v, want empty", slow)
	}
}

// 边界：差一次留，正好达到就剔。
func TestPlanThroneActionDeadThresholdBoundary(t *testing.T) {
	members := []string{"10.0.0.1", "10.0.0.2"}
	stats := map[string]*throneProbeStat{
		"10.0.0.1": stat(2),
		"10.0.0.2": stat(3),
	}
	dead, _ := planThroneAction(members, noPins(), stats, 3, 2)
	if !reflect.DeepEqual(dead, []string{"10.0.0.2"}) {
		t.Fatalf("dead = %v, want [10.0.0.2]", dead)
	}
}

// 绝不踢空：全部判死时留失败数最少的一个，剩下的才是待换清单。
// 但「整组已死」的判定仍读原始账本，救援快车道不受影响（见 pool_test 的救援用例）。
func TestPlanThroneActionNeverEmpties(t *testing.T) {
	members := []string{"9.9.9.9", "8.8.8.8", "7.7.7.7"}
	stats := map[string]*throneProbeStat{
		"9.9.9.9": stat(9),
		"8.8.8.8": stat(4),
		"7.7.7.7": stat(7),
	}
	dead, _ := planThroneAction(members, noPins(), stats, 3, 2)
	if !reflect.DeepEqual(dead, []string{"9.9.9.9", "7.7.7.7"}) {
		t.Fatalf("dead = %v, want [9.9.9.9 7.7.7.7] (least failures kept)", dead)
	}
}

// 单人池全死也留人：-fixed 变空串 cfnat 起不来，比留着病号严重得多。
func TestPlanThroneActionSingleMemberNeverRemoved(t *testing.T) {
	dead, slow := planThroneAction([]string{"5.5.5.5"}, noPins(), map[string]*throneProbeStat{"5.5.5.5": stat(9)}, 3, 2)
	if len(dead) != 0 || len(slow) != 0 {
		t.Fatalf("dead=%v slow=%v, want both empty", dead, slow)
	}
}

// 无记录视为正常：凭旧账或空白账踢人就是误杀。
func TestPlanThroneActionNoStatsKeepsAll(t *testing.T) {
	dead, slow := planThroneAction([]string{"1.1.1.1", "2.2.2.2"}, noPins(), map[string]*throneProbeStat{}, 3, 2)
	if len(dead) != 0 || len(slow) != 0 {
		t.Fatalf("dead=%v slow=%v, want all kept", dead, slow)
	}
}

// 慢成员要能被抓出来——这是本轮扩版的核心：握手通但一直慢的成员以前永远不动。
func TestPlanThroneActionSlowMembers(t *testing.T) {
	members := []string{"1.1.1.1", "2.2.2.2"}
	stats := map[string]*throneProbeStat{
		"1.1.1.1": slowStat(2, 1650),
		"2.2.2.2": slowStat(1, 1809), // 差一轮，先不动
	}
	dead, slow := planThroneAction(members, noPins(), stats, 3, 2)
	if len(dead) != 0 {
		t.Fatalf("dead = %v, want empty", dead)
	}
	if !reflect.DeepEqual(slow, []string{"1.1.1.1"}) {
		t.Fatalf("slow = %v, want [1.1.1.1]", slow)
	}
}

// 死优先于慢：连不通的成员不必再讨论它慢不慢。
func TestPlanThroneActionDeadBeatsSlow(t *testing.T) {
	st := &throneProbeStat{ConsecutiveFailures: 3, SlowStrikes: 5}
	dead, slow := planThroneAction([]string{"1.1.1.1", "2.2.2.2"}, noPins(),
		map[string]*throneProbeStat{"1.1.1.1": st}, 3, 2)
	if !reflect.DeepEqual(dead, []string{"1.1.1.1"}) {
		t.Fatalf("dead = %v", dead)
	}
	if len(slow) != 0 {
		t.Fatalf("slow = %v, want empty", slow)
	}
}

// 慢只处置 IP：域名判慢不换。域名没有「就地换人」这条路，而一轮整体变慢
// 就能把域名池削掉一层——这不是我们想要的。
func TestPlanThroneActionSlowSkipsDomains(t *testing.T) {
	members := []string{"1.1.1.1", "www.example.com"}
	stats := map[string]*throneProbeStat{
		"1.1.1.1":         slowStat(3, 1650),
		"www.example.com": slowStat(3, 1700),
	}
	dead, slow := planThroneAction(members, noPins(), stats, 3, 2)
	if len(dead) != 0 {
		t.Fatalf("dead = %v, want empty", dead)
	}
	if !reflect.DeepEqual(slow, []string{"1.1.1.1"}) {
		t.Fatalf("slow = %v, want only the IP", slow)
	}
}

// 域名判死仍然照剔：慢可以放过，连不上不能放过。
func TestPlanThroneActionDeadIncludesDomains(t *testing.T) {
	stats := map[string]*throneProbeStat{"www.example.com": stat(3)}
	dead, slow := planThroneAction([]string{"www.example.com", "1.1.1.1"}, noPins(), stats, 3, 2)
	if !reflect.DeepEqual(dead, []string{"www.example.com"}) {
		t.Fatalf("dead = %v, want [www.example.com]", dead)
	}
	if len(slow) != 0 {
		t.Fatalf("slow = %v, want empty", slow)
	}
}

// 用户固定的成员：判慢不动（运营者的担保），判死照剔（死了留着也没用）。
func TestPlanThroneActionPinnedMemberProtectedFromSlowOnly(t *testing.T) {
	pinned := map[string]bool{"1.1.1.1": true, "2.2.2.2": true}
	stats := map[string]*throneProbeStat{
		"1.1.1.1": slowStat(4, 1900),
		"2.2.2.2": stat(3),
	}
	dead, slow := planThroneAction([]string{"1.1.1.1", "2.2.2.2"}, pinned, stats, 3, 2)
	if !reflect.DeepEqual(dead, []string{"2.2.2.2"}) {
		t.Fatalf("dead = %v, want [2.2.2.2] (pinned but dead)", dead)
	}
	if len(slow) != 0 {
		t.Fatalf("slow = %v, want empty (pinned slow member must be left alone)", slow)
	}
}

// 替补挑选：优先不减少片区多样性的候选。
func TestPickThroneReplacementsKeepsSegmentDiversity(t *testing.T) {
	current := []string{"172.64.0.1", "104.16.0.1"}
	candidates := []string{"104.16.0.9", "172.64.0.9"} // 第二个与 172.64.0.1 同段
	picked, unresolved := pickThroneReplacements([]string{"172.64.0.1"}, candidates, current)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v", unresolved)
	}
	if picked["172.64.0.1"] != "172.64.0.9" {
		t.Fatalf("picked = %v, want 172.64.0.9 (same segment keeps diversity)", picked)
	}
}

// 没有多样性更好的候选时，宁可降级也不留病号：可用性优先于结构美观。
func TestPickThroneReplacementsFallsBackWhenDiversityUnavailable(t *testing.T) {
	current := []string{"172.64.0.1", "104.16.0.1"}
	candidates := []string{"104.16.0.9"} // 与 104.16.0.1 同段
	picked, unresolved := pickThroneReplacements([]string{"172.64.0.1"}, candidates, current)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v, want empty (degenerate replacement is better than a dead member)", unresolved)
	}
	if picked["172.64.0.1"] != "104.16.0.9" {
		t.Fatalf("picked = %v", picked)
	}
}

// 候选被多个受害者共用时不许重复分配；分不到的进 unresolved，调用方据此暂留。
func TestPickThroneReplacementsNoDoubleAssignment(t *testing.T) {
	current := []string{"172.64.0.1", "104.16.0.1", "188.114.0.1"}
	candidates := []string{"104.16.0.9"}
	victims := []string{"172.64.0.1", "188.114.0.1"}
	picked, unresolved := pickThroneReplacements(victims, candidates, current)
	if len(picked) != 1 {
		t.Fatalf("picked = %v, want exactly one assignment", picked)
	}
	if len(unresolved) != 1 {
		t.Fatalf("unresolved = %v, want one victim left unresolved", unresolved)
	}
}

// 已在池里的 IP 不能当替补（会让池子出现重复成员）。
func TestPickThroneReplacementsSkipsInPoolCandidates(t *testing.T) {
	current := []string{"172.64.0.1", "104.16.0.1"}
	picked, unresolved := pickThroneReplacements([]string{"172.64.0.1"}, []string{"104.16.0.1"}, current)
	if len(picked) != 0 {
		t.Fatalf("picked = %v, want empty (candidate already in pool)", picked)
	}
	if !reflect.DeepEqual(unresolved, []string{"172.64.0.1"}) {
		t.Fatalf("unresolved = %v", unresolved)
	}
}

func TestThroneProbeIntervalClamp(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"120", 600},   // below min → default
		{"300", 300},   // min
		{"600", 600},   // default
		{"1800", 1800}, // max
		{"3600", 600},  // above max → default
		{"", 600},      // unset → default
	}
	for _, c := range cases {
		if c.env == "" {
			t.Setenv("PROXY_ACTIVE_PROBE_INTERVAL_SECONDS", "")
		} else {
			t.Setenv("PROXY_ACTIVE_PROBE_INTERVAL_SECONDS", c.env)
		}
		got := int(throneProbeInterval().Seconds())
		if got != c.want {
			t.Errorf("interval(%q) = %d, want %d", c.env, got, c.want)
		}
	}
}

func TestThroneProbeFailThresholdClamp(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"0", 3},
		{"1", 1},
		{"3", 3},
		{"10", 10},
		{"99", 3},
	}
	for _, c := range cases {
		t.Setenv("PROXY_ACTIVE_PROBE_FAIL_THRESHOLD", c.env)
		if got := throneProbeFailThreshold(); got != c.want {
			t.Errorf("threshold(%q) = %d, want %d", c.env, got, c.want)
		}
	}
}

func TestThroneProbeSlowMsClamp(t *testing.T) {
	cases := []struct {
		env  string
		want int64
	}{
		{"100", 1500},
		{"300", 300},
		{"1500", 1500},
		{"5000", 5000},
		{"9000", 1500},
		{"", 1500},
	}
	for _, c := range cases {
		t.Setenv("PROXY_ACTIVE_PROBE_SLOW_MS", c.env)
		if got := throneProbeSlowMs(); got != c.want {
			t.Errorf("slowMs(%q) = %d, want %d", c.env, got, c.want)
		}
	}
}

// 退避：没有嫌疑成员时回常态节拍，不许长期锁在 2 分钟高频探测上。
func TestThroneNextDelayBacksOffToNormalInterval(t *testing.T) {
	now := time.Now()
	a := &app{throneProbe: map[string]*throneProbeStat{
		"1.1.1.1": {ConsecutiveFailures: 1},
		"2.2.2.2": {SlowStrikes: 1},
	}}
	if got := a.throneNextDelay(); got != throneProbeInterval() {
		t.Fatalf("delay = %v, want normal interval %v", got, throneProbeInterval())
	}
	a.throneProbe["1.1.1.1"].RecheckAt = now.Add(throneProbeFastRecheck)
	if got := a.throneNextDelay(); got != throneProbeFastRecheck {
		t.Fatalf("delay = %v, want fast recheck %v", got, throneProbeFastRecheck)
	}
}

// 处置过的成员必须清掉复检标记：否则「判慢但换不掉」会让系统一直高频探测。
func TestMarkThroneHeldClearsRecheck(t *testing.T) {
	now := time.Now()
	a := &app{throneProbe: map[string]*throneProbeStat{
		"1.1.1.1": {SlowStrikes: 2, RecheckAt: now.Add(time.Minute)},
	}}
	a.markThroneHeld([]string{"1.1.1.1"}, now)
	st := a.throneProbe["1.1.1.1"]
	if !st.RecheckAt.IsZero() {
		t.Fatalf("recheckAt = %v, want cleared", st.RecheckAt)
	}
	if !st.Held || st.HeldReason == "" {
		t.Fatalf("held=%v reason=%q, want held with a reason", st.Held, st.HeldReason)
	}
	if got := a.throneNextDelay(); got != throneProbeInterval() {
		t.Fatalf("delay = %v, want normal interval after hold", got)
	}
}

// 「本轮没考成替补」的 IP 不参与处置：没有结论不等于有结论。
// 只有域名照常走——它下线不找替身，没必要陪着 IP 一起等。
func TestDropDeferredKeepsDomains(t *testing.T) {
	victims := []string{"1.1.1.1", "2.2.2.2", "www.example.com"}
	got := dropDeferred(victims, []string{"1.1.1.1", "2.2.2.2"})
	if !reflect.DeepEqual(got, []string{"www.example.com"}) {
		t.Fatalf("dropDeferred = %v, want [www.example.com]", got)
	}
	if len(dropDeferred(victims, nil)) != 3 {
		t.Fatalf("empty deferred must not change the list")
	}
}

// 快速复检只排给「会有人动手」的成员：慢 IP 排，慢域名不排。
// 域名判慢没有处置通道，给它排复检等于每 2 分钟白摸一次，还把主节拍拽到高频档。
func TestRecordThroneProbeResultsRecheckSkipsSlowDomains(t *testing.T) {
	t.Setenv("PROXY_ACTIVE_PROBE_SLOW_MS", "1500")
	a := &app{throneProbe: map[string]*throneProbeStat{}}
	now := time.Now()
	a.recordThroneProbeResults([]proxyScanResult{
		{IP: "1.1.1.1", Latency: 1800},
		{IP: "www.example.com", Latency: 1800},
	}, now)
	if st := a.throneProbe["1.1.1.1"]; st == nil || st.RecheckAt.IsZero() {
		t.Fatalf("slow IP must get a fast recheck, got %+v", st)
	}
	if st := a.throneProbe["www.example.com"]; st == nil || !st.RecheckAt.IsZero() {
		t.Fatalf("slow domain must not get a fast recheck, got %+v", st)
	}
	// 域名连败仍要排：死了是真要处置的。
	a.recordThroneProbeResults([]proxyScanResult{
		{IP: "www.example.com", Error: "context deadline exceeded"},
	}, now)
	if st := a.throneProbe["www.example.com"]; st.RecheckAt.IsZero() {
		t.Fatalf("failed domain must get a fast recheck, got %+v", st)
	}
}

// 域名观察期：期内挡住，过期自动放行（不靠人工重置）。
func TestDomainQuarantineExpiresAutomatically(t *testing.T) {
	now := time.Now()
	a := &app{domainQuarantine: map[string]time.Time{}}
	a.quarantineDomain("dead.example.com", now.Add(domainQuarantine))
	if !a.domainQuarantined("dead.example.com", now.Add(time.Minute)) {
		t.Fatal("domain must be quarantined inside the window")
	}
	if a.domainQuarantined("dead.example.com", now.Add(domainQuarantine+time.Second)) {
		t.Fatal("domain must be released after the window expires")
	}
	if _, ok := a.domainQuarantine["dead.example.com"]; ok {
		t.Fatal("expired entry must be cleaned up")
	}
}
