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

// cfdata 是本机亲手测的延迟与丢包，比任何地址表都准——官方段外的公网地址
// 照样放行（2026-11-05 起官方段降级为标签；线上实测非官方段反代同样在转发）。
// 私网与非 IPv4 仍然必须挡住。
func TestCFdataCandidateIPsKeepsAllPublicIPv4(t *testing.T) {
	body := cfdataSampleResult + "192.168.1.50,1,2,1.5,0\nnot-an-ip,1,2,1.5,0\n"
	a := writeCFdataResult(t, body)
	got, err := a.cfdataCandidateIPs(10)
	if err != nil {
		t.Fatalf("cfdataCandidateIPs: %v", err)
	}
	for _, ip := range got {
		if ip == "192.168.1.50" || ip == "not-an-ip" {
			t.Fatalf("non-public address leaked into cfdata candidates: %v", got)
		}
	}
	if len(got) != 5 {
		t.Fatalf("got %d IPs (%v), want 5 public IPv4", len(got), got)
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
	// 零丢包组内按延迟升序：47.245.131.215(15ms) < 104.16.0.9(55ms) < 172.64.229.135(72ms)。
	// 47.245 不是官方段地址，但它是这台机器实测最快、零丢包的一条——本机实测
	// 高于地址表归属，这正是 cfdata 层排在 subscription/bestcf 之前的原因。
	if got[0] != "47.245.131.215" || got[1] != "104.16.0.9" {
		t.Fatalf("got %v, want [47.245.131.215 104.16.0.9]", got)
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
