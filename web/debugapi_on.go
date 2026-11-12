//go:build debugapi

package main

// ============================================================================
// 调试接口（仅测试构建）
// ============================================================================
//
// 这些接口能直接改写生效池、候选快照、进度状态，属于「想怎么折腾就怎么
// 折腾」的测试工具。它们绝不能被编进生产二进制，所以用 build tag 隔离：
//
//	go build ./web                 # 生产构建：本文件不参与编译，路由不存在
//	go build -tags debugapi ./web  # 测试构建：多出 /api/debug/ 一组接口
//
// 为什么用 build tag 而不是环境变量开关：环境变量只能让接口「不响应」，
// 代码仍在二进制里；一旦哪天配置写错、或者有人直接 curl 内网地址，
// 能改池的接口就暴露了。编译期不存在的代码没有这种风险。
//
// 第二道保险：即使误用了测试构建，非回环来源仍一律 403，除非显式设置
// DEBUG_API_ALLOW_REMOTE=1。NAS 上通过 SSH 转发到 127.0.0.1 即可使用。
//
// 操作清单（详细说明见 docs/DEBUG-API.md）：
//
//	GET  /api/debug/state                当前内部状态总览
//	GET  /api/debug/env                  白名单环境变量（敏感值脱敏）
//	POST /api/debug/pool                 直接写入并应用生效池
//	POST /api/debug/pool/reset-cooldown  清零换池冷却与最近拦截
//	POST /api/debug/candidates           替换候选快照（可只改内存）
//	POST /api/debug/kick                 踢成员（force 可踢最后一个）
//	POST /api/debug/lock                 开/关「只转固定的」
//	POST /api/debug/probe                对单个 IP 做 WS 体检
//	POST /api/debug/revalidate           触发「当前测试」
//	POST /api/debug/optimize             触发「立即优选」
//	POST /api/debug/ledger               清空指定账本

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// registerDebugRoutes 只在 debugapi 构建里存在；对应的 off 版本是空实现。
func registerDebugRoutes(mux *http.ServeMux, a *app) {
	h := &debugAPI{a: a}
	mux.HandleFunc("/api/debug/", h.serve)
	log.Printf("DEBUG API ENABLED (build tag debugapi) — loopback only unless DEBUG_API_ALLOW_REMOTE=1")
}

type debugAPI struct {
	a *app
}

func (h *debugAPI) serve(w http.ResponseWriter, r *http.Request) {
	if !debugRemoteAllowed(r) {
		http.Error(w, "debug api is loopback-only (set DEBUG_API_ALLOW_REMOTE=1 to override)", http.StatusForbidden)
		return
	}
	op := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/debug/"), "/")
	switch op {
	case "state":
		h.state(w, r)
	case "env":
		h.env(w, r)
	case "pool":
		h.pool(w, r)
	case "pool/reset-cooldown":
		h.resetCooldown(w, r)
	case "candidates":
		h.candidates(w, r)
	case "kick":
		h.kick(w, r)
	case "lock":
		h.lock(w, r)
	case "probe":
		h.probe(w, r)
	case "revalidate":
		h.revalidate(w, r)
	case "optimize":
		h.optimize(w, r)
	case "ledger":
		h.ledger(w, r)
	default:
		http.NotFound(w, r)
	}
}

// debugRemoteAllowed 是第二道保险：默认只认回环地址。
func debugRemoteAllowed(r *http.Request) bool {
	if envBool("DEBUG_API_ALLOW_REMOTE", false) {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (h *debugAPI) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (h *debugAPI) state(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"pool":        h.a.proxyActivePoolSnapshot(),
		"candidates":  h.a.proxyCandidateSnapshot(),
		"sources":     h.a.sourceHealthSnapshot(),
		"channels":    h.a.sourceChannelSummary(),
		"quality":     h.a.qualitySnapshot(),
		"activeProbe": h.a.activeProbeStatus(),
		"revalidate":  h.a.revalidateStatus(),
		"optimize":    h.a.optimizeStatus(),
		"poolBlock":   h.a.poolBlockStatus(),
		"poolGuard":   h.a.poolGuardStatus(),
		"cfnat":       h.a.cfnat.status(),
	})
}

// debugEnvAllowlist 是允许读取的环境变量。密钥类一律不列，宁可少给。
var debugEnvAllowlist = []string{
	"PROXY_AUTO_APPLY", "PROXY_AUTO_HOST", "PROXY_AUTO_PATH", "PROXY_AUTO_PORT",
	"PROXY_AUTO_POOL_SIZE", "PROXY_AUTO_MIN_POOL", "PROXY_AUTO_MAX_LATENCY",
	"PROXY_AUTO_CONCURRENCY", "PROXY_AUTO_SHORTLIST", "PROXY_AUTO_PRESCREEN_CONCURRENCY",
	"PROXY_POOL_MAX_ENTRY_LATENCY_MS", "PROXY_POOL_COLO_PREFER",
	"PROXY_BACKGROUND_OPTIMIZER", "PROXY_VLESS_PROBE", "PROXY_DOMAIN_FORWARD",
	"DATA_DIR", "WEB_ADDR", "DEBUG_API_ALLOW_REMOTE",
}

func (h *debugAPI) env(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	out := make(map[string]string, len(debugEnvAllowlist))
	for _, key := range debugEnvAllowlist {
		out[key] = os.Getenv(key)
	}
	writeJSON(w, out)
}

// pool 直接覆盖并应用生效池——绕过换池闸、冷却、健康检查与片区配额。
// 这正是测试构建存在的意义：验证「池子换成这样之后系统怎么反应」。
func (h *debugAPI) pool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IPs      *[]string          `json:"ips"`
		Domains  *[]string          `json:"domains"`
		Priority *[]string          `json:"priority"`
		Results  *[]proxyScanResult `json:"results"`
		Lock     *bool              `json:"lock"`
		Apply    *bool              `json:"apply"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	current := h.a.proxyActivePoolSnapshot()
	pool := current
	if req.IPs != nil {
		pool.IPs = append([]string(nil), (*req.IPs)...)
	}
	if req.Domains != nil {
		pool.Domains = append([]string(nil), (*req.Domains)...)
	}
	if req.Priority != nil {
		pool.Priority = append([]string(nil), (*req.Priority)...)
	}
	if req.Results != nil {
		pool.Results = append([]proxyScanResult(nil), (*req.Results)...)
	}
	if req.Lock != nil {
		pool.Lock = *req.Lock
	}
	pool.UpdatedAt = time.Now()
	apply := req.Apply == nil || *req.Apply
	if !apply {
		if err := h.a.saveProxyActivePool(pool); err != nil {
			http.Error(w, "save pool: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.a.activeMu.Lock()
		h.a.activePool = pool
		h.a.activeMu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "applied": false, "pool": pool})
		return
	}
	h.a.cfnatCtlMu.Lock()
	err := h.a.applyPoolLocked(pool, current)
	h.a.cfnatCtlMu.Unlock()
	if err != nil {
		http.Error(w, "apply pool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "applied": true, "pool": h.a.proxyActivePoolSnapshot()})
}

func (h *debugAPI) resetCooldown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	h.a.poolGuardMu.Lock()
	h.a.lastAutoSwitchAt = time.Time{}
	h.a.lastPoolSwitchAt = time.Time{}
	h.a.poolBlock = poolSwitchVerdict{}
	h.a.poolBlockAt = time.Time{}
	h.a.poolBlockBy = ""
	h.a.poolGuardMu.Unlock()

	current := h.a.proxyActivePoolSnapshot()
	if current.Error != "" || !current.ErrorAt.IsZero() {
		current.Error = ""
		current.ErrorAt = time.Time{}
		_ = h.a.saveProxyActivePool(current)
		h.a.activeMu.Lock()
		h.a.activePool = current
		h.a.activeMu.Unlock()
	}
	writeJSON(w, map[string]any{"ok": true, "poolGuard": h.a.poolGuardStatus()})
}

func (h *debugAPI) candidates(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IPs        []string          `json:"ips"`
		SourceByIP map[string]string `json:"sourceByIp"`
		Save       *bool             `json:"save"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	snapshot := h.a.proxyCandidateSnapshot()
	snapshot.UpdatedAt = time.Now()
	snapshot.IPs = append([]string(nil), req.IPs...)
	if req.SourceByIP != nil {
		snapshot.SourceByIP = req.SourceByIP
	}
	// 调试注入的 IP 未必有真实来源账；不补默认值会让环形图把它们算成「手动」，
	// 那恰好是调试时最容易被误读的地方。
	for _, ip := range snapshot.IPs {
		if snapshot.SourceByIP == nil {
			snapshot.SourceByIP = map[string]string{}
		}
		if snapshot.SourceByIP[ip] == "" {
			snapshot.SourceByIP[ip] = "user"
		}
	}
	h.a.candidateMu.Lock()
	h.a.candidates = snapshot
	h.a.candidateMu.Unlock()
	if req.Save == nil || *req.Save {
		if err := h.a.saveProxyCandidateCache(snapshot); err != nil {
			http.Error(w, "save candidates: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true, "candidates": h.a.proxyCandidateSnapshot()})
}

func (h *debugAPI) kick(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Member string `json:"member"`
		Force  bool   `json:"force"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	member := strings.TrimSpace(req.Member)
	if member == "" {
		http.Error(w, "member required", http.StatusBadRequest)
		return
	}
	h.a.cfnatCtlMu.Lock()
	defer h.a.cfnatCtlMu.Unlock()
	current := h.a.proxyActivePoolSnapshot()
	newIPs, ipHit := withoutPoolIP(current.IPs, member)
	newDomains, domHit := withoutPoolIP(current.Domains, member)
	if !ipHit && !domHit {
		http.Error(w, "member not in pool", http.StatusNotFound)
		return
	}
	if len(newIPs)+len(newDomains) == 0 {
		if !req.Force {
			http.Error(w, "last member refused (force=true saves an empty pool without applying)", http.StatusConflict)
			return
		}
		// 空池不是可运行状态：cfnat 没有成员可转发，applyPoolLocked 里的
		// planFixedMembers 会直接拒绝。所以 force 的语义定为「只写内存与磁盘，
		// 不重启 cfnat」——用于模拟空池、验证空池下的界面与调度行为。
		// 返回体明确 applied:false，别让调用方以为 cfnat 已经在跑空池。
		pool := current
		pool.UpdatedAt = time.Now()
		pool.IPs = newIPs
		pool.Domains = newDomains
		pool.Priority = removeStringValue(current.Priority, member)
		pool.Disabled = removeStringValue(current.Disabled, member)
		kept := make([]proxyScanResult, 0, len(current.Results))
		for _, result := range current.Results {
			if result.IP != member {
				kept = append(kept, result)
			}
		}
		pool.Results = kept
		if err := h.a.saveProxyActivePool(pool); err != nil {
			http.Error(w, "save empty pool: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.a.activeMu.Lock()
		h.a.activePool = pool
		h.a.activeMu.Unlock()
		writeJSON(w, map[string]any{
			"ok": true, "kicked": member, "applied": false,
			"note": "空池只写入内存与磁盘，cfnat 未重启",
		})
		return
	}
	pool := current
	pool.UpdatedAt = time.Now()
	pool.IPs = newIPs
	pool.Domains = newDomains
	pool.Priority = removeStringValue(current.Priority, member)
	pool.Disabled = removeStringValue(current.Disabled, member)
	if err := h.a.applyPoolLocked(pool, current); err != nil {
		http.Error(w, "apply pool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.a.markPoolSwitched(time.Now())
	writeJSON(w, map[string]any{"ok": true, "kicked": member, "ips": newIPs, "domains": newDomains})
}

func (h *debugAPI) lock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Locked bool `json:"locked"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	h.a.cfnatCtlMu.Lock()
	defer h.a.cfnatCtlMu.Unlock()
	current := h.a.proxyActivePoolSnapshot()
	pool := current
	pool.Lock = req.Locked
	pool.UpdatedAt = time.Now()
	if err := h.a.applyPoolLocked(pool, current); err != nil {
		http.Error(w, "apply pool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "lock": req.Locked})
}

// debugProbeTimeoutMs 把 probe 的 timeoutMs 夹到 [1s, 60s]：未填/非正
// 沿用 6s 默认；不夹上界时一次请求可挂数小时（timeoutMs 是 int，
// 2^31-1ms 约 24 天）。
func debugProbeTimeoutMs(ms int) time.Duration {
	if ms <= 0 {
		return 6 * time.Second
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return time.Second
	}
	if d > 60*time.Second {
		return 60 * time.Second
	}
	return d
}

func (h *debugAPI) probe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP      string `json:"ip"`
		Timeout int    `json:"timeoutMs"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	ip := net.ParseIP(strings.TrimSpace(req.IP))
	if !isPublicIPv4(ip) {
		http.Error(w, "invalid public IPv4", http.StatusBadRequest)
		return
	}
	cfg := defaultProxyAutoConfig()
	timeout := debugProbeTimeoutMs(req.Timeout)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	started := time.Now()
	colo, err := probeProxyWebSocket(ctx, ip.To4().String(), cfg)
	result := map[string]any{
		"ip":        ip.To4().String(),
		"latencyMs": time.Since(started).Milliseconds(),
		"colo":      colo,
		"ok":        err == nil,
	}
	if err != nil {
		result["error"] = err.Error()
		result["errorClass"] = classifyProbeError(err)
	}
	writeJSON(w, result)
}

func (h *debugAPI) revalidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	ok, conflict := h.a.claimRevalidate(h.a.revalidateTargetCount())
	if !ok {
		if conflict == "optimize" {
			http.Error(w, "optimize running", http.StatusConflict)
		} else {
			http.Error(w, "revalidate already running", http.StatusTooManyRequests)
		}
		return
	}
	if r.URL.Query().Get("wait") == "1" {
		report, err := h.a.revalidateActivePool(context.Background())
		body := map[string]any{"ok": err == nil, "report": report}
		if err != nil {
			body["error"] = err.Error()
		}
		writeJSON(w, body)
		return
	}
	go func() {
		if _, err := h.a.revalidateActivePool(context.Background()); err != nil {
			log.Printf("debug revalidate: %v", err)
		}
	}()
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"ok": true, "started": true})
}

func (h *debugAPI) optimize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	ok, conflict := h.a.claimOptimize()
	if !ok {
		if conflict == "revalidate" {
			http.Error(w, "revalidate running", http.StatusConflict)
		} else {
			http.Error(w, "optimize already running", http.StatusTooManyRequests)
		}
		return
	}
	if r.URL.Query().Get("wait") == "1" {
		writeJSON(w, map[string]any{"ok": true, "report": h.a.runOptimize(context.Background())})
		return
	}
	go h.a.runOptimize(context.Background())
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"ok": true, "started": true})
}

func (h *debugAPI) ledger(w http.ResponseWriter, r *http.Request) {
	var req struct {
		What string `json:"what"`
	}
	if !h.decode(w, r, &req) {
		return
	}
	cleared := []string{}
	what := strings.TrimSpace(req.What)
	all := what == "all"
	if all || what == "sources" {
		h.a.sourceHealthMu.Lock()
		h.a.sourceHealth = make(map[string]*sourceHealthStat)
		h.a.sourceHealthMu.Unlock()
		cleared = append(cleared, "sources")
	}
	if all || what == "throne" {
		h.a.throneProbeMu.Lock()
		h.a.throneProbe = make(map[string]*throneProbeStat)
		h.a.throneProbeMu.Unlock()
		h.a.activeHealthMu.Lock()
		h.a.activeHealth = make(map[string]*activeHealthStat)
		h.a.activeHealthMu.Unlock()
		cleared = append(cleared, "throne", "activeHealth")
	}
	if all || what == "domain" {
		h.a.domainQualityMu.Lock()
		h.a.domainQuality = make(map[string]*domainQuality)
		h.a.domainQualityMu.Unlock()
		h.a.domainQuarantineMu.Lock()
		h.a.domainQuarantine = make(map[string]time.Time)
		h.a.domainQuarantineMu.Unlock()
		cleared = append(cleared, "domainQuality", "domainQuarantine")
	}
	if len(cleared) == 0 {
		http.Error(w, "unknown ledger; use sources|throne|domain|all", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "cleared": cleared})
}

// removeStringValue 返回去掉指定值后的新切片（不修改入参）。
func removeStringValue(values []string, want string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != want {
			out = append(out, v)
		}
	}
	return out
}
