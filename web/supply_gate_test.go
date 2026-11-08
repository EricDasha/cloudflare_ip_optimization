package main

import (
	"testing"
	"time"
)

// 生产回归：考生供给只做 TCP/TLS 就放行，35 个里 24 个连 WS 都敲不开；
// 同时它把 TLS 延迟上限压到 300ms，把握手 1.1s 但 WS 正常的好 IP 误杀。
// 下面三条钉死「同一口径」和「只做该做的事」。
func TestAdmitPassedUnderCeilingDropsSlowKeepsOrder(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Latency: 410},
		{IP: "2.2.2.2", Latency: 1605},
		{IP: "3.3.3.3", Latency: 520},
		{IP: "4.4.4.4", Latency: 1400},
	}
	passed := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"}
	got := admitPassedUnderCeiling(passed, results, 800)
	want := []string{"1.1.1.1", "3.3.3.3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order not preserved: got %v, want %v", got, want)
		}
	}
}

func TestAdmitPassedUnderCeilingKeepsUnknownAndZeroCeiling(t *testing.T) {
	// 成绩单里查不到（理论上不该发生）时保守保留，不做无依据的删除。
	got := admitPassedUnderCeiling([]string{"9.9.9.9"}, nil, 800)
	if len(got) != 1 || got[0] != "9.9.9.9" {
		t.Fatalf("unknown latency should be kept, got %v", got)
	}
	// 上限为 0 表示「不设闸」，原样返回。
	got = admitPassedUnderCeiling([]string{"1.1.1.1"}, []proxyScanResult{{IP: "1.1.1.1", Latency: 9999}}, 0)
	if len(got) != 1 {
		t.Fatalf("zero ceiling should not drop, got %v", got)
	}
}

// 自动重挑不能顺手把用户置顶的成员冲掉；但已不在现池里的置顶成员不复活。
func TestCarryPinnedIntoRebuildKeepsOnlyLivePins(t *testing.T) {
	current := proxyActivePool{
		IPs:      []string{"1.2.3.4", "5.6.7.8"},
		Domains:  []string{"pinned.example.com"},
		Priority: []string{"1.2.3.4", "9.9.9.9", "pinned.example.com"},
	}
	ips, domains := carryPinnedIntoRebuild([]string{"5.6.7.8"}, nil, current)
	if len(ips) != 2 || ips[0] != "5.6.7.8" || ips[1] != "1.2.3.4" {
		t.Fatalf("pinned live IP should be carried once, got %v", ips)
	}
	if len(domains) != 1 || domains[0] != "pinned.example.com" {
		t.Fatalf("pinned live domain should be carried, got %v", domains)
	}
	for _, ip := range ips {
		if ip == "9.9.9.9" {
			t.Fatalf("pinned IP no longer in pool must not be resurrected: %v", ips)
		}
	}
}

// 冷却锚点只认自动换池。维修动作（在位验活剔人、扶持、手动采用）不该把
// 30 分钟重新拨满——旧实现因此 6 小时里自动换池 0 次成功。
func TestCooldownAnchorIgnoresRepairSwitches(t *testing.T) {
	base := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	a := &app{
		activePool: proxyActivePool{IPs: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}},
	}
	a.markAutoSwitch(base)

	// 自动换池后 20 分钟发生一次维修换池：自动锚点必须不动。
	a.markPoolSwitched(base.Add(20 * time.Minute))
	if v := a.allowPoolSwitch(base.Add(31*time.Minute), "auto-apply"); !v.Allowed {
		t.Fatalf("repair switch must not restart auto cooldown, blocked by %q", v.Reason)
	}

	// 自动换池自己执行后才重新计时。
	a.markAutoSwitch(base.Add(31 * time.Minute))
	if v := a.allowPoolSwitch(base.Add(36*time.Minute), "auto-apply"); v.Allowed || v.Reason != "cooldown" {
		t.Fatalf("auto switch should start cooldown, got allowed=%v reason=%q", v.Allowed, v.Reason)
	}
	if v := a.allowPoolSwitch(base.Add(62*time.Minute), "auto-apply"); !v.Allowed {
		t.Fatalf("cooldown should expire after 30m, blocked by %q", v.Reason)
	}
}
