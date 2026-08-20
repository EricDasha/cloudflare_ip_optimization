package main

import (
	"testing"
	"time"
)

func TestSchedulerRejectsNormalFluctuation(t *testing.T) {
	policy := defaultSchedulerPolicy()
	now := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC)
	active := lineQuality{IP: "A", State: lineActive, AverageMbps: 800, SuccessRate: 1}
	standby := []lineQuality{
		{IP: "B", State: lineStandby, AverageMbps: 850, P95Mbps: 860, PeakMbps: 900, SuccessRate: 1, ConsecutiveSuperior: 10},
		{IP: "C", State: lineStandby, AverageMbps: 830, P95Mbps: 850, PeakMbps: 880, SuccessRate: 1, ConsecutiveSuperior: 10},
	}
	decision, err := decideLineSwitch(schedulerInput{Now: now, Active: &active, Standby: standby}, policy)
	if err != nil || decision.Event != switchNone {
		t.Fatalf("normal fluctuation triggered switch: %#v, %v", decision, err)
	}
}

func TestSchedulerPromotionNeedsBothGainsAndTwoRounds(t *testing.T) {
	policy := defaultSchedulerPolicy()
	now := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC)
	active := lineQuality{IP: "A", State: lineActive, AverageMbps: 300, SuccessRate: 1}
	candidate := lineQuality{IP: "B", State: lineStandby, AverageMbps: 500, P95Mbps: 480, PeakMbps: 520, SuccessRate: 1, ConsecutiveSuperior: 1}

	decision, _ := decideLineSwitch(schedulerInput{Now: now, Active: &active, Standby: []lineQuality{candidate}}, policy)
	if decision.Event != switchNone {
		t.Fatalf("single superior round promoted candidate: %#v", decision)
	}
	candidate.ConsecutiveSuperior = 2
	decision, _ = decideLineSwitch(schedulerInput{Now: now, Active: &active, Standby: []lineQuality{candidate}}, policy)
	if decision.Event != switchPromotion || decision.ToIP != "B" || decision.Reason != reasonPromotedSuperiorThroughput {
		t.Fatalf("qualified candidate was not promoted: %#v", decision)
	}
}

func TestSchedulerMinimumSwitchIntervalBlocksPromotion(t *testing.T) {
	policy := defaultSchedulerPolicy()
	now := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC)
	active := lineQuality{IP: "A", State: lineActive, AverageMbps: 100, SuccessRate: 1}
	standby := []lineQuality{{IP: "B", State: lineStandby, AverageMbps: 900, P95Mbps: 850, PeakMbps: 950, SuccessRate: 1, ConsecutiveSuperior: 2}}
	decision, _ := decideLineSwitch(schedulerInput{Now: now, LastSwitch: now.Add(-2 * time.Minute), Active: &active, Standby: standby}, policy)
	if decision.Event != switchNone {
		t.Fatalf("cooldown did not block promotion: %#v", decision)
	}
}

func TestSchedulerFailoverBypassesPromotionCooldown(t *testing.T) {
	policy := defaultSchedulerPolicy()
	now := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC)
	active := lineQuality{IP: "A", State: lineFailed, ConsecutiveFailures: 1}
	standby := []lineQuality{{IP: "B", State: lineStandby, AverageMbps: 600, P95Mbps: 580, PeakMbps: 650, SuccessRate: 0.99}}
	decision, _ := decideLineSwitch(schedulerInput{Now: now, LastSwitch: now.Add(-time.Minute), Active: &active, Standby: standby}, policy)
	if decision.Event != switchFailover || decision.ToIP != "B" || decision.Reason != reasonFailoverAfterActiveFailure {
		t.Fatalf("active failure did not fail over immediately: %#v", decision)
	}
}

func TestSchedulerCapsScoresAtLinkCapacity(t *testing.T) {
	line := lineQuality{AverageMbps: 1500, P95Mbps: 1400, PeakMbps: 2000, SuccessRate: 1}
	if got, want := lineQualityScore(line, 1024), float64(1024); got != want {
		t.Fatalf("score = %v, want capacity-capped %v", got, want)
	}
}

func TestSchedulerRecordsStateReasonAndRejectsIllegalTransition(t *testing.T) {
	now := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC)
	line := lineQuality{IP: "A", State: lineUnknown}
	probing, err := transitionLineState(line, lineProbing, reasonProbeStarted, now)
	if err != nil || probing.StateReason != reasonProbeStarted || !probing.StateChangedAt.Equal(now) {
		t.Fatalf("valid transition lost audit data: %#v, %v", probing, err)
	}
	if _, err := transitionLineState(line, lineActive, reasonPromotedNoActive, now); err == nil {
		t.Fatal("illegal UNKNOWN to ACTIVE transition was accepted")
	}
}

func TestSchedulerSimulationDoesNotOscillate(t *testing.T) {
	policy := defaultSchedulerPolicy()
	now := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC)
	active := lineQuality{IP: "A", State: lineActive, AverageMbps: 900, SuccessRate: 1}
	series := []float64{920, 910, 950, 980, 915, 960}
	for i, speed := range series {
		standby := lineQuality{IP: "B", State: lineStandby, AverageMbps: speed, P95Mbps: speed, PeakMbps: speed, SuccessRate: 1, ConsecutiveSuperior: i + 1}
		decision, err := decideLineSwitch(schedulerInput{Now: now.Add(time.Duration(i) * 5 * time.Minute), Active: &active, Standby: []lineQuality{standby}}, policy)
		if err != nil || decision.Event != switchNone {
			t.Fatalf("network jitter caused promotion at sample %d: %#v, %v", i, decision, err)
		}
	}
}
