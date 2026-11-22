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

// --- 真实吞吐重排（对标 CloudflareST 的「延迟初筛 → 下载测速 → 按速度排序」）---

func TestCFdataSpeedTierOrdering(t *testing.T) {
	const min, maxLoss = 3.0, 20.0
	cases := []struct {
		name string
		mbps float64
		loss float64
		want int
	}{
		{"measured fast", 9.5, 0, 0},
		{"measured at threshold", 3.0, 0, 0},
		{"unmeasured", 0, 0, 1},
		{"measured slow", 1.2, 0, 2},
		{"unmeasured but lossy", 0, 30, 3},
		{"measured fast but lossy", 9.5, 40, 3},
	}
	for _, c := range cases {
		if got := cfdataSpeedTier(c.mbps, min, c.loss, maxLoss); got != c.want {
			t.Errorf("%s: tier = %d, want %d", c.name, got, c.want)
		}
	}
}

// 这条钉的是 2026-11-13 实测暴露的问题：cfdata 按延迟排出来的第一名，
// 真跑数据面只有 0.5 Mbps；而延迟最差的那个反而是 9.5 Mbps。
// 台账里有成绩之后，排序必须跟着成绩走，不能再跟着延迟走。
func TestCFdataCandidateIPsPrefersMeasuredThroughput(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "3")
	a := writeCFdataResult(t, cfdataSampleResult)
	a.loadSourceLedger()
	// 172.64.229.135 延迟 72ms（全表最差），但真跑出 9.5 Mbps。
	a.ledgerObserveProbe("172.64.229.135", true, 9.5)

	got, err := a.cfdataCandidateIPs(2)
	if err != nil {
		t.Fatalf("cfdataCandidateIPs: %v", err)
	}
	if len(got) != 2 || got[0] != "172.64.229.135" {
		t.Fatalf("measured-fast IP should rank first, got %v", got)
	}
}

func TestCFdataCandidateIPsBuriesMeasuredSlow(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "3")
	a := writeCFdataResult(t, cfdataSampleResult)
	a.loadSourceLedger()
	// 47.245.131.215 延迟 15ms（全表最快），但真跑只有 0.5 Mbps。
	a.ledgerObserveProbe("47.245.131.215", true, 0.5)

	got, err := a.cfdataCandidateIPs(10)
	if err != nil {
		t.Fatalf("cfdataCandidateIPs: %v", err)
	}
	if len(got) == 0 || got[0] == "47.245.131.215" {
		t.Fatalf("measured-slow IP must not lead the list: %v", got)
	}
	if got[len(got)-1] != "47.245.131.215" {
		t.Fatalf("measured-slow IP should sink to the bottom, got %v", got)
	}
}

// 没有成绩时必须原样退回延迟排序——这是升级前的行为，不能被新逻辑改掉。
func TestCFdataCandidateIPsFallsBackToLatencyWithoutMeasurements(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "3")
	a := writeCFdataResult(t, cfdataSampleResult)
	a.loadSourceLedger()
	got, err := a.cfdataCandidateIPs(2)
	if err != nil {
		t.Fatalf("cfdataCandidateIPs: %v", err)
	}
	if got[0] != "47.245.131.215" || got[1] != "104.16.0.9" {
		t.Fatalf("got %v, want latency order [47.245.131.215 104.16.0.9]", got)
	}
}

func TestCFdataColoThroughputRequiresThreeSamples(t *testing.T) {
	coloByIP := map[string]string{
		"1.1.1.1": "NRT", "1.1.1.2": "NRT", "1.1.1.3": "NRT",
		"2.2.2.1": "SIN", "2.2.2.2": "SIN",
	}
	mbps := map[string]float64{
		"1.1.1.1": 8, "1.1.1.2": 10, "1.1.1.3": 12,
		"2.2.2.1": 1, "2.2.2.2": 1.5,
	}
	got := cfdataColoThroughput(coloByIP, mbps)
	if got["NRT"] != 10 {
		t.Fatalf("NRT median = %v, want 10", got["NRT"])
	}
	if _, ok := got["SIN"]; ok {
		t.Fatal("a colo with fewer than 3 samples must not get a prior")
	}
}

// 三个 IP 延迟、丢包完全相同，只有机房不同。NRT 有三条 9 Mbps 的成绩、
// FRA 有三条 1 Mbps 的成绩、AMS 没有任何成绩。正确次序是
// NRT（已知好）→ AMS（未知）→ FRA（已知差）。
// 这条钉的是「没有先验 ≠ 先验为零」：拿 0 去比会把 AMS 判到 FRA 后面。
func TestCFdataColoPriorOrdersUnknownBetweenGoodAndBad(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "3")
	dir := t.TempDir()
	body := `IP地址,最小延迟(ms),最大延迟(ms),平均延迟(ms),丢包率(%)
1.1.1.1,100,100,100,0
2.2.2.2,100,100,100,0
3.3.3.3,100,100,100,0
`
	if err := os.WriteFile(filepath.Join(dir, cfdataResultFile), []byte(body), 0644); err != nil {
		t.Fatalf("write result.csv: %v", err)
	}
	ipCSV := `IP地址,数据中心,地区,城市,网络延迟
1.1.1.1,NRT,JP,Tokyo,100
2.2.2.2,FRA,DE,Frankfurt,100
3.3.3.3,AMS,NL,Amsterdam,100
9.9.9.1,NRT,JP,Tokyo,100
9.9.9.2,NRT,JP,Tokyo,100
9.9.9.3,NRT,JP,Tokyo,100
8.8.8.1,FRA,DE,Frankfurt,100
8.8.8.2,FRA,DE,Frankfurt,100
8.8.8.3,FRA,DE,Frankfurt,100
`
	if err := os.WriteFile(filepath.Join(dir, "ip.csv"), []byte(ipCSV), 0644); err != nil {
		t.Fatalf("write ip.csv: %v", err)
	}
	a := &app{dataDir: dir}
	a.loadSourceLedger()
	// 9.9.9.x 是 NRT 的三条好线，8.8.8.x 是 FRA 的三条差线。
	for i, ip := range []string{"9.9.9.1", "9.9.9.2", "9.9.9.3"} {
		a.ledgerObserveProbe(ip, true, float64(8+i))
	}
	for i, ip := range []string{"8.8.8.1", "8.8.8.2", "8.8.8.3"} {
		a.ledgerObserveProbe(ip, true, 1+float64(i)*0.1)
	}

	got, err := a.cfdataCandidateIPs(3)
	if err != nil {
		t.Fatalf("cfdataCandidateIPs: %v", err)
	}
	want := []string{"1.1.1.1", "3.3.3.3", "2.2.2.2"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
