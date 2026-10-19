package main

import (
	"net"
	"testing"
)

func TestIsCloudflareIPv4AcceptsEdgeRanges(t *testing.T) {
	// 每段取一个段内地址，覆盖首段、中段、尾段。
	inside := []string{
		"173.245.48.1",   // 173.245.48.0/20
		"103.21.244.5",   // 103.21.244.0/22
		"141.101.64.9",   // 141.101.64.0/18
		"162.158.0.1",    // 162.158.0.0/15
		"162.159.198.1",  // 162.158.0.0/15（实际优选域名常用段）
		"104.16.0.1",     // 104.16.0.0/13
		"104.24.0.1",     // 104.24.0.0/14
		"172.64.0.1",     // 172.64.0.0/13
		"172.67.186.20",  // 172.64.0.0/13（生产实测出现的地址）
		"198.41.128.1",   // 198.41.128.0/17
		"131.0.72.1",     // 131.0.72.0/22
	}
	for _, raw := range inside {
		if !isCloudflareIPv4(net.ParseIP(raw)) {
			t.Errorf("isCloudflareIPv4(%s) = false, want true", raw)
		}
	}
}

func TestIsCloudflareIPv4RejectsNonEdge(t *testing.T) {
	// 这些正是 bestcf 里 lzj/cmliu 给出的「优选 IP」——第三方 VPS，非 CF 边缘。
	// 阿里云德国 / DigitalOcean / OVH / M247，以及私网与 IPv6。
	outside := []string{
		"43.161.228.229",
		"47.245.131.215",
		"8.209.104.124",
		"178.17.49.243",
		"46.101.121.62",
		"54.37.96.177",
		"209.146.115.81",
		"188.137.248.120",
		"1.1.1.1",
		"192.168.1.11",
		"10.0.0.1",
	}
	for _, raw := range outside {
		if isCloudflareIPv4(net.ParseIP(raw)) {
			t.Errorf("isCloudflareIPv4(%s) = true, want false", raw)
		}
	}
	if isCloudflareIPv4(nil) {
		t.Error("isCloudflareIPv4(nil) = true, want false")
	}
	if isCloudflareIPv4(net.ParseIP("2606:4700::1111")) {
		t.Error("isCloudflareIPv4(IPv6) = true, want false")
	}
}

func TestCloudflareCIDRCount(t *testing.T) {
	// 官方 IPv4 段共 15 条；数量变化意味着有人在改动准入闸，需人工复核。
	if len(cloudflareIPv4Nets) != 15 {
		t.Fatalf("cloudflare IPv4 nets = %d, want 15", len(cloudflareIPv4Nets))
	}
}
