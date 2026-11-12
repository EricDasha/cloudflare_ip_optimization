package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// 对端完成 TLS 握手后装死：没有 deadline 时 ReadResponse 会无限等下去，
// goroutine 与信号量槽位一起被占住。探针必须在 ctx 预算内返回。
func TestProbeProxyWebSocketStalledPeerTimesOut(t *testing.T) {
	done := make(chan struct{})
	stall := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 正常情况探针 500ms 内就因 deadline 返回，测试随即关 done 放行；
		// 旧代码（无 deadline）则会等满 6 秒兜底才收到 200，elapsed 超标而失败。
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
	}))
	defer stall.Close()
	defer close(done)
	leaf, err := x509.ParseCertificate(stall.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse test cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	old := probeTLSConfig
	probeTLSConfig = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	defer func() { probeTLSConfig = old }()

	_, portStr, err := net.SplitHostPort(stall.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := proxyAutoConfig{Host: "example.com", Path: "/", Port: port, MaxLatency: 500}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := probeProxyWebSocket(ctx, "127.0.0.1", cfg); err == nil {
		t.Fatalf("stalled peer: got nil error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("probe hung %v on stalled peer, want < 5s", elapsed)
	}
}
