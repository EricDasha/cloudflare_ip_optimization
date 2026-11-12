package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 端口被抢占时换端口重试，而不是把绑定失败记到被测 IP 头上：
// 假 sing-box 每次启动都报 bind address already in use，
// 探针应重试 3 次（计数文件 3 行）再返回该错误。
func TestProbeVLESSCandidateMetricsRetriesOnBindConflict(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "attempts.log")
	// 每次启动往 counter 追加一行并报 bind conflict（见 fakebin_test.go）。
	script := fakeSingBoxBinWithArgFile(t, fakeModeSingBoxBind, counter)
	cfg := vlessProbeConfig{
		Binary:       script,
		TemplatePath: filepath.Join(dir, "template.json"),
		TestURL:      "https://example.com/",
		Timeout:      10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := probeVLESSCandidateMetrics(ctx, "1.2.3.4", 443, cfg, map[string]any{"type": "vless"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "address already in use") {
		t.Fatalf("err = %v, want bind conflict", err)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != maxVLESSProbePortAttempts {
		t.Fatalf("attempts = %d, want %d", got, maxVLESSProbePortAttempts)
	}
}
