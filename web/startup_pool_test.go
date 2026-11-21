package main

import (
	"os"
	"strings"
	"testing"
)

// fixedMembersOf 返回冷启动配置真正会交给 cfnat 的成员清单。
//
// 现在成员走清单文件（-fixedfile）而不是命令行，断言必须落到文件内容上，
// 只看 cfg.Fixed 会永远看到空串而误判通过。
func fixedMembersOf(t *testing.T, cfg cfnatConfig) string {
	t.Helper()
	if cfg.FixedFile == "" {
		return cfg.Fixed
	}
	b, err := os.ReadFile(cfg.FixedFile)
	if err != nil {
		t.Fatalf("读目标清单 %s: %v", cfg.FixedFile, err)
	}
	return strings.Join(strings.Fields(string(b)), ",")
}

// 冷启动配置必须与运行态换池同一套契约：Disabled 成员不能因为进程重启复活。
func TestCFnatStartupConfigExcludesDisabledMembers(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2"},
		Disabled: []string{"2.2.2.2"},
	}
	cfg := a.cfnatStartupConfig()
	if got := fixedMembersOf(t, cfg); got != "1.1.1.1" {
		t.Fatalf("fixed = %q, want only enabled member", got)
	}
	if cfg.Priority != "" {
		t.Fatalf("priority = %q, want empty", cfg.Priority)
	}
}

func TestCFnatStartupConfigHonorsPriorityWhenLocked(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	a.activePool = proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2"},
		Priority: []string{"2.2.2.2"},
		Lock:     true,
	}
	cfg := a.cfnatStartupConfig()
	if got := fixedMembersOf(t, cfg); got != "2.2.2.2" {
		t.Fatalf("fixed = %q, want pinned member only", got)
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
