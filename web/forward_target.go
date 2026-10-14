package main

import (
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 在途转发目标观测（Actual Forward Target）
//
// cfnat 是四层哑管道：TLS 加密后读不到 UA/Host，/proc 里也只看到解析后的 CF 边缘
// IP。于是「现在到底在转发到哪个 IP / 域名」在 UI 上一直是黑洞——用户抱怨「在工作的
// ip/域名不会显示」正源于此。
//
// 唯一诚实的真相源是 cfnat 自己的日志：每次选定上游都会打一行
//   「选择转发连接: 地址: <目标:端口>，拨号耗时: <n> ms」
// 本文件只读该日志尾部的内存缓冲（logBuffer），解析出最近一次目标与近窗分布，
// 既回答「谁在干活」，也让用户一眼看清负载有没有真的铺开。

// forwardTargetRe 攫取一行「选择转发连接」的目标与拨号耗时。
var forwardTargetRe = regexp.MustCompile(`选择转发连接: 地址: ([^，,\s]+)，拨号耗时: (\d+) ms`)

// logTimeRe 匹配 Go log 默认前缀「2006/01/02 15:04:05」。
var logTimeRe = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)

type forwardTargetCount struct {
	Host  string `json:"host"`
	Count int    `json:"count"`
}

// forwardTargetStat 是 cfnat 近期转发目标的实况快照。
type forwardTargetStat struct {
	Host   string               `json:"host,omitempty"`
	DialMs int64                `json:"dialMs,omitempty"`
	At     *time.Time           `json:"at,omitempty"`
	Total  int                  `json:"total"`
	Counts []forwardTargetCount `json:"counts,omitempty"`
}

// parseForwardTargets 纯函数：从 cfnat 日志行提取转发目标分布，便于无网络单测。
func parseForwardTargets(lines []string) forwardTargetStat {
	stat := forwardTargetStat{}
	counts := make(map[string]int)
	for _, line := range lines {
		m := forwardTargetRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		host := hostOnly(m[1])
		if host == "" {
			continue
		}
		counts[host]++
		stat.Total++
		stat.Host = host
		if ms, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			stat.DialMs = ms
		}
		if ts := logTimeRe.FindStringSubmatch(line); ts != nil {
			if t, err := time.ParseInLocation("2006/01/02 15:04:05", ts[1], time.Local); err == nil {
				stat.At = &t
			}
		}
	}
	if stat.Total == 0 {
		return stat
	}
	stat.Counts = make([]forwardTargetCount, 0, len(counts))
	for host, n := range counts {
		stat.Counts = append(stat.Counts, forwardTargetCount{Host: host, Count: n})
	}
	sort.Slice(stat.Counts, func(i, j int) bool {
		if stat.Counts[i].Count != stat.Counts[j].Count {
			return stat.Counts[i].Count > stat.Counts[j].Count
		}
		return stat.Counts[i].Host < stat.Counts[j].Host
	})
	if len(stat.Counts) > 8 {
		stat.Counts = stat.Counts[:8]
	}
	return stat
}

// hostOnly 去掉「host:port」的端口（域名与 IP 通用，兼容 IPv6）。
func hostOnly(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h
	}
	return target
}

// forwardTargetStats 从 cfnat 内存日志尾部（scan 行上限）提取实况。
func (a *app) forwardTargetStats(scan int) forwardTargetStat {
	if a.cfnat == nil || a.cfnat.logs == nil {
		return forwardTargetStat{}
	}
	return parseForwardTargets(a.cfnat.logs.lines(scan))
}
