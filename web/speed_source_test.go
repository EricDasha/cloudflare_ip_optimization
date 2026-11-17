package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreflightSpeedEndpoint(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
	}))
	defer ok.Close()
	if err := preflightSpeedEndpoint(context.Background(), ok.URL+"/file?bytes={bytes}"); err != nil {
		t.Fatalf("healthy endpoint: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer bad.Close()
	if err := preflightSpeedEndpoint(context.Background(), bad.URL); err == nil {
		t.Fatal("403 endpoint should fail preflight")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer empty.Close()
	if err := preflightSpeedEndpoint(context.Background(), empty.URL); err == nil {
		t.Fatal("empty endpoint should fail preflight")
	}
}

func TestSpeedURLFallsBackWhenCustomEndpointUnhealthy(t *testing.T) {
	t.Setenv("PROXY_VLESS_SPEED_URL", "https://custom.example/speed")
	t.Setenv("PROXY_DISABLED_BENCH_SPEED_URL", "")
	customSpeedURLUnhealthy.Store(true)
	t.Cleanup(func() { customSpeedURLUnhealthy.Store(false) })

	if got := vlessSpeedTestURL(1024); got != disabledBenchSpeedURLDefault {
		t.Fatalf("vless URL = %q, want default", got)
	}
	if got := disabledBenchSpeedURL(1024); got != disabledBenchSpeedURLDefault {
		t.Fatalf("bench URL = %q, want default", got)
	}
}

func TestDisabledBenchURLKeepsExplicitOverrideWhenGlobalUnhealthy(t *testing.T) {
	t.Setenv("PROXY_VLESS_SPEED_URL", "https://custom.example/speed")
	t.Setenv("PROXY_DISABLED_BENCH_SPEED_URL", "https://bench.example/file")
	customSpeedURLUnhealthy.Store(true)
	t.Cleanup(func() { customSpeedURLUnhealthy.Store(false) })

	if got := disabledBenchSpeedURL(1024); got != "https://bench.example/file" {
		t.Fatalf("explicit bench URL = %q, want override preserved", got)
	}
}
