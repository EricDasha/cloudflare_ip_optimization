package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileAtomicWritesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.json")
	if err := writeFileAtomic(path, []byte(`{"v":1}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != `{"v":1}` {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows 的 chmod 只切换只读位，0600 会显示成 0666；权限位只在
	// Linux 生产环境有意义，测试在 Windows 上不校验。
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("perm = %o, want 600", info.Mode().Perm())
	}
	if err := writeFileAtomic(path, []byte(`{"v":2}`), 0600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read overwrite: %v", err)
	}
	if string(data) != `{"v":2}` {
		t.Fatalf("overwritten content = %q", data)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, "pool.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}
