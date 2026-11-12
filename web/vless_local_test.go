package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

func TestVLESSReviewBudget(t *testing.T) {
	t.Setenv("PROXY_VLESS_SPEED_TIMEOUT", "20")
	// 默认：20×15 秒 + 6×20 秒 + 60 秒 = 480 秒，不足 10 分钟下限。
	if got := vlessReviewBudget(vlessProbeConfig{MaxCandidates: 20, Timeout: 15 * time.Second}); got != 10*time.Minute {
		t.Errorf("default workload should get the 10m floor, got %v", got)
	}
	// batch 50×15 秒：750+120+60=930 秒。
	if got := vlessReviewBudget(vlessProbeConfig{MaxCandidates: 50, Timeout: 15 * time.Second}); got != 930*time.Second {
		t.Errorf("50x15s workload should need 930s, got %v", got)
	}
	// 100×60 秒：6000+120+60=6180 秒，封顶 30 分钟。
	if got := vlessReviewBudget(vlessProbeConfig{MaxCandidates: 100, Timeout: 60 * time.Second}); got != 30*time.Minute {
		t.Errorf("100x60s workload should cap at 30m, got %v", got)
	}
	// 非法值按最小工作量算，回落到 10 分钟下限。
	if got := vlessReviewBudget(vlessProbeConfig{MaxCandidates: 0, Timeout: 0}); got != 10*time.Minute {
		t.Errorf("zero config should get the 10m floor, got %v", got)
	}
}

// 父预算已到期：一条都不该真测，全部记 skipped。
func TestProbeVLESSPoolWithProbeSkipsBudgetExhaustion(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "WS_PASS"},
		{IP: "2.2.2.2", Stage: "WS_PASS"},
	}
	calls := 0
	stub := func(ctx context.Context, ip string, port int, cfg vlessProbeConfig, template map[string]any) (vlessProbeMetrics, error) {
		calls++
		return vlessProbeMetrics{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := vlessProbeConfig{Timeout: 5 * time.Second, MaxCandidates: 10}
	if passed := probeVLESSPoolWithProbe(ctx, results, 443, 2, cfg, nil, stub); len(passed) != 0 {
		t.Fatalf("expected no passes with an expired budget, got %v", passed)
	}
	if calls != 0 {
		t.Fatalf("probe must not be called with an expired budget, called %d times", calls)
	}
	for _, r := range results {
		if r.Stage != "skipped" {
			t.Errorf("result %s must be skipped, got stage=%q", r.IP, r.Stage)
		}
	}
}

// 本条正在跑时父预算到期：不是候选死了，是预算没了，记 skipped。
func TestProbeVLESSPoolWithProbeSkipsTruncatedAttempt(t *testing.T) {
	results := []proxyScanResult{{IP: "1.1.1.1", Stage: "WS_PASS"}}
	ctx, cancel := context.WithCancel(context.Background())
	stub := func(ctx context.Context, ip string, port int, cfg vlessProbeConfig, template map[string]any) (vlessProbeMetrics, error) {
		cancel()
		return vlessProbeMetrics{}, errors.New("VLESS 数据面请求失败: context deadline exceeded")
	}
	cfg := vlessProbeConfig{Timeout: 5 * time.Second, MaxCandidates: 10}
	probeVLESSPoolWithProbe(ctx, results, 443, 1, cfg, nil, stub)
	if results[0].Stage != "skipped" {
		t.Fatalf("truncated attempt must be skipped, got stage=%q class=%q", results[0].Stage, results[0].ErrorClass)
	}
}

// 名额已满没轮上的不是失败：记 skipped，不进质量账。
func TestProbeVLESSPoolWithProbeSkipsOverCap(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "WS_PASS"},
		{IP: "2.2.2.2", Stage: "WS_PASS"},
		{IP: "3.3.3.3", Stage: "WS_PASS"},
	}
	stub := func(ctx context.Context, ip string, port int, cfg vlessProbeConfig, template map[string]any) (vlessProbeMetrics, error) {
		return vlessProbeMetrics{Proven: true}, nil
	}
	cfg := vlessProbeConfig{Timeout: 5 * time.Second, MaxCandidates: 1}
	passed := probeVLESSPoolWithProbe(context.Background(), results, 443, 3, cfg, nil, stub)
	if len(passed) != 1 {
		t.Fatalf("expected 1 pass, got %v", passed)
	}
	if results[0].Stage != "VLESS_PASS" {
		t.Errorf("first result must be VLESS_PASS, got %q", results[0].Stage)
	}
	for _, r := range results[1:] {
		if r.Stage != "skipped" {
			t.Errorf("over-cap result %s must be skipped, got stage=%q", r.IP, r.Stage)
		}
	}
}

// 撞端口时：别人的监听端口是通的，但自己的 sing-box 已绑定失败退出——
// 就绪拨号不得直接 break 去用别人的隧道，必须发现进程已退出、
// 按绑定冲突返回（调用方重试换端口）。
//
// 时序安排：假 sing-box 60ms 后退出；「赢家」监听在 100ms 后才出现，
// 就绪循环前两次拨号失败、第三次拨通时进程已死。无修复时第三次拨通
// 即 break，随后 HTTP 测试对着只接不回的监听空等到 ctx 到期。
func TestProbeVLESSCandidateMetricsOnceDetectsDeadProcessOnDial(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-sing-box-dead.sh")
	body := "#!/bin/sh\nsleep 0.06\necho 'FATAL: listen tcp 127.0.0.1: bind: address already in use' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatalf("write fake sing-box: %v", err)
	}
	cfg := probeTestConfig(t, dir, script)

	// 先取一个空闲端口号（不占着），100ms 后再由「赢家」监听它。
	ln0, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	port := ln0.Addr().(*net.TCPAddr).Port
	_ = ln0.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return
		}
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = probeVLESSCandidateMetricsOnce(ctx, "1.2.3.4", 443, cfg, map[string]any{"type": "vless"}, port)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("probe took %v, expected a fast bind-conflict return", elapsed)
	}
	if err == nil || !isBindConflictError(err) {
		t.Fatalf("err = %v, want bind conflict error", err)
	}
}

// 入口钳制：MaxCandidates 0 不得零次尝试，500 不得超 100 次；
// Timeout 0 不得让每条探针 ctx 立即过期。
func TestProbeVLESSPoolWithProbeClampsConfig(t *testing.T) {
	calls := 0
	stub := func(ctx context.Context, ip string, port int, cfg vlessProbeConfig, template map[string]any) (vlessProbeMetrics, error) {
		calls++
		if ctx.Err() != nil {
			t.Errorf("probe ctx must not be expired at call time (ip %s)", ip)
		}
		return vlessProbeMetrics{}, errors.New("VLESS 数据面请求失败: context deadline exceeded")
	}
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "WS_PASS"},
		{IP: "2.2.2.2", Stage: "WS_PASS"},
	}
	probeVLESSPoolWithProbe(context.Background(), results, 443, 2, vlessProbeConfig{Timeout: 0, MaxCandidates: 0}, nil, stub)
	if calls != 1 {
		t.Errorf("MaxCandidates 0 should clamp to 1 attempt, got %d calls", calls)
	}

	calls = 0
	many := make([]proxyScanResult, 150)
	for i := range many {
		many[i] = proxyScanResult{IP: "10.0.0." + strconv.Itoa(i+1), Stage: "WS_PASS"}
	}
	probeVLESSPoolWithProbe(context.Background(), many, 443, 150, vlessProbeConfig{Timeout: 5 * time.Second, MaxCandidates: 500}, nil, stub)
	if calls != 100 {
		t.Errorf("MaxCandidates 500 should clamp to 100 attempts, got %d calls", calls)
	}
}
