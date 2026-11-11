package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 超大明细 CSV 直接拒绝，不再 ReadAll 吃内存。
func TestReadDetailCSVRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.csv")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// 11 MiB 单行字段，超过 10 MiB 上限。
	if _, err := f.Write(make([]byte, maxDetailCSVBytes+1)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := readDetailCSV(big); err == nil {
		t.Fatalf("readDetailCSV(11MiB) = nil error, want too large")
	}
	if _, err := countCSVRows(big); err == nil {
		t.Fatalf("countCSVRows(11MiB) = nil error, want too large")
	}
	// 正常小文件仍可读。
	small := filepath.Join(dir, "small.csv")
	if err := os.WriteFile(small, []byte("ip,min,max,avg,loss\n1.1.1.1,10,20,15,0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := readDetailCSV(small)
	if err != nil || len(rows) != 1 || rows[0].IP != "1.1.1.1" {
		t.Fatalf("readDetailCSV(small) = %v, %v", rows, err)
	}
}
