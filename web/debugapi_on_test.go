//go:build debugapi

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 调试接口默认只认回环来源：即使有人误发了带 debugapi 标签的构建，
// 内网其它机器也不该能改池。
func TestDebugAPILoopbackOnly(t *testing.T) {
	t.Setenv("DEBUG_API_ALLOW_REMOTE", "false")
	a := &app{}
	mux := http.NewServeMux()
	registerDebugRoutes(mux, a)

	req := httptest.NewRequest(http.MethodGet, "/api/debug/env", nil)
	req.RemoteAddr = "192.168.1.9:5555"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非回环来源 = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/debug/env", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("回环来源 = %d, want 200", rec.Code)
	}
}

// 环境变量接口不能把密钥带出去：白名单之外的变量一律不出现。
func TestDebugAPIEnvAllowlistExcludesSecrets(t *testing.T) {
	t.Setenv("DEBUG_API_ALLOW_REMOTE", "false")
	t.Setenv("PROXY_SUBSCRIPTION_TOKEN", "super-secret-token")
	a := &app{}
	mux := http.NewServeMux()
	registerDebugRoutes(mux, a)
	req := httptest.NewRequest(http.MethodGet, "/api/debug/env", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("env = %d", rec.Code)
	}
	body := rec.Body.String()
	if containsSubstring(body, "super-secret-token") || containsSubstring(body, "PROXY_SUBSCRIPTION_TOKEN") {
		t.Fatalf("env 响应泄漏了密钥: %s", body)
	}
}

// 冷却清零是测试里最常用的动作：确认它真的动了锚点，而不是回个 ok 就完事。
func TestDebugResetCooldown(t *testing.T) {
	t.Setenv("DEBUG_API_ALLOW_REMOTE", "false")
	a := &app{}
	a.lastAutoSwitchAt = time.Now()
	a.lastPoolSwitchAt = time.Now()
	a.poolBlock = poolSwitchVerdict{Reason: "cooldown"}
	a.poolBlockAt = time.Now()
	a.poolBlockBy = "scheduler"

	mux := http.NewServeMux()
	registerDebugRoutes(mux, a)
	req := httptest.NewRequest(http.MethodPost, "/api/debug/pool/reset-cooldown", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset-cooldown = %d: %s", rec.Code, rec.Body.String())
	}
	if !a.lastAutoSwitchAt.IsZero() || !a.lastPoolSwitchAt.IsZero() {
		t.Fatal("冷却锚点没有清零")
	}
	if block := a.poolBlockStatus(); block["blocked"] != false {
		t.Fatalf("拦截记录没有清掉: %v", block)
	}
}

func TestDebugUnknownOp(t *testing.T) {
	t.Setenv("DEBUG_API_ALLOW_REMOTE", "false")
	mux := http.NewServeMux()
	registerDebugRoutes(mux, &app{})
	req := httptest.NewRequest(http.MethodGet, "/api/debug/nope", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知操作 = %d, want 404", rec.Code)
	}
}

func containsSubstring(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
