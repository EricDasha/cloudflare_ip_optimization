package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

func TestExtractPublicIPv4FromTextAndBase64(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("vless://id@8.8.8.8:443?host=example.com"))
	got := extractPublicIPv4("1.1.1.1 192.168.1.1 invalid\n" + encoded)
	want := []string{"1.1.1.1", "8.8.8.8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractPublicIPv4() = %v, want %v", got, want)
	}
}

func TestThirdPartyProxySourcesAreHTTPSAllowlisted(t *testing.T) {
	for _, id := range []string{"cmliussss-proxyip", "090227"} {
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
	source := proxyCandidateSources["090227"]
	if len(source.Domains) < 10 || len(source.URLs) != 4 {
		t.Fatalf("090227 source was not expanded: %#v", source)
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

func TestProxyCandidateCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := proxyCandidateSnapshot{
		UpdatedAt:   time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		NextRefresh: time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC),
		Sources:     []string{"zhaobo", "william"},
		IPs:         []string{"1.1.1.1", "192.168.1.1"},
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
		{IP: "198.51.100.2", Latency: 200, DataLatency: 300, Stage: "VLESS_PASS"},
		{IP: "198.51.100.3", Latency: 50, DataLatency: 600, Stage: "VLESS_PASS"},
	}
	got := fastestVLESSPasses(results, 2)
	want := []string{"198.51.100.2", "198.51.100.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fastestVLESSPasses() = %v, want %v", got, want)
	}
}
