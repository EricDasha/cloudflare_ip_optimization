package main

import (
	"net"
	"testing"
)

// 「指定 IP 登基」前端用同一个 /api/cfnat/proxy-promote 端点，后端只校验
// isPublicIPv4 —— 不要求该 IP 在候选池内。这里把「任意公网 IPv4 都可登基」
// 的前置条件钉死，防止有人日后误加候选池成员校验而堵死手动通道。
func TestHandleProxyPromoteAcceptsAnyPublicIPv4(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"候选池外的普通公网 IP", "203.0.113.77", true},
		{"Cloudflare 段内但不在池里", "104.16.99.99", true},
		{"私有地址", "192.168.1.50", false},
		{"环回", "127.0.0.1", false},
		{"未指定", "0.0.0.0", false},
		{"组播", "224.0.0.1", false},
		{"非法格式", "not-an-ip", false},
		{"IPv6 不支持（池只收 IPv4）", "2606:4700::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isPublicIPv4(net.ParseIP(tc.ip))
			if got != tc.want {
				t.Fatalf("isPublicIPv4(%q) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}
