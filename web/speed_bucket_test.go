package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// 三档排序：实测及格 > 未测 > 实测不及格。
//
// 旧实现的缺陷就在这里——实测 0.5 Mbps 与从未测过同为零分，慢 IP 靠
// 更低的握手延迟和更好的来源排到最前。现场实测：34.92.1.142 延迟 227ms、
// 吞吐 0.58 Mbps，却比 263ms / 8.8 Mbps 的 IP 更靠前。
func TestRankVLESSPassesRanksBelowThresholdLast(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "3")
	t.Setenv("PROXY_VLESS_SPEED_PROBES", "10")
	results := []proxyScanResult{
		{IP: "198.51.100.1", Latency: 10, SourceRank: 0, Stage: "VLESS_PASS"},
		{IP: "198.51.100.2", Latency: 200, SourceRank: 3, Stage: "VLESS_PASS"},
		{IP: "198.51.100.3", Latency: 300, SourceRank: 3, Stage: "VLESS_PASS"},
	}
	probe := func(_ context.Context, ip string, _ int, _ vlessProbeConfig, _ map[string]any) (vlessProbeMetrics, error) {
		switch ip {
		case "198.51.100.1":
			return vlessProbeMetrics{Duration: 100 * time.Millisecond, Mbps: 0.5}, nil
		case "198.51.100.3":
			return vlessProbeMetrics{Duration: 900 * time.Millisecond, Mbps: 20}, nil
		}
		return vlessProbeMetrics{}, errors.New("download failed")
	}
	got := rankVLESSPassesBySpeedWithProbe(
		context.Background(),
		[]string{"198.51.100.1", "198.51.100.2", "198.51.100.3"},
		results,
		443,
		vlessProbeConfig{Timeout: time.Second},
		nil,
		probe,
	)
	want := []string{"198.51.100.3", "198.51.100.2", "198.51.100.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (below-threshold must rank last)", got, want)
	}
	// 低分也必须留下成绩，否则后续无法区分「实测差」与「没测」。
	if results[0].DownloadMbps != 0.5 {
		t.Fatalf("below-threshold speed must still be recorded, got %v", results[0].DownloadMbps)
	}
}

func TestDropBelowReferenceIPs(t *testing.T) {
	passed := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"}
	results := []proxyScanResult{
		{IP: "198.51.100.1", DownloadMbps: 0.5}, // 实测不及格 → 剔除
		{IP: "198.51.100.2", DownloadMbps: 0},   // 未测 → 保留
		{IP: "198.51.100.3", DownloadMbps: 20},  // 及格 → 保留
	}
	got := dropBelowReferenceIPs(passed, results, 1, 3)
	want := []string{"198.51.100.2", "198.51.100.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// 剔除后会跌破最小池：宁可保留，也不让池子空掉。
	if got := dropBelowReferenceIPs(passed, results, 3, 3); !reflect.DeepEqual(got, passed) {
		t.Fatalf("minKeep guard failed: got %v, want %v", got, passed)
	}
	// 参考值为 0 时不启用该闸门。
	if got := dropBelowReferenceIPs(passed, results, 1, 0); !reflect.DeepEqual(got, passed) {
		t.Fatalf("zero reference must be a no-op: got %v", got)
	}
}
