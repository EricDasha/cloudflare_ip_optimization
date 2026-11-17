package main

import "testing"

func TestLatencyAverageAndJitter(t *testing.T) {
	avg, jitter := latencyAverageAndJitter([]int64{10, 20, 30})
	if avg != 20 {
		t.Fatalf("avg = %d, want 20", avg)
	}
	if jitter != 6 {
		t.Fatalf("jitter = %d, want 6", jitter)
	}
	if avg, jitter := latencyAverageAndJitter(nil); avg != 0 || jitter != 0 {
		t.Fatalf("empty = (%d,%d), want (0,0)", avg, jitter)
	}
}

func TestCapPassedPerColo(t *testing.T) {
	results := []proxyScanResult{
		{IP: "a1", Colo: "HKG"},
		{IP: "a2", Colo: "HKG"},
		{IP: "a3", Colo: "HKG"},
		{IP: "b1", Colo: "NRT"},
		{IP: "c1", Colo: "SJC"},
		{IP: "u1"},
	}
	ips := []string{"a1", "a2", "a3", "b1", "c1", "u1"}
	got := capPassedPerColo(ips, results, 2, 2)
	want := []string{"a1", "a2", "b1", "c1", "u1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestCapPassedPerColoBackfillsMinimum(t *testing.T) {
	results := []proxyScanResult{
		{IP: "a1", Colo: "HKG"},
		{IP: "a2", Colo: "HKG"},
		{IP: "a3", Colo: "HKG"},
		{IP: "a4", Colo: "HKG"},
	}
	ips := []string{"a1", "a2", "a3", "a4"}
	got := capPassedPerColo(ips, results, 2, 3)
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 members", got)
	}
	if got[2] != "a3" {
		t.Fatalf("backfill should preserve order, got %v", got)
	}
}

func TestCapPassedPerColoDisabled(t *testing.T) {
	results := []proxyScanResult{{IP: "a1", Colo: "HKG"}, {IP: "a2", Colo: "HKG"}}
	ips := []string{"a1", "a2"}
	got := capPassedPerColo(ips, results, 0, 1)
	if len(got) != 2 {
		t.Fatalf("disabled cap should keep all, got %v", got)
	}
}
