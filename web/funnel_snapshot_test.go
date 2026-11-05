package main

import "testing"

// 官方段内与官方段外的公网 IPv4 一律放行。
//
// 判据只问「是不是公网 IPv4」，不问「像不像 Cloudflare 自己的地址」。
// 2026-11-05 线上实测：在位池 45 个成员里 30 个不在官方 15 段内，全部在正常
// 转发，延迟与官方段相差不到 30ms。官方段从准入闸降级为标签（用于段多样性
// 分组与展示），能不能进池由端到端考试说了算。
func TestFilterPoolAdmissibleKeepsCloudflareEdge(t *testing.T) {
	admitted, rejected := filterPoolAdmissibleIPs([]string{"162.159.131.118", "104.16.224.171"})
	if len(rejected) != 0 {
		t.Fatalf("CF edge IPs rejected: %v", rejected)
	}
	if len(admitted) != 2 {
		t.Fatalf("admitted = %v", admitted)
	}
}

// 官方段外的公网 IPv4（反代/ProxyIP）不再被剔除——这正是 2026-11-05 改动的
// 目的：它们可以是可用的，判死要交给真实考试，不能交给一张会过期的地址表。
func TestFilterPoolAdmissibleKeepsNonCloudflarePublicIPv4(t *testing.T) {
	admitted, rejected := filterPoolAdmissibleIPs([]string{"162.159.131.118", "45.32.100.7"})
	if len(rejected) != 0 {
		t.Fatalf("non-CF public IPv4 rejected: %v", rejected)
	}
	if len(admitted) != 2 {
		t.Fatalf("admitted = %v", admitted)
	}
}

// 私网 / 回环 / 非法字符串一律不进池。
func TestFilterPoolAdmissibleDropsPrivateAndGarbage(t *testing.T) {
	_, rejected := filterPoolAdmissibleIPs([]string{"192.168.1.50", "127.0.0.1", "not-an-ip", ""})
	if len(rejected) != 4 {
		t.Fatalf("rejected = %v, want all 4 dropped", rejected)
	}
}

// 混合场景：全部合格，一个都不作废。
func TestFilterPoolAdmissibleKeepsRoundWithFewBadEntries(t *testing.T) {
	passed := []string{"162.159.131.118", "162.159.194.115", "172.64.68.116", "104.16.224.171", "45.32.100.7"}
	admitted, rejected := filterPoolAdmissibleIPs(passed)
	if len(admitted) != 5 || len(rejected) != 0 {
		t.Fatalf("admitted=%v rejected=%v, want 5 kept / 0 dropped", admitted, rejected)
	}
}
