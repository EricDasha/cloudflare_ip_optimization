package main

import (
	"reflect"
	"testing"
)

func stat(failures int) *throneProbeStat {
	return &throneProbeStat{ConsecutiveFailures: failures}
}

func TestReapDeadThroneMembersRemovesReached(t *testing.T) {
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	stats := map[string]*throneProbeStat{
		"1.1.1.1": stat(0),
		"2.2.2.2": stat(3),
		"3.3.3.3": stat(5),
	}
	kept, removed := reapDeadThroneMembers(ips, stats, 3)
	if !reflect.DeepEqual(kept, []string{"1.1.1.1"}) {
		t.Fatalf("kept = %v, want [1.1.1.1]", kept)
	}
	if !reflect.DeepEqual(removed, []string{"2.2.2.2", "3.3.3.3"}) {
		t.Fatalf("removed = %v, want [2.2.2.2 3.3.3.3]", removed)
	}
}

func TestReapDeadThroneMembersThresholdBoundary(t *testing.T) {
	ips := []string{"10.0.0.1", "10.0.0.2"}
	stats := map[string]*throneProbeStat{
		"10.0.0.1": stat(2), // 差一次，留
		"10.0.0.2": stat(3), // 正好达阈值，剔
	}
	kept, removed := reapDeadThroneMembers(ips, stats, 3)
	if !reflect.DeepEqual(kept, []string{"10.0.0.1"}) {
		t.Fatalf("kept = %v, want [10.0.0.1]", kept)
	}
	if !reflect.DeepEqual(removed, []string{"10.0.0.2"}) {
		t.Fatalf("removed = %v, want [10.0.0.2]", removed)
	}
}

func TestReapDeadThroneMembersNeverEmpties(t *testing.T) {
	ips := []string{"9.9.9.9", "8.8.8.8", "7.7.7.7"}
	stats := map[string]*throneProbeStat{
		"9.9.9.9": stat(9),
		"8.8.8.8": stat(4),
		"7.7.7.7": stat(7),
	}
	kept, removed := reapDeadThroneMembers(ips, stats, 3)
	if !reflect.DeepEqual(kept, []string{"8.8.8.8"}) {
		t.Fatalf("kept = %v, want [8.8.8.8] (least failures, never empty)", kept)
	}
	if !reflect.DeepEqual(removed, []string{"9.9.9.9", "7.7.7.7"}) {
		t.Fatalf("removed = %v, want [9.9.9.9 7.7.7.7]", removed)
	}
}

func TestReapDeadThroneMembersNeverEmptyPicksLeastFailed(t *testing.T) {
	ips := []string{"a", "b", "c"}
	// a/b/c 非法 IP 也无妨：纯函数不校验合法性，只按 stats 决策。
	stats := map[string]*throneProbeStat{
		"a": stat(7),
		"b": stat(4),
		"c": stat(9),
	}
	kept, removed := reapDeadThroneMembers(ips, stats, 3)
	if !reflect.DeepEqual(kept, []string{"b"}) {
		t.Fatalf("kept = %v, want [b] (least failures)", kept)
	}
	if !reflect.DeepEqual(removed, []string{"a", "c"}) {
		t.Fatalf("removed = %v, want [a c]", removed)
	}
}

func TestReapDeadThroneMembersNoStatsKeepsAll(t *testing.T) {
	ips := []string{"1.1.1.1", "2.2.2.2"}
	kept, removed := reapDeadThroneMembers(ips, map[string]*throneProbeStat{}, 3)
	if len(kept) != 2 || len(removed) != 0 {
		t.Fatalf("kept=%v removed=%v, want all kept", kept, removed)
	}
}

func TestThroneProbeIntervalClamp(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"120", 600},   // below min → default
		{"300", 300},   // min
		{"600", 600},   // default
		{"1800", 1800}, // max
		{"3600", 600},  // above max → default
		{"", 600},      // unset → default
	}
	for _, c := range cases {
		if c.env == "" {
			t.Setenv("PROXY_ACTIVE_PROBE_INTERVAL_SECONDS", "")
		} else {
			t.Setenv("PROXY_ACTIVE_PROBE_INTERVAL_SECONDS", c.env)
		}
		got := int(throneProbeInterval().Seconds())
		if got != c.want {
			t.Errorf("interval(%q) = %d, want %d", c.env, got, c.want)
		}
	}
}

func TestThroneProbeFailThresholdClamp(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"0", 3},
		{"1", 1},
		{"3", 3},
		{"10", 10},
		{"99", 3},
	}
	for _, c := range cases {
		t.Setenv("PROXY_ACTIVE_PROBE_FAIL_THRESHOLD", c.env)
		if got := throneProbeFailThreshold(); got != c.want {
			t.Errorf("threshold(%q) = %d, want %d", c.env, got, c.want)
		}
	}
}
