package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestCFnatArgsIncludesStickyFlag(t *testing.T) {
	on := defaultCFnatConfig()
	on.Sticky = true
	if !slices.Contains(on.args(), "-sticky=true") {
		t.Fatalf("args() 缺少 -sticky=true: %v", on.args())
	}
	off := defaultCFnatConfig()
	off.Sticky = false
	if !slices.Contains(off.args(), "-sticky=false") {
		t.Fatalf("args() 缺少 -sticky=false: %v", off.args())
	}
}

// 布尔旗标必须以「-flag=值」单参形式出现：Go flag 对 bool 不消费后随参数，
// "-flag true" 会被当位置参数截断后续解析。
func TestCFnatBoolFlagsUseEqualsForm(t *testing.T) {
	args := defaultCFnatConfig().args()
	for i, arg := range args {
		if arg == "-random" || arg == "-tls" || arg == "-sticky" {
			t.Fatalf("布尔旗标 %q 未使用 = 形式（下标 %d）: %v", arg, i, args)
		}
	}
	for _, want := range []string{"-random=", "-tls=", "-sticky="} {
		found := false
		for _, arg := range args {
			if len(arg) >= len(want) && arg[:len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("args() 缺少 %s 前缀的布尔旗标: %v", want, args)
		}
	}
}

func TestDefaultCFnatStickyFromEnv(t *testing.T) {
	t.Setenv("CFNAT_STICKY", "false")
	if defaultCFnatConfig().Sticky {
		t.Fatal("CFNAT_STICKY=false 未生效")
	}
	t.Setenv("CFNAT_STICKY", "true")
	if !defaultCFnatConfig().Sticky {
		t.Fatal("CFNAT_STICKY=true 未生效")
	}
}

func TestRotatingLogWriterRotatesBySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfnat.log")
	w, err := newRotatingLogWriter(path, 8, 3)
	if err != nil {
		t.Fatalf("newRotatingLogWriter() error = %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatalf("first Write() error = %v", err)
	}
	if _, err := w.Write([]byte("abcdefgh")); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	if got := readFile(t, path); got != "abcdefgh" {
		t.Fatalf("current log = %q, want %q", got, "abcdefgh")
	}
	if got := readFile(t, path+".1"); got != "12345678" {
		t.Fatalf("backup log = %q, want %q", got, "12345678")
	}
}

func TestRotatingLogWriterRotatesDaily(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfnat.log")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	w, err := newRotatingLogWriter(path, 1<<20, 3)
	if err != nil {
		t.Fatalf("newRotatingLogWriter() error = %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := readFile(t, path); got != "new" {
		t.Fatalf("current log = %q, want %q", got, "new")
	}
	if got := readFile(t, path+".1"); got != "old" {
		t.Fatalf("backup log = %q, want %q", got, "old")
	}
}

func TestRotatingLogWriterKeepsOnlyThreeBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfnat.log")
	w, err := newRotatingLogWriter(path, 1, 3)
	if err != nil {
		t.Fatalf("newRotatingLogWriter() error = %v", err)
	}
	defer w.Close()
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatalf("Write(%q) error = %v", s, err)
		}
	}
	if _, err := os.Stat(path + ".3"); err != nil {
		t.Fatalf("third backup missing: %v", err)
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("fourth backup should not exist, err = %v", err)
	}
}

// 存量日志超过上限时（线上 cfnat.log 曾到 639 MB），轮转只保留尾部，
// 不能让「切一次」之后磁盘上仍压着几百 MB。
func TestRotatingLogWriterTrimsOversizedLegacyLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfnat.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0644); err != nil {
		t.Fatal(err)
	}
	w, err := newRotatingLogWriter(path, 4, 3)
	if err != nil {
		t.Fatalf("newRotatingLogWriter() error = %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := readFile(t, path+".1"); got != "6789" {
		t.Fatalf("backup tail = %q, want %q", got, "6789")
	}
	if got := readFile(t, path); got != "x" {
		t.Fatalf("current log = %q, want %q", got, "x")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
