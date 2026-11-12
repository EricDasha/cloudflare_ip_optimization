package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 状态码必须在读 body 之前校验：403 的错误体不必读满预算才判失败，
// 且读超时会先于状态码返回、把报错原因写错。假 sing-box 回 403，
// 响应头后拖 10 秒才写 body——探针应立即因状态码失败，不等 body。
func TestProbeVLESSCandidateMetricsFailsFastOnBadStatus(t *testing.T) {
	dir := t.TempDir()
	// 假 sing-box 回 403，且响应头之后再拖 10 秒才结束响应体。
	script := fakeSingBoxBin(t, fakeModeSingBoxSlow)
	cfg := vlessProbeConfig{
		Binary:         script,
		TemplatePath:   filepath.Join(dir, "template.json"),
		TestURL:        "http://example.com/",
		ExpectedStatus: 200,
		Timeout:        30 * time.Second,
		ReadLimit:      5 * 1024 * 1024,
		MinBytes:       1024,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := probeVLESSCandidateMetrics(ctx, "1.2.3.4", 443, cfg, map[string]any{"type": "vless"})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "状态码 403") {
		t.Fatalf("err = %v, want 状态码 403", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("bad-status probe took %v, want < 5s (must fail before reading body)", elapsed)
	}
}
