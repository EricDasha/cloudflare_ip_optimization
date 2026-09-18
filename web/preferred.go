package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	a.preferredLimit = envInt("PROXY_PREFERRED_MAX_DOMAINS", 20)
	a.preferredStatus = make(map[string]preferredDomainStatus)
	data, err := os.ReadFile(filepath.Join(a.dataDir, preferredSettingsFile))
	if err != nil {
		return
	}
	var stored struct {
		Enabled []string `json:"enabled"`
		Limit   int      `json:"limit"`
	}
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	if stored.Limit >= 1 && stored.Limit <= maxPreferredDomains {
		a.preferredLimit = stored.Limit
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
	data := []byte(`{"enabled":["` + strings.Join(enabled, `","`) + `"],"limit":` + strconv.Itoa(a.preferredLimit) + `}`)
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

func (a *app) preferredLimitValue() int {
	a.preferredMu.Lock()
	defer a.preferredMu.Unlock()
	return a.preferredLimit
}

// rankedEnabledPreferredDomains 返回已启用域名（导入顺序）。
// 优选域名已改为纯兜底转发，不再探测排序；保留函数仅防旧调用残留。
func (a *app) rankedEnabledPreferredDomains() []string {
	a.preferredMu.Lock()
	defer a.preferredMu.Unlock()
	all := a.importedPreferredDomains()
	type entry struct {
		d   string
		lat int64
		ok  bool
	}
	entries := make([]entry, 0, len(all))
	for _, domain := range all {
		if !a.preferredEnabled[domain] {
			continue
		}
		entries = append(entries, entry{d: domain})
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.d)
	}
	return out
}

// enabledPreferredDomainsForCandidates 取前 limit 个启用域名供给候选快照展示。
// 注意：优选域名不再解析成 IP 合入候选池，仅作 cfnat 兜底转发。
func (a *app) enabledPreferredDomainsForCandidates() []string {
	ranked := a.rankedEnabledPreferredDomains()
	limit := a.preferredLimitValue()
	if limit < 1 || limit > maxPreferredDomains {
		limit = envInt("PROXY_PREFERRED_MAX_DOMAINS", 20)
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
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

// 优选域名是纯兜底转发目标：不解析、不探测、不合入候选池。
// 以下函数保留空壳仅防旧调用残留，实际行为：什么都不做。
func (a *app) runPreferredDomainProbeLoop() {
	log.Printf("preferred-domain probe loop disabled: domains are fallback-only, no resolve/probe")
}

func (a *app) probePreferredDomains(parent context.Context) {
	_ = parent
	// 已废弃：优选域名不再解析、不探测、不合入候选池，仅作 cfnat 兜底转发。
}

// updatePreferredDomainStatus 保留空壳：优选域名无慢测状态可记录。
func (a *app) updatePreferredDomainStatus(domain string, st preferredDomainStatus) {
	_, _ = domain, st
}

// handlePreferredDomains 读取/设置优选域名启用状态。
func (a *app) handlePreferredDomains(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{
			"all":       a.getAllPreferredDomains(),
			"enabled":   a.enabledPreferredDomains(),
			"limit":     a.preferredLimitValue(),
			"status":    a.preferredStatusSnapshot(),
			"lastProbe": a.preferredLastProbe,
			"lastError": a.preferredLastError,
			"imported":  len(a.importedPreferredDomains()),
		})
	case http.MethodPost:
		var body struct {
			Enabled []string `json:"enabled"`
			Limit   int      `json:"limit"`
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
		if body.Limit >= 1 && body.Limit <= maxPreferredDomains {
			a.preferredLimit = body.Limit
		} else if body.Limit != 0 {
			a.preferredLimit = envInt("PROXY_PREFERRED_MAX_DOMAINS", 20)
		}
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
