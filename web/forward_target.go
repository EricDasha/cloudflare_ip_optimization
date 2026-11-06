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
// 旧版 cfnat 不带客户端字段，故客户端部分写成可选：一条正则同时兼容两代日志，
// 升级窗口内新旧日志混在同一个缓冲里也不会有一半解析不出来。
var forwardTargetRe = regexp.MustCompile(`选择转发连接: (?:客户端 ([^，,\s]+)，)?地址: ([^，,\s]+)，拨号耗时: (\d+) ms`)

// logTimeRe 匹配 Go log 默认前缀「2006/01/02 15:04:05」。
var logTimeRe = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)

type forwardTargetCount struct {
	Host  string `json:"host"`
	Count int    `json:"count"`
}

// forwardClientTarget 是「某台局域网设备现在走哪个出口」。
//
// 用户要看的不是全池分布，而是「我这台设备被分到了哪个 IP」。粘性分发下
// 同一台设备的所有连接应当指向同一个出口；出现多个，说明它在按回退顺序
// 落到了备用成员上，那是有价值的信息，不该被合并掉。
type forwardClientTarget struct {
	Client string               `json:"client"`
	Host   string               `json:"host"`
	Total  int                  `json:"total"`
	Counts []forwardTargetCount `json:"counts,omitempty"`
}

// forwardTargetStat 是 cfnat 近期转发目标的实况快照。
type forwardTargetStat struct {
	Host    string                `json:"host,omitempty"`
	DialMs  int64                 `json:"dialMs,omitempty"`
	At      *time.Time            `json:"at,omitempty"`
	Total   int                   `json:"total"`
	Counts  []forwardTargetCount  `json:"counts,omitempty"`
	Clients []forwardClientTarget `json:"clients,omitempty"`
}

// parseForwardTargets 纯函数：从 cfnat 日志行提取转发目标分布，便于无网络单测。
func parseForwardTargets(lines []string) forwardTargetStat {
	stat := forwardTargetStat{}
	counts := make(map[string]int)
	clientCounts := make(map[string]map[string]int)
	clientLast := make(map[string]string)
	for _, line := range lines {
		m := forwardTargetRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		client := strings.TrimSpace(m[1])
		host := hostOnly(m[2])
		if host == "" {
			continue
		}
		counts[host]++
		stat.Total++
		stat.Host = host
		if client != "" {
			if clientCounts[client] == nil {
				clientCounts[client] = make(map[string]int)
			}
			clientCounts[client][host]++
			clientLast[client] = host
		}
		if ms, err := strconv.ParseInt(m[3], 10, 64); err == nil {
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
	stat.Counts = topForwardCounts(counts)
	// 每台设备按「最近一次实际选用的出口」单列，并附上它的完整分布：
	// 粘性正常时分布只有一项；出现多项就是回退到了备用成员，值得看见。
	for client, perHost := range clientCounts {
		total := 0
		for _, n := range perHost {
			total += n
		}
		stat.Clients = append(stat.Clients, forwardClientTarget{
			Client: client,
			Host:   clientLast[client],
			Total:  total,
			Counts: topForwardCounts(perHost),
		})
	}
	sort.Slice(stat.Clients, func(i, j int) bool {
		if stat.Clients[i].Total != stat.Clients[j].Total {
			return stat.Clients[i].Total > stat.Clients[j].Total
		}
		return stat.Clients[i].Client < stat.Clients[j].Client
	})
	return stat
}

// topForwardCounts 把「目标 → 次数」排成降序切片，最多留 8 项。
func topForwardCounts(counts map[string]int) []forwardTargetCount {
	out := make([]forwardTargetCount, 0, len(counts))
	for host, n := range counts {
		out = append(out, forwardTargetCount{Host: host, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Host < out[j].Host
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
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
