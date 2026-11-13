package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newLedgerTestApp 起一个只有台账的最小 app。台账的读写都只碰 dataDir，
// 不需要 cfnat/quality 那一整套。
func newLedgerTestApp(t *testing.T) *app {
	t.Helper()
	a := &app{dataDir: t.TempDir()}
	a.loadSourceLedger()
	return a
}

// ageEntry 把某条记录的首次出现时间往前拨，用来跨过观察期。
func ageEntry(t *testing.T, a *app, ip string, d time.Duration) {
	t.Helper()
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	entry, ok := a.ledger.IPs[ip]
	if !ok {
		t.Fatalf("ledger entry %s missing", ip)
	}
	entry.FirstSeen = entry.FirstSeen.Add(-d)
}

func TestSourceLedgerEvictsNeverPassedThirdParty(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.Rules = staleRule{Enabled: true, GraceHours: 1, MinRounds: 2, MinProbes: 1, NonCFOnly: true, NeverPassedOnly: true, AutoExclude: true}
	a.ledgerMu.Unlock()

	// 一个不在 CF 官方段内的地址，连续两轮出现，考了一次没过。
	for i := 0; i < 2; i++ {
		a.observeCandidateRound(map[string]string{"1.2.3.4": "bestcf"})
	}
	a.ledgerObserveProbe("1.2.3.4", false, 0)
	ageEntry(t, a, "1.2.3.4", 2*time.Hour)

	if marked := a.evaluateStale(time.Now()); marked != 1 {
		t.Fatalf("expected 1 eviction, got %d", marked)
	}
	if !a.ledgerExcluded("1.2.3.4") {
		t.Fatal("third-party IP that never passed should be excluded")
	}
}

func TestSourceLedgerKeepsIPThatPassedOnce(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.Rules = staleRule{Enabled: true, GraceHours: 1, MinRounds: 1, MinProbes: 1, NonCFOnly: true, NeverPassedOnly: true, AutoExclude: true}
	a.ledgerMu.Unlock()

	a.observeCandidateRound(map[string]string{"1.2.3.4": "bestcf"})
	// 考两次：先失败后通过。通过过一次就不该再被判死。
	a.ledgerObserveProbe("1.2.3.4", false, 0)
	a.ledgerObserveProbe("1.2.3.4", true, 6.5)
	ageEntry(t, a, "1.2.3.4", 48*time.Hour)

	if marked := a.evaluateStale(time.Now()); marked != 0 {
		t.Fatalf("expected no eviction, got %d", marked)
	}
	if a.ledgerExcluded("1.2.3.4") {
		t.Fatal("IP that passed a real probe must not be evicted")
	}
}

func TestSourceLedgerKeepsCloudflareEdgeAddresses(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.Rules = staleRule{Enabled: true, GraceHours: 1, MinRounds: 1, MinProbes: 1, NonCFOnly: true, NeverPassedOnly: true, AutoExclude: true}
	a.ledgerMu.Unlock()

	// 104.16.0.1 在 CF 官方段内：即使从没考过，也不按「第三方冒充」处理。
	a.observeCandidateRound(map[string]string{"104.16.0.1": "bestcf"})
	a.ledgerObserveProbe("104.16.0.1", false, 0)
	ageEntry(t, a, "104.16.0.1", 48*time.Hour)

	if marked := a.evaluateStale(time.Now()); marked != 0 {
		t.Fatalf("expected no eviction for CF edge address, got %d", marked)
	}
}

func TestSourceLedgerRespectsObservationPeriod(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.Rules = staleRule{Enabled: true, GraceHours: 24, MinRounds: 1, MinProbes: 1, NonCFOnly: true, NeverPassedOnly: true, AutoExclude: true}
	a.ledgerMu.Unlock()

	a.observeCandidateRound(map[string]string{"1.2.3.4": "bestcf"})
	a.ledgerObserveProbe("1.2.3.4", false, 0)
	// 只过了一小时，观察期 24 小时，不许动。
	ageEntry(t, a, "1.2.3.4", time.Hour)

	if marked := a.evaluateStale(time.Now()); marked != 0 {
		t.Fatalf("expected no eviction inside grace period, got %d", marked)
	}
}

func TestSourceLedgerManualAllowSurvivesEvaluation(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.Rules = staleRule{Enabled: true, GraceHours: 1, MinRounds: 1, MinProbes: 1, NonCFOnly: true, NeverPassedOnly: true, AutoExclude: true}
	a.ledgerMu.Unlock()

	a.observeCandidateRound(map[string]string{"1.2.3.4": "bestcf"})
	a.ledgerObserveProbe("1.2.3.4", false, 0)
	ageEntry(t, a, "1.2.3.4", 48*time.Hour)
	if marked := a.evaluateStale(time.Now()); marked != 1 {
		t.Fatalf("expected eviction before manual allow, got %d", marked)
	}

	if _, err := a.updateSourceLedger("allow", nil, "1.2.3.4"); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if a.ledgerExcluded("1.2.3.4") {
		t.Fatal("manual allow must clear the exclusion")
	}
	// 再判一次：人工放行的记录不该被自动规则重新剔掉。
	if marked := a.evaluateStale(time.Now()); marked != 0 {
		t.Fatalf("manual allow must survive re-evaluation, got %d evictions", marked)
	}
}

func TestSourceLedgerPassClearsAutomaticExclusion(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.Rules = staleRule{Enabled: true, GraceHours: 1, MinRounds: 1, MinProbes: 1, NonCFOnly: true, NeverPassedOnly: true, AutoExclude: true}
	a.ledgerMu.Unlock()

	a.observeCandidateRound(map[string]string{"1.2.3.4": "bestcf"})
	a.ledgerObserveProbe("1.2.3.4", false, 0)
	ageEntry(t, a, "1.2.3.4", 48*time.Hour)
	a.evaluateStale(time.Now())
	if !a.ledgerExcluded("1.2.3.4") {
		t.Fatal("precondition: entry should be excluded")
	}

	// 地址后来活了：真实考试通过，自动排除应当解除。
	a.ledgerObserveProbe("1.2.3.4", true, 8.2)
	if a.ledgerExcluded("1.2.3.4") {
		t.Fatal("a passing probe must lift an automatic exclusion")
	}
}

func TestSourceLedgerPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	a := &app{dataDir: dir}
	a.loadSourceLedger()
	a.observeCandidateRound(map[string]string{"1.2.3.4": "bestcf"})
	a.ledgerObserveProbe("1.2.3.4", true, 7.7)
	a.ledgerMu.Lock()
	if err := a.saveSourceLedgerLocked(); err != nil {
		a.ledgerMu.Unlock()
		t.Fatalf("save: %v", err)
	}
	a.ledgerMu.Unlock()

	b := &app{dataDir: dir}
	b.loadSourceLedger()
	b.ledgerMu.Lock()
	entry := b.ledger.IPs["1.2.3.4"]
	b.ledgerMu.Unlock()
	if entry == nil {
		t.Fatal("ledger entry lost across reload")
	}
	if entry.PassCount != 1 || entry.ProbeCount != 1 {
		t.Fatalf("probe counters not persisted: %+v", entry)
	}
	if entry.BestMbps != 7.7 {
		t.Fatalf("bestMbps not persisted: %v", entry.BestMbps)
	}
	if _, err := filepath.Abs(dir); err != nil {
		t.Fatalf("abs: %v", err)
	}
}

func TestSourceLedgerTracksUnchangedSourceContent(t *testing.T) {
	a := newLedgerTestApp(t)
	list := []string{"1.2.3.4", "5.6.7.8"}

	a.recordSourceContent("https://example.test/all.txt", "bestcf", "ip", list, nil)
	a.recordSourceContent("https://example.test/all.txt", "bestcf", "ip", list, nil)
	a.recordSourceContent("https://example.test/all.txt", "bestcf", "ip", list, nil)

	a.ledgerMu.Lock()
	stat := a.ledger.Sources["https://example.test/all.txt"]
	a.ledgerMu.Unlock()
	if stat == nil {
		t.Fatal("source stat missing")
	}
	if stat.UnchangedRounds != 2 {
		t.Fatalf("expected 2 unchanged rounds, got %d", stat.UnchangedRounds)
	}
	if stat.Fetches != 3 || stat.Successes != 3 || stat.LastCount != 2 {
		t.Fatalf("unexpected source counters: %+v", stat)
	}

	// 内容一变，未变计数立刻归零——「有没有人在维护」看的就是这个跳变。
	a.recordSourceContent("https://example.test/all.txt", "bestcf", "ip", []string{"9.9.9.9"}, nil)
	a.ledgerMu.Lock()
	stat = a.ledger.Sources["https://example.test/all.txt"]
	a.ledgerMu.Unlock()
	if stat.UnchangedRounds != 0 {
		t.Fatalf("expected unchanged rounds reset to 0, got %d", stat.UnchangedRounds)
	}
}

func TestSourceLedgerRecordsFetchErrors(t *testing.T) {
	a := newLedgerTestApp(t)
	a.recordSourceContent("https://example.test/all.txt", "bestcf", "ip", nil, errors.New("boom"))
	a.ledgerMu.Lock()
	stat := a.ledger.Sources["https://example.test/all.txt"]
	a.ledgerMu.Unlock()
	if stat.LastError != "boom" || stat.Fetches != 1 || stat.Successes != 0 {
		t.Fatalf("fetch error not recorded: %+v", stat)
	}
}

func TestSourceLedgerSnapshotFiltersAndCounts(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.Rules = staleRule{Enabled: true, GraceHours: 1, MinRounds: 1, MinProbes: 1, NonCFOnly: true, NeverPassedOnly: true, AutoExclude: true, SourceStaleRounds: 2}
	a.ledgerMu.Unlock()

	a.observeCandidateRound(map[string]string{"1.2.3.4": "bestcf", "5.6.7.8": "bestcf"})
	a.ledgerObserveProbe("5.6.7.8", true, 9)
	if _, err := a.updateSourceLedger("exclude", nil, "1.2.3.4"); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	a.recordSourceContent("src-a", "bestcf", "ip", []string{"1.1.1.1"}, nil)
	a.recordSourceContent("src-a", "bestcf", "ip", []string{"1.1.1.1"}, nil)
	a.recordSourceContent("src-a", "bestcf", "ip", []string{"1.1.1.1"}, nil)

	snap := a.sourceLedgerSnapshot("excluded", 10)
	counts, _ := snap["counts"].(map[string]any)
	if counts["excluded"] != 1 || counts["tracked"] != 2 {
		t.Fatalf("unexpected counts: %+v", counts)
	}
	if counts["stale"] != 1 {
		t.Fatalf("source with 2 unchanged rounds should be stale, counts=%+v", counts)
	}
	ips, _ := snap["ips"].([]map[string]any)
	if len(ips) != 1 || ips[0]["ip"] != "1.2.3.4" {
		t.Fatalf("excluded filter returned %+v", ips)
	}
}

func TestSourceLedgerRulesClamp(t *testing.T) {
	a := newLedgerTestApp(t)
	if _, err := a.updateSourceLedger("rules", &staleRule{GraceHours: -5, MinRounds: 0, MinProbes: 0, SourceStaleRounds: 0}, ""); err != nil {
		t.Fatalf("rules: %v", err)
	}
	a.ledgerMu.Lock()
	rules := a.ledger.Rules
	a.ledgerMu.Unlock()
	if rules.GraceHours != 1 || rules.MinRounds != 1 || rules.MinProbes != 1 || rules.SourceStaleRounds != 2 {
		t.Fatalf("rules not clamped: %+v", rules)
	}
}

func TestSourceLedgerUnknownActionRejected(t *testing.T) {
	a := newLedgerTestApp(t)
	if _, err := a.updateSourceLedger("nope", nil, ""); !errors.Is(err, errLedgerBadRequest) {
		t.Fatalf("expected bad request, got %v", err)
	}
	if _, err := a.updateSourceLedger("exclude", nil, "9.9.9.9"); !errors.Is(err, errLedgerNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

// evaluate 走的是「自己加锁」的路径。它曾经写在持锁 switch 里、先 Unlock
// 再 return，于是撞上函数顶部的 defer Unlock 重复解锁直接 panic。
// 这条用例把那个顺序钉住。
func TestSourceLedgerEvaluateActionDoesNotDeadlock(t *testing.T) {
	a := newLedgerTestApp(t)
	a.ledgerMu.Lock()
	a.ledger.Rules = staleRule{Enabled: true, GraceHours: 1, MinRounds: 1, MinProbes: 1, NonCFOnly: true, NeverPassedOnly: true, AutoExclude: true}
	a.ledgerMu.Unlock()

	a.observeCandidateRound(map[string]string{"1.2.3.4": "bestcf"})
	a.ledgerObserveProbe("1.2.3.4", false, 0)
	ageEntry(t, a, "1.2.3.4", 48*time.Hour)

	done := make(chan map[string]any, 1)
	go func() {
		result, err := a.updateSourceLedger("evaluate", nil, "")
		if err != nil {
			t.Errorf("evaluate: %v", err)
		}
		done <- result
	}()
	select {
	case result := <-done:
		if result["newlyExcluded"] != 1 {
			t.Fatalf("expected 1 newly excluded, got %v", result["newlyExcluded"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("evaluate action deadlocked")
	}
}
