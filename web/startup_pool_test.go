package main

import "testing"

// 冷启动配置必须与运行态换池同一套契约：Disabled 成员不能因为进程重启复活。
func TestCFnatStartupConfigExcludesDisabledMembers(t *testing.T) {
	a := &app{}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2"},
		Disabled: []string{"2.2.2.2"},
	}
	cfg := a.cfnatStartupConfig()
	if cfg.Fixed != "1.1.1.1" {
		t.Fatalf("fixed = %q, want only enabled member", cfg.Fixed)
	}
	if cfg.Priority != "" {
		t.Fatalf("priority = %q, want empty", cfg.Priority)
	}
}

func TestCFnatStartupConfigHonorsPriorityWhenLocked(t *testing.T) {
	a := &app{}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2"},
		Priority: []string{"2.2.2.2"},
		Lock:     true,
	}
	cfg := a.cfnatStartupConfig()
	if cfg.Fixed != "2.2.2.2" {
		t.Fatalf("fixed = %q, want pinned member only", cfg.Fixed)
	}
	if cfg.Priority != "2.2.2.2" {
		t.Fatalf("priority = %q", cfg.Priority)
	}
}

func TestCFnatStartupConfigReturnsEmptyWhenAllMembersDisabled(t *testing.T) {
	a := &app{}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1"},
		Disabled: []string{"1.1.1.1"},
	}
	cfg := a.cfnatStartupConfig()
	if cfg.Fixed != "" {
		t.Fatalf("fixed = %q, want empty when every member is disabled", cfg.Fixed)
	}
}

func TestProxyActivePoolSnapshotCopiesDisabledAndPriority(t *testing.T) {
	a := &app{}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1"},
		Disabled: []string{"2.2.2.2"},
		Priority: []string{"3.3.3.3"},
	}
	snap := a.proxyActivePoolSnapshot()
	snap.Disabled[0] = "changed"
	snap.Priority[0] = "changed"
	if a.activePool.Disabled[0] != "2.2.2.2" || a.activePool.Priority[0] != "3.3.3.3" {
		t.Fatal("snapshot must not share Disabled/Priority backing arrays with activePool")
	}
}
