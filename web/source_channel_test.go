package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// 渠道归类是页面饼图的口径，错一个前缀就把整层算到别人头上。
func TestSourceChannelOf(t *testing.T) {
	cases := map[string]string{
		"user:env":                              "user",
		"cfdata:cfdata-result.txt":              "cfdata",
		"sub:example.com":                       "subscription",
		"subscription:pool":                     "subscription",
		"https://bestcf.pages.dev/s5gy/all.txt": "bestcf",
		"http://legacy.example.com/list.txt":    "bestcf",
		"dns:proxy.example.com":                 "",
		"":                                      "",
	}
	for key, want := range cases {
		if got := sourceChannelOf(key); got != want {
			t.Errorf("sourceChannelOf(%q) = %q, want %q", key, got, want)
		}
	}
}

// 汇总要同时回答两个问题：这一层出过多少 IP，现在还有多少在位可用。
// 在位分档用验活账：连败达阈值算失败，有慢记录算慢，没记录的新成员算活。
func TestSourceChannelSummaryAggregates(t *testing.T) {
	a := &app{
		sourceHealth: make(map[string]*sourceHealthStat),
		activeHealth: make(map[string]*activeHealthStat),
		throneProbe:  make(map[string]*throneProbeStat),
	}
	a.recordSourceHealth("user:env", 5, nil)
	a.recordSourceHealth("https://src.example.com/a.txt", 10, nil)
	a.recordSourceHealth("https://src.example.com/a.txt", 10, nil)
	a.recordSourceHealth("https://src.example.com/a.txt", 0, errTestSourceDead)
	a.recordSourceHealth("sub:someone", 7, nil)
	a.recordSourceHealth("cfdata:result.txt", 3, nil)

	a.activePool = proxyActivePool{
		IPs:     []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "9.9.9.9"},
		Domains: []string{"edge.example.com"},
	}
	a.candidates = proxyCandidateSnapshot{
		IPs: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"},
		SourceByIP: map[string]string{
			"1.1.1.1": "bestcf",
			"2.2.2.2": "bestcf",
			"3.3.3.3": "user",
			"4.4.4.4": "user",
			"5.5.5.5": "subscription",
		},
	}
	a.throneProbe["1.1.1.1"] = &throneProbeStat{ConsecutiveFailures: throneProbeFailThreshold()}
	a.throneProbe["2.2.2.2"] = &throneProbeStat{SlowStrikes: 1}
	a.throneProbe["3.3.3.3"] = &throneProbeStat{}

	got := map[string]sourceChannelStat{}
	for _, ch := range a.sourceChannelSummary() {
		got[ch.Channel] = ch
	}

	bestcf := got["bestcf"]
	// 环里只算 IP：1.1.1.1 连败、2.2.2.2 慢 → 在位 2，活 0，慢 1，败 1。
	// 域名 edge.example.com 不进来（优选域名没有来源账，混进来会让 BestCF 恒占大头）。
	if bestcf.InPool != 2 || bestcf.Alive != 0 || bestcf.Slow != 1 || bestcf.Failing != 1 {
		t.Fatalf("bestcf 分档 = inPool %d alive %d slow %d failing %d, want 2/0/1/1",
			bestcf.InPool, bestcf.Alive, bestcf.Slow, bestcf.Failing)
	}
	if bestcf.Candidates != 2 || bestcf.Fetches != 3 || bestcf.Successes != 2 {
		t.Fatalf("bestcf 账 = candidates %d fetches %d successes %d, want 2/3/2",
			bestcf.Candidates, bestcf.Fetches, bestcf.Successes)
	}

	user := got["user"]
	// 3.3.3.3 来自手动层；9.9.9.9 不在候选来源表里，只可能是手动扶持/固定。
	if user.InPool != 2 || user.Alive != 2 || user.Failing != 0 {
		t.Fatalf("user 分档 = inPool %d alive %d failing %d, want 2/2/0",
			user.InPool, user.Alive, user.Failing)
	}
	if user.Candidates != 2 {
		t.Fatalf("user 候选数 = %d, want 2", user.Candidates)
	}
	if user.Score <= bestcf.Score {
		t.Fatalf("全活的手动层应排在 bestcf 之前: user %.1f bestcf %.1f", user.Score, bestcf.Score)
	}
	if sub := got["subscription"]; sub.InPool != 0 || sub.Candidates != 1 {
		t.Fatalf("subscription = inPool %d candidates %d, want 0/1", sub.InPool, sub.Candidates)
	}
	if cfd := got["cfdata"]; cfd.InPool != 0 || cfd.Candidates != 0 || cfd.Sources != 1 {
		t.Fatalf("cfdata = %+v, want inPool 0 candidates 0 sources 1", cfd)
	}
}

// 测速通路：连续三轮「TCP 可达、测速全零」才报警，一次成功立即解除。
func TestSpeedPathSuspectAndRecovery(t *testing.T) {
	a := &app{}
	now := time.Now()
	for i := 0; i < speedPathWarnRounds; i++ {
		a.recordSpeedPathRound(now, 10, 0, 10)
	}
	if st := a.speedPathStatus(); !st.Suspect || st.ZeroRounds != speedPathWarnRounds {
		t.Fatalf("连续 %d 轮零成绩应判可疑，实际 %+v", speedPathWarnRounds, st)
	}
	a.recordSpeedPathRound(now, 10, 3, 10)
	st := a.speedPathStatus()
	if st.Suspect || st.ZeroRounds != 0 || st.Reason != "" {
		t.Fatalf("测出速度后应解除可疑，实际 %+v", st)
	}

	// TCP 全不通说明问题在网络/域名，不能算到测速端点头上。
	a2 := &app{}
	for i := 0; i < speedPathWarnRounds+2; i++ {
		a2.recordSpeedPathRound(now, 10, 0, 0)
	}
	if st := a2.speedPathStatus(); st.Suspect {
		t.Fatalf("TCP 全灭不应判测速通路可疑，实际 %+v", st)
	}

	// Q,Q,N,Q：中间隔了一轮 TCP 全灭，就不该说「连续」。
	a3 := &app{}
	a3.recordSpeedPathRound(now, 10, 0, 10)
	a3.recordSpeedPathRound(now, 10, 0, 10)
	a3.recordSpeedPathRound(now, 10, 0, 0)
	a3.recordSpeedPathRound(now, 10, 0, 10)
	if st := a3.speedPathStatus(); st.Suspect || st.ZeroRounds != 1 {
		t.Fatalf("断档后应重新计数，实际 %+v", st)
	}

	// 报警后每轮都要重写原因，不能停在阈值那一轮的数字上。
	a4 := &app{}
	for i := 0; i < speedPathWarnRounds+2; i++ {
		a4.recordSpeedPathRound(now, 10, 0, 10)
	}
	st4 := a4.speedPathStatus()
	if st4.ZeroRounds != speedPathWarnRounds+2 || !strings.Contains(st4.Reason, "连续 5 轮") {
		t.Fatalf("原因应反映当前轮数，实际 %+v", st4)
	}
}

// 来源映射必须在换池时延续：候选快照会整份换掉，不能拿它反推历史。
func TestReconcileMemberChannelsKeepsProvenance(t *testing.T) {
	a := &app{}
	a.candidates = proxyCandidateSnapshot{SourceByIP: map[string]string{"4.4.4.4": "bestcf"}}
	got := a.reconcileMemberChannels(
		proxyActivePool{IPs: []string{"1.1.1.1", "4.4.4.4", "5.5.5.5", "6.6.6.6"}},
		proxyActivePool{
			MemberChannels: map[string]string{"1.1.1.1": "cfdata"},
			// 旧版本落盘的池没有映射时，用上次扫描的 SourceRank 补一次。
			Results: []proxyScanResult{{IP: "5.5.5.5", SourceRank: 1}},
		},
	)
	want := map[string]string{
		"1.1.1.1": "cfdata", // 已入池：映射优先
		"4.4.4.4": "bestcf", // 新成员：当前候选快照
		"5.5.5.5": "cfdata", // 老成员：上次扫描留下的名次
		"6.6.6.6": "user",   // 什么都不知道：按手动
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("memberChannels = %#v, want %#v", got, want)
	}
}

// 渠道汇总优先读入池时记下的来源，快照换了也不改口径。
func TestSourceChannelSummaryPrefersMemberChannels(t *testing.T) {
	a := &app{
		sourceHealth: make(map[string]*sourceHealthStat),
		throneProbe:  make(map[string]*throneProbeStat),
		activePool: proxyActivePool{
			IPs: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"},
			MemberChannels: map[string]string{
				"1.1.1.1": "cfdata",
				"2.2.2.2": "subscription",
			},
		},
		candidates: proxyCandidateSnapshot{
			IPs:        []string{"1.1.1.1"},
			SourceByIP: map[string]string{"1.1.1.1": "bestcf"},
		},
	}
	got := map[string]int{}
	for _, ch := range a.sourceChannelSummary() {
		got[ch.Channel] = ch.InPool
	}
	// 1.1.1.1 入池时是 cfdata，即使当前快照说 bestcf 也不改；3.3.3.3 无记录按手动。
	if got["cfdata"] != 1 || got["subscription"] != 1 || got["user"] != 1 || got["bestcf"] != 0 {
		t.Fatalf("渠道在位 = %#v, want cfdata 1 / subscription 1 / user 1 / bestcf 0", got)
	}
}

type testSourceError struct{}

func (testSourceError) Error() string { return "boom" }

var errTestSourceDead = testSourceError{}
