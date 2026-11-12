package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIsLocalProbeError(t *testing.T) {
	local := []string{
		"分配本地探针端口失败: listen tcp: address already in use",
		"生成 sing-box 配置失败: bad template",
		"创建 sing-box 临时配置失败: no space",
		"启动 sing-box 失败: exec: no such file",
		"sing-box 提前退出: bind: address already in use",
		"sing-box 提前退出: exit status 1",
		"VLESS 探针启动超时",
		`VLESS 数据面请求失败: Get "http://example.com/generate_204": proxyconnect tcp: dial tcp 127.0.0.1:45678: connect: connection refused`,
	}
	for _, msg := range local {
		if !isLocalProbeError(errors.New(msg)) {
			t.Errorf("expected local probe error for %q", msg)
		}
	}
	candidate := []string{
		"VLESS 数据面请求失败: context deadline exceeded",
		"connection refused",
		"VLESS 数据面请求失败: Get \"https://example.com\": context deadline exceeded",
	}
	for _, msg := range candidate {
		if isLocalProbeError(errors.New(msg)) {
			t.Errorf("expected candidate error (not local) for %q", msg)
		}
	}
	if isLocalProbeError(nil) {
		t.Error("nil error must not be local")
	}
}

// 本地探针故障记 skipped、不进 passed、不进 VLESS_FAIL；候选自身失败仍记
// VLESS_FAIL；成功仍记 VLESS_PASS。
func TestProbeVLESSPoolWithProbeSkipsLocalFailures(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "WS_PASS"},
		{IP: "2.2.2.2", Stage: "WS_PASS"},
		{IP: "3.3.3.3", Stage: "WS_PASS"},
	}
	stub := func(ctx context.Context, ip string, port int, cfg vlessProbeConfig, template map[string]any) (vlessProbeMetrics, error) {
		switch ip {
		case "1.1.1.1":
			return vlessProbeMetrics{}, errors.New("sing-box 提前退出: bind: address already in use")
		case "2.2.2.2":
			return vlessProbeMetrics{}, errors.New("VLESS 数据面请求失败: context deadline exceeded")
		default:
			return vlessProbeMetrics{Proven: true}, nil
		}
	}
	cfg := vlessProbeConfig{Timeout: 5 * time.Second, MaxCandidates: 10}
	passed := probeVLESSPoolWithProbe(context.Background(), results, 443, 3, cfg, nil, stub)
	if len(passed) != 1 || passed[0] != "3.3.3.3" {
		t.Fatalf("expected only 3.3.3.3 passed, got %v", passed)
	}
	if results[0].Stage != "skipped" || results[0].ErrorClass != failureLocal {
		t.Errorf("local failure must be skipped/local, got stage=%q class=%q", results[0].Stage, results[0].ErrorClass)
	}
	if results[1].Stage != "VLESS_FAIL" || results[1].ErrorClass != failureTimeout {
		t.Errorf("candidate timeout must be VLESS_FAIL/timeout, got stage=%q class=%q", results[1].Stage, results[1].ErrorClass)
	}
	if results[2].Stage != "VLESS_PASS" || !results[2].DataProven {
		t.Errorf("success must be VLESS_PASS/proven, got stage=%q proven=%v", results[2].Stage, results[2].DataProven)
	}
	// skipped 不得被降级采纳。
	adopted, _ := degradedFromWSResults(results, nil, 3)
	for _, ip := range adopted {
		if ip == "1.1.1.1" {
			t.Errorf("local-failure candidate must never enter the degraded pool, adopted=%v", adopted)
		}
	}
}
