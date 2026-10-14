package main

import (
	"testing"
	"time"
)

func TestDomainProbeIntervalClamp(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"300", 300 * time.Second},
		{"120", 120 * time.Second},
		{"1800", 1800 * time.Second},
		{"60", 300 * time.Second},   // 低于下限 → 回落默认
		{"9999", 300 * time.Second}, // 高于上限 → 回落默认
		{"abc", 300 * time.Second},  // 非法 → 回落默认
	}
	for _, tc := range cases {
		t.Setenv("PROXY_DOMAIN_PROBE_INTERVAL_SECONDS", tc.env)
		if got := domainProbeInterval(); got != tc.want {
			t.Fatalf("interval(%q) = %v，期望 %v", tc.env, got, tc.want)
		}
	}
}

func TestDomainProbeTimeoutClamp(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"3000", 3000 * time.Millisecond},
		{"500", 500 * time.Millisecond},
		{"10000", 10000 * time.Millisecond},
		{"100", 3000 * time.Millisecond},   // 低于下限 → 回落默认
		{"60000", 3000 * time.Millisecond}, // 高于上限 → 回落默认
	}
	for _, tc := range cases {
		t.Setenv("PROXY_DOMAIN_PROBE_TIMEOUT_MS", tc.env)
		if got := domainProbeTimeout(); got != tc.want {
			t.Fatalf("timeout(%q) = %v，期望 %v", tc.env, got, tc.want)
		}
	}
}

func TestDomainProbeEnabledDefault(t *testing.T) {
	t.Setenv("PROXY_DOMAIN_PROBE_ENABLED", "")
	if !domainProbeEnabled() {
		t.Fatal("默认应开启域名延迟探测")
	}
	t.Setenv("PROXY_DOMAIN_PROBE_ENABLED", "false")
	if domainProbeEnabled() {
		t.Fatal("PROXY_DOMAIN_PROBE_ENABLED=false 未生效")
	}
}
