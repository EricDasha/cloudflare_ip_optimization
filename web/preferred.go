package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// preferredSettingsFile 也定义于 main.go：常量同名不同作用域会冲突，这里不再重复声明。

func (a *app) loadPreferredSettings() {
	a.preferredMu.Lock()
	defer a.preferredMu.Unlock()
	enabled := make(map[string]bool)
	all := a.importedPreferredDomains()
	for _, domain := range all {
		enabled[domain] = true // 默认全部启用
	}
	a.preferredEnabled = enabled
	a.preferredStatus = make(map[string]preferredDomainStatus)
	data, err := os.ReadFile(filepath.Join(a.dataDir, preferredSettingsFile))
	if err != nil {
		return
	}
	var stored struct {
		Enabled []string `json:"enabled"`
	}
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	if len(stored.Enabled) == 0 {
		return
	}
	kept := make(map[string]bool)
	for _, domain := range stored.Enabled {
		if validCandidateHostname(domain) {
			kept[domain] = true
		}
	}
	if len(kept) > 0 {
		a.preferredEnabled = kept
	}
}

func (a *app) savePreferredSettingsLocked() error {
	all := a.importedPreferredDomains()
	enabled := make([]string, 0, len(a.preferredEnabled))
	for _, domain := range all {
		if a.preferredEnabled[domain] {
			enabled = append(enabled, domain)
		}
	}
	path := filepath.Join(a.dataDir, preferredSettingsFile)
	tmp := path + ".tmp"
	data := []byte(`{"enabled":["` + strings.Join(enabled, `","`) + `"]}`)
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// enabledPreferredDomains 返回用户当前启用的优选域名（按导入列表顺序）。
func (a *app) enabledPreferredDomains() []string {
	a.preferredMu.Lock()
	defer a.preferredMu.Unlock()
	all := a.importedPreferredDomains()
	out := make([]string, 0, len(all))
	for _, domain := range all {
		if a.preferredEnabled[domain] {
			out = append(out, domain)
		}
	}
	return out
}

// preferredStatusSnapshot 返回每个导入域名的最新慢测状态与启用状态。
func (a *app) preferredStatusSnapshot() map[string]preferredDomainStatus {
	a.preferredMu.Lock()
	defer a.preferredMu.Unlock()
	out := make(map[string]preferredDomainStatus, len(a.preferredStatus))
	for domain, st := range a.preferredStatus {
		enabled := a.preferredEnabled[domain]
		out[domain] = preferredDomainStatus{
			Domain:    domain,
			Enabled:   enabled,
			IPs:       append([]string(nil), st.IPs...),
			Latency:   st.Latency,
			Mbps:      st.Mbps,
			Stage:     st.Stage,
			LastProbe: st.LastProbe,
			LastError: st.LastError,
		}
	}
	return out
}

func (a *app) getAllPreferredDomains() []string {
	return a.importedPreferredDomains()
}

// updatePreferredDomainStatus 记录单域名的最近慢测结果。
func (a *app) updatePreferredDomainStatus(domain string, st preferredDomainStatus) {
	a.preferredMu.Lock()
	a.preferredStatus[domain] = st
	a.preferredLastProbe = time.Now()
	a.preferredMu.Unlock()
}

// runPreferredDomainProbeLoop 后台慢测启用的优选域名：
// DNS 解析 → WS 初筛（可选 VLESS）→ 记录每域名最优延迟/速度 → 返回通过域名候选。
// 换池动作由候选池优化器统一执行（比 active 更优才推送），此处只产出候选与状态。
func (a *app) runPreferredDomainProbeLoop() {
	initial := time.NewTimer(60 * time.Second)
	defer initial.Stop()
	<-initial.C
	interval := time.Duration(envInt("PROXY_PREFERRED_PROBE_MINUTES", 10)) * time.Minute
	if interval < time.Minute {
		interval = 10 * time.Minute
	}
	for {
		a.probePreferredDomains(context.Background())
		ticker := time.NewTimer(interval)
		<-ticker.C
		ticker.Stop()
	}
}

// probePreferredDomains 对启用优选域名执行一轮慢测并写回候选池。
func (a *app) probePreferredDomains(parent context.Context) {
	domains := a.enabledPreferredDomains()
	if len(domains) == 0 {
		return
	}
	if !a.proxyScanMu.TryLock() {
		a.preferredMu.Lock()
		a.preferredLastError = "上一轮扫描未结束，跳过本轮优选域名慢测"
		a.preferredMu.Unlock()
		return
	}
	defer a.proxyScanMu.Unlock()

	cfg := defaultProxyAutoConfig()
	// 慢测：小并发、短延迟上限，避免抢占主数据面。
	cfg.Concurrency = 4
	cfg.MaxLatency = 2000

	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()

	ipByDomain := make(map[string][]string, len(domains))
	allIPs := make([]string, 0, len(domains)*4)
	for _, domain := range domains {
		ips := resolvePreferredDomains(ctx, []string{domain}, 6)
		if len(ips) == 0 {
			a.updatePreferredDomainStatus(domain, preferredDomainStatus{Domain: domain, Enabled: a.preferredEnabled[domain], Stage: "RESOLVE_FAIL", LastError: "DNS 无可用 IPv4"})
			continue
		}
		ipByDomain[domain] = ips
		allIPs = append(allIPs, ips...)
	}
	if len(allIPs) == 0 {
		a.preferredMu.Lock()
		a.preferredLastError = "本轮无优选域名解析到 IPv4"
		a.preferredMu.Unlock()
		return
	}

	results := scanProxyWebSockets(ctx, allIPs, cfg)
	latencyByIP := make(map[string]int64, len(results))
	okSet := make(map[string]struct{})
	for _, result := range results {
		if result.Error == "" {
			latencyByIP[result.IP] = result.Latency
			okSet[result.IP] = struct{}{}
		}
	}
	statusByDomain := make(map[string]preferredDomainStatus)
	for domain, ips := range ipByDomain {
		var best string
		var bestLatency int64
		for _, ip := range ips {
			if _, ok := okSet[ip]; !ok {
				continue
			}
			if best == "" || latencyByIP[ip] < bestLatency {
				best = ip
				bestLatency = latencyByIP[ip]
			}
		}
		if best == "" {
			statusByDomain[domain] = preferredDomainStatus{Domain: domain, Enabled: a.preferredEnabled[domain], Stage: "WS_FAIL"}
			continue
		}
		statusByDomain[domain] = preferredDomainStatus{Domain: domain, Enabled: a.preferredEnabled[domain], IPs: []string{best}, Latency: bestLatency, Stage: "WS_PASS", LastProbe: time.Now()}
	}

	// 将本轮 WS 通过的优选域名 IP 合并进候选池（proxy 层），
	// 后续候选池优化器会做 VLESS 数据面终审并按“更优换池”规则推送。
	a.mergePreferredIPsIntoCandidates(ctx, okSet)

	a.preferredMu.Lock()
	a.preferredStatus = statusByDomain
	a.preferredLastProbe = time.Now()
	a.preferredLastError = ""
	a.preferredMu.Unlock()
}

// mergePreferredIPsIntoCandidates 将从优选域名解析出的已通过 IP 合入候选快照。
func (a *app) mergePreferredIPsIntoCandidates(parent context.Context, passed map[string]struct{}) {
	a.candidateMu.Lock()
	defer a.candidateMu.Unlock()
	snapshot := a.candidates
	if snapshot.SourceByIP == nil {
		snapshot.SourceByIP = make(map[string]string)
	}
	for ip := range passed {
		key := ip
		if _, ok := snapshot.SourceByIP[key]; ok {
			continue
		}
		snapshot.SourceByIP[key] = "proxy"
		snapshot.IPs = append(snapshot.IPs, key)
		if len(snapshot.IPs) > 1000 {
			snapshot.IPs = snapshot.IPs[:1000]
		}
	}
	if len(snapshot.IPs) > 0 && !snapshot.UpdatedAt.IsZero() {
		a.candidates = snapshot
		_ = a.saveProxyCandidateCache(snapshot)
	}
}

// handlePreferredDomains 读取/设置优选域名启用状态。
func (a *app) handlePreferredDomains(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{
			"all":       a.getAllPreferredDomains(),
			"enabled":   a.enabledPreferredDomains(),
			"status":    a.preferredStatusSnapshot(),
			"lastProbe": a.preferredLastProbe,
			"lastError": a.preferredLastError,
			"imported":  len(a.importedPreferredDomains()),
		})
	case http.MethodPost:
		var body struct {
			Enabled []string `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		all := a.importedPreferredDomains()
		want := make(map[string]bool)
		for _, domain := range body.Enabled {
			if validCandidateHostname(domain) {
				want[domain] = true
			}
		}
		valid := make([]string, 0, len(want))
		for _, domain := range all {
			if want[domain] {
				valid = append(valid, domain)
			}
		}
		a.preferredMu.Lock()
		a.preferredEnabled = want
		if err := a.savePreferredSettingsLocked(); err != nil {
			a.preferredMu.Unlock()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.preferredLastError = ""
		a.preferredMu.Unlock()
		// 重新拉取候选，使启用变化立即生效。
		go a.refreshProxyCandidates(context.Background())
		writeJSON(w, map[string]any{"ok": true, "enabled": valid, "count": len(valid)})
	default:
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

func (a *app) handleCFdataPushCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		DataCenter string `json:"dataCenter"`
		Limit      int    `json:"limit"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Limit <= 0 || body.Limit > 3000 {
		body.Limit = 300
	}
	rows, _, err := a.readScanRows()
	if err != nil {
		http.Error(w, "读取 cfdata 结果失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	filtered := make([]string, 0, body.Limit)
	for _, row := range rows {
		if body.DataCenter != "" && !strings.EqualFold(row.DataCenter, body.DataCenter) &&
			!strings.EqualFold(strings.TrimSpace(strings.ToLower(row.DataCenter)), strings.TrimSpace(strings.ToLower(body.DataCenter))) {
			continue
		}
		if ip := net.ParseIP(row.IP); isPublicIPv4(ip) {
			filtered = append(filtered, ip.To4().String())
			if len(filtered) >= body.Limit {
				break
			}
		}
	}
	if len(filtered) == 0 {
		http.Error(w, "没有可推送的筛选结果", http.StatusBadRequest)
		return
	}

	// 合并进候选池（cfdata 层）。
	seen := make(map[string]struct{})
	unique := make([]string, 0, len(filtered))
	for _, ip := range filtered {
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		unique = append(unique, ip)
	}
	a.mergeCfdataIPsIntoCandidates(unique)
	log.Printf("cfdata push candidates: %d IPs (dc=%s)", len(unique), body.DataCenter)
	writeJSON(w, map[string]any{"ok": true, "pushed": len(unique), "dataCenter": body.DataCenter})
}

func (a *app) mergeCfdataIPsIntoCandidates(ips []string) {
	a.candidateMu.Lock()
	defer a.candidateMu.Unlock()
	snapshot := a.candidates
	if snapshot.SourceByIP == nil {
		snapshot.SourceByIP = make(map[string]string)
	}
	for _, ip := range ips {
		if _, ok := snapshot.SourceByIP[ip]; ok {
			continue
		}
		snapshot.SourceByIP[ip] = "cfdata"
		snapshot.IPs = append(snapshot.IPs, ip)
		if len(snapshot.IPs) > 1000 {
			snapshot.IPs = snapshot.IPs[:1000]
		}
	}
	if len(snapshot.IPs) > 0 {
		a.candidates = snapshot
		_ = a.saveProxyCandidateCache(snapshot)
	}
}

// 说明：net/sort 在此文件中被间接引用，保持显式 import 以通过静态检查。
