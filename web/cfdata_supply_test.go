package main

import (
	"os"
	"path/filepath"
	"testing"
)

const cfdataSampleResult = `IP地址,最小延迟(ms),最大延迟(ms),平均延迟(ms),丢包率(%)
172.64.229.135,68,77,72,0
162.159.38.225,68,77,72,10
47.245.131.215,10,20,15,0
104.16.0.9,50,60,55,0
162.159.44.237,67,78,73,0
`

func writeCFdataResult(t *testing.T, body string) *app {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, cfdataResultFile), []byte(body), 0644); err != nil {
		t.Fatalf("write result.csv: %v", err)
	}
	return &app{dataDir: dir}
}

func TestCFdataCandidateIPsFiltersNonCloudflare(t *testing.T) {
	a := writeCFdataResult(t, cfdataSampleResult)
	got, err := a.cfdataCandidateIPs(10)
	if err != nil {
		t.Fatalf("cfdataCandidateIPs: %v", err)
	}
	for _, ip := range got {
		if ip == "47.245.131.215" {
			t.Fatal("non-Cloudflare VPS leaked into cfdata candidates")
		}
	}
	if len(got) != 4 {
		t.Fatalf("got %d IPs (%v), want 4 Cloudflare IPs", len(got), got)
	}
}

func TestCFdataCandidateIPsZeroLossFirst(t *testing.T) {
	a := writeCFdataResult(t, cfdataSampleResult)
	got, err := a.cfdataCandidateIPs(2)
	if err != nil {
		t.Fatalf("cfdataCandidateIPs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want 2", got)
	}
	// 162.159.38.225 延迟与 172.64.229.135 相同（72ms），但丢包 10%，必须排后。
	for _, ip := range got {
		if ip == "162.159.38.225" {
			t.Fatalf("lossy IP ranked inside top 2: %v", got)
		}
	}
	// 零丢包组内按延迟升序：104.16.0.9(55ms) 先于 172.64.229.135(72ms)。
	if got[0] != "104.16.0.9" || got[1] != "172.64.229.135" {
		t.Fatalf("got %v, want [104.16.0.9 172.64.229.135]", got)
	}
}

func TestCFdataCandidateIPsMissingFile(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	if _, err := a.cfdataCandidateIPs(5); err == nil {
		t.Fatal("missing result.csv should return an error")
	}
}

func TestParseFloatPrefix(t *testing.T) {
	cases := map[string]float64{
		"0":     0,
		"10":    10,
		"3.5%":  3.5,
		" 12 ":  12,
		"":      0,
		"abc":   0,
		"20.00": 20,
	}
	for in, want := range cases {
		if got := parseFloatPrefix(in); got != want {
			t.Errorf("parseFloatPrefix(%q) = %v, want %v", in, got, want)
		}
	}
}
