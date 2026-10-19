package main

import (
	"bufio"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type cfnatConnection struct {
	IP          string         `json:"ip"`
	Connections int            `json:"connections"`
	States      map[string]int `json:"states"`
}

// upstreamConnection 是「正在被使用的上游 IP」快照：按 remote IP 分组的出站连接。
type upstreamConnection struct {
	IP          string         `json:"ip"`
	Connections int            `json:"connections"`
	States      map[string]int `json:"states"`
	InPool      bool           `json:"inPool"`
}

type upstreamSnapshot struct {
	UpdatedAt time.Time            `json:"updatedAt"`
	Port      int                  `json:"port"`
	Total     int                  `json:"total"`
	Upstreams []upstreamConnection `json:"upstreams"`
	Error     string               `json:"error,omitempty"`
}

// handleCFnatUpstreams 列出 cfnat 当前出站（remote:CFNAT_PORT）连接按上游 IP 分组，
// 并标记该 IP 是否在生效池内。用户由此看到「在连接的实例正在用哪个 IP」。
func (a *app) handleCFnatUpstreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	// PROXY_AUTO_PORT is the candidate probe destination. CFnat's actual
	// upstream port is controlled by CFNAT_PORT and may intentionally differ.
	port := defaultCFnatConfig().Port
	snapshot := readCFnatUpstreams(port, a.cfnat.status().PID)
	inPool := make(map[string]bool)
	for _, ip := range a.proxyActivePoolSnapshot().IPs {
		inPool[ip] = true
	}
	for i := range snapshot.Upstreams {
		snapshot.Upstreams[i].InPool = inPool[snapshot.Upstreams[i].IP]
	}
	writeJSON(w, snapshot)
}

// readCFnatUpstreams 统计出站转发的上游分布。显式走 /proc/self/net（不依赖
// /proc/net symlink），ErrNotExist 不再静默——容器内 procfs 异常必须暴露。
func readCFnatUpstreams(upstreamPort, pid int) upstreamSnapshot {
	snapshot := upstreamSnapshot{UpdatedAt: time.Now(), Port: upstreamPort, Upstreams: []upstreamConnection{}}
	byIP := make(map[string]*upstreamConnection)
	_, sources := procNetSources(pid)
	for _, source := range sources {
		f, err := os.Open(source.path)
		if err != nil {
			if snapshot.Error == "" {
				snapshot.Error = source.path + ": " + err.Error()
			}
			continue
		}
		if err := parseProcTCPUpstream(f, source.ipv6, upstreamPort, byIP, &snapshot.Total); err != nil && snapshot.Error == "" {
			snapshot.Error = err.Error()
		}
		_ = f.Close()
	}
	for _, item := range byIP {
		snapshot.Upstreams = append(snapshot.Upstreams, *item)
	}
	sort.Slice(snapshot.Upstreams, func(i, j int) bool {
		if snapshot.Upstreams[i].Connections != snapshot.Upstreams[j].Connections {
			return snapshot.Upstreams[i].Connections > snapshot.Upstreams[j].Connections
		}
		return snapshot.Upstreams[i].IP < snapshot.Upstreams[j].IP
	})
	return snapshot
}

// parseProcTCPUpstream 统计出站转发连接：remote 端口为上游端口、状态非 LISTEN、
// remote 为公网 IP（入站客户端连接 remote 端口是随机端口，天然被排除）。
func parseProcTCPUpstream(reader io.Reader, ipv6 bool, upstreamPort int, byIP map[string]*upstreamConnection, total *int) error {
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		return scanner.Err()
	}
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		remoteIP, remotePort, ok := parseProcEndpoint(fields[2], ipv6)
		if !ok || remotePort != upstreamPort || !procTCPStateActive(fields[3]) {
			continue
		}
		parsed := net.ParseIP(remoteIP)
		if parsed == nil || !isPublicIPv4(parsed) {
			continue
		}
		state := procTCPState(fields[3])
		item := byIP[remoteIP]
		if item == nil {
			item = &upstreamConnection{IP: remoteIP, States: map[string]int{}}
			byIP[remoteIP] = item
		}
		item.Connections++
		item.States[state]++
		*total++
	}
	return scanner.Err()
}

type cfnatConnectionsSnapshot struct {
	UpdatedAt time.Time         `json:"updatedAt"`
	Port      int               `json:"port"`
	Total     int               `json:"total"`
	Sources   []cfnatConnection `json:"sources"`
	Error     string            `json:"error,omitempty"`
	// 诊断三件：扫了多少行、本地端口命中多少行、读的是哪个 netns。
	// 「有设备在连却认不出」时，靠它区分「没数据」与「被过滤掉」。
	Scanned  int    `json:"scanned"`
	PortHits int    `json:"portHits"`
	ProcPath string `json:"procPath,omitempty"`
}

func (a *app) handleCFnatConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, readCFnatConnections(cfnatListenPort(defaultCFnatConfig().Addr), a.cfnat.status().PID))
}

func cfnatListenPort(addr string) int {
	_, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		return 1234
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return 1234
	}
	return port
}

// procNetSources 取 cfnat 自己 netns 的 TCP 表路径，拿不到 PID 才退回 self。
// 直读 /proc/<pid>/net 而非 /proc/self/net：即使两者 netns 不同也读到正确的那张表。
func procNetSources(pid int) (string, []struct {
	path string
	ipv6 bool
}) {
	base := "/proc/self/net/"
	if pid > 0 {
		base = "/proc/" + strconv.Itoa(pid) + "/net/"
	}
	return base, []struct {
		path string
		ipv6 bool
	}{{base + "tcp", false}, {base + "tcp6", true}}
}

func readCFnatConnections(port, pid int) cfnatConnectionsSnapshot {
	snapshot := cfnatConnectionsSnapshot{UpdatedAt: time.Now(), Port: port, Sources: []cfnatConnection{}}
	base, sources := procNetSources(pid)
	snapshot.ProcPath = base
	byIP := make(map[string]*cfnatConnection)
	stats := procTCPStats{}
	for _, source := range sources {
		f, err := os.Open(source.path)
		if err != nil {
			if snapshot.Error == "" {
				snapshot.Error = source.path + ": " + err.Error()
			}
			continue
		}
		err = parseProcTCP(f, source.ipv6, port, byIP, &stats)
		_ = f.Close()
		if err != nil && snapshot.Error == "" {
			snapshot.Error = err.Error()
		}
	}
	snapshot.Scanned = stats.Scanned
	snapshot.PortHits = stats.PortHits
	snapshot.Total = stats.Total
	for _, item := range byIP {
		snapshot.Sources = append(snapshot.Sources, *item)
	}
	sort.Slice(snapshot.Sources, func(i, j int) bool {
		if snapshot.Sources[i].Connections != snapshot.Sources[j].Connections {
			return snapshot.Sources[i].Connections > snapshot.Sources[j].Connections
		}
		return snapshot.Sources[i].IP < snapshot.Sources[j].IP
	})
	return snapshot
}

// procTCPStats 是一次解析的计数：Scanned 为有效行数，PortHits 为本地端口
// 命中监听端口的行数，Total 为最终计入设备数的连接数。
type procTCPStats struct {
	Scanned  int
	PortHits int
	Total    int
}

func parseProcTCP(reader io.Reader, ipv6 bool, port int, byIP map[string]*cfnatConnection, stats *procTCPStats) error {
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		return scanner.Err()
	}
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		stats.Scanned++
		_, localPort, ok := parseProcEndpoint(fields[1], ipv6)
		if !ok || localPort != port || strings.EqualFold(fields[3], "0A") {
			continue
		}
		stats.PortHits++
		remoteIP, _, ok := parseProcEndpoint(fields[2], ipv6)
		parsed := net.ParseIP(remoteIP)
		// 只排掉回环与未指定地址：设备经端口映射从外网进来，一样要认得出。
		if !ok || parsed == nil || parsed.IsLoopback() || parsed.IsUnspecified() {
			continue
		}
		state := procTCPState(fields[3])
		item := byIP[remoteIP]
		if item == nil {
			item = &cfnatConnection{IP: remoteIP, States: map[string]int{}}
			byIP[remoteIP] = item
		}
		item.Connections++
		item.States[state]++
		stats.Total++
	}
	return scanner.Err()
}

func parseProcEndpoint(raw string, ipv6 bool) (string, int, bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		return "", 0, false
	}
	port64, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return "", 0, false
	}
	bytes, err := hex.DecodeString(parts[0])
	if err != nil {
		return "", 0, false
	}
	if !ipv6 && len(bytes) == 4 {
		return net.IPv4(bytes[3], bytes[2], bytes[1], bytes[0]).String(), int(port64), true
	}
	if ipv6 && len(bytes) == 16 {
		// /proc/net/tcp6 把 IPv6 地址写成四个 32 位字，每字内部小端排列。
		// 不逐字反转就会解出 `0b12:12c0::ffff` 这类假地址——私网判定与展示全错，
		// 双栈监听下的入站设备会整批消失（历史 bug：设备名册恒为空）。
		for i := 0; i < 16; i += 4 {
			bytes[i], bytes[i+3] = bytes[i+3], bytes[i]
			bytes[i+1], bytes[i+2] = bytes[i+2], bytes[i+1]
		}
		return net.IP(bytes).String(), int(port64), true
	}
	return "", 0, false
}

func procTCPState(code string) string {
	states := map[string]string{"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1", "05": "FIN_WAIT2", "06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING"}
	if state, ok := states[strings.ToUpper(code)]; ok {
		return state
	}
	return "UNKNOWN"
}

// procTCPStateActive 判定该状态是否代表仍在途的连接。
// TIME_WAIT/CLOSE/LAST_ACK 是连接死亡的残影，不计入「正在使用」。
func procTCPStateActive(code string) bool {
	switch strings.ToUpper(code) {
	case "01", "02", "03", "04", "05", "08":
		return true
	}
	return false
}
