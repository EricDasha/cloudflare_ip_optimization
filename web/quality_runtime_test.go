package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testQualityRuntime(t *testing.T) *qualitySchedulerRuntime {
	t.Helper()
	r := &qualitySchedulerRuntime{path: filepath.Join(t.TempDir(), proxyQualityFile), policy: defaultSchedulerPolicy(), apply: true, interval: 5 * time.Minute, batch: 12, data: qualityStoreData{Version: 1, Records: make(map[string]*qualityRecord)}}
	return r
}

func TestQualityRuntimePersistsAndRestores(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	r.seedActive("1.1.1.1", now)
	r.observe("8.8.8.8", "official", 400, 40, true, now)
	loaded := &qualitySchedulerRuntime{path: r.path, policy: defaultSchedulerPolicy()}
	if err := loaded.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.data.ActiveIP != "1.1.1.1" || loaded.data.Records["8.8.8.8"].State != lineStandby {
		t.Fatalf("restored data mismatch: %#v", loaded.data)
	}
	var raw map[string]any
	b, _ := os.ReadFile(r.path)
	if json.Unmarshal(b, &raw) != nil {
		t.Fatal("quality cache is not valid JSON")
	}
}

func TestQualityRuntimeSeedActiveDemotesPreviousRepresentative(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	r.seedActive("1.1.1.1", now)
	r.seedActive("8.8.8.8", now.Add(time.Minute))
	if got := r.data.ActiveIP; got != "8.8.8.8" {
		t.Fatalf("active representative = %q", got)
	}
	if got := r.data.Records["1.1.1.1"].State; got != lineStandby {
		t.Fatalf("previous representative state = %q, want STANDBY", got)
	}
}

func TestQualityRuntimePromotionNeedsThreeObservedRounds(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	r.seedActive("1.1.1.1", now.Add(-time.Hour))
	r.observe("1.1.1.1", "active_pool", 100, 60, true, now.Add(-time.Hour))
	r.observe("8.8.8.8", "official", 500, 40, true, now)
	if got := r.decide(now); got.Event != switchNone {
		t.Fatalf("promoted after one round: %#v", got)
	}
	r.observe("8.8.8.8", "official", 520, 40, true, now.Add(5*time.Minute))
	if got := r.decide(now.Add(5 * time.Minute)); got.Event != switchNone {
		t.Fatalf("promoted after two rounds under 3-round policy: %#v", got)
	}
	r.observe("8.8.8.8", "official", 540, 40, true, now.Add(10*time.Minute))
	if got := r.decide(now.Add(10 * time.Minute)); got.Event != switchPromotion || got.ToIP != "8.8.8.8" {
		t.Fatalf("third superior round did not promote: %#v", got)
	}
}

func TestQualityRuntimeActiveFailureFailsOverAfterThreeStrikes(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	r.seedActive("1.1.1.1", now)
	r.observe("8.8.8.8", "official", 600, 40, true, now)
	r.observeActiveHealth(false, 80, now.Add(time.Minute))
	if got := r.decide(now.Add(time.Minute)); got.Event != switchNone {
		t.Fatalf("single active failure failed over under 3-strike policy: %#v", got)
	}
	r.observeActiveHealth(false, 80, now.Add(2*time.Minute))
	r.observeActiveHealth(false, 80, now.Add(3*time.Minute))
	got := r.decide(now.Add(3 * time.Minute))
	if got.Event != switchFailover || got.ToIP != "8.8.8.8" {
		t.Fatalf("sustained active failure did not fail over: %#v", got)
	}
}

// --- 单 ACTIVE 不变量 ---

// 线上脏数据形态：两条记录同时是 ACTIVE，其中一条 lastProbeAt 为零值。
// seedActive 换代表时必须把幽灵一起降级，否则它永不被墓碑清理。
func TestSeedActiveDemotesOrphanActives(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	// 手工造出历史脏数据：两条 ACTIVE，activeIp 指向第一条。
	r.data.Records["172.64.229.111"] = &qualityRecord{IP: "172.64.229.111", State: lineActive, StateReason: reasonPromotedNoActive, StateChangedAt: now}
	r.data.Records["43.132.185.96"] = &qualityRecord{IP: "43.132.185.96", State: lineActive, StateReason: reasonPromotedNoActive}
	r.data.ActiveIP = "172.64.229.111"

	r.seedActive("131.0.72.5", now.Add(time.Minute))

	active := make([]string, 0, 3)
	for ip, rec := range r.data.Records {
		if rec.State == lineActive {
			active = append(active, ip)
		}
	}
	if len(active) != 1 || active[0] != "131.0.72.5" {
		t.Fatalf("ACTIVE records after seed = %v, want exactly [131.0.72.5]", active)
	}
	ghost := r.data.Records["43.132.185.96"]
	if ghost.State != lineStandby || ghost.StateReason != reasonOrphanActiveDemoted {
		t.Fatalf("orphan ACTIVE not demoted: state=%s reason=%s", ghost.State, ghost.StateReason)
	}
}

// 无 Active 时的晋升（FromIP 为空）不能留下第二条 ACTIVE。
func TestCommitSwitchWithoutFromIPDemotesOrphanActives(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	r.data.Records["old.ip"] = &qualityRecord{IP: "old.ip", State: lineActive, StateReason: reasonPromotedNoActive}
	r.data.ActiveIP = "old.ip"
	r.data.Records["new.ip"] = &qualityRecord{IP: "new.ip", State: lineStandby}

	r.commitSwitch(schedulerDecision{Event: switchPromotion, ToIP: "new.ip", Reason: reasonPromotedNoActive}, now)

	if r.data.ActiveIP != "new.ip" {
		t.Fatalf("activeIp = %q", r.data.ActiveIP)
	}
	if got := r.data.Records["old.ip"].State; got != lineStandby {
		t.Fatalf("previous ACTIVE left behind: state=%s", got)
	}
	if got := r.data.Records["old.ip"].StateReason; got != reasonOrphanActiveDemoted {
		t.Fatalf("demotion reason = %q, want %q", got, reasonOrphanActiveDemoted)
	}
}

// 正常晋升（FromIP 非空）仍走原有语义：旧 Active 记为 decision.Reason，
// 不变量修复不能把它改写成一个新的原因。
func TestCommitSwitchKeepsNormalDemotionReason(t *testing.T) {
	r := testQualityRuntime(t)
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	r.data.Records["A"] = &qualityRecord{IP: "A", State: lineActive}
	r.data.Records["B"] = &qualityRecord{IP: "B", State: lineStandby}
	r.data.ActiveIP = "A"

	r.commitSwitch(schedulerDecision{Event: switchFailover, FromIP: "A", ToIP: "B", Reason: reasonFailoverAfterActiveFailure}, now)

	if got := r.data.Records["A"].StateReason; got != reasonFailoverAfterActiveFailure {
		t.Fatalf("normal demotion reason overwritten: %q", got)
	}
}
