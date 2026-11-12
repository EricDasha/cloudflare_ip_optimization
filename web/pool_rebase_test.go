package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// startFakeCFnatWithPool 按给定池的参数启动假 cfnat（sleep 30），
// 供 applyPoolRebased 测试：提交走真实的 stop/start/落盘路径。
func startFakeCFnatWithPool(t *testing.T, a *app, pool proxyActivePool) {
	t.Helper()
	dir := a.dataDir
	script := filepath.Join(dir, "fake-cfnat.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
		t.Fatalf("write fake cfnat: %v", err)
	}
	a.cfnat = newManagedProcess("cfnat", script)
	fixed, priority, err := planFixedMembers(pool)
	if err != nil {
		t.Fatalf("planFixedMembers: %v", err)
	}
	cfg := defaultCFnatConfig()
	cfg.Fixed = strings.Join(fixed, ",")
	cfg.Priority = strings.Join(priority, ",")
	if err := a.cfnat.start(dir, cfg.args(), ""); err != nil {
		t.Fatalf("seed start: %v", err)
	}
	t.Cleanup(func() { _ = a.cfnat.stop() })
}

func setActivePoolForTest(a *app, pool proxyActivePool) {
	a.activeMu.Lock()
	a.activePool = pool
	a.activeMu.Unlock()
}

// F1/F2/F3 的根因：长探测期间别的路径换了池，提交仍用旧快照会把
// 并发改动覆盖掉。本测试模拟「探测开考后验活加了一个成员」，
// rebase 必须把新成员保留在最终池里，而不是用旧基底把它丢掉。
func TestApplyPoolRebasedKeepsConcurrentChange(t *testing.T) {
	dir := t.TempDir()
	base := proxyActivePool{
		IPs:       []string{"1.1.1.1", "2.2.2.2"},
		UpdatedAt: time.Now().Add(-time.Minute),
	}
	a := &app{dataDir: dir}
	setActivePoolForTest(a, base)
	startFakeCFnatWithPool(t, a, base)

	// 并发路径在探测期间换池：加了 9.9.9.9，UpdatedAt 随之更新。
	concurrent := proxyActivePool{
		IPs:       []string{"1.1.1.1", "2.2.2.2", "9.9.9.9"},
		UpdatedAt: time.Now(),
	}
	setActivePoolForTest(a, concurrent)

	err := a.applyPoolRebased(func(fresh proxyActivePool) (proxyActivePool, bool) {
		if !reflect.DeepEqual(fresh.IPs, concurrent.IPs) {
			t.Errorf("rebase 收到的快照 = %v, want %v（应为最新池而非开考快照）", fresh.IPs, concurrent.IPs)
		}
		pool := fresh
		pool.IPs = append(append([]string(nil), fresh.IPs...), "3.3.3.3")
		pool.UpdatedAt = time.Now()
		return pool, true
	})
	if err != nil {
		t.Fatalf("applyPoolRebased: %v", err)
	}
	got := a.proxyActivePoolSnapshot()
	want := []string{"1.1.1.1", "2.2.2.2", "9.9.9.9", "3.3.3.3"}
	if !reflect.DeepEqual(got.IPs, want) {
		t.Fatalf("提交后池 = %v, want %v（并发加的 9.9.9.9 被丢了）", got.IPs, want)
	}
}

// 提交窗口内池被改动时放弃提交，不拿旧基底覆盖新池。
// 闭包内改 UpdatedAt 是在模拟「窗口内已有别的路径换池」——
// 真实窗口在解锁结算来源映射的那一刻，测试只验证 CAS 判据本身。
func TestApplyPoolRebasedAbortsWhenPoolChangedDuringSubmit(t *testing.T) {
	dir := t.TempDir()
	base := proxyActivePool{
		IPs:       []string{"1.1.1.1", "2.2.2.2"},
		UpdatedAt: time.Now().Add(-time.Minute),
	}
	a := &app{dataDir: dir}
	setActivePoolForTest(a, base)
	startFakeCFnatWithPool(t, a, base)

	err := a.applyPoolRebased(func(fresh proxyActivePool) (proxyActivePool, bool) {
		// 模拟窗口内别的路径换池：成员与 UpdatedAt 都变了。
		changed := fresh
		changed.IPs = []string{"1.1.1.1", "2.2.2.2", "9.9.9.9"}
		changed.UpdatedAt = time.Now()
		setActivePoolForTest(a, changed)
		pool := fresh
		pool.IPs = append(append([]string(nil), fresh.IPs...), "3.3.3.3")
		pool.UpdatedAt = time.Now()
		return pool, true
	})
	if !errors.Is(err, errPoolChangedDuringSubmit) {
		t.Fatalf("err = %v, want errPoolChangedDuringSubmit", err)
	}
	got := a.proxyActivePoolSnapshot()
	want := []string{"1.1.1.1", "2.2.2.2", "9.9.9.9"}
	if !reflect.DeepEqual(got.IPs, want) {
		t.Fatalf("放弃提交后池 = %v, want %v（不应被旧基底覆盖）", got.IPs, want)
	}
}

// rebase 闭包返回 ok=false 时不提交、不重启、不报错（哨兵除外）。
func TestApplyPoolRebasedAbortedIsNoop(t *testing.T) {
	dir := t.TempDir()
	base := proxyActivePool{
		IPs:       []string{"1.1.1.1", "2.2.2.2"},
		UpdatedAt: time.Now().Add(-time.Minute),
	}
	a := &app{dataDir: dir}
	setActivePoolForTest(a, base)
	startFakeCFnatWithPool(t, a, base)
	argsBefore := a.cfnat.status().Args

	err := a.applyPoolRebased(func(fresh proxyActivePool) (proxyActivePool, bool) {
		return proxyActivePool{}, false
	})
	if !errors.Is(err, errPoolRebaseAborted) {
		t.Fatalf("err = %v, want errPoolRebaseAborted", err)
	}
	got := a.proxyActivePoolSnapshot()
	if !reflect.DeepEqual(got.IPs, base.IPs) {
		t.Fatalf("放弃后池 = %v, want %v", got.IPs, base.IPs)
	}
	if !reflect.DeepEqual(a.cfnat.status().Args, argsBefore) {
		t.Fatal("放弃提交不应重启 cfnat")
	}
}
