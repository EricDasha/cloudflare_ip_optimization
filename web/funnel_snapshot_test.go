package main

import "testing"

// 通过的 IP 只要过得了「公网 + CF 边缘段」这道闸，就必须放行。
// 这是刷新时那道段闸的同一把尺子，与候选名单顺序无关。
func TestFilterPoolAdmissibleKeepsCloudflareEdge(t *testing.T) {
	admitted, rejected := filterPoolAdmissibleIPs([]string{"162.159.131.118", "104.16.224.171"})
	if len(rejected) != 0 {
		t.Fatalf("CF edge IPs rejected: %v", rejected)
	}
	if len(admitted) != 2 {
		t.Fatalf("admitted = %v", admitted)
	}
}

// 第三方 VPS 冒充优选（非 CF 段）必须被剔除。
func TestFilterPoolAdmissibleDropsNonCloudflare(t *testing.T) {
	admitted, rejected := filterPoolAdmissibleIPs([]string{"162.159.131.118", "45.32.100.7"})
	if len(admitted) != 1 || admitted[0] != "162.159.131.118" {
		t.Fatalf("admitted = %v", admitted)
	}
	if len(rejected) != 1 || rejected[0] != "45.32.100.7" {
		t.Fatalf("rejected = %v", rejected)
	}
}

// 私网 / 回环 / 非法字符串一律不进池。
func TestFilterPoolAdmissibleDropsPrivateAndGarbage(t *testing.T) {
	_, rejected := filterPoolAdmissibleIPs([]string{"192.168.1.50", "127.0.0.1", "not-an-ip", ""})
	if len(rejected) != 4 {
		t.Fatalf("rejected = %v, want all 4 dropped", rejected)
	}
}

// 混合场景：多数合格、少量失效时不作废整轮——这是 12:38 那轮被误丢的形态。
func TestFilterPoolAdmissibleKeepsRoundWithFewBadEntries(t *testing.T) {
	passed := []string{"162.159.131.118", "162.159.194.115", "172.64.68.116", "104.16.224.171", "45.32.100.7"}
	admitted, rejected := filterPoolAdmissibleIPs(passed)
	if len(admitted) != 4 || len(rejected) != 1 {
		t.Fatalf("admitted=%v rejected=%v, want 4 kept / 1 dropped", admitted, rejected)
	}
	if len(admitted) < 3 {
		t.Fatal("a predominantly valid round must not be voided by one bad entry")
	}
}
