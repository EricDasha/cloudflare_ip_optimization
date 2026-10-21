package main

import (
	"context"
	"testing"
	"time"
)

// 建池失败时「保留旧池」必须先证明旧池活着。
// 2026-10-22 事故形态：终审 0 通过，旧池唯一 IP 早已 dial timeout，
// 系统仍报「保留旧池」，对外显示一切正常。
func TestVerifyActivePoolAliveAllDead(t *testing.T) {
	orig := probeTCPAlive
	defer func() { probeTCPAlive = orig }()
	probeTCPAlive = func(context.Context, string, time.Duration) bool { return false }

	a := newProbeTestApp(t)
	a.activePool = proxyActivePool{
		IPs:     []string{"172.64.229.235"},
		Domains: []string{"store.ubi.com"},
	}
	alive, total := a.verifyActivePoolAlive()
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if alive != 0 {
		t.Fatalf("alive = %d, want 0（全部不可达时必须判死）", alive)
	}
}

func TestVerifyActivePoolAliveAllLive(t *testing.T) {
	orig := probeTCPAlive
	defer func() { probeTCPAlive = orig }()
	probeTCPAlive = func(context.Context, string, time.Duration) bool { return true }

	a := newProbeTestApp(t)
	a.activePool = proxyActivePool{
		IPs:     []string{"23.146.248.149", "207.57.131.117"},
		Domains: []string{"www.shopify.com"},
	}
	alive, total := a.verifyActivePoolAlive()
	if alive != 3 || total != 3 {
		t.Fatalf("verifyActivePoolAlive() = %d/%d, want 3/3", alive, total)
	}
}

// 部分存活也要如实报告，不能因为有一个活的就宣称全好。
func TestVerifyActivePoolAlivePartial(t *testing.T) {
	orig := probeTCPAlive
	defer func() { probeTCPAlive = orig }()
	probeTCPAlive = func(_ context.Context, addr string, _ time.Duration) bool {
		return addr == "23.146.248.149:443"
	}

	a := newProbeTestApp(t)
	a.activePool = proxyActivePool{
		IPs:     []string{"23.146.248.149", "172.64.229.235"},
		Domains: []string{"store.ubi.com"},
	}
	alive, total := a.verifyActivePoolAlive()
	if alive != 1 || total != 3 {
		t.Fatalf("verifyActivePoolAlive() = %d/%d, want 1/3", alive, total)
	}
}

// 空池不算「全灭」，否则会把「本来就没有池」误报成紧急故障。
func TestVerifyActivePoolAliveEmptyPool(t *testing.T) {
	orig := probeTCPAlive
	defer func() { probeTCPAlive = orig }()
	called := false
	probeTCPAlive = func(context.Context, string, time.Duration) bool {
		called = true
		return true
	}

	a := newProbeTestApp(t)
	alive, total := a.verifyActivePoolAlive()
	if alive != 0 || total != 0 {
		t.Fatalf("verifyActivePoolAlive() = %d/%d, want 0/0", alive, total)
	}
	if called {
		t.Fatal("空池不应发起任何探测")
	}
}

// 探测端口必须跟随 PROXY_AUTO_PORT，而不是硬编码 443。
func TestVerifyActivePoolAliveUsesConfiguredPort(t *testing.T) {
	orig := probeTCPAlive
	defer func() { probeTCPAlive = orig }()
	t.Setenv("PROXY_AUTO_PORT", "8443")

	var got []string
	probeTCPAlive = func(_ context.Context, addr string, _ time.Duration) bool {
		got = append(got, addr)
		return true
	}

	a := newProbeTestApp(t)
	a.activePool = proxyActivePool{IPs: []string{"23.146.248.149"}}
	a.verifyActivePoolAlive()
	if len(got) != 1 || got[0] != "23.146.248.149:8443" {
		t.Fatalf("probed addresses = %v, want [23.146.248.149:8443]", got)
	}
}
