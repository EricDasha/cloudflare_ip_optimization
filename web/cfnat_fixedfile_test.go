package main

import (
	"os"
	"strings"
	"testing"
)

// 成员清单是一行一个，cfnat 用 -fixedfile 读它：写成逗号分隔或带空行都会
// 让某些成员被解析成空串。
func TestWriteCFnatFixedFileOneMemberPerLine(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	path, prev, err := a.writeCFnatFixedFile([]string{"1.1.1.1", "example.com"})
	if err != nil {
		t.Fatalf("writeCFnatFixedFile: %v", err)
	}
	if len(prev) != 0 {
		t.Fatalf("首写不该有上一份内容，got %q", prev)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(b), "1.1.1.1\nexample.com\n"; got != want {
		t.Fatalf("清单内容 = %q, want %q", got, want)
	}
}

// 回滚必须把清单改回上一份：cfnat 会在 5 秒内读它，改不回去等于没回滚。
func TestRestoreCFnatFixedFileRollsBackContent(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	path, _, err := a.writeCFnatFixedFile([]string{"1.1.1.1"})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	path, prev, err := a.writeCFnatFixedFile([]string{"2.2.2.2"})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if string(prev) != string(before) {
		t.Fatalf("prev = %q, want %q", prev, before)
	}
	restoreCFnatFixedFile(path, prev)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("回滚后内容 = %q, want %q", after, before)
	}
}

// 没有数据目录时宁可报错，也不要把清单写进进程工作目录。
func TestWriteCFnatFixedFileRefusesEmptyDataDir(t *testing.T) {
	a := &app{}
	if _, _, err := a.writeCFnatFixedFile([]string{"1.1.1.1"}); err == nil {
		t.Fatal("数据目录为空时必须报错")
	}
}

// -fixedfile 必须进命令行，否则 cfnat 根本不会去读清单。
func TestCFnatArgsCarryFixedFile(t *testing.T) {
	cfg := defaultCFnatConfig()
	cfg.FixedFile = "/data/cfnat-fixed.txt"
	joined := strings.Join(cfg.args(), " ")
	if !strings.Contains(joined, "-fixedfile /data/cfnat-fixed.txt") {
		t.Fatalf("args 里没有 -fixedfile: %s", joined)
	}
}
