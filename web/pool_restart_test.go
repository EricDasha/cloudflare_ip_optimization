package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 同参不重启的判据必须两头都硬：进程在跑且参数逐字相同，才允许跳过重启。
func TestShouldSkipCFnatRestart(t *testing.T) {
	cfg := defaultCFnatConfig()
	cfg.Fixed = "1.1.1.1,edge.example.com"
	cfg.Priority = "1.1.1.1"
	want := cfg.args()

	running := processStatus{Running: true, Args: append([]string(nil), want...)}
	if !shouldSkipCFnatRestart(running, cfg.args()) {
		t.Fatal("参数逐字相同且进程在跑，应跳过重启")
	}

	stopped := processStatus{Running: false, Args: append([]string(nil), want...)}
	if shouldSkipCFnatRestart(stopped, cfg.args()) {
		t.Fatal("进程未运行时跳过重启会让新池没人转发，必须重启")
	}

	// 顺序不同也算不同：args() 输出顺序固定，顺序变了说明构造逻辑有别。
	shuffled := append([]string(nil), want...)
	shuffled[0], shuffled[1] = shuffled[1], shuffled[0]
	if sameCFnatArgs(shuffled, want) {
		t.Fatal("顺序不同不应判为相同")
	}

	changedCfg := defaultCFnatConfig()
	changedCfg.Fixed = "1.1.1.1,other.example.com"
	changedCfg.Priority = "1.1.1.1"
	changed := processStatus{Running: true, Args: changedCfg.args()}
	if shouldSkipCFnatRestart(changed, cfg.args()) {
		t.Fatal("-fixed 变了必须重启")
	}

	if sameCFnatArgs([]string{"-fixed", "a"}, []string{"-fixed", "a", "-num", "1"}) {
		t.Fatal("长度不同不应判为相同")
	}
}

// 闸门不能把服务拦死：IP 全无、域名又全被判坏时，池级兜底必须放行原域名，
// 否则 planFixedMembers 拿到空集合直接报错，1234 上一个目标都没有。
func TestGatePoolDomainsFallsBackWhenNoIPAndAllDomainsBlocked(t *testing.T) {
	a := newProbeTestApp(t)
	a.domainQuality["dead.example.com"] = freshRecord(domainStateDead)

	pool := proxyActivePool{Domains: []string{"dead.example.com"}}
	got := a.gatePoolDomains(pool)
	if !reflect.DeepEqual(got, []string{"dead.example.com"}) {
		t.Fatalf("无 IP 且域名全坏时应兜底放行，got %v", got)
	}

	// 还有可用 IP 时不兜底：坏域名该拦就拦，别拿它凑数。
	withIP := proxyActivePool{IPs: []string{"1.1.1.1"}, Domains: []string{"dead.example.com"}}
	if got := a.gatePoolDomains(withIP); len(got) != 0 {
		t.Fatalf("有可用 IP 时不得兜底坏域名，got %v", got)
	}
}

// 二进制），而不是以「参数相同」为由静默落盘。静默落盘的后果是池子改了、
// 1234 上却没人转发，而接口一路报成功。
func TestApplyPoolLockedRestartsWhenProcessDown(t *testing.T) {
	a := &app{
		dataDir: t.TempDir(),
		cfnat:   newManagedProcess("cfnat", filepath.Join(t.TempDir(), "missing-cfnat")),
	}
	pool := proxyActivePool{
		IPs:       []string{"1.1.1.1"},
		Domains:   []string{"edge.example.com"},
		UpdatedAt: time.Now(),
	}
	// 把参数预先写成与目标一致：若判据只看参数不看运行状态，会误跳过重启。
	fixed, priority, err := planFixedMembers(pool)
	if err != nil {
		t.Fatalf("planFixedMembers: %v", err)
	}
	cfg := defaultCFnatConfig()
	cfg.Fixed = strings.Join(fixed, ",")
	cfg.Priority = strings.Join(priority, ",")
	a.cfnat.args = cfg.args()

	if err := a.applyPoolLocked(pool, pool); err == nil {
		t.Fatal("进程未运行时应尝试启动，期望启动失败并返回错误")
	}
}

// 成功换池必须清掉粘滞的 pool.Error：调用方传 pool:=current，旧报错
// 会一路带进落盘，GUI 在换池成功后仍挂着上一轮的拦截原因。
func TestApplyPoolLockedClearsStalePoolError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-cfnat.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	a := &app{dataDir: dir, cfnat: newManagedProcess("cfnat", script)}
	pool := proxyActivePool{
		IPs:       []string{"1.1.1.1"},
		Error:     "上一轮拦截",
		ErrorAt:   time.Now(),
		UpdatedAt: time.Now(),
	}
	fixed, priority, err := planFixedMembers(pool)
	if err != nil {
		t.Fatalf("planFixedMembers: %v", err)
	}
	cfg := defaultCFnatConfig()
	cfg.Fixed = strings.Join(fixed, ",")
	cfg.Priority = strings.Join(priority, ",")
	cfg.Fallback = strings.TrimSpace(env("CFNAT_FALLBACK", ""))
	if err := a.cfnat.start(dir, cfg.args(), ""); err != nil {
		t.Fatalf("seed start: %v", err)
	}
	defer a.cfnat.stop()
	if err := a.applyPoolLocked(pool, pool); err != nil {
		t.Fatalf("applyPoolLocked: %v", err)
	}
	saved := a.proxyActivePoolSnapshot()
	if saved.Error != "" || !saved.ErrorAt.IsZero() {
		t.Fatalf("saved error = %q %v, want empty", saved.Error, saved.ErrorAt)
	}
}
