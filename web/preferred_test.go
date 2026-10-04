package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPreferredSettingsPersistExplicitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, preferredSettingsFile)
	if err := os.WriteFile(path, []byte(`{"enabled":[],"limit":20}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &app{dataDir: dir}
	a.loadPreferredSettings()
	if got := a.enabledPreferredDomains(); len(got) != 0 {
		t.Fatalf("explicit empty enabled list restored %v", got)
	}
}

// 优选域名职责已收敛为「直接作为 -fixed 转发成员」：
// 解析供给（resolvePreferredDomainIP*）随 C 区一并拆除，此处仅保留设置持久化行为。
