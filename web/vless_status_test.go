package main

import (
	"context"
	"os"
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
	script := filepath.Join(dir, "fake-sing-box.py")
	py := `#!/usr/bin/env python3
import json, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer
cfg = json.load(open(sys.argv[3]))
port = cfg["inbounds"][0]["listen_port"]
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(403)
        self.send_header("Content-Length", "5000000")
        self.end_headers()
        time.sleep(10)
        try:
            self.wfile.write(b"x" * 1000)
        except Exception:
            pass
    def log_message(self, *a):
        pass
HTTPServer(("127.0.0.1", port), H).serve_forever()
`
	if err := os.WriteFile(script, []byte(py), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
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
