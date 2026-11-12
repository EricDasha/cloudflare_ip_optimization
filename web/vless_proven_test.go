package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vlessResponseProvesDataPlane 的判定边界：只有源站的真实响应才算数，
// sing-box 就地合成的 502/503/504 不算，无响应更不算。
func TestVLESSResponseProvesDataPlane(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		expected int
		want     bool
	}{
		{"预期状态 204", 204, 204, true},
		{"预期状态 200", 200, 200, true},
		{"状态码不对但源站回了 403", 403, 204, true},
		{"状态码不对但源站回了 404", 404, 204, true},
		{"状态码不对但源站回了 200", 200, 204, true},
		{"源站 500", 500, 204, true},
		{"sing-box 合成的 502", 502, 204, false},
		{"sing-box 合成的 503", 503, 204, false},
		{"sing-box 合成的 504", 504, 204, false},
		{"502 即使撞上预期也不算", 502, 502, false},
		{"503 即使撞上预期也不算", 503, 503, false},
		{"无响应", 0, 204, false},
		{"负状态", -1, 204, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vlessResponseProvesDataPlane(tc.status, tc.expected); got != tc.want {
				t.Fatalf("vlessResponseProvesDataPlane(%d, %d) = %v, want %v", tc.status, tc.expected, got, tc.want)
			}
		})
	}
}

// probeVLESSPoolWithProbe 的接线：stub 探针返回的 Proven 必须原样
// 写进结果的 DataProven，失败归类照常；成功则进 VLESS_PASS。
func TestProbeVLESSPoolWithProbePropagatesDataProven(t *testing.T) {
	stub := func(ctx context.Context, ip string, port int, cfg vlessProbeConfig, template map[string]any) (vlessProbeMetrics, error) {
		switch ip {
		case "1.1.1.1":
			// 状态码不对但源站回了响应：有证据的失败。
			return vlessProbeMetrics{Proven: true}, errors.New("VLESS 数据面状态码 403，预期 204")
		case "2.2.2.2":
			return vlessProbeMetrics{}, errors.New("context deadline exceeded")
		case "3.3.3.3":
			return vlessProbeMetrics{Proven: true, Bytes: 4096}, nil
		}
		return vlessProbeMetrics{}, errors.New("unexpected ip " + ip)
	}
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "WS_PASS"},
		{IP: "2.2.2.2", Stage: "WS_PASS"},
		{IP: "3.3.3.3", Stage: "WS_PASS"},
	}
	cfg := vlessProbeConfig{Timeout: 5 * time.Second, MaxCandidates: 10}
	passed := probeVLESSPoolWithProbe(context.Background(), results, 443, 3, cfg, nil, stub)
	if len(passed) != 1 || passed[0] != "3.3.3.3" {
		t.Fatalf("passed = %v, want [3.3.3.3]", passed)
	}
	if !results[0].DataProven || results[0].Stage != "VLESS_FAIL" {
		t.Fatalf("1.1.1.1 = %+v, want VLESS_FAIL 且 DataProven", results[0])
	}
	if results[1].DataProven || results[1].ErrorClass != "timeout" {
		t.Fatalf("2.2.2.2 = %+v, want 超时失败且无证据", results[1])
	}
	if !results[2].DataProven || results[2].Stage != "VLESS_PASS" {
		t.Fatalf("3.3.3.3 = %+v, want VLESS_PASS 且 DataProven", results[2])
	}
}

// fakeSingBoxProxy 是一段假 sing-box：读 -c 指定的探针配置，
// 在其中的 inbound 端口上起一个 HTTP 代理，按环境变量回固定状态码。
// 探针的 HTTP 客户端经它发请求——它回什么状态码，探针就当源站回了什么。
func writeFakeSingBoxProxy(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-sing-box.py")
	body := `import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

cfgPath = sys.argv[sys.argv.index("-c") + 1]
with open(cfgPath) as f:
    cfg = json.load(f)
port = cfg["inbounds"][0]["listen_port"]
status = int(os.environ.get("FAKE_PROXY_STATUS", "403"))
payload = os.environ.get("FAKE_PROXY_BODY", "forbidden").encode()

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(status)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(payload)
    def log_message(self, *args):
        pass

HTTPServer(("127.0.0.1", port), H).serve_forever()
`
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatalf("write fake sing-box: %v", err)
	}
	wrapper := filepath.Join(dir, "fake-sing-box.sh")
	wrapperBody := "#!/bin/sh\nexec python3 " + script + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0755); err != nil {
		t.Fatalf("write fake sing-box wrapper: %v", err)
	}
	return wrapper
}

func probeTestConfig(t *testing.T, dir, binary string) vlessProbeConfig {
	t.Helper()
	return vlessProbeConfig{
		Binary:         binary,
		TemplatePath:   filepath.Join(dir, "template.json"),
		// 故意用 http 而非 https：经代理取 https 时客户端先发 CONNECT，
		// 假 sing-box 得先接 TLS 才能继续；http 直接发绝对路径 GET，
		// 足以测 Proven 接线（本测试不测 TLS 本身）。
		TestURL:        "http://example.com/generate_204",
		ExpectedStatus: http.StatusNoContent,
		Timeout:        10 * time.Second,
		ReadLimit:      4096,
	}
}

// 端到端：假 sing-box 回 403（源站真实响应）时，探针失败但 Proven 为真——
// 隧道确实通了，只是状态码不对；这正是降级可采的情形。
func TestProbeVLESSCandidateMetricsProvenOnStatusMismatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAKE_PROXY_STATUS", "403")
	t.Setenv("FAKE_PROXY_BODY", "forbidden")
	cfg := probeTestConfig(t, dir, writeFakeSingBoxProxy(t, dir))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	metrics, err := probeVLESSCandidateMetrics(ctx, "1.2.3.4", 443, cfg, map[string]any{"type": "vless"})
	if err == nil || !strings.Contains(err.Error(), "状态码 403") {
		t.Fatalf("err = %v, want 状态码 403 失败", err)
	}
	if !metrics.Proven {
		t.Fatalf("metrics = %+v, want Proven=true（源站回了 403，隧道通了）", metrics)
	}
}

// 端到端：假 sing-box 回 502（sing-box 出站失败时就地合成的错误码）
// 时，Proven 必须为假——请求根本没进隧道，不能当成候选通了的证据。
func TestProbeVLESSCandidateMetricsNotProvenOnProxyError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAKE_PROXY_STATUS", "502")
	t.Setenv("FAKE_PROXY_BODY", "Bad Gateway")
	cfg := probeTestConfig(t, dir, writeFakeSingBoxProxy(t, dir))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	metrics, err := probeVLESSCandidateMetrics(ctx, "1.2.3.4", 443, cfg, map[string]any{"type": "vless"})
	if err == nil || !strings.Contains(err.Error(), "状态码 502") {
		t.Fatalf("err = %v, want 状态码 502 失败", err)
	}
	if metrics.Proven {
		t.Fatalf("metrics = %+v, want Proven=false（502 可能是 sing-box 就地合成的）", metrics)
	}
}

// 端到端：假 sing-box 回预期 204 时探针成功，Proven 为真。
func TestProbeVLESSCandidateMetricsProvenOnSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAKE_PROXY_STATUS", "204")
	t.Setenv("FAKE_PROXY_BODY", "")
	cfg := probeTestConfig(t, dir, writeFakeSingBoxProxy(t, dir))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	metrics, err := probeVLESSCandidateMetrics(ctx, "1.2.3.4", 443, cfg, map[string]any{"type": "vless"})
	if err != nil {
		t.Fatalf("err = %v, want 成功", err)
	}
	if !metrics.Proven {
		t.Fatalf("metrics = %+v, want Proven=true", metrics)
	}
}
