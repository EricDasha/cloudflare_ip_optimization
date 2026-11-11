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
	"sync"
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

// cfnat 的真实约束是「一次前端逻辑连接只产生一个真实上游会话」，
// 由 cfnat_test.go 的 UsesOneUpstreamPerSession / FallsBackOnlyAfterDialFailure 守住。
// 这里的 -num 只是拨号失败后的顺序回退深度：调大它不增加并发，
// 只把「一次 2s 超时就断连」变成「最多顺延试几个目标」。
// 历史教训：曾把它钉成 1，池里混入高失效率成员后，每 4 次轮转有 3 次撞死，
// 单次超时就直接丢连接，用户侧表现为整页卡死。
func TestDefaultCFnatFallbackDepthIsBounded(t *testing.T) {
	t.Setenv("CFNAT_NUM", "")
	cfg := defaultCFnatConfig()
	if cfg.Num != defaultCFnatNum {
		t.Fatalf("default fallback depth = %d, want %d", cfg.Num, defaultCFnatNum)
	}
	if defaultCFnatNum < 2 {
		t.Fatalf("default fallback depth = %d, want >= 2 so a single dead target cannot drop the connection", defaultCFnatNum)
	}
	if defaultCFnatNum > maxCFnatNum {
		t.Fatalf("default fallback depth %d exceeds bound %d", defaultCFnatNum, maxCFnatNum)
	}
}

func TestNormalizeCFnatClampsFallbackDepth(t *testing.T) {
	t.Setenv("CFNAT_NUM", "")
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultCFnatNum},
		{1, 1}, // 显式指定仍尊重运营者选择
		{2, 2},
		{maxCFnatNum, maxCFnatNum},
		{maxCFnatNum + 1, defaultCFnatNum},
		{-5, defaultCFnatNum},
	}
	for _, c := range cases {
		if got := normalizeCFnat(cfnatConfig{Num: c.in}).Num; got != c.want {
			t.Fatalf("normalizeCFnat(Num=%d).Num = %d, want %d", c.in, got, c.want)
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
	// 090227 已于 2026-10-22 并入 bestcf 层，其 URL 由
	// TestBestcfSourceRegisteredAndEnabled 的白名单统一覆盖。
	// 这里改为遍历全部候选源，保证没有任何一处走明文 HTTP。
	for id, source := range proxyCandidateSources {
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
	// 090227 于 2026-10-22 并入 bestcf 层，但同一天实测发现该域已被
	// Cloudflare 封（error 1034），4 个 API 与 3 条 cf./youxuan./123. 域名
	// 全部作废，只保留两个实测可用的真实站点域名。
	bestcf := proxyCandidateSources["bestcf"]
	for _, rawURL := range bestcf.URLs {
		if strings.Contains(rawURL, "090227.xyz") {
			t.Fatalf("dead 090227 API must not be a candidate source: %s", rawURL)
		}
	}
	if len(bestcfExtraForwardDomains) < 2 {
		t.Fatalf("expected >=2 verified 090227 forward domains, got %d", len(bestcfExtraForwardDomains))
	}
	for _, domain := range bestcfExtraForwardDomains {
		if strings.Contains(domain, "090227.xyz") {
			t.Fatalf("dead 090227 domain must not be forwarded: %s", domain)
		}
	}
}

func TestSameIPSetIgnoresOrder(t *testing.T) {
	if !sameIPSet([]string{"1.1.1.1", "8.8.8.8"}, []string{"8.8.8.8", "1.1.1.1"}) {
		t.Fatal("same IPs in different order were treated as changed")
	}
	if sameIPSet([]string{"1.1.1.1"}, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatal("different pools were treated as same")
	}
}

func TestCandidateSourcePriorityUsesDeclaredOrder(t *testing.T) {
	// 供给层：user > cfdata > subscription > bestcf。
	// preferred(域名直连 fixed) 与 official(官方段) 已砍，未知来源落 rank 末尾。
	results := []proxyScanResult{
		{IP: "203.0.113.6", Latency: 1},
		{IP: "203.0.113.1", Latency: 6},
		{IP: "203.0.113.4", Latency: 4},
		{IP: "203.0.113.2", Latency: 2},
		{IP: "203.0.113.5", Latency: 5},
		{IP: "203.0.113.3", Latency: 3},
	}
	applyCandidateSourcePriority(results, map[string]string{
		"203.0.113.1": "user",
		"203.0.113.2": "subscription",
		"203.0.113.3": "preferred",
		"203.0.113.4": "cfdata",
		"203.0.113.5": "bestcf",
		"203.0.113.6": "official",
	})
	got := make([]string, len(results))
	for i, result := range results {
		got[i] = result.IP
	}
	// user(0) → cfdata(1) → subscription(2) → bestcf(3) → 未知/已砍来源按 rank 末尾、层内延迟序
	want := []string{"203.0.113.1", "203.0.113.4", "203.0.113.2", "203.0.113.5", "203.0.113.6", "203.0.113.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate source order = %v, want %v", got, want)
	}
}

func TestReplacePoolMemberPreservesOtherActiveIPs(t *testing.T) {
	got := replacePoolMember([]string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}, "2.2.2.2", "8.8.8.8")
	want := []string{"1.1.1.1", "8.8.8.8", "3.3.3.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replaced pool = %v, want %v", got, want)
	}
	got = replacePoolMember([]string{"1.1.1.1", "2.2.2.2"}, "missing", "8.8.8.8")
	if !reflect.DeepEqual(got, []string{"8.8.8.8", "2.2.2.2"}) {
		t.Fatalf("missing source replacement = %v", got)
	}
}

func TestMergePoolResultsKeepsExistingMembers(t *testing.T) {
	got := mergePoolResults(
		[]string{"1.1.1.1", "8.8.8.8"},
		[]proxyScanResult{{IP: "1.1.1.1", Latency: 100}},
		[]proxyScanResult{{IP: "8.8.8.8", Latency: 20}},
	)
	if len(got) != 2 || got[0].IP != "1.1.1.1" || got[0].Latency != 100 || got[1].IP != "8.8.8.8" {
		t.Fatalf("merged pool results = %#v", got)
	}
}

func TestActivePoolHealthRequiresFreshCheck(t *testing.T) {
	a := &app{activeHealth: map[string]*activeHealthStat{
		"203.0.113.1": {Success: 1, LastCheck: time.Now().Add(-2 * activeHealthWindow())},
	}}
	if a.activePoolHealthy([]string{"203.0.113.1"}) {
		t.Fatal("stale active health record blocked a pool switch")
	}
	now := time.Now()
	a.activeHealth["203.0.113.1"] = &activeHealthStat{Success: 1, LastCheck: now}
	if !a.activePoolHealthy([]string{"203.0.113.1"}) {
		t.Fatal("fresh successful active health record was not accepted")
	}
}

// 残员豁免：池缩到 MinPool 之下时冷却不再拦补员，且全员健康也不拦——
// 生产教训：连踢 4 次池剩 1 员，后续两次通过终审的立即优选被冷却闸静默吞单。
func TestAllowPoolSwitchWaivesUnderstaffedPool(t *testing.T) {
	now := time.Now()
	a := &app{
		dataDir:      t.TempDir(),
		activeHealth: map[string]*activeHealthStat{},
		poolGuardMu:  sync.Mutex{},
	}
	a.activePool = proxyActivePool{UpdatedAt: now.Add(-time.Minute), Host: "h", Path: "/p", IPs: []string{"10.0.0.1"}}
	a.markPoolSwitched(now) // 冷却进行中
	// 残员（1 < MinPool=3）:放行
	if !a.allowPoolSwitch(now.Add(time.Minute), "test-understaffed").Allowed {
		t.Fatal("understaffed pool (1 < MinPool) was blocked by cooldown")
	}
	// 整池（≥ MinPool）:同条件必须拦——防抖闸本体仍在
	a.activePool = proxyActivePool{UpdatedAt: now.Add(-time.Minute), Host: "h", Path: "/p", IPs: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}}
	if a.allowPoolSwitch(now.Add(time.Minute), "test-fullpool").Allowed {
		t.Fatal("full healthy-cooldown pool passed a switch it should block")
	}
}

// 残员豁免不绕健康闸的语义反例：整池且健康 → 健康闸仍拦（冷却过后）。
func TestAllowPoolSwitchHealthGateStillBlocksHealthyFullPool(t *testing.T) {
	now := time.Now()
	a := &app{
		dataDir:      t.TempDir(),
		activeHealth: map[string]*activeHealthStat{},
	}
	for _, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		a.activeHealth[ip] = &activeHealthStat{Success: 5, LastCheck: now}
	}
	a.activePool = proxyActivePool{UpdatedAt: now.Add(-time.Hour), Host: "h", Path: "/p", IPs: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}}
	a.lastPoolSwitchAt = now.Add(-time.Hour) // 冷却已过
	if a.allowPoolSwitch(now, "test-healthgate").Allowed {
		t.Fatal("all-healthy full pool was not blocked by health gate")
	}
}

// 救援快车道：整池每个成员都达到连续失败阈值 → 跳过冷却与健康闸。
// 冷却仍在进行、池子也不「健康」，但此时不换就是断网，防抖必须让路。
func TestAllowPoolSwitchRescueBypassesCooldownAndHealth(t *testing.T) {
	now := time.Now()
	a := &app{
		dataDir:      t.TempDir(),
		activeHealth: map[string]*activeHealthStat{},
		throneProbe:  map[string]*throneProbeStat{},
	}
	ips := []string{"104.16.0.1", "104.17.0.1", "104.18.0.1"}
	a.activePool = proxyActivePool{UpdatedAt: now.Add(-time.Minute), Host: "h", Path: "/p", IPs: ips}
	a.markPoolSwitched(now) // 冷却进行中
	threshold := throneProbeFailThreshold()
	for _, ip := range ips {
		a.throneProbe[ip] = &throneProbeStat{ConsecutiveFailures: threshold, LastProbe: now}
	}
	verdict := a.allowPoolSwitch(now.Add(time.Minute), "test-rescue")
	if !verdict.Allowed || !verdict.Rescue {
		t.Fatalf("all-dead pool was not rescued: %+v", verdict)
	}
	if verdict.Reason != "rescue" {
		t.Fatalf("rescue reason = %q, want rescue", verdict.Reason)
	}
}

// 救援不误伤：只要还有成员没到阈值，防抖闸照常生效。
func TestAllowPoolSwitchRescueRequiresEveryMemberDead(t *testing.T) {
	now := time.Now()
	a := &app{
		dataDir:      t.TempDir(),
		activeHealth: map[string]*activeHealthStat{},
		throneProbe:  map[string]*throneProbeStat{},
	}
	ips := []string{"104.16.0.1", "104.17.0.1", "104.18.0.1"}
	a.activePool = proxyActivePool{UpdatedAt: now.Add(-time.Minute), Host: "h", Path: "/p", IPs: ips}
	a.markPoolSwitched(now) // 冷却进行中
	threshold := throneProbeFailThreshold()
	// 两个死、一个还活着（计数未达阈值）
	a.throneProbe[ips[0]] = &throneProbeStat{ConsecutiveFailures: threshold, LastProbe: now}
	a.throneProbe[ips[1]] = &throneProbeStat{ConsecutiveFailures: threshold, LastProbe: now}
	a.throneProbe[ips[2]] = &throneProbeStat{ConsecutiveFailures: threshold - 1, LastProbe: now}
	verdict := a.allowPoolSwitch(now.Add(time.Minute), "test-partial")
	if verdict.Allowed {
		t.Fatal("partially dead pool must still obey the cooldown gate")
	}
	if verdict.Reason != "cooldown" {
		t.Fatalf("blocked reason = %q, want cooldown", verdict.Reason)
	}
	if verdict.NextAllowedAt.IsZero() {
		t.Fatal("cooldown verdict must carry nextAllowedAt")
	}
}

// 无验活记录的空池不算「整组已死」：没有成员就没有可救援对象。
func TestAllowPoolSwitchEmptyPoolIsNotRescue(t *testing.T) {
	now := time.Now()
	a := &app{dataDir: t.TempDir(), activeHealth: map[string]*activeHealthStat{}, throneProbe: map[string]*throneProbeStat{}}
	a.activePool = proxyActivePool{Host: "h", Path: "/p"}
	verdict := a.allowPoolSwitch(now, "test-empty")
	if verdict.Rescue {
		t.Fatal("empty pool must not be treated as an all-dead rescue")
	}
}

// 拦截原因可读：被拦时 blocked=true 且带来源与时间；换池成功后自动清空。
func TestPoolBlockStatusTracksAndClears(t *testing.T) {
	now := time.Now()
	a := &app{dataDir: t.TempDir()}
	a.recordPoolBlock(poolSwitchVerdict{Reason: "cooldown", NextAllowedAt: now.Add(30 * time.Minute)}, "scheduler", now)
	got := a.poolBlockStatus()
	if got["blocked"] != true || got["reason"] != "cooldown" || got["source"] != "scheduler" {
		t.Fatalf("poolBlockStatus() = %v", got)
	}
	if got["nextAllowedAt"] == nil {
		t.Fatal("blocked status must expose nextAllowedAt")
	}
	a.markPoolSwitched(now)
	if a.poolBlockStatus()["blocked"] != false {
		t.Fatal("poolBlock must clear after a successful switch")
	}
}

func TestSubscriptionMaintainersDefaultList(t *testing.T) {
	t.Setenv("PROXY_SUBSCRIPTION_MAINTAINERS", "")
	got := subscriptionMaintainers()
	for _, want := range []string{"owo.o00o.ooo", "cm.soso.edu.kg", "zrf.zrf.me"} {
		found := false
		for _, host := range got {
			if host == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("default maintainers missing %q: %v", want, got)
		}
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
	// 改动 B：VLESS 失败必须归类，否则降级逻辑无从区分超时与快速失败。
	for _, r := range results[:2] {
		if r.ErrorClass == "" {
			t.Fatalf("VLESS_FAIL %s 缺少 ErrorClass 归类: %#v", r.IP, r)
		}
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
		"198.51.100.1": "bestcf",
		"198.51.100.2": "official",
		"198.51.100.3": "bestcf",
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
	// 软门槛：204 已通过的 IP 全部保留（测速失败不再一票否决），
	// 测速成功的按吞吐降序排在前，测速失败/未测的按来源与延迟排在后面。
	want := []string{"198.51.100.2", "198.51.100.3", "198.51.100.1", "198.51.100.4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rankVLESSPassesBySpeedWithProbe() = %v, want %v", got, want)
	}
	if results[2].DownloadMbps != 30 || results[0].DownloadMbps != 12 || results[1].DownloadMbps != 50 {
		t.Fatalf("download metrics were not recorded: %#v", results)
	}
	if results[3].Stage != "VLESS_PASS" || results[3].Error != "" || results[3].Note == "" {
		t.Fatalf("soft-gated speed failure should stay eligible with a note: %#v", results[3])
	}
}

func TestRankVLESSPassesKeepsBelowThresholdIPsEligible(t *testing.T) {
	t.Setenv("PROXY_VLESS_MIN_DOWNLOAD_MBPS", "100")
	results := []proxyScanResult{
		{IP: "198.51.100.1", Latency: 100, SourceRank: 0, Stage: "VLESS_PASS"},
		{IP: "198.51.100.2", Latency: 50, SourceRank: 1, Stage: "VLESS_PASS"},
	}
	probe := func(_ context.Context, ip string, _ int, _ vlessProbeConfig, _ map[string]any) (vlessProbeMetrics, error) {
		return vlessProbeMetrics{Duration: 500 * time.Millisecond, Mbps: 5}, nil
	}
	got := rankVLESSPassesBySpeedWithProbe(
		context.Background(),
		[]string{"198.51.100.1", "198.51.100.2"},
		results,
		443,
		vlessProbeConfig{Timeout: time.Second},
		nil,
		probe,
	)
	if len(got) != 2 {
		t.Fatalf("below-threshold IPs must stay eligible, got %v", got)
	}
	for _, r := range results {
		if r.Stage != "VLESS_PASS" || r.Error != "" || r.Note == "" {
			t.Fatalf("expected soft-gated pass with note, got %#v", r)
		}
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

// normalize 越界值一律回落默认值，不再透传到 cfnat/cfdata 命令行。
func TestNormalizeClampsOutOfRange(t *testing.T) {
	c := normalizeCFnat(cfnatConfig{Port: -1, Code: 99999, Delay: -5, Task: -5, IPNum: -2})
	d := defaultCFnatConfig()
	if c.Port != d.Port || c.Code != d.Code || c.Delay != d.Delay || c.Task != d.Task || c.IPNum != d.IPNum {
		t.Fatalf("normalizeCFnat out-of-range not clamped: %+v", c)
	}
	if got := normalizeCFnat(cfnatConfig{Port: 8443, Code: 200, Delay: 100, Task: 50, IPNum: 5}); got.Port != 8443 || got.Code != 200 || got.Delay != 100 || got.Task != 50 || got.IPNum != 5 {
		t.Fatalf("normalizeCFnat clamped valid values: %+v", got)
	}
	f := normalizeCFdata(cfdataConfig{Scan: -1, Test: 0, Port: 70000, Delay: -1})
	fd := defaultCFdataConfig()
	if f.Scan != fd.Scan || f.Test != fd.Test || f.Port != fd.Port || f.Delay != fd.Delay {
		t.Fatalf("normalizeCFdata out-of-range not clamped: %+v", f)
	}
}
