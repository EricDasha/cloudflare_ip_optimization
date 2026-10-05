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
