package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

// 延迟天花板：吞吐排序看不见往返时间，一个 4 Mbps / 720ms 的入口下载能跑满，
// 但每次请求都要多等半秒。2026-11-14 线上实证：cfdata 灌进池子的 4 个 IP
// 吞吐 3.4~4.3 Mbps、握手 550~722ms，压过了未测速但只有 245~292ms 的订阅 IP。
func TestRankVLESSPassesDemotesHighLatencyFastIPs(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "3")
	t.Setenv("PROXY_VLESS_SPEED_PROBES", "10")
	t.Setenv("PROXY_VLESS_MAX_LATENCY_MS", "450")
	results := []proxyScanResult{
		{IP: "198.51.100.10", Latency: 250, SourceRank: 2, Stage: "VLESS_PASS"},
		{IP: "198.51.100.11", Latency: 720, SourceRank: 2, Stage: "VLESS_PASS"},
		{IP: "198.51.100.12", Latency: 290, SourceRank: 2, Stage: "VLESS_PASS"},
	}
	probe := func(_ context.Context, ip string, _ int, _ vlessProbeConfig, _ map[string]any) (vlessProbeMetrics, error) {
		if ip == "198.51.100.11" {
			return vlessProbeMetrics{Duration: 500 * time.Millisecond, Mbps: 4.2}, nil
		}
		return vlessProbeMetrics{}, errors.New("download failed")
	}
	got := rankVLESSPassesBySpeedWithProbe(
		context.Background(),
		[]string{"198.51.100.10", "198.51.100.11", "198.51.100.12"},
		results,
		443,
		vlessProbeConfig{Timeout: time.Second},
		nil,
		probe,
	)
	want := []string{"198.51.100.10", "198.51.100.12", "198.51.100.11"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (high-latency IP must sink despite speed)", got, want)
	}
}

// 关掉天花板（0）必须回到旧的纯吞吐排序，否则这个开关等于摆设。
func TestRankVLESSPassesLatencyCeilingDisabled(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "3")
	t.Setenv("PROXY_VLESS_SPEED_PROBES", "10")
	t.Setenv("PROXY_VLESS_MAX_LATENCY_MS", "0")
	results := []proxyScanResult{
		{IP: "198.51.100.10", Latency: 250, SourceRank: 2, Stage: "VLESS_PASS"},
		{IP: "198.51.100.11", Latency: 720, SourceRank: 2, Stage: "VLESS_PASS"},
	}
	probe := func(_ context.Context, ip string, _ int, _ vlessProbeConfig, _ map[string]any) (vlessProbeMetrics, error) {
		if ip == "198.51.100.11" {
			return vlessProbeMetrics{Duration: 500 * time.Millisecond, Mbps: 4.2}, nil
		}
		return vlessProbeMetrics{}, errors.New("download failed")
	}
	got := rankVLESSPassesBySpeedWithProbe(
		context.Background(),
		[]string{"198.51.100.10", "198.51.100.11"},
		results,
		443,
		vlessProbeConfig{Timeout: time.Second},
		nil,
		probe,
	)
	want := []string{"198.51.100.11", "198.51.100.10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (ceiling disabled must keep speed order)", got, want)
	}
}

// 没有握手成绩的成员不能因为「查不到」被判成慢线。
func TestVlessLatencyLaggyTreatsUnknownAsNeutral(t *testing.T) {
	results := []proxyScanResult{{IP: "198.51.100.1", Latency: 200}}
	if vlessLatencyLaggy(results, "198.51.100.1", 450) {
		t.Fatal("200ms must not be laggy under a 450ms ceiling")
	}
	if vlessLatencyLaggy(results, "198.51.100.9", 450) {
		t.Fatal("unknown latency must be treated as neutral, not laggy")
	}
	if !vlessLatencyLaggy([]proxyScanResult{{IP: "198.51.100.2", Latency: 700}}, "198.51.100.2", 450) {
		t.Fatal("700ms must be laggy under a 450ms ceiling")
	}
}

// 段轮转会把独占某段的慢线成员在第一轮就取走，所以入池前必须再分一次。
func TestPartitionLaggyLastPreservesInnerOrder(t *testing.T) {
	results := []proxyScanResult{
		{IP: "198.51.100.1", Latency: 250},
		{IP: "198.51.100.2", Latency: 700},
		{IP: "198.51.100.3", Latency: 290},
		{IP: "198.51.100.4", Latency: 900},
	}
	got := partitionLaggyLast([]string{"198.51.100.2", "198.51.100.1", "198.51.100.4", "198.51.100.3"}, results, 450)
	want := []string{"198.51.100.1", "198.51.100.3", "198.51.100.2", "198.51.100.4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// 关掉天花板时原样返回。
	in := []string{"198.51.100.2", "198.51.100.1"}
	if got := partitionLaggyLast(in, results, 0); !reflect.DeepEqual(got, in) {
		t.Fatalf("ceiling 0 must be a no-op, got %v", got)
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

// 跌破最小池时只补最快的慢 IP 到 minKeep，不是把全部慢 IP 原样留下。
func TestDropBelowReferenceIPsBackfillsFastestOnly(t *testing.T) {
	passed := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4", "198.51.100.5"}
	results := []proxyScanResult{
		{IP: "198.51.100.1", DownloadMbps: 20},
		{IP: "198.51.100.2", DownloadMbps: 1.2},
		{IP: "198.51.100.3", DownloadMbps: 2.4},
		{IP: "198.51.100.4", DownloadMbps: 1.8},
		{IP: "198.51.100.5", DownloadMbps: 2.9},
	}
	got := dropBelowReferenceIPs(passed, results, 3, 3)
	// 及格 1 个，需补 2 个：2.9 与 2.4 入选，1.8/1.2 剔除。
	want := []string{"198.51.100.1", "198.51.100.3", "198.51.100.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backfill = %v, want %v", got, want)
	}
}

// 数据面延迟超线的成员与吞吐不足同属「低于参考值」；未测（0）不判慢。
func TestDropBelowReferenceIPsHonorsDataLatencyCeiling(t *testing.T) {
	passed := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"}
	results := []proxyScanResult{
		{IP: "198.51.100.1", DownloadMbps: 15, DataLatency: 2000}, // 快但数据面 2 秒 → 剔除
		{IP: "198.51.100.2", DataLatency: 180},                    // 未测吞吐、延迟正常 → 保留
		{IP: "198.51.100.3"},                                      // 全未知 → 保留
	}
	got := dropBelowReferenceIPsWithDataCeiling(passed, results, 1, 3, 1200)
	want := []string{"198.51.100.2", "198.51.100.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// 关闭天花板后恢复旧行为：只看吞吐。
	got = dropBelowReferenceIPsWithDataCeiling(passed, results, 1, 3, 0)
	if !reflect.DeepEqual(got, passed) {
		t.Fatalf("ceiling 0 must be a no-op, got %v", got)
	}
}

// 补位时优先延迟正常的慢线，而不是「吞吐很高但每次交互都慢半拍」的入口。
func TestDropBelowReferenceIPsBackfillPrefersLowDataLatency(t *testing.T) {
	passed := []string{"198.51.100.1", "198.51.100.2"}
	results := []proxyScanResult{
		{IP: "198.51.100.1", DownloadMbps: 15, DataLatency: 1600}, // 快但延迟超线
		{IP: "198.51.100.2", DownloadMbps: 2, DataLatency: 200},   // 慢但延迟正常
	}
	got := dropBelowReferenceIPsWithDataCeiling(passed, results, 1, 3, 1200)
	want := []string{"198.51.100.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backfill = %v, want %v", got, want)
	}
}

// 最终名单里没测过速的成员必须补测；已经测过的、已经试过但失败的都不重试。
func TestFillMissingSpeedMetricsProbesOnlyUnmeasuredFinalists(t *testing.T) {
	t.Setenv("PROXY_VLESS_SPEED_INTERVAL_MS", "0")
	t.Setenv("PROXY_VLESS_SPEED_PROBES", "10")
	results := []proxyScanResult{
		{IP: "198.51.100.1", DownloadMbps: 10},        // 已测
		{IP: "198.51.100.2"},                          // 未测 → 补
		{IP: "198.51.100.3", Note: "测速未完成（软门槛）：boom"}, // 试过失败 → 不重试
	}
	calls := 0
	probe := func(context.Context, string, int, vlessProbeConfig, map[string]any) (vlessProbeMetrics, error) {
		calls++
		return vlessProbeMetrics{Mbps: 8, Duration: time.Second}, nil
	}
	passed := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"}
	if !fillMissingSpeedMetrics(context.Background(), passed, results, 443, vlessProbeConfig{}, nil, probe) {
		t.Fatal("expected one fill probe")
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
	if results[1].DownloadMbps != 8 {
		t.Fatalf("filled Mbps = %v, want 8", results[1].DownloadMbps)
	}
	if !strings.HasPrefix(results[1].Note, "测速 8.00 Mbps") {
		t.Fatalf("filled note = %q", results[1].Note)
	}
	// 第二轮没有可补的成员，必须返回 false，避免调用方无限重跑。
	if fillMissingSpeedMetrics(context.Background(), passed, results, 443, vlessProbeConfig{}, nil, probe) {
		t.Fatal("second fill should be a no-op")
	}
}
