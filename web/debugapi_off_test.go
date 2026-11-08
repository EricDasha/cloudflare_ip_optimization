//go:build !debugapi

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 生产构建里 /api/debug/ 必须根本不存在。注意这不是「开关关掉了」，
// 而是 registerDebugRoutes 是空实现、调试代码不参与编译。
func TestDebugAPIAbsentInProductionBuild(t *testing.T) {
	mux := http.NewServeMux()
	registerDebugRoutes(mux, &app{})
	mux.Handle("/", staticHandler())

	for _, path := range []string{"/api/debug/state", "/api/debug/env", "/api/debug/pool"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 在生产构建里 = %d, want 404", path, rec.Code)
		}
	}
}
