package main

import (
	"strings"
	"testing"
)

// 「用户扶持登基」的解析契约钉死：公网 IPv4 与合法域名入池，私网/环回/乱码
// 整份拒绝并指名，重复项去重，注释与 host:port 形态容忍，规模超顶拒绝。
func TestParseEndorsedMembers(t *testing.T) {
	t.Run("公网 IPv4 与优选域名各行入位", func(t *testing.T) {
		got, err := parseEndorsedMembers("104.16.123.96\nbest.example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.IPs) != 1 || got.IPs[0] != "104.16.123.96" {
			t.Fatalf("IPs = %v", got.IPs)
		}
		if len(got.Domains) != 1 || got.Domains[0] != "best.example.com" {
			t.Fatalf("Domains = %v", got.Domains)
		}
	})
	t.Run("域名小写化、去尾点", func(t *testing.T) {
		got, err := parseEndorsedMembers("Best.Example.COM.")
		if err != nil || len(got.Domains) != 1 || got.Domains[0] != "best.example.com" {
			t.Fatalf("got %+v err %v", got, err)
		}
	})
	t.Run("重复项去重", func(t *testing.T) {
		got, err := parseEndorsedMembers("104.16.123.96\n104.16.123.96\nbest.example.com\nbest.example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.IPs) != 1 || len(got.Domains) != 1 {
			t.Fatalf("dedupe failed: %+v", got)
		}
	})
	t.Run("注释与空行容忍", func(t *testing.T) {
		got, err := parseEndorsedMembers("# 今晚用这些\n\n104.16.123.96 # 香港\n")
		if err != nil || len(got.IPs) != 1 {
			t.Fatalf("got %+v err %v", got, err)
		}
	})
	t.Run("host:port 只取主机", func(t *testing.T) {
		got, err := parseEndorsedMembers("104.16.123.96:443")
		if err != nil || len(got.IPs) != 1 || got.IPs[0] != "104.16.123.96" {
			t.Fatalf("got %+v err %v", got, err)
		}
	})
	rejects := []struct {
		name string
		text string
	}{
		{"私网地址", "192.168.1.50"},
		{"环回", "127.0.0.1"},
		{"组播", "224.0.0.1"},
		{"非法格式", "not-an-ip"},
		{"无点裸词", "localhost"},
		{"只有注释", "# 空的"},
		{"空白", "   \n  "},
	}
	for _, tc := range rejects {
		t.Run("拒绝："+tc.name, func(t *testing.T) {
			if _, err := parseEndorsedMembers(tc.text); err == nil {
				t.Fatalf("expected rejection for %q", tc.text)
			}
		})
	}
	t.Run("坏条目整份拒绝并指名", func(t *testing.T) {
		_, err := parseEndorsedMembers("104.16.123.96\n10.0.0.1")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "10.0.0.1") {
			t.Fatalf("error should name the bad entry: %v", err)
		}
	})
}