package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *app) handleProxyCandidates(w http.ResponseWriter, r *http.Request) {
	writeSnapshot := func() {
		writeJSON(w, struct {
			proxyCandidateSnapshot
			Active       proxyActivePool `json:"active"`
			Subscription map[string]any  `json:"subscription"`
			PoolGuard    map[string]any  `json:"poolGuard"`
		}{proxyCandidateSnapshot: a.proxyCandidateSnapshot(), Active: a.proxyActivePoolSnapshot(), Subscription: a.subscriptionStatus(), PoolGuard: a.poolGuardStatus()})
	}
	switch r.Method {
	case http.MethodGet:
		writeSnapshot()
	case http.MethodPost:
		ok, err := a.refreshProxyCandidates(r.Context())
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, errProxyCandidatesBusy) {
				status = http.StatusTooManyRequests
			}
			http.Error(w, err.Error(), status)
			return
		}
		if ok {
			a.autoApplyProxyPool(r.Context())
		}
		writeSnapshot()
	default:
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

// handleSubscriptionRefresh 手动立即触发一轮订阅拉取（不阻塞 HTTP）。
func (a *app) handleSubscriptionRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	go a.sweepSubscriptions(context.Background())
	writeJSON(w, map[string]any{"ok": true, "started": true})
}

func (a *app) handleProxyScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !lockTimeout(&a.proxyScanMu, 60*time.Second) {
		http.Error(w, "已有优选在运行（等待 60 秒未结束），请稍后再试", http.StatusTooManyRequests)
		return
	}
	defer a.proxyScanMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var cfg proxyScanConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var err error
	if cfg, err = normalizeProxyScan(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	seen := make(map[string]struct{})
	ips := make([]string, 0, cfg.Limit)
	addIP := func(raw string) {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if !isPublicIPv4(ip) || len(ips) >= cfg.Limit {
			return
		}
		key := ip.To4().String()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		ips = append(ips, key)
	}
	for _, raw := range strings.FieldsFunc(cfg.IPs, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		addIP(raw)
	}
	for _, raw := range extractPublicIPv4(cfg.Subscription) {
		addIP(raw)
	}
	for _, raw := range resolveSubscriptionHostnames(r.Context(), extractSubscriptionHostnames(cfg.Subscription), cfg.Limit-len(ips)) {
		addIP(raw)
	}
	// 社区候选源已停用：手动扫描只解析用户输入的 IP 与订阅内容。
	// resolved, sourceErrors, err := resolveProxySources(r.Context(), cfg.Sources, cfg.Limit-len(ips))
	// if err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }
	// for _, ip := range resolved { addIP(ip) }
	sourceErrors := []string{}
	if len(ips) == 0 {
		http.Error(w, "未提供有效 IPv4 地址", http.StatusBadRequest)
		return
	}
	results := scanProxyIPs(r.Context(), ips, cfg)
	writeJSON(w, map[string]any{"ok": true, "scanned": len(ips), "results": results, "sourceErrors": sourceErrors})
}

func (a *app) handleProxyScanApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !lockTimeout(&a.proxyScanMu, 60*time.Second) {
		http.Error(w, "已有优选在运行（等待 60 秒未结束），请稍后再试", http.StatusTooManyRequests)
		return
	}
	defer a.proxyScanMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var cfg proxyScanConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "无效的请求体", http.StatusBadRequest)
		return
	}
	cfg, err := normalizeProxyScan(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	seen := make(map[string]struct{})
	ips := make([]string, 0, cfg.Limit)
	// 域名一等公民轨：直接粘的优选域名（域名 / 域名:端口 / 域名:端口#备注）
	// 不做任何 DNS 解析，直接进生效池 Domains，cfnat 拨号时活解析。
	domains := make([]string, 0, maxForwardDomains)
	addDomain := func(entry string) {
		if at := strings.IndexAny(entry, "#＃"); at >= 0 {
			entry = strings.TrimSpace(entry[:at])
		}
		if host, _, err := net.SplitHostPort(entry); err == nil {
			entry = host
		}
		entry = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(entry)), ".")
		if entry == "" || net.ParseIP(entry) != nil || !validCandidateHostname(entry) {
			return
		}
		if _, ok := seen[entry]; ok || len(domains) >= maxForwardDomains {
			return
		}
		seen[entry] = struct{}{}
		domains = append(domains, entry)
	}
	addIP := func(raw string) {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if !isPublicIPv4(ip) || len(ips) >= cfg.Limit {
			return
		}
		key := ip.To4().String()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		ips = append(ips, key)
	}
	// 粘贴文本逐行先走域名轨：域名行收为直连成员，其余走 IP/订阅链路。
	for _, line := range strings.FieldsFunc(cfg.IPs, func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		entry := strings.TrimSpace(line)
		if entry == "" {
			continue
		}
		probe := entry
		if at := strings.IndexAny(probe, "#＃"); at >= 0 {
			probe = strings.TrimSpace(probe[:at])
		}
		if host, _, err := net.SplitHostPort(probe); err == nil {
			probe = host
		}
		if net.ParseIP(probe) == nil && validCandidateHostname(probe) {
			addDomain(entry)
		}
	}
	for _, raw := range strings.FieldsFunc(cfg.IPs, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		addIP(raw)
	}
	for _, raw := range extractPublicIPv4(cfg.Subscription) {
		addIP(raw)
	}
	for _, raw := range resolveSubscriptionHostnames(r.Context(), extractSubscriptionHostnames(cfg.Subscription), cfg.Limit-len(ips)) {
		addIP(raw)
	}
	// 社区候选源已停用：手动扫描只解析用户输入的 IP 与订阅内容。
	// resolved, _, _ := resolveProxySources(r.Context(), cfg.Sources, cfg.Limit-len(ips))
	// for _, ip := range resolved { addIP(ip) }
	results := scanProxyIPs(r.Context(), ips, cfg)
	passed := make([]string, 0)
	for _, result := range results {
		if result.Error == "" {
			passed = append(passed, result.IP)
		}
	}
	if len(passed) == 0 && len(domains) == 0 {
		http.Error(w, "未通过任何 IP", http.StatusBadRequest)
		return
	}
	current := a.proxyActivePoolSnapshot()
	now := time.Now()
	// 手动采用是最高优先级：用户显式点了采用，直接换池，不受冷却限制，
	// 但同池仍跳过，避免无意义重启 cfnat。域名直连成员同理：粘贴的域名
	// 并入现有域名（保留未提及的旧域名），不预解析、拨号时活解析。
	mergedDomains := append([]string(nil), current.Domains...)
	seenDomains := make(map[string]struct{}, len(mergedDomains)+len(domains))
	for _, domain := range mergedDomains {
		seenDomains[domain] = struct{}{}
	}
	for _, domain := range domains {
		if _, ok := seenDomains[domain]; !ok {
			mergedDomains = append(mergedDomains, domain)
		}
	}
	if sameIPSet(current.IPs, passed) && sameStringSet(current.Domains, mergedDomains) {
		log.Printf("manual apply skipped: pool unchanged (%d IPs, %d domains)", len(passed), len(mergedDomains))
		writeJSON(w, map[string]any{"ok": true, "applied": 0, "unchanged": true})
		return
	}
	pool := proxyActivePool{UpdatedAt: now, Host: current.Host, Path: current.Path, IPs: passed, Domains: mergedDomains, Results: results}
	if err := a.applyProxyPool(pool, current); err != nil {
		http.Error(w, "换池失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.markPoolSwitched(now)
	log.Printf("manual supply applied: %d IPs + %d domains", len(passed), len(domains))
	writeJSON(w, map[string]any{"ok": true, "applied": len(passed), "domains": len(domains)})
}

// handleProxyProbe 单 IP 精密体检：WS 延迟实测 + 可选 VLESS 数据面测速。
// 供主页每行的「测延迟 / 测速」按钮。只读体检，绝不动池。
func (a *app) handleProxyProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "无效的请求体", http.StatusBadRequest)
		return
	}
	target := net.ParseIP(strings.TrimSpace(body.IP))
	if !isPublicIPv4(target) {
		http.Error(w, "无效的公网 IPv4", http.StatusBadRequest)
		return
	}
	ip := target.To4().String()
	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		http.Error(w, "未配置 PROXY_AUTO_HOST/PATH，无法体检", http.StatusPreconditionFailed)
		return
	}
	// 第一针：WS 101 握手延迟（三次取最优，兼记机房码与失败次数）。
	wsLatency := int64(0)
	wsColo := ""
	wsFails := 0
	for range 3 {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		started := time.Now()
		colo, err := probeProxyWebSocket(ctx, ip, cfg)
		cancel()
		if err != nil {
			wsFails++
			continue
		}
		if colo != "" && wsColo == "" {
			wsColo = colo
		}
		if ms := time.Since(started).Milliseconds(); wsLatency == 0 || ms < wsLatency {
			wsLatency = ms
		}
	}
	if wsLatency == 0 {
		writeJSON(w, map[string]any{"ok": false, "ip": ip, "error": "WS 握手三次全部失败"})
		return
	}
	// 第二针（可选）：VLESS 数据面真实测速。
	mbps := 0.0
	vlessErr := ""
	if cfg.VLESS.Enabled {
		template, err := loadVLESSOutboundTemplate(cfg.VLESS.TemplatePath)
		if err == nil {
			speedCfg := cfg.VLESS
			speedBytes := envInt("PROXY_VLESS_SPEED_BYTES", 2097152)
			if speedBytes < 1048576 || speedBytes > 33554432 {
				speedBytes = 2097152
			}
			speedCfg.TestURL = vlessSpeedTestURL(speedBytes)
			speedCfg.ExpectedStatus = http.StatusOK
			speedCfg.ReadLimit = int64(speedBytes)
			speedCfg.MinBytes = int64(speedBytes)
			speedCfg.Timeout = 20 * time.Second
			ctx, cancel := context.WithTimeout(r.Context(), speedCfg.Timeout)
			metrics, err := probeVLESSCandidateMetrics(ctx, ip, cfg.Port, speedCfg, template)
			cancel()
			if err != nil {
				vlessErr = err.Error()
			} else {
				mbps = metrics.Mbps
			}
		} else {
			vlessErr = err.Error()
		}
	}
	resp := map[string]any{"ok": true, "ip": ip, "wsLatencyMs": wsLatency, "colo": wsColo, "wsFails": wsFails}
	if cfg.VLESS.Enabled {
		if vlessErr != "" {
			resp["vless"] = map[string]any{"ok": false, "error": vlessErr}
		} else {
			resp["vless"] = map[string]any{"ok": true, "mbps": mbps}
		}
	}
	writeJSON(w, resp)
}

// handleProxyPromote 单 IP 顶替：把用户指定的候选 IP 与在位最差成员换座。
// 手动路径语义：直接换池、不受冷却限制、同 IP 已在位则幂等成功。
func (a *app) handleProxyPromote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	// 只拿换池锁；与 kick 同链，等待而非立即拒绝。
	if !lockTimeout(&a.cfnatCtlMu, 15*time.Second) {
		http.Error(w, "换池操作正在运行，请稍后再试", http.StatusTooManyRequests)
		return
	}
	defer a.cfnatCtlMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "无效的请求体", http.StatusBadRequest)
		return
	}
	target := net.ParseIP(strings.TrimSpace(body.IP))
	if !isPublicIPv4(target) {
		http.Error(w, "无效的公网 IPv4", http.StatusBadRequest)
		return
	}
	ip := target.To4().String()
	current := a.proxyActivePoolSnapshot()
	if containsString(current.IPs, ip) {
		writeJSON(w, map[string]any{"ok": true, "promoted": ip, "already": true})
		return
	}
	if len(current.IPs) == 0 {
		http.Error(w, "当前池为空，请先立即优选", http.StatusConflict)
		return
	}
	// 顶替前体检两道关：WS 握手必须通，且延迟必须低于进池上限。
	// 旧实现只考握手，于是 1168ms 的慢 IP 也能堂堂正正换进池，
	// 再被设备粘性锁死——生产事故的根因。
	cfg := defaultProxyAutoConfig()
	entryColo := ""
	fresh := make([]proxyScanResult, 0, 1)
	if cfg.Host != "" && cfg.Path != "" {
		measureCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		latency, colo, fails := measurePoolEntryLatency(measureCtx, ip, cfg)
		cancel()
		if latency == 0 {
			http.Error(w, "体检未通过（WS 握手三次全败），拒绝顶替", http.StatusBadRequest)
			return
		}
		if ceiling := poolEntryMaxLatencyMs(); latency > ceiling {
			http.Error(w, fmt.Sprintf("延迟 %dms 超过进池上限 %dms，拒绝顶替：握手虽通但太慢，进池会拖垮粘在那台设备上的所有连接", latency, ceiling), http.StatusBadRequest)
			return
		}
		entryColo = colo
		_ = fails // 失败次数并入体检注记，软信号不否决
		// 入池体检结果写进新成员的成绩单：机房码随行，丢手率记档。
		fresh = append(fresh, proxyScanResult{
			IP: ip, Latency: latency, Colo: colo, Stage: "WS_PASS",
			Note: fmt.Sprintf("入池体检 %d/3 通过", 3-fails),
		})
	}
	// 先踢在位最差者：按质量记录里延迟最差者，无记录则末位。
	victim := a.worstPoolMember(current.IPs)
	pool := current
	pool.UpdatedAt = time.Now()
	pool.IPs = replacePoolMember(current.IPs, victim, ip)
	pool.Results = mergePoolResults(pool.IPs, current.Results, fresh)
	if err := a.applyPoolLocked(pool, current); err != nil {
		http.Error(w, "顶替失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.markPoolSwitched(time.Now())
	log.Printf("manual promote: %s replaced %s", ip, victim)
	resp := map[string]any{"ok": true, "promoted": ip, "replaced": victim}
	if entryColo != "" {
		resp["colo"] = entryColo
	}
	writeJSON(w, resp)
}

// handleActiveKick 用户手动把某个在位 IP 踢出皇位（卡顿救火）：
// handleActiveKick 用户手动把某个池成员踢出皇位（卡顿救火）：
//  1. 从生效池移除该成员（IP 或优选域名皆可）并以剩余成员重启 cfnat（手动路径，豁免换池冷却）
//  2. 踢 IP 时同步从候选缓存除名，防止后台立刻又把它选回来
//  3. 踢后异步触发补位终审，把池考回 PoolSize 目标规模（5~15 保障）
//  4. 最后一个成员不允许踢（避免空池），提示先立即优选补位
func (a *app) handleActiveKick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	// 只拿换池锁（cfnatCtlMu），锁序全局规范 refresh → proxyScan → cfnatCtl，
	// 绝不交叉持锁。等待而非立即拒绝——用户操作不排队吃闭门羹。
	if !lockTimeout(&a.cfnatCtlMu, 15*time.Second) {
		http.Error(w, "换池操作正在运行，请稍后再试", http.StatusTooManyRequests)
		return
	}
	defer a.cfnatCtlMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "无效的请求体", http.StatusBadRequest)
		return
	}
	member := strings.TrimSpace(body.IP)
	isIP := false
	if ip := net.ParseIP(member); isPublicIPv4(ip) {
		member = ip.To4().String()
		isIP = true
	} else if !validCandidateHostname(member) {
		http.Error(w, "无效的 IP 或域名", http.StatusBadRequest)
		return
	}
	current := a.proxyActivePoolSnapshot()
	newIPs, ipHit := withoutPoolIP(current.IPs, member)
	newDomains, domainHit := withoutPoolIP(current.Domains, member)
	if !ipHit && !domainHit {
		http.Error(w, "该成员不在生效池中", http.StatusNotFound)
		return
	}
	if len(newIPs)+len(newDomains) == 0 {
		http.Error(w, "最后一个在位成员不能踢：先点「立即优选」补充新池", http.StatusConflict)
		return
	}
	keptResults := make([]proxyScanResult, 0, len(current.Results))
	for _, result := range current.Results {
		if result.IP != member {
			keptResults = append(keptResults, result)
		}
	}
	now := time.Now()
	pool := current
	pool.UpdatedAt = now
	pool.IPs = newIPs
	pool.Domains = newDomains
	pool.Results = keptResults
	if err := a.applyPoolLocked(pool, current); err != nil {
		http.Error(w, "踢除换池失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if isIP {
		a.removeCandidateIP(member)
	}
	a.markPoolSwitched(now)
	log.Printf("active pool kicked %s: %d IPs + %d domains remain", member, len(newIPs), len(newDomains))
	writeJSON(w, map[string]any{"ok": true, "kicked": member, "remaining": len(newIPs), "ips": newIPs})
	// 自动补位：后台考试补满 PoolSize，失败静默（六小时全量维护兜底）。
	go a.refillPoolAfterKick()
}

func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"ok":   true,
		"time": time.Now().Format(time.RFC3339),
		"version": map[string]any{
			"gitCommit": gitCommit,
			"buildTime": buildTime,
		},
	})
}

func (a *app) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"cfnat": a.cfnatStartupConfig(), "cfdata": defaultCFdataConfig(), "backgroundOptimizer": a.backgroundOptimizerStatus(), "qualityScheduler": a.qualitySnapshot()})
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"cfnat": a.cfnat.status(), "cfdata": a.cfdata.status(), "proxyAuto": a.proxyActivePoolSnapshot(), "backgroundOptimizer": a.backgroundOptimizerStatus(), "qualityScheduler": a.qualitySnapshot(), "sources": a.sourceHealthSnapshot()})
}

func (a *app) qualitySnapshot() map[string]any {
	if a.quality == nil {
		return map[string]any{"enabled": false}
	}
	return a.quality.snapshot()
}

func (a *app) backgroundOptimizerStatus() map[string]any {
	a.optimizerMu.RLock()
	defer a.optimizerMu.RUnlock()
	return map[string]any{
		"enabled":         a.optimizerEnabled,
		"lastRun":         a.optimizerLastRun,
		"lastError":       a.optimizerLastError,
		"intervalMinutes": 5,
		"batchSize":       12,
		"concurrency":     4,
	}
}

func (a *app) handleBackgroundOptimizer(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, a.backgroundOptimizerStatus())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil {
		http.Error(w, "invalid optimizer setting", http.StatusBadRequest)
		return
	}
	a.optimizerMu.Lock()
	a.optimizerEnabled = request.Enabled
	a.optimizerMu.Unlock()
	if err := a.saveOptimizerSettings(request.Enabled); err != nil {
		http.Error(w, "save optimizer setting failed", http.StatusInternalServerError)
		return
	}
	log.Printf("background proxy optimizer enabled=%t", request.Enabled)
	writeJSON(w, a.backgroundOptimizerStatus())
}

func (a *app) handleCFnatStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	a.cfnatCtlMu.Lock()
	defer a.cfnatCtlMu.Unlock()
	var cfg cfnatConfig
	_ = json.NewDecoder(r.Body).Decode(&cfg)
	cfg = normalizeCFnat(cfg)
	if err := a.cfnat.stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	time.Sleep(300 * time.Millisecond)
	if err := a.cfnat.start(a.dataDir, cfg.args(), ""); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": a.cfnat.status()})
}

func (a *app) handleCFnatStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	a.cfnatCtlMu.Lock()
	defer a.cfnatCtlMu.Unlock()
	if err := a.cfnat.stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleCFdataRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var cfg cfdataConfig
	_ = json.NewDecoder(r.Body).Decode(&cfg)
	cfg = normalizeCFdata(cfg)
	if err := a.cfdata.start(a.dataDir, cfg.args(), ""); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": a.cfdata.status()})
}

func (a *app) handleCFdataStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := a.cfdata.stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleLogs(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	limit := queryInt(r, "lines", 300)
	var lines []string
	switch target {
	case "cfdata":
		lines = a.cfdata.logs.lines(limit)
	default:
		lines = a.cfnat.logs.lines(limit)
	}
	writeJSON(w, map[string]any{"target": target, "lines": lines})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func queryInt(r *http.Request, key string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Truncate(time.Millisecond))
		}
	})
}
