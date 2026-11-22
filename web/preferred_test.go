package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestMergeCfdataIPsIntoCandidatesNoOrphanSource(t *testing.T) {
	a := &app{}
	a.candidates = proxyCandidateSnapshot{
		IPs:        []string{"1.1.1.1"},
		SourceByIP: map[string]string{"1.1.1.1": "user"},
	}
	a.dataDir = t.TempDir()
	// 塞满到上限再推 3 个：都不应进 IPs，更不应只进 SourceByIP。
	fill := make([]string, 0, candidatePoolTotalCap)
	for i := 0; i < candidatePoolTotalCap-1; i++ {
		fill = append(fill, fmt.Sprintf("10.0.%d.%d", i/256, i%256+1))
	}
	a.mergeCfdataIPsIntoCandidates(fill)
	if len(a.candidates.IPs) != candidatePoolTotalCap {
		t.Fatalf("after fill IPs = %d, want %d", len(a.candidates.IPs), candidatePoolTotalCap)
	}
	a.mergeCfdataIPsIntoCandidates([]string{"8.8.8.8", "8.8.4.4", "1.0.0.1"})
	if len(a.candidates.IPs) != candidatePoolTotalCap {
		t.Fatalf("IPs grew past cap: %d", len(a.candidates.IPs))
	}
	for _, ip := range []string{"8.8.8.8", "8.8.4.4", "1.0.0.1"} {
		if _, ok := a.candidates.SourceByIP[ip]; ok {
			t.Fatalf("orphan SourceByIP entry for %s (not in IPs)", ip)
		}
	}
}

// POST 里带未导入域名：内存不得比落盘多，否则重启后悄悄丢失。
func TestHandlePreferredDomainsPostDropsUnimported(t *testing.T) {
	a := &app{dataDir: t.TempDir(), preferredEnabled: map[string]bool{}, preferredLimit: 20,
		// 后台刷新 goroutine 会写 sourceHealth：生产 main() 里已初始化，
		// 测试里不初始化会在 goroutine 里 panic（零值 app 的 nil map）。
		sourceHealth: make(map[string]*sourceHealthStat)}
	body := strings.NewReader(`{"enabled":["www.shopify.com","notimported.example.com"],"limit":20}`)
	req := httptest.NewRequest(http.MethodPost, "/api/preferred", body)
	rec := httptest.NewRecorder()
	a.handlePreferredDomains(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if a.preferredEnabled["notimported.example.com"] {
		t.Fatalf("unimported domain kept in memory but never saved to disk")
	}
	if !a.preferredEnabled["www.shopify.com"] {
		t.Fatalf("imported domain not enabled")
	}
	data, err := os.ReadFile(filepath.Join(a.dataDir, preferredSettingsFile))
	if err != nil {
		t.Fatalf("read saved settings: %v", err)
	}
	if strings.Contains(string(data), "notimported") {
		t.Fatalf("saved settings contain unimported domain: %s", data)
	}
}
