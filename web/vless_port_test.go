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
	script := filepath.Join(dir, "fake-sing-box.sh")
	body := "#!/bin/sh\necho x >> " + counter + "\necho 'FATAL: listen tcp: bind: address already in use' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
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
