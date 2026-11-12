package main

import (
	"context"
	"testing"
	"time"
)

// PROXY_AUTO_CONCURRENCY=0 曾让补位永久卡死扫描锁（cap 为 0 的 channel），
// 负数直接 panic。入口钳制后，非法值一律回落到默认值。
func TestDefaultProxyAutoConfigClampsRanges(t *testing.T) {
	t.Setenv("PROXY_AUTO_CONCURRENCY", "0")
	if got := defaultProxyAutoConfig().Concurrency; got != 20 {
		t.Fatalf("concurrency 0 should fall back to 20, got %d", got)
	}
	t.Setenv("PROXY_AUTO_CONCURRENCY", "-5")
	if got := defaultProxyAutoConfig().Concurrency; got != 20 {
		t.Fatalf("concurrency -5 should fall back to 20, got %d", got)
	}
	t.Setenv("PROXY_AUTO_CONCURRENCY", "200")
	if got := defaultProxyAutoConfig().Concurrency; got != 20 {
		t.Fatalf("concurrency 200 should fall back to 20, got %d", got)
	}
	t.Setenv("PROXY_AUTO_CONCURRENCY", "50")
	if got := defaultProxyAutoConfig().Concurrency; got != 50 {
		t.Fatalf("concurrency 50 should be kept, got %d", got)
	}

	t.Setenv("PROXY_AUTO_PORT", "0")
	if got := defaultProxyAutoConfig().Port; got != 443 {
		t.Fatalf("port 0 should fall back to 443, got %d", got)
	}
	t.Setenv("PROXY_AUTO_POOL_SIZE", "0")
	if got := defaultProxyAutoConfig().PoolSize; got != 5 {
		t.Fatalf("pool size 0 should fall back to 5, got %d", got)
	}
	t.Setenv("PROXY_AUTO_MIN_POOL", "99")
	if got := defaultProxyAutoConfig().MinPool; got != 5 {
		t.Fatalf("min pool 99 should clamp to pool size 5, got %d", got)
	}
	t.Setenv("PROXY_AUTO_MIN_POOL", "0")
	if got := defaultProxyAutoConfig().MinPool; got != 1 {
		t.Fatalf("min pool 0 should clamp to 1, got %d", got)
	}
}

// 即使调用方直接传了 Concurrency=0 的 cfg（手动采用的 gate 路径），
// scanProxyWebSockets 也不得卡死：10 秒内必须返回全部结果。
func TestScanProxyWebSocketsZeroConcurrencyDoesNotDeadlock(t *testing.T) {
	cfg := proxyAutoConfig{Host: "example.com", Port: 443, Concurrency: 0, MaxLatency: 200}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan []proxyScanResult, 1)
	go func() {
		done <- scanProxyWebSockets(ctx, []string{"127.0.0.1", "127.0.0.1"}, cfg)
	}()
	select {
	case results := <-done:
		if len(results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(results))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scanProxyWebSockets with Concurrency 0 deadlocked")
	}
}
