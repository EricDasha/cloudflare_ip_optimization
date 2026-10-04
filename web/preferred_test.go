package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func mustIP(t *testing.T, raw string) net.IP {
	t.Helper()
	ip := net.ParseIP(raw)
	if ip == nil {
		t.Fatalf("invalid IP literal %q", raw)
	}
	return ip
}

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

func TestResolvePreferredDomainIPListFiltersAndTruncates(t *testing.T) {
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "a.example.com":
			// 公网 + 私网混合：私网必须被滤除。
			return []net.IP{mustIP(t, "203.0.113.10"), mustIP(t, "192.168.1.5"), mustIP(t, "203.0.113.11")}, nil
		case "b.example.com":
			return nil, errors.New("NXDOMAIN")
		case "c.example.com":
			return nil, nil
		default:
			return []net.IP{mustIP(t, "198.51.100.1")}, nil
		}
	}
	outcomes := resolvePreferredDomainIPList(context.Background(), []string{"a.example.com", "b.example.com", "c.example.com"}, 4, lookup)
	if len(outcomes) != 3 {
		t.Fatalf("outcomes = %d, want 3", len(outcomes))
	}
	if outcomes[0].failed || len(outcomes[0].ips) != 2 {
		t.Fatalf("a.example.com ips = %v failed=%v, want 2 public IPs", outcomes[0].ips, outcomes[0].failed)
	}
	if outcomes[0].ips[0] != "203.0.113.10" || outcomes[0].ips[1] != "203.0.113.11" {
		t.Fatalf("a.example.com ips = %v, want [203.0.113.10 203.0.113.11]", outcomes[0].ips)
	}
	if !outcomes[1].failed {
		t.Fatalf("b.example.com must be marked failed on resolver error")
	}
	if outcomes[2].failed || len(outcomes[2].ips) != 0 {
		t.Fatalf("c.example.com should succeed with zero IPs, got failed=%v ips=%v", outcomes[2].failed, outcomes[2].ips)
	}
}

func TestResolvePreferredDomainIPListPerDomainCap(t *testing.T) {
	lookup := func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{
			mustIP(t, "203.0.113.1"), mustIP(t, "203.0.113.2"),
			mustIP(t, "203.0.113.3"), mustIP(t, "203.0.113.4"),
			mustIP(t, "203.0.113.5"),
		}, nil
	}
	outcomes := resolvePreferredDomainIPList(context.Background(), []string{"a.example.com"}, 4, lookup)
	if len(outcomes) != 1 || outcomes[0].failed {
		t.Fatalf("unexpected outcome: %+v", outcomes)
	}
	if len(outcomes[0].ips) != 4 {
		t.Fatalf("per-domain cap not applied: got %v", outcomes[0].ips)
	}
}

func TestResolvePreferredDomainIPListRespectsParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lookup := func(ctx context.Context, _ string) ([]net.IP, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	outcomes := resolvePreferredDomainIPList(ctx, []string{"a.example.com"}, 4, lookup)
	if len(outcomes) != 1 || !outcomes[0].failed {
		t.Fatalf("cancelled context must yield failed outcome, got %+v", outcomes)
	}
}

func TestResolvePreferredDomainIPsAggregatesAndDedupes(t *testing.T) {
	a := &app{}
	a.preferredEnabled = map[string]bool{
		"bestcf.030101.xyz": true,
		"cf.090227.xyz":     true,
		"unused.example":    false,
	}
	// preferredLimit 默认路径走 envInt，这里直接给状态位不需要文件。
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "bestcf.030101.xyz":
			return []net.IP{mustIP(t, "203.0.113.10"), mustIP(t, "203.0.113.10")}, nil
		case "cf.090227.xyz":
			return []net.IP{mustIP(t, "203.0.113.11")}, nil
		default:
			return nil, errors.New("unexpected host " + host)
		}
	}
	ips, errs := a.resolvePreferredDomainIPs(context.Background(), lookup)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(ips) != 2 || ips[0] != "203.0.113.10" || ips[1] != "203.0.113.11" {
		t.Fatalf("ips = %v, want [203.0.113.10 203.0.113.11]", ips)
	}
	if a.preferredStatus == nil {
		t.Fatalf("preferredStatus map must be initialized by resolve")
	}
	st, ok := a.preferredStatus["bestcf.030101.xyz"]
	if !ok || st.Stage != "RESOLVED" || st.Domain != "bestcf.030101.xyz" {
		t.Fatalf("status for bestcf.030101.xyz = %+v", st)
	}
	if st.IPs == nil || len(st.IPs) != 1 || st.IPs[0] != "203.0.113.10" {
		t.Fatalf("status ips = %v, want [203.0.113.10]", st.IPs)
	}
}

func TestResolvePreferredDomainIPsReportsFailedDomains(t *testing.T) {
	a := &app{}
	a.preferredEnabled = map[string]bool{"bestcf.030101.xyz": true}
	lookup := func(_ context.Context, _ string) ([]net.IP, error) {
		return nil, errors.New("DNS timeout")
	}
	ips, errs := a.resolvePreferredDomainIPs(context.Background(), lookup)
	if len(ips) != 0 {
		t.Fatalf("ips = %v, want empty", ips)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want one entry", errs)
	}
	if a.preferredStatus["bestcf.030101.xyz"].Stage != "RESOLVE_FAIL" {
		t.Fatalf("stage = %+v", a.preferredStatus["bestcf.030101.xyz"])
	}
}
