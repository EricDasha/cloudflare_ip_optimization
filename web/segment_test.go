package main

import (
	"net"
	"reflect"
	"testing"
)

// 单段占满时必须让位给其他段：2026-10-22 事故就是池里只有一个段，一挂全灭。
func TestSpreadBySegmentCapsPerSegment(t *testing.T) {
	ips := []string{
		"104.16.0.1", "104.16.0.2", "104.16.0.3", "104.16.0.4",
		"172.64.0.1",
	}
	got := spreadBySegment(ips, 4)
	// 轮转取：104 段与 172 段交替，104 段凭数量优势拿到剩余名额；
	// 取完按原序列位置排回，所以延迟序仍然成立。
	want := []string{"104.16.0.1", "104.16.0.2", "104.16.0.3", "172.64.0.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("spreadBySegment() = %v, want %v", got, want)
	}
}

// 段足够多时应完全按配额分散，不被单段挤占。
func TestSpreadBySegmentDiversifies(t *testing.T) {
	ips := []string{
		"104.16.0.1", "104.16.0.2", "104.16.0.3",
		"172.64.0.1", "172.64.0.2",
		"162.158.0.1",
		"108.162.192.1",
	}
	got := spreadBySegment(ips, 4)
	segs := poolSegmentStats(got)
	if len(segs) < 3 {
		t.Fatalf("4 个池成员应至少覆盖 3 个段，实际 %v", segs)
	}
	for seg, n := range segs {
		if n > 2 {
			t.Fatalf("段 %s 占用 %d 个，超过配额 2", seg, n)
		}
	}
}

// 候选全在一个段时只能全收——宁可同段也不能留空位。
func TestSpreadBySegmentAllowsSameSegmentWhenNoAlternative(t *testing.T) {
	ips := []string{"104.16.0.1", "104.16.0.2", "104.16.0.3"}
	got := spreadBySegment(ips, 3)
	if len(got) != 3 {
		t.Fatalf("同段候选应全部保留，got %v", got)
	}
}

// poolSize 小于候选数时要截断，且不超过上限。
func TestSpreadBySegmentTruncatesToPoolSize(t *testing.T) {
	ips := []string{"104.16.0.1", "172.64.0.1", "162.158.0.1", "108.162.192.1"}
	got := spreadBySegment(ips, 2)
	if len(got) != 2 {
		t.Fatalf("应截断到 2 个，got %v", got)
	}
}

// 单元素与空输入必须安全。
func TestSpreadBySegmentEdgeCases(t *testing.T) {
	if got := spreadBySegment(nil, 3); len(got) != 0 {
		t.Fatalf("空输入应返回空，got %v", got)
	}
	if got := spreadBySegment([]string{"104.16.0.1"}, 3); !reflect.DeepEqual(got, []string{"104.16.0.1"}) {
		t.Fatalf("单元素应原样返回，got %v", got)
	}
}

// 段识别必须命中官方 15 段；官方段外的公网地址按 /16 归组；
// 非公网地址（私网、回环）返回空串。
//
// 官方段外的公网地址必须归组，不能返回空串：官方段降级为标签之后，反代 IP
// 也能进池，若它们在这里隐形，段多样性与片区降权就管不到池里的大半成员。
func TestSegmentOf(t *testing.T) {
	cases := map[string]string{
		"104.16.0.1":    "104.16.0.0/13",
		"172.64.0.1":    "172.64.0.0/13",
		"162.158.0.1":   "162.158.0.0/15",
		"108.162.192.1": "108.162.192.0/18",
		"8.8.8.8":       "8.8.0.0/16",     // 官方段外公网：按 /16 归组
		"45.32.100.7":   "45.32.0.0/16",   // 生产实测的反代地址
		"43.159.12.34":  "43.159.0.0/16",
		"192.168.1.1":   "",               // 私网
		"127.0.0.1":     "",               // 回环
		"":              "",
	}
	for ip, want := range cases {
		if got := segmentOf(net.ParseIP(ip)); got != want {
			t.Fatalf("segmentOf(%s) = %q, want %q", ip, got, want)
		}
	}
}

// 同一 /16 内的非官方段地址必须归为同一片区，否则片区降权会被稀释到失效。
func TestSegmentOfGroupsNonCloudflareBy16(t *testing.T) {
	a := segmentOf(net.ParseIP("45.32.100.7"))
	b := segmentOf(net.ParseIP("45.32.200.9"))
	if a == "" || a != b {
		t.Fatalf("同一 /16 内应同组：%q vs %q", a, b)
	}
	c := segmentOf(net.ParseIP("45.33.1.1"))
	if c == a {
		t.Fatalf("不同 /16 不应同组：%q vs %q", a, c)
	}
}
