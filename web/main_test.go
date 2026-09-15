package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIsPublicIPv4(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"192.168.1.1", false},
		{"169.254.1.1", false},
		{"224.0.0.1", false},
		{"::1", false},
	}
	for _, tt := range tests {
		if got := isPublicIPv4(net.ParseIP(tt.ip)); got != tt.want {
			t.Fatalf("isPublicIPv4(%q) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestDefaultCFnatUsesSingleTargetPerConnection(t *testing.T) {
	t.Setenv("CFNAT_NUM", "")
	if got := defaultCFnatConfig().Num; got != 1 {
		t.Fatalf("default CFnat target count = %d, want 1", got)
	}
}

func TestExtractPublicIPv4FromTextAndBase64(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("vless://id@8.8.8.8:443?host=example.com"))
	got := extractPublicIPv4("1.1.1.1 192.168.1.1 invalid\n" + encoded)
	want := []string{"1.1.1.1", "8.8.8.8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractPublicIPv4() = %v, want %v", got, want)
	}
}

func TestThirdPartyProxySourcesAreHTTPSAllowlisted(t *testing.T) {
	for _, id := range []string{"090227"} {
		source, ok := proxyCandidateSources[id]
		if !ok {
			t.Fatalf("missing source %q", id)
		}
		for _, rawURL := range source.URLs {
			if !strings.HasPrefix(rawURL, "https://") {
				t.Fatalf("source %q contains non-HTTPS URL %q", id, rawURL)
			}
		}
	}
	if _, ok := proxyCandidateSources["third-party-subscriptions"]; ok {
		t.Fatal("subscription converter frontends must not be automatic candidate sources")
	}
	// 社区候选源已停用：IP 来源改为「用户输入 + 订阅 + CFdata 筛选推送 + 优选域名」。
	for _, disabled := range []string{"zhaobo", "william", "euorg", "cmliussss-proxyip"} {
		if _, ok := proxyCandidateSources[disabled]; ok {
			t.Fatalf("community candidate source %q must be disabled", disabled)
		}
	}
	source := proxyCandidateSources["090227"]
	if len(source.Domains) < 10 || len(source.URLs) != 4 {
		t.Fatalf("090227 source was not expanded: %#v", source)
	}
}

func TestEnabledPreferredDomainsDefaultsToAllImported(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	a.loadPreferredSettings()
	all := a.importedPreferredDomains()
	enabled := a.enabledPreferredDomains()
	if len(all) == 0 {
		t.Fatal("no imported preferred domains defined")
	}
	if !reflect.DeepEqual(all, enabled) {
		t.Fatalf("default enabled = %d, want all imported %d", len(enabled), len(all))
	}
}

func TestPreferredSettingsPersistEnableSubset(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	a.loadPreferredSettings()
	all := a.importedPreferredDomains()
	a.preferredMu.Lock()
	a.preferredEnabled = map[string]bool{all[0]: true}
	_ = a.savePreferredSettingsLocked()
	a.preferredMu.Unlock()

	b := &app{dataDir: a.dataDir}
	b.loadPreferredSettings()
	enabled := b.enabledPreferredDomains()
	if len(enabled) != 1 || enabled[0] != all[0] {
		t.Fatalf("restored enabled = %v, want only %s", enabled, all[0])
	}
}

func TestSampleIPv4CIDRs(t *testing.T) {
	got := sampleIPv4CIDRs("104.16.0.0/24\n172.64.0.0/24\ninvalid", 12)
	if len(got) != 12 {
		t.Fatalf("sample count = %d, want 12", len(got))
	}
	for _, raw := range got {
		ip := net.ParseIP(raw)
		if ip == nil || !(strings.HasPrefix(raw, "104.16.0.") || strings.HasPrefix(raw, "172.64.0.")) {
			t.Fatalf("sample %q is outside source CIDRs", raw)
		}
	}
}

func TestBundledOfficialCloudflareCIDRsProvideFallback(t *testing.T) {
	got := sampleIPv4CIDRs(bundledOfficialCloudflareIPv4CIDRs, 30)
	if len(got) != 30 {
		t.Fatalf("bundled official candidates = %d, want 30", len(got))
	}
	for _, raw := range got {
		ip := net.ParseIP(raw)
		if !isPublicIPv4(ip) {
			t.Fatalf("bundled official candidate is not public IPv4: %q", raw)
		}
	}
}

func TestProxyCandidateCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := proxyCandidateSnapshot{
		UpdatedAt:   time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		NextRefresh: time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC),
		Sources:     []string{"zhaobo", "william"},
		IPs:         []string{"1.1.1.1", "192.168.1.1"},
		SourceByIP:  map[string]string{"1.1.1.1": "official", "192.168.1.1": "user"},
		Errors:      []string{},
	}
	a := &app{dataDir: dir}
	if err := a.saveProxyCandidateCache(want); err != nil {
		t.Fatalf("saveProxyCandidateCache: %v", err)
	}
	b := &app{dataDir: dir}
	b.loadProxyCandidateCache()
	got := b.proxyCandidateSnapshot()
	if !reflect.DeepEqual(got.IPs, []string{"1.1.1.1"}) {
		t.Fatalf("loaded IPs = %#v, want only public IPv4", got.IPs)
	}
	if !reflect.DeepEqual(got.SourceByIP, map[string]string{"1.1.1.1": "official"}) {
		t.Fatalf("loaded source map = %#v", got.SourceByIP)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) || !got.NextRefresh.Equal(want.NextRefresh) {
		t.Fatalf("loaded timestamps differ: %#v", got)
	}
}

func TestOptimizerSettingRoundTrip(t *testing.T) {
	a := &app{dataDir: t.TempDir(), optimizerEnabled: true}
	if err := a.saveOptimizerSettings(false); err != nil {
		t.Fatalf("save optimizer setting: %v", err)
	}
	a.optimizerEnabled = true
	a.loadOptimizerSettings()
	if a.optimizerEnabled {
		t.Fatal("optimizer setting was not restored")
	}
}

func TestVLESSOutboundTemplateAndProbeConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "template.json")
	templateJSON := `{
  "outbounds": [{
    "type": "vless",
    "tag": "original",
    "server": "192.0.2.1",
    "server_port": 1234,
    "uuid": "11111111-2222-4333-8444-555555555555",
    "tls": {"enabled": true, "server_name": "node.example.com"},
    "transport": {"type": "ws", "path": "/probe?ed=2560", "headers": {"Host": "node.example.com"}}
  }]
}`
	if err := os.WriteFile(path, []byte(templateJSON), 0600); err != nil {
		t.Fatal(err)
	}
	template, err := loadVLESSOutboundTemplate(path)
	if err != nil {
		t.Fatalf("loadVLESSOutboundTemplate: %v", err)
	}
	config, err := buildSingBoxProbeConfig(template, "1.1.1.1", 443, 2080)
	if err != nil {
		t.Fatalf("buildSingBoxProbeConfig: %v", err)
	}
	outbounds := config["outbounds"].([]any)
	outbound := outbounds[0].(map[string]any)
	if outbound["server"] != "1.1.1.1" || outbound["server_port"] != 443 {
		t.Fatalf("candidate endpoint was not applied: %#v", outbound)
	}
	if template["server"] != "192.0.2.1" {
		t.Fatalf("template was mutated: %#v", template)
	}
	encoded, err := json.Marshal(config)
	if err != nil || !strings.Contains(string(encoded), `"final":"vless-probe"`) {
		t.Fatalf("probe config is incomplete: %s, %v", encoded, err)
	}
}

func TestVLESSOutboundTemplateValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(path, []byte(`{"type":"vless","uuid":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVLESSOutboundTemplate(path); err == nil {
		t.Fatal("invalid VLESS template was accepted")
	}
}

func TestSanitizeProbeMessage(t *testing.T) {
	template := map[string]any{
		"uuid": "11111111-2222-4333-8444-555555555555",
		"tls":  map[string]any{"server_name": "node.example.com"},
		"transport": map[string]any{
			"path":    "/secret-path",
			"headers": map[string]any{"Host": "node.example.com"},
		},
	}
	raw := "uuid=11111111-2222-4333-8444-555555555555 host=node.example.com path=/secret-path"
	got := sanitizeProbeMessage(raw, template)
	for _, secret := range []string{"11111111-2222-4333-8444-555555555555", "node.example.com", "/secret-path"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q remained in %q", secret, got)
		}
	}
}

func TestVLESSProbeConfigValidation(t *testing.T) {
	valid := vlessProbeConfig{
		Enabled:        true,
		Binary:         "/usr/local/bin/sing-box",
		TemplatePath:   "/data/vless.json",
		TestURL:        "https://example.com/generate_204",
		ExpectedStatus: 204,
		Timeout:        15 * time.Second,
		MaxCandidates:  20,
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	invalid := valid
	invalid.TestURL = "file:///etc/passwd"
	if err := invalid.validate(); err == nil {
		t.Fatal("non-HTTP VLESS test URL was accepted")
	}
}

func TestConfiguredVLESSProbeTemplate(t *testing.T) {
	path := os.Getenv("TEST_VLESS_TEMPLATE")
	if path == "" {
		t.Skip("TEST_VLESS_TEMPLATE is not set")
	}
	if _, err := loadVLESSOutboundTemplate(path); err != nil {
		t.Fatalf("configured VLESS template is invalid: %v", err)
	}
}

func TestVLESSProbeCandidateLimit(t *testing.T) {
	dir := t.TempDir()
	template := map[string]any{
		"type": "vless",
		"uuid": "11111111-2222-4333-8444-555555555555",
		"tls":  map[string]any{"enabled": true, "server_name": "node.example.com"},
		"transport": map[string]any{
			"type":    "ws",
			"path":    "/probe",
			"headers": map[string]any{"Host": "node.example.com"},
		},
	}
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "WS_PASS"},
		{IP: "1.0.0.1", Stage: "WS_PASS"},
		{IP: "8.8.8.8", Stage: "WS_PASS"},
	}
	cfg := vlessProbeConfig{
		Binary:         filepath.Join(dir, "missing-sing-box"),
		TemplatePath:   filepath.Join(dir, "template.json"),
		TestURL:        "https://example.com/generate_204",
		ExpectedStatus: 204,
		Timeout:        3 * time.Second,
		MaxCandidates:  2,
	}
	passed := probeVLESSPool(context.Background(), results, 443, 3, cfg, template)
	if len(passed) != 0 {
		t.Fatalf("unexpected passes: %#v", passed)
	}
	if results[0].Stage != "VLESS_FAIL" || results[1].Stage != "VLESS_FAIL" {
		t.Fatalf("first two candidates were not attempted: %#v", results)
	}
	if results[2].Stage != "WS_PASS" || results[2].Error != "" {
		t.Fatalf("candidate limit was not enforced: %#v", results[2])
	}
}

func TestVLESSProbeOrderUsesWebSocketLatency(t *testing.T) {
	results := []proxyScanResult{
		{IP: "198.51.100.1", Latency: 300},
		{IP: "198.51.100.2", Latency: 100},
		{IP: "198.51.100.3", Latency: 200, Error: "failed"},
	}
	order := vlessProbeOrder(results)
	if !reflect.DeepEqual(order, []int{1, 0}) {
		t.Fatalf("vlessProbeOrder() = %v, want [1 0]", order)
	}
}

func TestFastestVLESSPassesUsesDataLatency(t *testing.T) {
	results := []proxyScanResult{
		{IP: "198.51.100.1", Latency: 100, DataLatency: 900, Stage: "VLESS_PASS"},
		{IP: "198.51.100.2", Latency: 200, DataLatency: 300, SourceRank: 1, Stage: "VLESS_PASS"},
		{IP: "198.51.100.3", Latency: 50, DataLatency: 600, Stage: "VLESS_PASS"},
	}
	got := fastestVLESSPasses(results, 2)
	want := []string{"198.51.100.3", "198.51.100.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fastestVLESSPasses() = %v, want %v", got, want)
	}
}

func TestCandidateSourcePriorityKeepsFailuresOut(t *testing.T) {
	results := []proxyScanResult{
		{IP: "198.51.100.1", Latency: 20, Stage: "WS_PASS"},
		{IP: "198.51.100.2", Latency: 10, Stage: "WS_PASS"},
		{IP: "198.51.100.3", Latency: 5, Stage: "WS_FAIL", Error: "failed"},
	}
	applyCandidateSourcePriority(results, map[string]string{
		"198.51.100.1": "official",
		"198.51.100.2": "proxy",
		"198.51.100.3": "official",
	})
	want := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"}
	got := []string{results[0].IP, results[1].IP, results[2].IP}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source priority = %v, want %v", got, want)
	}
}

func TestRankVLESSPassesBySpeedUsesSourceThenThroughput(t *testing.T) {
	results := []proxyScanResult{
		{IP: "198.51.100.1", Latency: 100, SourceRank: 0, Stage: "VLESS_PASS"},
		{IP: "198.51.100.2", Latency: 50, SourceRank: 1, Stage: "VLESS_PASS"},
		{IP: "198.51.100.3", Latency: 150, SourceRank: 0, Stage: "VLESS_PASS"},
		{IP: "198.51.100.4", Latency: 25, SourceRank: 0, Stage: "VLESS_PASS"},
	}
	metrics := map[string]vlessProbeMetrics{
		"198.51.100.1": {Duration: 400 * time.Millisecond, Mbps: 12},
		"198.51.100.2": {Duration: 200 * time.Millisecond, Mbps: 50},
		"198.51.100.3": {Duration: 300 * time.Millisecond, Mbps: 30},
	}
	probe := func(_ context.Context, ip string, _ int, _ vlessProbeConfig, _ map[string]any) (vlessProbeMetrics, error) {
		metric, ok := metrics[ip]
		if !ok {
			return vlessProbeMetrics{}, errors.New("download failed")
		}
		return metric, nil
	}

	got := rankVLESSPassesBySpeedWithProbe(
		context.Background(),
		[]string{"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4"},
		results,
		443,
		vlessProbeConfig{Timeout: time.Second},
		nil,
		probe,
	)
	want := []string{"198.51.100.2", "198.51.100.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rankVLESSPassesBySpeedWithProbe() = %v, want %v", got, want)
	}
	if results[2].DownloadMbps != 30 || results[0].DownloadMbps != 12 || results[1].DownloadMbps != 50 {
		t.Fatalf("download metrics were not recorded: %#v", results)
	}
	if results[3].Stage != "VLESS_SPEED_FAIL" || results[3].Error == "" {
		t.Fatalf("failed speed probe remained eligible: %#v", results[3])
	}
}

func TestExtractPreferredDomainsFrom090227Page(t *testing.T) {
	page := `<section class="section"><h2 class="section-title">CM优选域名</h2><div class="domain-cards-grid">
<div class="domain-card"><button class="copy-domain" onclick="copyDomain('youxuan.cf.090227.xyz')">*.cf.090227.xyz</button></div>
<div class="domain-card"><button class="copy-domain" onclick="copyDomain('cf.877774.xyz')">cf.877774.xyz</button></div>
</div></section>
<section><h2 class="section-title">官方优选域名</h2><div class="domain-cards-grid">
<div class="domain-card"><button class="copy-domain" onclick="copyDomain('www.visa.cn')">www.visa.cn</button></div>
<div class="domain-card"><button class="copy-domain" onclick="copyDomain('bad host')">bad host</button></div>
</div></section>`
	got := extractPreferredDomains(page)
	want := []string{"youxuan.cf.090227.xyz", "cf.877774.xyz", "www.visa.cn", "cf.090227.xyz"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractPreferredDomains() = %v, want %v", got, want)
	}
}

func TestExtractPreferredDomainsRejectsInfrastructureNoise(t *testing.T) {
	// 页面里出现的非优选域名外链不应被提取。
	page := `<a href="https://fonts.googleapis.com">fonts</a><span>ipip.net</span>
<button onclick="copyDomain('cf.tencentapp.cn')">cf.tencentapp.cn</button>`
	got := extractPreferredDomains(page)
	if !reflect.DeepEqual(got, []string{"cf.tencentapp.cn"}) {
		t.Fatalf("extractPreferredDomains() = %v, want only copyDomain card", got)
	}
}

func TestEnvForwardDomainsParsesCommaAndWildcard(t *testing.T) {
	t.Setenv("PROXY_DOMAIN_FORWARD", "youxuan.cf.090227.xyz, www.visa.cn, bad host, *.bestcf.030101.xyz")
	got := envForwardDomains("PROXY_DOMAIN_FORWARD")
	want := []string{"youxuan.cf.090227.xyz", "www.visa.cn", "bestcf.030101.xyz"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("envForwardDomains() = %v, want %v", got, want)
	}
}
