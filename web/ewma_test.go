package main

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestBlendSpeedsWithHistorySmoothsSingleDip(t *testing.T) {
	speeds := map[string]float64{"a": 8}
	history := []proxyScanResult{{IP: "a", DownloadMbps: 10}}
	blendSpeedsWithHistory(speeds, history, 0.3)
	if math.Abs(speeds["a"]-9.4) > 1e-9 {
		t.Fatalf("blended = %v, want 9.4", speeds["a"])
	}
}

func TestBlendSpeedsWithHistoryCapsCollapse(t *testing.T) {
	speeds := map[string]float64{"a": 1}
	history := []proxyScanResult{{IP: "a", DownloadMbps: 100}}
	blendSpeedsWithHistory(speeds, history, 0.3)
	if math.Abs(speeds["a"]-1.5) > 1e-9 {
		t.Fatalf("collapsed line was not clamped: %v, want 1.5", speeds["a"])
	}
}

func TestBlendSpeedsWithHistoryIgnoresMissingHistory(t *testing.T) {
	speeds := map[string]float64{"a": 5}
	blendSpeedsWithHistory(speeds, nil, 0.3)
	if speeds["a"] != 5 {
		t.Fatalf("no history changed value to %v", speeds["a"])
	}
	blendSpeedsWithHistory(speeds, []proxyScanResult{{IP: "b", DownloadMbps: 99}}, 0.3)
	if speeds["a"] != 5 {
		t.Fatalf("history for another IP changed value to %v", speeds["a"])
	}
}

func TestEwmaAlphaZeroDisables(t *testing.T) {
	t.Setenv("PROXY_EWMA_ALPHA", "0")
	if got := ewmaRankingAlpha(); got != 0 {
		t.Fatalf("alpha = %v, want 0", got)
	}
	speeds := map[string]float64{"a": 5}
	blendSpeedsWithHistory(speeds, []proxyScanResult{{IP: "a", DownloadMbps: 100}}, ewmaRankingAlpha())
	if speeds["a"] != 5 {
		t.Fatalf("alpha 0 must keep fresh value, got %v", speeds["a"])
	}
}

// 历史只能改变先后，不能改写 results[].DownloadMbps（准入与展示读新鲜值）。
func TestRankWithHistoryKeepsFreshThroughput(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "3")
	t.Setenv("PROXY_EWMA_ALPHA", "0.3")
	results := []proxyScanResult{
		{IP: "198.51.100.1", Latency: 50, Stage: "VLESS_PASS"},
		{IP: "198.51.100.2", Latency: 50, Stage: "VLESS_PASS"},
	}
	probe := func(_ context.Context, ip string, _ int, _ vlessProbeConfig, _ map[string]any) (vlessProbeMetrics, error) {
		switch ip {
		case "198.51.100.1":
			return vlessProbeMetrics{Duration: 100 * time.Millisecond, Mbps: 5}, nil
		case "198.51.100.2":
			return vlessProbeMetrics{Duration: 100 * time.Millisecond, Mbps: 6}, nil
		}
		return vlessProbeMetrics{}, errors.New("unknown ip")
	}
	history := []proxyScanResult{
		{IP: "198.51.100.1", DownloadMbps: 100},
		{IP: "198.51.100.2", DownloadMbps: 6},
	}
	got := rankVLESSPassesBySpeedWithProbeAndHistory(
		context.Background(),
		[]string{"198.51.100.1", "198.51.100.2"},
		results,
		443,
		vlessProbeConfig{Timeout: time.Second},
		nil,
		probe,
		history,
	)
	want := []string{"198.51.100.1", "198.51.100.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (history should lift the dipped line)", got, want)
	}
	if results[0].DownloadMbps != 5 || results[1].DownloadMbps != 6 {
		t.Fatalf("results were overwritten by blended values: %v, %v", results[0].DownloadMbps, results[1].DownloadMbps)
	}
}
