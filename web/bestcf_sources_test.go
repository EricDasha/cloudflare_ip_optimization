package main

import (
	"strings"
	"testing"
)

// --- bestcf `域名:端口#备注` / `IP:端口#备注` 双格式 ---

func TestExtractPublicIPv4ParsesBestcfIPPortFormat(t *testing.T) {
	body := "162.159.198.1:8443#▼ 精选\n104.16.1.1:443#官方\n坏行\n"
	got := extractPublicIPv4(body)
	want := []string{"162.159.198.1", "104.16.1.1"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("extractPublicIPv4(bestcf) = %v, want %v", got, want)
	}
}

func TestExtractForwardDomainListParsesBestcfDomainFeed(t *testing.T) {
	body := strings.Join([]string{
		"162.159.198.1:443#官方入口 | MASQUE", // IP 行必须被排除
		"www.decathlon.com:443#企业域名 | 迪卡侬",
		"saas.sin.fan:443#大佬维护 | MIYU",
		"skk.moe:443#大佬维护 | Sukka",
		"www.decathlon.com:443#重复行",
		"裸域名例: nota", // 无端口的裸域名不支持（防误收），应被排除
	}, "\n")
	got := extractForwardDomainList(body)
	want := []string{"www.decathlon.com", "saas.sin.fan", "skk.moe"}
	if len(got) != len(want) {
		t.Fatalf("extractForwardDomainList() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("extractForwardDomainList()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractForwardDomainListCapsAndDedups(t *testing.T) {
	var lines []string
	for i := range maxForwardDomains + 20 {
		lines = append(lines, string(rune('a'+i%26))+"host"+strings.Repeat("x", i%3)+".example.com:443#n"+string(rune('0'+i%10)))
	}
	got := extractForwardDomainList(strings.Join(lines, "\n"))
	if len(got) > maxForwardDomains {
		t.Fatalf("forward domain list exceeded cap: %d > %d", len(got), maxForwardDomains)
	}
	seen := map[string]struct{}{}
	for _, domain := range got {
		if _, ok := seen[domain]; ok {
			t.Fatalf("duplicate domain in list: %s", domain)
		}
		seen[domain] = struct{}{}
	}
}

// --- 池域名成员合并 ---

func TestPoolDomainMembersMergesEnvSnapshotAndDedups(t *testing.T) {
	t.Setenv("PROXY_DOMAIN_FORWARD", "explicit.one.example.com,explicit.two.example.com")
	a := &app{}
	a.candidateMu.Lock()
	a.candidates = proxyCandidateSnapshot{
		ForwardDomains: []string{"bestcf.one.example.com", "explicit.one.example.com", "not_a_domain!"},
	}
	a.candidateMu.Unlock()

	got := a.poolDomainMembers()
	if len(got) != 3 {
		t.Fatalf("poolDomainMembers() = %v, want 3 entries", got)
	}
	if got[0] != "explicit.one.example.com" || got[1] != "explicit.two.example.com" || got[2] != "bestcf.one.example.com" {
		t.Fatalf("poolDomainMembers() order/merge wrong: %v", got)
	}
}

// --- bestcf 源白名单与默认启用 ---

func TestBestcfSourceRegisteredAndEnabled(t *testing.T) {
	source, ok := proxyCandidateSources["bestcf"]
	if !ok {
		t.Fatal("bestcf source not registered")
	}
	if len(source.URLs) == 0 {
		t.Fatal("bestcf source has no URLs")
	}
	enabled := false
	for _, id := range defaultProxyCandidateSourceIDs {
		if id == "bestcf" {
			enabled = true
		}
	}
	if !enabled {
		t.Fatal("bestcf source not in defaultProxyCandidateSourceIDs")
	}
	for _, rawURL := range source.URLs {
		if !strings.HasPrefix(rawURL, "https://bestcf.pages.dev/") {
			t.Fatalf("bestcf URL outside allowlist origin: %s", rawURL)
		}
	}
}
