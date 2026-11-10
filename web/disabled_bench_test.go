package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 关闭成员不进 -fixed，就不该参与「谁在转发」的任何计算。
func TestForwardingIPsExcludesDisabled(t *testing.T) {
	pool := proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"},
		Domains:  []string{"a.example.com", "b.example.com"},
		Disabled: []string{"2.2.2.2", "b.example.com"},
	}
	ips := forwardingIPs(pool)
	if len(ips) != 2 || ips[0] != "1.1.1.1" || ips[1] != "3.3.3.3" {
		t.Fatalf("forwardingIPs() = %v, want [1.1.1.1 3.3.3.3]", ips)
	}
	members := forwardingMembers(pool)
	want := []string{"1.1.1.1", "3.3.3.3", "a.example.com"}
	if len(members) != len(want) {
		t.Fatalf("forwardingMembers() = %v, want %v", members, want)
	}
	for i := range want {
		if members[i] != want[i] {
			t.Fatalf("forwardingMembers() = %v, want %v", members, want)
		}
	}
}

// 旧池存活证明不能拿关闭成员的死活充数。
func TestVerifyActivePoolAliveSkipsDisabled(t *testing.T) {
	orig := probeTCPAlive
	defer func() { probeTCPAlive = orig }()
	var probed []string
	probeTCPAlive = func(_ context.Context, addr string, _ time.Duration) bool {
		probed = append(probed, addr)
		return true
	}
	a := &app{dataDir: t.TempDir()}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2"},
		Domains:  []string{"a.example.com", "b.example.com"},
		Disabled: []string{"2.2.2.2", "b.example.com"},
	}
	alive, total := a.verifyActivePoolAlive()
	if alive != 2 || total != 2 {
		t.Fatalf("verifyActivePoolAlive() = %d/%d, want 2/2", alive, total)
	}
	for _, addr := range probed {
		if addr == "2.2.2.2:443" || addr == "b.example.com:443" {
			t.Fatalf("关闭成员不应被探测：%v", probed)
		}
	}
}

// 关闭成员的健康记录不能顶住健康闸。旧实现看 pool.IPs 全集，三个在转发的
// 成员全健康、两个关闭成员有失败记录时，会被判成「不健康」而放行换池。
func TestAllowPoolSwitchHealthGateIgnoresDisabled(t *testing.T) {
	now := time.Now()
	a := &app{dataDir: t.TempDir(), activeHealth: map[string]*activeHealthStat{}}
	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		a.activeHealth[ip] = &activeHealthStat{Success: 5, LastCheck: now}
	}
	for _, ip := range []string{"8.8.8.8", "9.9.9.9"} {
		a.activeHealth[ip] = &activeHealthStat{Success: 0, Failures: 5, LastFail: now, LastCheck: now}
	}
	a.activePool = proxyActivePool{
		UpdatedAt: now.Add(-time.Hour), Host: "h", Path: "/p",
		IPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "8.8.8.8", "9.9.9.9"},
		Disabled: []string{"8.8.8.8", "9.9.9.9"},
	}
	a.lastPoolSwitchAt = now.Add(-time.Hour)
	verdict := a.allowPoolSwitch(now, "test-disabled-health")
	if verdict.Allowed || verdict.Reason != "healthy" {
		t.Fatalf("转发成员全健康时应被健康闸拦下，got %+v", verdict)
	}
}

// 关闭成员死光不能触发救援；转发成员死光即使关闭成员还活着也必须救援。
func TestAllowPoolSwitchRescueOnlyCountsForwarding(t *testing.T) {
	now := time.Now()
	threshold := throneProbeFailThreshold()
	mk := func(deadIPs, liveIPs []string) *app {
		a := &app{dataDir: t.TempDir(), activeHealth: map[string]*activeHealthStat{}, throneProbe: map[string]*throneProbeStat{}}
		for _, ip := range deadIPs {
			a.throneProbe[ip] = &throneProbeStat{ConsecutiveFailures: threshold, LastProbe: now}
		}
		for _, ip := range liveIPs {
			a.throneProbe[ip] = &throneProbeStat{ConsecutiveFailures: 0, LastProbe: now}
		}
		return a
	}

	a := mk([]string{"8.8.8.8", "9.9.9.9"}, []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"})
	a.activePool = proxyActivePool{
		UpdatedAt: now.Add(-time.Minute), Host: "h", Path: "/p",
		IPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "8.8.8.8", "9.9.9.9"},
		Disabled: []string{"8.8.8.8", "9.9.9.9"},
	}
	a.markPoolSwitched(now)
	if v := a.allowPoolSwitch(now.Add(time.Minute), "test-disabled-dead"); v.Rescue {
		t.Fatal("关闭成员全死不应触发救援")
	}

	b := mk([]string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}, []string{"8.8.8.8", "9.9.9.9"})
	b.activePool = proxyActivePool{
		UpdatedAt: now.Add(-time.Minute), Host: "h", Path: "/p",
		IPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "8.8.8.8", "9.9.9.9"},
		Disabled: []string{"8.8.8.8", "9.9.9.9"},
	}
	b.markPoolSwitched(now)
	if v := b.allowPoolSwitch(now.Add(time.Minute), "test-forwarding-dead"); !v.Rescue {
		t.Fatalf("转发成员全死时应救援，got %+v", v)
	}
}

// 在位探针只处置转发成员：关闭成员判死不在它的职权内。
func TestThroneActionPlanSkipsDisabled(t *testing.T) {
	threshold := throneProbeFailThreshold()
	a := &app{throneProbe: map[string]*throneProbeStat{
		"1.1.1.1": {ConsecutiveFailures: threshold},
		"2.2.2.2": {ConsecutiveFailures: threshold},
	}}
	// 再放一个健康的转发成员：避免触发「全部判死时保留一个」的兜底规则，
	// 那会把唯一死者留下，测试就测不到「关闭成员不在处置名单」这件事。
	pool := proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"},
		Disabled: []string{"2.2.2.2"},
	}
	dead, _ := a.throneActionPlan(pool)
	if len(dead) != 1 || dead[0] != "1.1.1.1" {
		t.Fatalf("throneActionPlan() = %v, want [1.1.1.1]", dead)
	}
}

// 账本只增不减会把页面撑成一张历史表，还会让历史成员的复检标记拖快节拍。
func TestPruneThroneLedger(t *testing.T) {
	a := &app{throneProbe: map[string]*throneProbeStat{
		"1.1.1.1": {LastProbe: time.Now()},
		"2.2.2.2": {LastProbe: time.Now(), RecheckAt: time.Now().Add(time.Minute)},
		"3.3.3.3": {LastProbe: time.Now()},
	}}
	a.pruneThroneLedger([]string{"1.1.1.1"})
	if len(a.throneProbe) != 1 {
		t.Fatalf("账本未裁剪：%v", a.throneProbe)
	}
	if a.throneProbe["1.1.1.1"] == nil {
		t.Fatal("在册成员被误删")
	}
}

func TestDecideDisabledBench(t *testing.T) {
	cases := []struct {
		name       string
		wsOK       bool
		fails      int
		speedOK    bool
		mbps       float64
		latencyMs  int64
		wantAction string
	}{
		{"握手首次失败只观察", false, 1, false, 0, 0, disabledBenchKeep},
		{"连续失败到阈值就扔", false, 2, false, 0, 0, disabledBenchRemove},
		{"速度低于下限进比较", true, 0, true, 1.5, 200, disabledBenchWeak},
		{"速度达标保留", true, 0, true, 12, 200, disabledBenchKeep},
		{"测速缺失但延迟超慢阈值进比较", true, 0, false, 0, 2000, disabledBenchWeak},
		{"测速缺失但延迟正常保留", true, 0, false, 0, 300, disabledBenchKeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, reason := decideDisabledBench(tc.wsOK, tc.fails, 2, tc.speedOK, tc.mbps, 3, tc.latencyMs, 1500)
			if action != tc.wantAction {
				t.Fatalf("action = %q (%s), want %q", action, reason, tc.wantAction)
			}
			if reason == "" {
				t.Fatal("每个结论都要带理由")
			}
		})
	}
}

func TestDisabledCandidateBeats(t *testing.T) {
	speedVictim := disabledBenchStat{SpeedTested: true, LastLatencyMs: 300, LastMbps: 4}
	if disabledCandidateBeats(speedVictim, 200, 4.9, true, 3) {
		t.Fatal("速度未达 1.25 倍不应换")
	}
	if !disabledCandidateBeats(speedVictim, 200, 5.1, true, 3) {
		t.Fatal("速度达 1.25 倍且不低于下限应换")
	}
	noSpeedVictim := disabledBenchStat{LastLatencyMs: 300}
	if disabledCandidateBeats(noSpeedVictim, 200, 2.9, true, 3) {
		t.Fatal("替补速度低于下限不应换")
	}
	if !disabledCandidateBeats(noSpeedVictim, 200, 3.1, true, 3) {
		t.Fatal("替补达到下限应换")
	}
	latencyVictim := disabledBenchStat{LastLatencyMs: 900}
	if !disabledCandidateBeats(latencyVictim, 600, 0, false, 3) {
		t.Fatal("延迟改善超过 25% 且 ≥15ms 应换")
	}
	if disabledCandidateBeats(latencyVictim, 700, 0, false, 3) {
		t.Fatal("延迟改善不足 25% 不应换")
	}
}

// 纯剔除关闭成员不改变 -fixed，只落盘、不重启 cfnat。
// 这里给 cfnat 配一个必然启动失败的假二进制：若代码误走 applyPoolLocked，
// 池子不会被改坏，测试也能从结果看出来。
func TestApplyDisabledBenchRemovalSkipsRestart(t *testing.T) {
	a := &app{
		dataDir:       t.TempDir(),
		cfnat:         newManagedProcess("cfnat", filepath.Join(t.TempDir(), "missing-cfnat")),
		disabledBench: map[string]*disabledBenchStat{"9.9.9.9": {LastLatencyMs: 1000}},
	}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "9.9.9.9"},
		Disabled: []string{"9.9.9.9"},
	}
	a.applyDisabledBenchChanges([]string{"9.9.9.9"}, nil, nil, time.Now())

	pool := a.proxyActivePoolSnapshot()
	if containsString(pool.IPs, "9.9.9.9") {
		t.Fatalf("关闭成员未被剔除：%v", pool.IPs)
	}
	if len(pool.Disabled) != 0 {
		t.Fatalf("Disabled 未清理：%v", pool.Disabled)
	}
	if _, err := os.Stat(filepath.Join(a.dataDir, proxyActivePoolFile)); err != nil {
		t.Fatalf("池变更未落盘：%v", err)
	}
	if len(a.disabledBenchEvents) != 1 || a.disabledBenchEvents[0].Member != "9.9.9.9" {
		t.Fatalf("处置记录缺失：%v", a.disabledBenchEvents)
	}
}

// 剔除关闭 IP 时不能把同样被关闭的域名一起「解冻」。Disabled 的清理范围必须
// 同时对照 IP 与域名：只按 IP 过滤会让域名在下一轮悄悄恢复转发——关是用户的
// 决定，不能被另一条链顺手撤销。
func TestApplyDisabledBenchKeepsDisabledDomains(t *testing.T) {
	a := &app{
		dataDir:       t.TempDir(),
		cfnat:         newManagedProcess("cfnat", filepath.Join(t.TempDir(), "missing-cfnat")),
		disabledBench: map[string]*disabledBenchStat{"9.9.9.9": {LastLatencyMs: 1000}},
	}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "9.9.9.9"},
		Domains:  []string{"edge.example.com"},
		Disabled: []string{"9.9.9.9", "edge.example.com"},
	}
	a.applyDisabledBenchChanges([]string{"9.9.9.9"}, nil, nil, time.Now())

	pool := a.proxyActivePoolSnapshot()
	if containsString(pool.IPs, "9.9.9.9") {
		t.Fatalf("关闭 IP 未被剔除：%v", pool.IPs)
	}
	if !containsString(pool.Domains, "edge.example.com") {
		t.Fatalf("域名离开了生效池：%v", pool.Domains)
	}
	if !containsString(pool.Disabled, "edge.example.com") {
		t.Fatalf("关闭的域名被错误解冻：%v", pool.Disabled)
	}
}
