package main

import (
	"strconv"
	"testing"
	"time"
)

func ledgerEntryOf(t *testing.T, a *app, ip string) *ipLedgerEntry {
	t.Helper()
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	entry, ok := a.ledger.IPs[ip]
	if !ok {
		t.Fatalf("ledger entry %s missing", ip)
	}
	return entry
}

func TestLedgerObserveUpdatesEWMAAndFailStreak(t *testing.T) {
	t.Setenv("PROXY_EWMA_ALPHA", "0.3")
	a := newLedgerTestApp(t)

	a.ledgerObserveProbe("1.1.1.1", true, 10)
	entry := ledgerEntryOf(t, a, "1.1.1.1")
	if entry.EWMAMbps != 10 || entry.LastMbps != 10 || entry.FailStreak != 0 {
		t.Fatalf("first pass: ewma=%v last=%v streak=%d", entry.EWMAMbps, entry.LastMbps, entry.FailStreak)
	}

	a.ledgerObserveProbe("1.1.1.1", true, 20)
	entry = ledgerEntryOf(t, a, "1.1.1.1")
	if want := 0.3*20 + 0.7*10; entry.EWMAMbps != want {
		t.Fatalf("ewma = %v, want %v", entry.EWMAMbps, want)
	}

	a.ledgerObserveProbe("1.1.1.1", false, 0)
	entry = ledgerEntryOf(t, a, "1.1.1.1")
	if entry.FailStreak != 1 {
		t.Fatalf("failStreak = %d, want 1", entry.FailStreak)
	}
	if entry.EWMAMbps != 0.3*20+0.7*10 {
		t.Fatalf("failure must not change ewma, got %v", entry.EWMAMbps)
	}

	// 通过但没有数据面成绩：清零失败计数，但不能把历史吞吐抹成 0。
	a.ledgerObserveProbe("1.1.1.1", true, 0)
	entry = ledgerEntryOf(t, a, "1.1.1.1")
	if entry.FailStreak != 0 {
		t.Fatalf("failStreak = %d, want 0", entry.FailStreak)
	}
	if entry.LastMbps != 20 || entry.EWMAMbps != 0.3*20+0.7*10 {
		t.Fatalf("mbps=0 must not overwrite history: last=%v ewma=%v", entry.LastMbps, entry.EWMAMbps)
	}
}

func TestLedgerRankingHistorySkipsUnstable(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.IPs["1.1.1.1"] = &ipLedgerEntry{IP: "1.1.1.1", PassCount: 2, EWMAMbps: 8, LastSeen: time.Now()}
	a.ledger.IPs["2.2.2.2"] = &ipLedgerEntry{IP: "2.2.2.2", PassCount: 3, EWMAMbps: 9, FailStreak: 3, LastSeen: time.Now()}
	a.ledger.IPs["3.3.3.3"] = &ipLedgerEntry{IP: "3.3.3.3", PassCount: 0, EWMAMbps: 7, LastSeen: time.Now()}
	a.ledger.IPs["4.4.4.4"] = &ipLedgerEntry{IP: "4.4.4.4", PassCount: 1, EWMAMbps: 0, LastSeen: time.Now()}
	a.ledgerMu.Unlock()

	history := a.ledgerRankingHistory()
	if len(history) != 1 || history[0].IP != "1.1.1.1" || history[0].DownloadMbps != 8 {
		t.Fatalf("history = %+v, want only 1.1.1.1 with 8 Mbps", history)
	}
}

func TestMergeRankingHistoryPrefersFresh(t *testing.T) {
	fresh := []proxyScanResult{{IP: "1.1.1.1", DownloadMbps: 10}}
	ledger := []proxyScanResult{{IP: "1.1.1.1", DownloadMbps: 3}, {IP: "2.2.2.2", DownloadMbps: 5}}

	merged := mergeRankingHistory(fresh, ledger)
	if len(merged) != 2 {
		t.Fatalf("merged = %+v", merged)
	}
	if merged[0].IP != "1.1.1.1" || merged[0].DownloadMbps != 10 {
		t.Fatalf("fresh must win: %+v", merged[0])
	}
	if merged[1].IP != "2.2.2.2" || merged[1].DownloadMbps != 5 {
		t.Fatalf("ledger member missing: %+v", merged[1])
	}
}

// 账本的意义：上一轮不在池里的成员，排名仍能看到它的历史吞吐。
func TestRankHistoryUsesLedgerForAbsentMember(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.IPs["1.1.1.1"] = &ipLedgerEntry{IP: "1.1.1.1", PassCount: 4, EWMAMbps: 20, LastSeen: time.Now()}
	a.ledgerMu.Unlock()

	history := mergeRankingHistory(nil, a.ledgerRankingHistory())
	speeds := map[string]float64{"1.1.1.1": 5}
	blendSpeedsWithHistory(speeds, history, 0.3)

	if speeds["1.1.1.1"] <= 5 {
		t.Fatalf("blended speed = %v, want ledger history to lift it above fresh 5", speeds["1.1.1.1"])
	}
}

func TestPoolLedgerSummaryBounded(t *testing.T) {
	a := newLedgerTestApp(t)
	base := time.Now().Add(-time.Hour)
	a.ledgerMu.Lock()
	for i := 0; i < 60; i++ {
		ip := "10.0.1." + strconv.Itoa(i+1)
		a.ledger.IPs[ip] = &ipLedgerEntry{
			IP: ip, ProbeCount: 4, PassCount: 2,
			LastSeen: base.Add(time.Duration(i) * time.Minute),
		}
	}
	a.ledgerMu.Unlock()

	summary := a.poolLedgerSummary(50)
	if summary.Total != 60 {
		t.Fatalf("total = %d, want 60", summary.Total)
	}
	if len(summary.Members) != 50 {
		t.Fatalf("members = %d, want 50", len(summary.Members))
	}
	if summary.Members[0].LastSeen.Before(summary.Members[49].LastSeen) {
		t.Fatalf("members must be ordered by lastSeen desc")
	}
	if summary.Members[0].SuccessRate != 0.5 {
		t.Fatalf("successRate = %v, want 0.5", summary.Members[0].SuccessRate)
	}
}
