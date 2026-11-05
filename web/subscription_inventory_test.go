package main

import (
	"fmt"
	"testing"
	"time"
)

// newTestApp 造一个只带本子的 app，数据目录指向临时目录。
func newInventoryTestApp(t *testing.T) *app {
	t.Helper()
	a := &app{dataDir: t.TempDir()}
	a.subInv = subscriptionInventory{
		Entries: make(map[string]*subEntry),
		Fetches: make(map[string][]subFetchRecord),
	}
	return a
}

// 同一 IP 被多个维护者供出时必须合并为一条，来源记成列表。
// 这是「去重」的实际实现方式：以 IP 为主键，不是事后补一道筛。
func TestMergeSubscriptionHarvestMergesSources(t *testing.T) {
	a := newInventoryTestApp(t)
	added, refreshed := a.mergeSubscriptionHarvest(
		map[string]string{"1.2.3.4": "a.com", "5.6.7.8": "b.com"},
		nil, nil)
	if added != 2 || refreshed != 0 {
		t.Fatalf("added=%d refreshed=%d, want 2/0", added, refreshed)
	}
	// 第二轮：1.2.3.4 由另一个维护者也供出。
	added, refreshed = a.mergeSubscriptionHarvest(
		map[string]string{"1.2.3.4": "c.com"}, nil, nil)
	if added != 0 || refreshed != 1 {
		t.Fatalf("second round added=%d refreshed=%d, want 0/1", added, refreshed)
	}
	entry := a.subInv.Entries["1.2.3.4"]
	if entry == nil {
		t.Fatal("entry missing")
	}
	if len(entry.Sources) != 2 {
		t.Fatalf("sources = %v, want two maintainers", entry.Sources)
	}
	// 同一维护者重复供出不能重复记。
	a.mergeSubscriptionHarvest(map[string]string{"1.2.3.4": "a.com"}, nil, nil)
	if len(a.subInv.Entries["1.2.3.4"].Sources) != 2 {
		t.Fatalf("duplicate maintainer appended: %v", a.subInv.Entries["1.2.3.4"].Sources)
	}
}

// 新收进来的候选一律是 pending——收不是考试，不能默认「能用」。
func TestMergeSubscriptionHarvestStartsPending(t *testing.T) {
	a := newInventoryTestApp(t)
	a.mergeSubscriptionHarvest(map[string]string{"9.9.9.9": "a.com"}, nil, nil)
	if got := a.subInv.Entries["9.9.9.9"].State; got != subStatePending {
		t.Fatalf("state = %q, want pending", got)
	}
}

// 私网、非法地址不进本子。
func TestMergeSubscriptionHarvestRejectsNonPublic(t *testing.T) {
	a := newInventoryTestApp(t)
	a.mergeSubscriptionHarvest(map[string]string{
		"192.168.1.1": "a.com", "10.0.0.1": "a.com", "not-an-ip": "a.com", "8.8.8.8": "a.com",
	}, nil, nil)
	if len(a.subInv.Entries) != 1 {
		t.Fatalf("entries = %v, want only 8.8.8.8", a.subInv.Entries)
	}
}

// 拉取留痕按维护者保存，超出上限时保留最近的。
func TestMergeSubscriptionHarvestKeepsFetchHistory(t *testing.T) {
	a := newInventoryTestApp(t)
	total := subscriptionFetchHistoryPerMaintainer + 5
	for i := 0; i < total; i++ {
		a.mergeSubscriptionHarvest(nil, nil, map[string]subFetchRecord{
			"a.com": {At: time.Now(), OK: true, Extracted: i},
		})
	}
	history := a.subInv.Fetches["a.com"]
	if len(history) != subscriptionFetchHistoryPerMaintainer {
		t.Fatalf("history len = %d, want %d", len(history), subscriptionFetchHistoryPerMaintainer)
	}
	// 保留的必须是最新的那批：最后一条的 Extracted 应是 total-1。
	if last := history[len(history)-1].Extracted; last != total-1 {
		t.Fatalf("last record extracted = %d, want %d", last, total-1)
	}
}

// 单次失败不判死：连续失败到阈值才降级。
func TestRecordSubscriptionProbeNeedsConsecutiveFails(t *testing.T) {
	a := newInventoryTestApp(t)
	a.mergeSubscriptionHarvest(map[string]string{"1.2.3.4": "a.com"}, nil, nil)
	now := time.Now()
	for i := 1; i < subscriptionEntryFailThreshold; i++ {
		a.recordSubscriptionProbeLocked("1.2.3.4", subscriptionProbeOutcome{
			At: now, OK: false, Error: "timeout", ErrorClass: failureTimeout,
		})
		if got := a.subInv.Entries["1.2.3.4"].State; got == subStateFail {
			t.Fatalf("judged dead after %d failure(s), want threshold %d", i, subscriptionEntryFailThreshold)
		}
	}
	a.recordSubscriptionProbeLocked("1.2.3.4", subscriptionProbeOutcome{
		At: now, OK: false, Error: "timeout", ErrorClass: failureTimeout,
	})
	if got := a.subInv.Entries["1.2.3.4"].State; got != subStateFail {
		t.Fatalf("state = %q after %d consecutive fails, want fail", got, subscriptionEntryFailThreshold)
	}
}

// 一次成功必须清空连败计数，并把成绩写进去。
func TestRecordSubscriptionProbeResetsOnPass(t *testing.T) {
	a := newInventoryTestApp(t)
	a.mergeSubscriptionHarvest(map[string]string{"1.2.3.4": "a.com"}, nil, nil)
	now := time.Now()
	a.recordSubscriptionProbeLocked("1.2.3.4", subscriptionProbeOutcome{At: now, OK: false, ErrorClass: failureTimeout})
	a.recordSubscriptionProbeLocked("1.2.3.4", subscriptionProbeOutcome{
		At: now, OK: true, Stage: "vless", LatencyMs: 88, Mbps: 42.5, Colo: "HKG",
	})
	entry := a.subInv.Entries["1.2.3.4"]
	if entry.State != subStatePass || entry.ConsecutiveFails != 0 {
		t.Fatalf("state=%q consecutiveFails=%d, want pass/0", entry.State, entry.ConsecutiveFails)
	}
	if entry.Mbps != 42.5 || entry.Stage != "vless" || entry.Colo != "HKG" {
		t.Fatalf("scores not recorded: %+v", entry)
	}
	if entry.LastError != "" || entry.ErrorClass != "" {
		t.Fatalf("stale error kept after pass: %q / %q", entry.LastError, entry.ErrorClass)
	}
}

// 考试优先级：从未考过的先考，其次过期复核，最后失败重试。
// 这一顺序保证新料不会因为一批顽固失败者占住预算而永远轮不到。
func TestSubscriptionProbeCandidatesPriority(t *testing.T) {
	a := newInventoryTestApp(t)
	now := time.Now()
	a.subInv.Entries["10.0.0.1"] = &subEntry{State: subStatePending, FirstSeen: now}
	a.subInv.Entries["10.0.0.2"] = &subEntry{State: subStatePass, LastProbe: now.Add(-10 * time.Hour), FirstSeen: now}
	a.subInv.Entries["10.0.0.3"] = &subEntry{State: subStateFail, LastProbe: now.Add(-10 * time.Hour), FirstSeen: now}
	a.subInv.Entries["10.0.0.4"] = &subEntry{State: subStatePass, LastProbe: now.Add(-1 * time.Minute), FirstSeen: now}
	got := a.subscriptionProbeCandidates(10, 6*time.Hour, 2*time.Hour)
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
}

// 刚考过且还在有效期内的 pass 不重复考；未到重试期的 fail 也不考。
func TestSubscriptionProbeCandidatesSkipsFresh(t *testing.T) {
	a := newInventoryTestApp(t)
	now := time.Now()
	a.subInv.Entries["10.0.0.1"] = &subEntry{State: subStatePass, LastProbe: now.Add(-1 * time.Minute)}
	a.subInv.Entries["10.0.0.2"] = &subEntry{State: subStateFail, LastProbe: now.Add(-1 * time.Minute)}
	if got := a.subscriptionProbeCandidates(10, 6*time.Hour, 2*time.Hour); len(got) != 0 {
		t.Fatalf("candidates = %v, want none", got)
	}
}

// 敲开过门但没轮到真数据面的 pending，必须按重试周期排队，
// 不能每轮都重新排最前——否则预算永远花在重复劳动上。
func TestSubscriptionProbeCandidatesDefersQueuedPending(t *testing.T) {
	a := newInventoryTestApp(t)
	now := time.Now()
	a.subInv.Entries["10.0.0.1"] = &subEntry{State: subStatePending, LastProbe: now.Add(-1 * time.Minute)}
	a.subInv.Entries["10.0.0.2"] = &subEntry{State: subStatePending}
	got := a.subscriptionProbeCandidates(10, 6*time.Hour, 2*time.Hour)
	if len(got) != 1 || got[0] != "10.0.0.2" {
		t.Fatalf("candidates = %v, want only the never-probed one", got)
	}
	// 过了重试期就回到队列，且仍是最高优先级。
	a.subInv.Entries["10.0.0.1"].LastProbe = now.Add(-3 * time.Hour)
	got = a.subscriptionProbeCandidates(10, 6*time.Hour, 2*time.Hour)
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want both after retry window", got)
	}
}

// 取用口径：只取 pass，按速度降序、延迟升序。
func TestSubscriptionPassedIPsOrdering(t *testing.T) {
	a := newInventoryTestApp(t)
	now := time.Now()
	a.subInv.Entries["10.0.0.1"] = &subEntry{State: subStatePass, Mbps: 10, LatencyMs: 100, FirstSeen: now}
	a.subInv.Entries["10.0.0.2"] = &subEntry{State: subStatePass, Mbps: 50, LatencyMs: 300, FirstSeen: now}
	a.subInv.Entries["10.0.0.3"] = &subEntry{State: subStatePass, Mbps: 10, LatencyMs: 50, FirstSeen: now}
	a.subInv.Entries["10.0.0.4"] = &subEntry{State: subStatePending}
	a.subInv.Entries["10.0.0.5"] = &subEntry{State: subStateFail}
	got := a.subscriptionPassedIPs(10)
	want := []string{"10.0.0.2", "10.0.0.3", "10.0.0.1"}
	if len(got) != len(want) {
		t.Fatalf("passed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("passed = %v, want %v", got, want)
		}
	}
	if got := a.subscriptionPassedIPs(2); len(got) != 2 {
		t.Fatalf("remaining cap ignored: %v", got)
	}
}

// 淘汰规则：必须同时满足「久未露面」与「连败到两倍阈值」。
// 只满足一条的留下——维护者暂时拉不到不等于 IP 死了。
func TestPruneSubscriptionInventoryRequiresBothConditions(t *testing.T) {
	a := newInventoryTestApp(t)
	now := time.Now()
	stale := now.Add(-subscriptionInventoryStale - time.Hour)

	a.subInv.Entries["10.0.0.1"] = &subEntry{State: subStateFail, ConsecutiveFails: subscriptionEntryFailThreshold * 2, LastSeen: stale}
	a.subInv.Entries["10.0.0.2"] = &subEntry{State: subStateFail, ConsecutiveFails: subscriptionEntryFailThreshold * 2, LastSeen: now}
	a.subInv.Entries["10.0.0.3"] = &subEntry{State: subStatePending, LastSeen: stale}
	a.subInv.Entries["10.0.0.4"] = &subEntry{State: subStatePass, LastSeen: stale}

	removed := a.pruneSubscriptionInventoryLocked(now)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, ok := a.subInv.Entries["10.0.0.1"]; ok {
		t.Fatal("stale + dead entry was kept")
	}
	for _, ip := range []string{"10.0.0.2", "10.0.0.3", "10.0.0.4"} {
		if _, ok := a.subInv.Entries[ip]; !ok {
			t.Fatalf("%s wrongly pruned", ip)
		}
	}
}

// 超容时按「先淘汰最没用」的顺序削，pass 的最后动。
func TestPruneSubscriptionInventoryCapsByRank(t *testing.T) {
	a := newInventoryTestApp(t)
	now := time.Now()
	// 构造超容：fail 若干、pending 若干、pass 若干。
	for i := 0; i < subscriptionInventoryCap+30; i++ {
		ip := fmt.Sprintf("10.1.%d.%d", i/250, i%250+1)
		state := subStatePass
		switch {
		case i < 20:
			state = subStateFail
		case i < 40:
			state = subStatePending
		}
		a.subInv.Entries[ip] = &subEntry{State: state, LastSeen: now}
	}
	a.pruneSubscriptionInventoryLocked(now)
	if len(a.subInv.Entries) > subscriptionInventoryCap {
		t.Fatalf("entries = %d, want <= %d", len(a.subInv.Entries), subscriptionInventoryCap)
	}
	// 先削 fail：容量只超 30，20 个 fail 全部该走。
	fails := 0
	for _, entry := range a.subInv.Entries {
		if entry.State == subStateFail {
			fails++
		}
	}
	if fails != 0 {
		t.Fatalf("%d fail entries survived the cap prune", fails)
	}
}

// 手工移除单条：合法 IPv4 删得掉，非法输入与不存在的地址返回 false。
func TestRemoveSubscriptionEntry(t *testing.T) {
	a := newInventoryTestApp(t)
	a.mergeSubscriptionHarvest(map[string]string{"1.2.3.4": "a.com"}, nil, nil)
	removed, ok := a.removeSubscriptionEntry("1.2.3.4")
	if !ok || removed != "1.2.3.4" {
		t.Fatalf("removed=%q ok=%v", removed, ok)
	}
	if _, ok := a.subInv.Entries["1.2.3.4"]; ok {
		t.Fatal("entry still present")
	}
	if _, ok := a.removeSubscriptionEntry("1.2.3.4"); ok {
		t.Fatal("removing a missing entry reported success")
	}
	if _, ok := a.removeSubscriptionEntry("not-an-ip"); ok {
		t.Fatal("garbage accepted")
	}
}

// 总览计数必须与条目一致，且失败分类只统计非 pass 的。
func TestSubscriptionInventorySummaryCounts(t *testing.T) {
	a := newInventoryTestApp(t)
	now := time.Now()
	a.subInv.Entries["10.0.0.1"] = &subEntry{State: subStatePass, Sources: []string{"a.com"}}
	a.subInv.Entries["10.0.0.2"] = &subEntry{State: subStatePending, Sources: []string{"a.com"}}
	a.subInv.Entries["10.0.0.3"] = &subEntry{State: subStateFail, Sources: []string{"b.com"}, ErrorClass: failureTimeout, LastProbe: now}
	a.subInv.Entries["10.0.0.4"] = &subEntry{State: subStateFail, Sources: []string{"b.com"}, ErrorClass: failureTimeout, LastProbe: now}
	summary := a.subscriptionInventorySummary()
	if summary["total"].(int) != 4 {
		t.Fatalf("total = %v", summary["total"])
	}
	if summary["pass"].(int) != 1 || summary["pending"].(int) != 1 || summary["fail"].(int) != 2 {
		t.Fatalf("counts = pass:%v pending:%v fail:%v", summary["pass"], summary["pending"], summary["fail"])
	}
	bySource := summary["bySource"].(map[string]int)
	if bySource["a.com"] != 2 || bySource["b.com"] != 2 {
		t.Fatalf("bySource = %v", bySource)
	}
	classes := summary["errorClasses"].(map[string]int)
	if classes[failureTimeout] != 2 {
		t.Fatalf("errorClasses = %v", classes)
	}
}
