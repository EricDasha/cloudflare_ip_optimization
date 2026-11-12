package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// startBlackHoleListener 接受 TCP 连接后什么都不回：WS/TLS 握手会一直等到
// ctx 到期。用于让第一个探测占住信号量，验证排队者被记 skipped 而非失败。
// 已接受的连接必须留引用（否则 GC 回收会关掉它们，握手立刻 EOF 失败，
// 排队者就测上了——满载跑全量测试时 GC 恰好会跑，测试因此飘）。
func startBlackHoleListener(t *testing.T) (port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen black hole: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 只持有连接不读写，握手方在 ctx 到期前收不到任何字节。
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	}
}

// 预算耗尽时还没排上槽位的候选必须记 skipped，不得记成 WS_FAIL 超时失败——
// 否则订阅粗筛每轮约 280 条尾部候选被记一次失败，3 次即判 fail。
// 300ms 预算对黑洞握手什么都盖不住：唯一开跑的那条也会在中途被预算
// 截断，按与 revalidate 在途探针同一规则记 skipped——本轮 10 条全是
// skipped，一条失败都不许记。
func TestScanProxyWebSocketsSkipsQueuedWhenBudgetExpires(t *testing.T) {
	port, closeFn := startBlackHoleListener(t)
	defer closeFn()
	cfg := proxyAutoConfig{Host: "example.com", Port: port, Concurrency: 1, MaxLatency: 5000}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ips := make([]string, 10)
	for i := range ips {
		ips[i] = "127.0.0.1"
	}
	start := time.Now()
	results := scanProxyWebSockets(ctx, ips, cfg)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("scan took %v, expected ~300ms", elapsed)
	}
	skipped, failed := 0, 0
	for _, r := range results {
		switch r.Stage {
		case "skipped":
			skipped++
			if r.Error == "" {
				t.Errorf("skipped result %s must carry an error message", r.IP)
			}
		case "WS_FAIL":
			failed++
		default:
			t.Errorf("unexpected stage %q for %s", r.Stage, r.IP)
		}
	}
	if failed != 0 {
		t.Errorf("expected no failures when the budget covers nothing, got %d: %+v", failed, results)
	}
	if skipped != len(ips) {
		t.Errorf("expected %d skipped, got %d: %+v", len(ips), skipped, results)
	}
}

// 初筛 scanProxyIPs 同款：TLS 握手排队耗尽预算时记 skipped，不记 TLS_FAIL。
func TestScanProxyIPsSkipsQueuedWhenBudgetExpires(t *testing.T) {
	port, closeFn := startBlackHoleListener(t)
	defer closeFn()
	cfg := proxyScanConfig{Host: "example.com", Port: port, Concurrency: 1, MaxLatency: 5000, TLS: true}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ips := make([]string, 6)
	for i := range ips {
		ips[i] = "127.0.0.1"
	}
	results := scanProxyIPs(ctx, ips, cfg)
	skipped, failed := 0, 0
	for _, r := range results {
		switch r.Stage {
		case "skipped":
			skipped++
		case "TLS_FAIL":
			failed++
		default:
			t.Errorf("unexpected stage %q for %s", r.Stage, r.IP)
		}
	}
	if failed != 0 {
		t.Errorf("expected no failures when the budget covers nothing, got %d: %+v", failed, results)
	}
	if skipped != len(ips) {
		t.Errorf("expected %d skipped, got %d: %+v", len(ips), skipped, results)
	}
}

// 降级不采纳 skipped：本轮没测过的候选既不进池，也不算失败账。
func TestDegradedRejectsSkippedResults(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "skipped", Error: "等待探测槽位时预算耗尽，未开始探测", ErrorClass: failureTimeout},
		{IP: "2.2.2.2", Stage: "WS_PASS"},
	}
	adopted, rejected := degradedFromWSResults(results, nil, 2)
	if len(adopted) != 0 {
		t.Fatalf("skipped/WS_PASS must not be adopted, got %v", adopted)
	}
	if len(rejected) != 2 {
		t.Fatalf("expected 2 rejections, got %v", rejected)
	}
}
