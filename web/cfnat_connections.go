package main

import (
	"bufio"
	"encoding/hex"
	"errors"
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
	snapshot := readCFnatUpstreams(port)
	inPool := make(map[string]bool)
	for _, ip := range a.proxyActivePoolSnapshot().IPs {
		inPool[ip] = true
	}
	for i := range snapshot.Upstreams {
		snapshot.Upstreams[i].InPool = inPool[snapshot.Upstreams[i].IP]
	}
	writeJSON(w, snapshot)
}

func readCFnatUpstreams(upstreamPort int) upstreamSnapshot {
	snapshot := upstreamSnapshot{UpdatedAt: time.Now(), Port: upstreamPort, Upstreams: []upstreamConnection{}}
	byIP := make(map[string]*upstreamConnection)
	for _, source := range []struct {
		path string
		ipv6 bool
	}{{"/proc/net/tcp", false}, {"/proc/net/tcp6", true}} {
		f, err := os.Open(source.path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && snapshot.Error == "" {
				snapshot.Error = err.Error()
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
}

func (a *app) handleCFnatConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, readCFnatConnections(cfnatListenPort(defaultCFnatConfig().Addr)))
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

func readCFnatConnections(port int) cfnatConnectionsSnapshot {
	snapshot := cfnatConnectionsSnapshot{UpdatedAt: time.Now(), Port: port, Sources: []cfnatConnection{}}
	byIP := make(map[string]*cfnatConnection)
	for _, source := range []struct {
		path string
		ipv6 bool
	}{{"/proc/net/tcp", false}, {"/proc/net/tcp6", true}} {
		f, err := os.Open(source.path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && snapshot.Error == "" {
				snapshot.Error = err.Error()
			}
			continue
		}
		err = parseProcTCP(f, source.ipv6, port, byIP, &snapshot.Total)
		_ = f.Close()
		if err != nil {
			if snapshot.Error == "" {
				snapshot.Error = err.Error()
			}
		}
	}
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

func parseProcTCP(reader io.Reader, ipv6 bool, port int, byIP map[string]*cfnatConnection, total *int) error {
	scanner := bufio.NewScanner(reader)
	if !scanner.Scan() {
		return scanner.Err()
	}
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		_, localPort, ok := parseProcEndpoint(fields[1], ipv6)
		if !ok || localPort != port || strings.EqualFold(fields[3], "0A") {
			continue
		}
		remoteIP, _, ok := parseProcEndpoint(fields[2], ipv6)
		parsed := net.ParseIP(remoteIP)
		if !ok || parsed == nil || !parsed.IsPrivate() {
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
		*total++
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
