package main

import (
	"net"
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
