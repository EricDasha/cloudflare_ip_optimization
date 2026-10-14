package main

import "testing"

func TestParseForwardTargets(t *testing.T) {
	lines := []string{
		`2026/10/14 20:01:01 选择转发连接: 地址: openai.com:443，拨号耗时: 178 ms`,
		`2026/10/14 20:01:02 选择转发连接: 地址: openai.com:443，拨号耗时: 311 ms`,
		`2026/10/14 20:01:03 选择转发连接: 地址: 47.242.15.101:443，拨号耗时: 8 ms`,
		`2026/10/14 20:01:04 无关日志行`,
		`2026/10/14 20:01:05 选择转发连接: 地址: nexusmods.com:443，拨号耗时: 1309 ms`,
	}
	stat := parseForwardTargets(lines)
	if stat.Total != 4 {
		t.Fatalf("Total = %d，期望 4", stat.Total)
	}
	if stat.Host != "nexusmods.com" {
		t.Fatalf("Host = %q，期望 nexusmods.com（最近一次）", stat.Host)
	}
	if stat.DialMs != 1309 {
		t.Fatalf("DialMs = %d，期望 1309", stat.DialMs)
	}
	if stat.At == nil || stat.At.Format("2006/01/02 15:04:05") != "2026/10/14 20:01:05" {
		t.Fatalf("At = %v，期望 2026/10/14 20:01:05", stat.At)
	}
	if len(stat.Counts) != 3 {
		t.Fatalf("Counts 长度 = %d，期望 3", len(stat.Counts))
	}
	if stat.Counts[0].Host != "openai.com" || stat.Counts[0].Count != 2 {
		t.Fatalf("首项 = %+v，期望 openai.com×2（按次数降序）", stat.Counts[0])
	}
}

func TestParseForwardTargetsEmpty(t *testing.T) {
	stat := parseForwardTargets(nil)
	if stat.Total != 0 || stat.Host != "" || stat.Counts != nil {
		t.Fatalf("空输入应得零值，实得 %+v", stat)
	}
}

func TestParseForwardTargetsIPv6(t *testing.T) {
	lines := []string{`选择转发连接: 地址: [2001:db8::1]:443，拨号耗时: 5 ms`}
	stat := parseForwardTargets(lines)
	if stat.Total != 1 || stat.Host != "2001:db8::1" {
		t.Fatalf("IPv6 解析错误：%+v", stat)
	}
}

func TestHostOnly(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"openai.com:443", "openai.com"},
		{"47.242.15.101:443", "47.242.15.101"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"bare.example.com", "bare.example.com"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := hostOnly(tc.in); got != tc.want {
			t.Fatalf("hostOnly(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}
