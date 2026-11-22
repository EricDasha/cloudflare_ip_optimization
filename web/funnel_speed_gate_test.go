package main

import (
	"reflect"
	"testing"
)

// 2026-11-22：跑过测速却没成绩的成员必须和实测不及格同等对待。
// 旧实现只判 DownloadMbps > 0，于是一次测速超时就能让成员以「未知」身份
// 留在池子里，再被 cfnat 的粘性钉给用户。
func TestDropBelowReferenceTreatsSpeedProbeFailureAsSlow(t *testing.T) {
	results := []proxyScanResult{
		{IP: "10.0.0.1", DownloadMbps: 0, Note: speedProbeFailureNotePrefix + "未完成（软门槛，仅影响排序）：timeout"},
		{IP: "10.0.0.2", DownloadMbps: 12.5},
		{IP: "10.0.0.3", DownloadMbps: 0},
	}
	got := dropBelowReferenceIPsWithDataCeiling(
		[]string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, results, 1, 3, 0)
	want := []string{"10.0.0.2", "10.0.0.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v（测速失败者应被剔除，从未测速者应保留）", got, want)
	}
}

// 从未测速仍然是未知，不能判慢——否则候选不够时会把整池清空。
func TestDropBelowReferenceKeepsNeverProbedMembers(t *testing.T) {
	results := []proxyScanResult{{IP: "10.0.0.1"}, {IP: "10.0.0.2"}, {IP: "10.0.0.3"}}
	passed := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	got := dropBelowReferenceIPsWithDataCeiling(passed, results, 3, 3, 0)
	if !reflect.DeepEqual(got, passed) {
		t.Fatalf("got %v, want %v", got, passed)
	}
}

// 测速失败但不够补位时应按 minKeep 补回，不能因为全被剔除就把池子清空。
func TestDropBelowReferenceRefillsFromFailedProbes(t *testing.T) {
	results := []proxyScanResult{
		{IP: "10.0.0.1", Note: speedProbeFailureNotePrefix + "未完成（补测，仅影响排序）：timeout"},
		{IP: "10.0.0.2", Note: speedProbeFailureNotePrefix + "未完成（补测，仅影响排序）：timeout"},
	}
	got := dropBelowReferenceIPsWithDataCeiling([]string{"10.0.0.1", "10.0.0.2"}, results, 2, 3, 0)
	if len(got) != 2 {
		t.Fatalf("got %v，minKeep=2 时应补回两个成员", got)
	}
}

func TestSpeedProbeFailed(t *testing.T) {
	cases := []struct {
		name   string
		result proxyScanResult
		want   bool
	}{
		{"有成绩", proxyScanResult{DownloadMbps: 4.2, Note: speedProbeFailureNotePrefix + "未完成"}, false},
		{"跑过测速失败", proxyScanResult{DownloadMbps: 0, Note: speedProbeFailureNotePrefix + "未完成（软门槛）"}, true},
		{"从未测速", proxyScanResult{DownloadMbps: 0}, false},
		{"无关备注", proxyScanResult{DownloadMbps: 0, Note: "colo=NRT"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := speedProbeFailed(c.result); got != c.want {
				t.Fatalf("speedProbeFailed=%v, want %v", got, c.want)
			}
		})
	}
}
