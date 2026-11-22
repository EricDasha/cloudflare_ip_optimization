//go:build ignore

// cfdata.go 的单元测试，用 `go test cfdata.go cfdata_test.go` 显式运行。
package main

import (
	"net"
	"testing"
)

func TestSafeDCFilename(t *testing.T) {
	cases := []struct {
		dc   string
		want string
	}{
		{"HKG", "HKG.csv"},
		{"../../x", "______x.csv"},
		{"result", "result.csv"},
		{"a/b", "a_b.csv"},
		{"", "result.csv"},
		{"SJC-1_x", "SJC-1_x.csv"},
	}
	for _, c := range cases {
		if got := safeDCFilename(c.dc); got != c.want {
			t.Errorf("safeDCFilename(%q) = %q, want %q", c.dc, got, c.want)
		}
	}
}

func TestGetRandomIPv4sRejectsMalformed(t *testing.T) {
	got := getRandomIPv4s([]string{"1.2.3.4.5/24", "a.b.c.d/24", "1.2.3.4/16", "1.2.3.0/24"})
	if len(got) != 1 {
		t.Fatalf("got %v, want exactly 1 valid address", got)
	}
	if got[0] == "1.2.3.0" || got[0] == "1.2.3.255" {
		t.Fatalf("got network/broadcast address %q", got[0])
	}
}

func TestGetRandomIPv6sHandlesCompressed(t *testing.T) {
	for _, cidr := range []string{"2400::/48", "2606:4700::/48"} {
		got := getRandomIPv6s([]string{cidr})
		if len(got) != 1 {
			t.Fatalf("%s: got %v, want exactly 1 address", cidr, got)
		}
		if ip := net.ParseIP(got[0]); ip == nil {
			t.Fatalf("%s: got %q, not a valid IPv6", cidr, got[0])
		}
	}
	if got := getRandomIPv6s([]string{"not-an-ip", "2001:db8::/32"}); len(got) != 0 {
		t.Fatalf("invalid input: got %v, want none", got)
	}
}
