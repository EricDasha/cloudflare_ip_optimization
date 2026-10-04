package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	a.preferredLimit = envInt("PROXY_PREFERRED_MAX_DOMAINS", 20)
	a.preferredStatus = make(map[string]preferredDomainStatus)
	data, err := os.ReadFile(filepath.Join(a.dataDir, preferredSettingsFile))
	if err != nil {
		return
	}
	var stored struct {
		// A pointer distinguishes a missing field (legacy/default settings)
		// from an explicitly persisted empty list (disable every domain).
		Enabled *[]string `json:"enabled"`
		Limit   int       `json:"limit"`
	}
	if json.Unmarshal(data, &stored) != nil {
		return
	}
	if stored.Limit >= 1 && stored.Limit <= maxPreferredDomains {
		a.preferredLimit = stored.Limit
	}
	if stored.Enabled == nil {
		return
	}
	kept := make(map[string]bool)
	for _, domain := range *stored.Enabled {
		if validCandidateHostname(domain) {
			kept[domain] = true
		}
	}
	// Keep an empty map when the user explicitly disabled every domain.
	a.preferredEnabled = kept
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

// enabledPreferredDomainsForCandidates 取前 limit 个启用域名供给候选快照展示
// 与 C 区解析（resolvePreferredDomainIPs）。
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

// 优选域名双职责：
//  1. C 区解析供给——enabledPreferredDomainsForCandidates → resolvePreferredDomainIPs，
//     解析出的 IP 走与订阅同款乡试/殿试入候选（见文件尾 C 区实现）。
//  2. -fallback 兜底转发——proxyForwardDomains，拨号失败时按当前 DNS 直连域名。
//
// runPreferredDomainProbeLoop 保留空壳：旧的域名级慢测循环已废弃，
// 探测发生在解析出的 IP 上（乡试/殿试管道），不在域名上。
func (a *app) runPreferredDomainProbeLoop() {
	log.Printf("preferred-domain probe loop disabled: candidates come from resolved IPs (C 区), domains stay fallback-only for dialing")
}

func (a *app) probePreferredDomains(parent context.Context) {
	_ = parent
	// 已废弃：域名级慢测不再运行；C 区解析在 refreshProxyCandidates 内完成。
}

// updatePreferredDomainStatus 保留空壳：域名状态由 C 区解析直接写入 preferredStatus。
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
		go func() { _, _ = a.refreshProxyCandidates(context.Background()) }()
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

// ============================================================================
// C 区：优选域名解析供给位
// ============================================================================
//
// 职责边界（与 fallback 兜底共存，互不冲突）：
//   - 解析产物作为 preferred 层候选进入候选缓存，走与订阅/官方段完全相同的
//     乡试（TLS/WS 初筛）与殿试（VLESS 数据面）管道；域名级不做探测。
//   - 优选域名本身保留 cfnat -fallback 兜底转发职责（拨号时按当前 DNS 解析）。
//   - 快照生命周期与候选刷新周期对齐：每次 refreshProxyCandidates 重新解析，
//     上一轮解析出的 IP 随候选缓存原子替换一并过期，不残留陈旧快照。
//   - 目的：以第三方维护者的优选结果覆盖原作者上游（baipiao→cfdata）的
//     日常 IP 供给；cfdata 退为手动扫描兜底层。

const (
	preferredResolveConcurrency = 8
	preferredResolveTimeout     = 3 * time.Second
)

// ipAddrLookup 可注入的 DNS 解析函数；测试用假解析器替换。
type ipAddrLookup func(ctx context.Context, host string) ([]net.IP, error)

func defaultIPAddrLookup(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

type preferredResolveOutcome struct {
	domain string
	ips    []string
	failed bool
}

// resolvePreferredDomainIPList 并发解析优选域名，返回逐域名结果。
// 纯逻辑层：只做公网 IPv4 过滤、每域名截断与失败记录，不触碰 app 状态。
func resolvePreferredDomainIPList(parent context.Context, domains []string, perDomain int, lookup ipAddrLookup) []preferredResolveOutcome {
	outcomes := make([]preferredResolveOutcome, len(domains))
	if len(domains) == 0 {
		return outcomes
	}
	if lookup == nil {
		lookup = defaultIPAddrLookup
	}
	ctx, cancel := context.WithTimeout(parent, preferredResolveTimeout)
	defer cancel()

	sem := make(chan struct{}, preferredResolveConcurrency)
	var wg sync.WaitGroup
	for index, domain := range domains {
		wg.Add(1)
		go func(index int, domain string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				outcomes[index] = preferredResolveOutcome{domain: domain, failed: true}
				return
			}
			ips, err := lookup(ctx, domain)
			if err != nil {
				outcomes[index] = preferredResolveOutcome{domain: domain, failed: true}
				return
			}
			collected := make([]string, 0, perDomain)
			seen := make(map[string]struct{}, perDomain)
			for _, ip := range ips {
				if len(collected) >= perDomain {
					break
				}
				if !isPublicIPv4(ip) {
					continue
				}
				key := ip.To4().String()
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				collected = append(collected, key)
			}
			outcomes[index] = preferredResolveOutcome{domain: domain, ips: collected}
		}(index, domain)
	}
	wg.Wait()
	return outcomes
}

// resolvePreferredDomainIPs 把启用的优选域名解析为公网 IPv4 候选（C 区供给位）。
// 返回 (ips, errors)：ips 为去重后按总量上限截断的候选；errors 为逐域名失败信息。
func (a *app) resolvePreferredDomainIPs(parent context.Context, lookup ipAddrLookup) ([]string, []string) {
	if !envBool("PROXY_PREFERRED_RESOLVE", true) {
		return nil, nil
	}
	domains := a.enabledPreferredDomainsForCandidates()
	if len(domains) == 0 {
		return nil, nil
	}
	perDomain := envInt("PROXY_PREFERRED_IPS_PER_DOMAIN", 4)
	if perDomain < 1 || perDomain > 64 {
		perDomain = 4
	}
	totalLimit := envInt("PROXY_PREFERRED_RESOLVED_CANDIDATES", 64)
	if totalLimit < 0 || totalLimit > 500 {
		totalLimit = 64
	}
	if totalLimit == 0 {
		return nil, nil
	}

	outcomes := resolvePreferredDomainIPList(parent, domains, perDomain, lookup)

	now := time.Now()
	a.preferredMu.Lock()
	if a.preferredStatus == nil {
		a.preferredStatus = make(map[string]preferredDomainStatus)
	}
	for _, item := range outcomes {
		st := preferredDomainStatus{
			Domain:    item.domain,
			Enabled:   true,
			LastProbe: now,
		}
		if item.failed {
			st.Stage = "RESOLVE_FAIL"
			st.LastError = "DNS 解析失败或超时"
		} else {
			st.Stage = "RESOLVED"
			st.IPs = item.ips
		}
		a.preferredStatus[item.domain] = st
	}
	a.preferredMu.Unlock()

	seen := make(map[string]struct{})
	ips := make([]string, 0, totalLimit)
	errs := make([]string, 0)
	for _, item := range outcomes {
		if item.failed {
			errs = append(errs, fmt.Sprintf("优选域名 %s: DNS 解析失败或超时", item.domain))
			continue
		}
		for _, ip := range item.ips {
			if _, ok := seen[ip]; ok {
				continue
			}
			seen[ip] = struct{}{}
			ips = append(ips, ip)
			if len(ips) >= totalLimit {
				return ips, errs
			}
		}
	}
	if len(ips) == 0 && len(errs) == 0 {
		errs = append(errs, "优选域名解析未返回公网 IPv4")
	}
	return ips, errs
}
