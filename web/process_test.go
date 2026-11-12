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

// start() 失败不得毒化进程状态：旧实现先把 exitCode 清 nil 再调
// cmd.Start()，失败后留下 cmd!=nil + exitCode==nil，status() 误报
// Running（旧 PID），之后每次 start() 都被「已在运行」挡回。
func TestManagedProcessFailedStartDoesNotPoisonState(t *testing.T) {
	dir := t.TempDir()
	// 用「立刻成功退出」的假二进制替代 /bin/true（Windows 上没有该路径）。
	// 见 fakebin_test.go：测试二进制复执行自身，靠环境变量认领身份。
	trueBin := fakeSingBoxBin(t, fakeModeExitZero)
	p := newManagedProcess("proc", trueBin)
	if err := p.start(dir, nil, ""); err != nil {
		t.Fatalf("first start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !p.status().Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p.status().Running {
		t.Fatalf("exit-zero fake binary still reported Running")
	}
	p.bin = filepath.Join(dir, "missing-binary")
	if err := p.start(dir, nil, ""); err == nil {
		t.Fatalf("missing binary start: got nil error")
	}
	if st := p.status(); st.Running {
		t.Fatalf("failed start reported Running with stale pid %d", st.PID)
	}
	p.bin = trueBin
	if err := p.start(dir, nil, ""); err != nil {
		t.Fatalf("start after failure: %v", err)
	}
}
