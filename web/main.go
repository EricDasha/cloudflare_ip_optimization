package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 构建信息由 build-dist 脚本经 -ldflags -X 注入；本地 go run 无注入时保持 dev。
// 动机：生产世代曾只能靠路由化石（API 路由出生日期）反推二进制年龄，
// 一条 GET /api/health 直接定生死。
var (
	gitCommit = "dev"
	buildTime = "unknown"
)

type app struct {
	dataDir            string
	cfnat              *managedProcess
	cfdata             *managedProcess
	proxyScanMu        sync.Mutex
	cfnatCtlMu         sync.Mutex
	candidateMu        sync.RWMutex
	refreshMu          sync.Mutex
	candidates         proxyCandidateSnapshot
	activeMu           sync.RWMutex
	activePool         proxyActivePool
	poolGuardMu        sync.Mutex
	lastPoolSwitchAt   time.Time
	activeHealthMu     sync.Mutex
	activeHealth       map[string]*activeHealthStat
	optimizerMu        sync.RWMutex
	optimizerEnabled   bool
	optimizerCursor    int
	optimizerLastRun   time.Time
	optimizerLastError string
	subMu              sync.RWMutex
	subSweepMu         sync.Mutex
	subIPs             []string
	subSources         map[string]string
	subLastRun         time.Time
	subLastError       string
	subLastCount       int
	quality            *qualitySchedulerRuntime
	preferredMu        sync.Mutex
	preferredProbeMu   sync.Mutex
	preferredEnabled   map[string]bool
	preferredLimit     int
	preferredLastProbe time.Time
	preferredLastError string
	preferredStatus    map[string]preferredDomainStatus
	domainLatencyMu    sync.Mutex
	domainLatency      map[string]domainLatency
	domainLatencyLast  time.Time
	domainQualityMu    sync.Mutex
	domainQuality      map[string]*domainQuality
	sourceHealthMu     sync.Mutex
	sourceHealth       map[string]*sourceHealthStat
	throneProbeMu      sync.Mutex
	throneProbe        map[string]*throneProbeStat
	throneLastProbe    time.Time
	throneLastError    string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runHealthcheck()
		return
	}

	dataDir := env("DATA_DIR", defaultDataDir)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	a := &app{
		dataDir:          dataDir,
		cfnat:            newManagedProcess("cfnat", env("CFNAT_BIN", "/usr/local/bin/cfnat")),
		cfdata:           newManagedProcess("cfdata", env("CFDATA_BIN", "/usr/local/bin/cfdata")),
		optimizerEnabled: envBool("PROXY_BACKGROUND_OPTIMIZER", true),
		activeHealth:     make(map[string]*activeHealthStat),
		subSources:       make(map[string]string),
		sourceHealth:     make(map[string]*sourceHealthStat),
		throneProbe:      make(map[string]*throneProbeStat),
		domainLatency:    make(map[string]domainLatency),
		domainQuality:    make(map[string]*domainQuality),
	}
	a.loadProxyCandidateCache()
	a.loadProxyActivePool()
	a.loadOptimizerSettings()
	a.loadPreferredSettings()
	a.loadSubscriptionPool()
	a.loadDomainQuality()
	a.seedPoolGuardFromActive()
	a.quality = newQualitySchedulerRuntime(a.dataDir)
	if pool := a.proxyActivePoolSnapshot(); len(pool.IPs) > 0 {
		a.quality.seedActive(pool.IPs[0], pool.UpdatedAt)
	}
	if envBool("CFNAT_AUTO_START", true) {
		cfg := a.cfnatStartupConfig()
		if strings.TrimSpace(cfg.Fixed) == "" && defaultProxyAutoConfig().Enabled {
			log.Printf("auto-start cfnat deferred until the proxy control plane produces a verified fixed pool")
		} else if err := a.cfnat.start(a.dataDir, cfg.args(), ""); err != nil {
			log.Printf("auto-start cfnat failed: %v", err)
		}
	}
	go a.runProxyCandidateRefreshLoop()
	go a.runBackgroundOptimizerLoop()
	go a.runSubscriptionSweepLoop()
	go a.runThroneProbeLoop()
	go a.runDomainLatencyProbeLoop()
	go a.runCFdataBackgroundScan()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", a.handleHealth)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/config", a.handleConfig)
	mux.HandleFunc("/api/cfnat/start", a.handleCFnatStart)
	mux.HandleFunc("/api/cfnat/stop", a.handleCFnatStop)
	mux.HandleFunc("/api/cfnat/proxy-scan", a.handleProxyScan)
	mux.HandleFunc("/api/cfnat/proxy-probe", a.handleProxyProbe)
	mux.HandleFunc("/api/cfnat/proxy-promote", a.handleProxyPromote)
	mux.HandleFunc("/api/cfnat/proxy-candidates", a.handleProxyCandidates)
	mux.HandleFunc("/api/cfnat/connections", a.handleCFnatConnections)
	mux.HandleFunc("/api/cfnat/upstreams", a.handleCFnatUpstreams)
	mux.HandleFunc("/api/cfnat/active/kick", a.handleActiveKick)
	mux.HandleFunc("/api/cfnat/active/endorse", a.handleActiveEndorse)
	mux.HandleFunc("/api/cfnat/active/select", a.handleActiveSelect)
	mux.HandleFunc("/api/cfnat/background-optimizer", a.handleBackgroundOptimizer)
	mux.HandleFunc("/api/cfnat/proxy-scan/apply", a.handleProxyScanApply)
	mux.HandleFunc("/api/cfnat/preferred", a.handlePreferredDomains)
	mux.HandleFunc("/api/subscriptions/refresh", a.handleSubscriptionRefresh)
	mux.HandleFunc("/api/cfdata/run", a.handleCFdataRun)
	mux.HandleFunc("/api/cfdata/stop", a.handleCFdataStop)
	mux.HandleFunc("/api/cfdata/results", a.handleCFdataResults)
	mux.HandleFunc("/api/cfdata/detail", a.handleCFdataDetail)
	mux.HandleFunc("/api/cfdata/speed", a.handleCFdataSpeed)
	mux.HandleFunc("/api/cfdata/push-candidates", a.handleCFdataPushCandidates)
	mux.HandleFunc("/api/logs", a.handleLogs)
	mux.HandleFunc("/api/files", a.handleFiles)
	mux.HandleFunc("/api/file/", a.handleFileDownload)
	mux.Handle("/", staticHandler())

	addr := env("WEB_ADDR", "0.0.0.0:8080")
	log.Printf("cloudflare-tools web listening on %s, data dir=%s", addr, dataDir)
	log.Fatal(http.ListenAndServe(addr, logRequests(mux)))
}

func (a *app) runProxyCandidateRefreshLoop() {
	a.refreshAndApplyProxyPool(context.Background())
	ticker := time.NewTicker(proxyCandidateRefreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.refreshAndApplyProxyPool(context.Background())
	}
}

// The background pass deliberately samples a small rotating slice. It must never
// compete with a manual scan or the six-hour source refresh.
func (a *app) runBackgroundOptimizerLoop() {
	initial := time.NewTimer(90 * time.Second)
	defer initial.Stop()
	<-initial.C
	a.runQualitySchedulerProbe(context.Background())
	interval := 5 * time.Minute
	if a.quality != nil {
		interval = a.quality.interval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		a.runQualitySchedulerProbe(context.Background())
	}
}

// cfdataScanInterval 是 cfdata 后台重扫的间隔。
//
// cfdata 是唯一以本机网络为基准的供给层——它在这台 NAS 上实测延迟与丢包，
// 比任何第三方优选表都更贴近这条线路的真实质量。扫描全程约 45 秒
// （-scan 100 -test 50，2000 余个候选），代价极低。
const cfdataScanInterval = 6 * time.Hour

// cfdataIPListFile 是 cfdata 的「待扫描清单」（由上游段列表生成）。
const cfdataIPListFile = "ip.csv"

// runCFdataBackgroundScan 定时重扫 cfdata。
//
// 扫描为 fire-and-forget：起进程即返回，结果写 result.csv，下一轮候选刷新
// 自然读取——扫描与候选两条链路彻底解耦，绝不阻塞调度。
func (a *app) runCFdataBackgroundScan() {
	if !envBool("CFDATA_AUTO_SCAN", true) {
		log.Printf("cfdata background scan disabled (CFDATA_AUTO_SCAN=false)")
		return
	}
	// 启动后错峰等 3 分钟：避开 cfnat 自启与首轮候选刷新的带宽竞争。
	timer := time.NewTimer(3 * time.Minute)
	defer timer.Stop()
	<-timer.C
	a.triggerCFdataScan()
	ticker := time.NewTicker(cfdataScanInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.triggerCFdataScan()
	}
}

// triggerCFdataScan 起一次 cfdata 扫描；已在跑则跳过。
//
// 扫描结束后主动重算一次候选：否则新测出的延迟与丢包要等下一轮 6 小时的
// 候选刷新才生效，本机实测的优势就白等一个周期。重算走正常的
// refreshProxyCandidates + autoApplyProxyPool，换池仍受健康闸与冷却约束。
func (a *app) triggerCFdataScan() {
	if a.cfdata.status().Running {
		return
	}
	cfg := defaultCFdataConfig()
	cfg.ForceUpdate = a.cfdataIPListStale()
	if err := a.cfdata.start(a.dataDir, cfg.args(), ""); err != nil {
		log.Printf("cfdata background scan failed to start: %v", err)
		return
	}
	log.Printf("cfdata background scan started: update=%v args=%v", cfg.ForceUpdate, cfg.args())
	go a.refreshCandidatesAfterCFdata()
}

// refreshCandidatesAfterCFdata 轮询等待 cfdata 退出，然后重算候选。
// 最多等 10 分钟（单轮实测约 2.5 分钟），超时只记日志、不阻塞任何链路。
func (a *app) refreshCandidatesAfterCFdata() {
	deadline := time.Now().Add(10 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		<-ticker.C
		if a.cfdata.status().Running {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		ok, err := a.refreshProxyCandidates(ctx)
		if err != nil {
			cancel()
			log.Printf("post-cfdata candidate refresh skipped: %v", err)
			return
		}
		if ok {
			a.autoApplyProxyPool(ctx)
		}
		cancel()
		log.Printf("post-cfdata candidate refresh done: ok=%v", ok)
		return
	}
	log.Printf("post-cfdata candidate refresh: cfdata still running after 10m, skipped")
}

// cfdataIPListStale 报告待扫描清单是否需要从上游重建。
// 清单超过 24 小时才重建，避免每一轮都去依赖 baipiao 上游。
func (a *app) cfdataIPListStale() bool {
	info, err := os.Stat(filepath.Join(a.dataDir, cfdataIPListFile))
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) > 24*time.Hour
}

// ============================================================================
// 三池模型 (Three-Pool Model)
// ============================================================================
//
// 整个优选系统维护三个池，各自承担不同的测试成本和生命周期：
//
// 1. Candidate Pool（候选池）
//    - 来源：用户IP/订阅 > 优选域名 > CFdata cache > 官方段采样
//    - 测试：低成本 TCP/TLS 延迟筛选
//    - 规模：大量（数百~数千）
//    - 职责：快速发现可能可用的 IP
//
// 2. Standby Pool（替补池）
//    - 来源：Candidate Pool 中通过 TCP 测试的 IP
//    - 测试：完整 VLESS 品质测试（延迟+速度+稳定性）
//    - 规模：中等（数十）
//    - 职责：预热验证，等待晋升机会
//
// 3. Active Pool（工作池）
//    - 来源：Standby Pool 中经过多轮验证的最优 IP
//    - 测试：仅被动连接指标（不主动测速，避免干扰工作流量）
//    - 规模：极小（1-5）
//    - 职责：实际转发用户流量
//
// 晋升规则（Promotion Threshold）：
//   - Standby IP 必须连续 3 轮证明明显优于 Active（RequiredSuperiorRounds=3）
//   - 优越判定双轨 superiorTo：
//     双方有速度成绩 → 相对 ≥25% 且绝对 ≥80Mbps；
//     任一方缺速度成绩 → WS 延迟改善 ≥25% 且 ≥15ms，且成功率不低于 Active
//   - 距离上次切换必须 >30 分钟（MinimumSwitchInterval=30min）
//   - 晋升后，旧 Active 降级为 Standby（不是丢弃）
//
// Active 隔离：
//   - 后台 Optimizer 只测试 Candidate/Standby IP，绝不触碰 Active IP
//   - Active IP 的健康检查仅通过被动连接指标（连接成功率、失败率）
//   - 这确保后台优选不会干扰正在工作的代理连接
//
// ============================================================================

// runQualitySchedulerProbe 后台优选主循环：
// 从 Candidate Pool 取一批 IP → WS/VLESS 测试 → 观察结果 → 决定是否换池。
// 此函数是三池模型的核心执行器。
func (a *app) runQualitySchedulerProbe(parent context.Context) {
	if a.quality == nil {
		return
	}
	a.optimizerMu.RLock()
	enabled := a.optimizerEnabled
	a.optimizerMu.RUnlock()
	if !enabled || !defaultProxyAutoConfig().Enabled {
		return
	}
	if !a.proxyScanMu.TryLock() {
		return
	}
	defer a.proxyScanMu.Unlock()
	cfg := defaultProxyAutoConfig()
	cfg.Concurrency = 4
	cfg.MaxLatency = 1500
	current := a.proxyActivePoolSnapshot()
	// Keep active health independent from candidate rotation. A pool can be
	// the only remaining source, so an empty candidate batch must not disable
	// the health gate.
	healthNow := time.Now()
	if len(current.IPs) > 0 {
		healthCtx, healthCancel := context.WithTimeout(parent, 5*time.Second)
		activeResults := scanProxyWebSockets(healthCtx, current.IPs, cfg)
		healthCancel()
		for _, result := range activeResults {
			ok := result.Error == ""
			a.recordActiveHealth(result.IP, ok, healthNow)
			if result.IP == a.quality.activeIP() {
				// 延迟一并入账：Active 靠每轮 WS 健康探测建立延迟基线（WS 轨裁判依据）。
				a.quality.observeActiveHealth(ok, result.Latency, healthNow)
			}
		}
	}

	// 从 Candidate Pool 获取候选 IP
	snapshot := a.proxyCandidateSnapshot()
	if len(snapshot.IPs) == 0 {
		return
	}

	// Active 隔离：候选轮转不重复测试生效 IP；生效池稍后单独做轻量健康探测。
	active := make(map[string]struct{})
	for _, ip := range current.IPs {
		active[ip] = struct{}{}
	}

	// 从 Candidate Pool 轮询取一批待测 IP（排除 Active）
	a.optimizerMu.Lock()
	start := a.optimizerCursor % len(snapshot.IPs)
	a.optimizerCursor = (start + a.quality.batch) % len(snapshot.IPs)
	a.optimizerLastRun = time.Now()
	a.optimizerLastError = ""
	a.optimizerMu.Unlock()
	batch := make([]string, 0, a.quality.batch)
	for i := 0; i < len(snapshot.IPs) && len(batch) < a.quality.batch; i++ {
		ip := snapshot.IPs[(start+i)%len(snapshot.IPs)]
		if _, ok := active[ip]; !ok {
			batch = append(batch, ip)
		}
	}
	// An empty batch is still a valid health-only scheduler round.

	// 第一阶段：WS/TLS 初筛（低成本）
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	results := scanProxyWebSockets(ctx, batch, cfg)
	applyCandidateSourcePriority(results, snapshot.SourceByIP)
	now := time.Now()

	// 第二阶段：VLESS 完整测试（高成本，仅对通过初筛的 IP）
	passed := make([]string, 0, len(results))
	if cfg.VLESS.Enabled {
		template, err := loadVLESSOutboundTemplate(cfg.VLESS.TemplatePath)
		if err != nil {
			a.quality.setError(err.Error())
			return
		}
		passed = probeVLESSPool(ctx, results, cfg.Port, len(results), cfg.VLESS, template)
		passed = rankVLESSPassesBySpeed(ctx, passed, results, cfg.Port, cfg.VLESS, template)
	} else {
		for _, result := range results {
			if result.Error == "" {
				passed = append(passed, result.IP)
			}
		}
	}

	// 观察结果：更新每个 IP 的质量记录（进入 Standby 状态），
	// 同时喂 active 被动健康：WS/VLESS 通过记成功，失败记失败，供换池“健康闸”使用。
	passedSet := make(map[string]struct{}, len(passed))
	for _, ip := range passed {
		passedSet[ip] = struct{}{}
	}
	for _, result := range results {
		_, ok := passedSet[result.IP]
		a.quality.observe(result.IP, snapshot.SourceByIP[result.IP], result.DownloadMbps, result.Latency, ok, now)
	}

	// 决策：检查是否有 Standby IP 应该晋升为 Active
	decision := a.quality.decide(now)
	if decision.Event == switchNone || decision.ToIP == "" || !a.quality.apply {
		return
	}

	// 执行换池：旧 Active → Standby，新 IP → Active。
	// 防顶池三道闸：同池跳过、全局冷却、active 自检先行。
	if !a.allowPoolSwitch(now, "scheduler") {
		return
	}
	if latest := a.proxyCandidateSnapshot(); !latest.UpdatedAt.Equal(snapshot.UpdatedAt) {
		log.Printf("scheduler pool switch skipped: candidate snapshot changed during probe")
		return
	}
	if containsString(current.IPs, decision.ToIP) {
		log.Printf("scheduler pool switch skipped: target already in current pool")
		return
	}
	pool := current
	pool.UpdatedAt, pool.Host, pool.Path = now, cfg.Host, cfg.Path
	pool.IPs = replacePoolMember(current.IPs, decision.FromIP, decision.ToIP)
	pool.Results = mergePoolResults(pool.IPs, current.Results, results)
	if err := a.applyProxyPool(pool, current); err != nil {
		a.quality.setError("调度器换池失败: " + err.Error())
		return
	}
	a.quality.commitSwitch(decision, now)
	a.markPoolSwitched(now)
}

func (a *app) refreshAndApplyProxyPool(ctx context.Context) {
	// CFdata 由独立后台扫描器（runCFdataBackgroundScan）驱动——本循环只读取
	// 它产出的 result.csv，不启动扫描。两条链路解耦，互不阻塞。
	ok, err := a.refreshProxyCandidates(ctx)
	if err != nil {
		log.Printf("proxy candidate refresh skipped: %v", err)
		return
	}
	if ok {
		a.autoApplyProxyPool(ctx)
	}
}

func (a *app) cfnatStartupConfig() cfnatConfig {
	cfg := defaultCFnatConfig()
	a.activeMu.RLock()
	defer a.activeMu.RUnlock()
	// 与 applyPoolLocked 一致：登基 IP + 优选域名同为 -fixed 成员（域名拨号时活解析）。
	fixed := append([]string(nil), a.activePool.IPs...)
	fixed = append(fixed, a.activePool.Domains...)
	if len(fixed) > 0 {
		cfg.Fixed = strings.Join(fixed, ",")
	}
	return cfg
}

// runSubscriptionSweepLoop 日常订阅慢扫：按维护者逐个拉取订阅内容，
// 提取明文 IP 与节点域名解析出的 IP，低成本 WS 验活后丢入订阅池慢慢候选。
func (a *app) runSubscriptionSweepLoop() {
	initial := time.NewTimer(60 * time.Second)
	defer initial.Stop()
	<-initial.C
	for {
		if subscriptionRefreshEnabled() {
			a.sweepSubscriptions(context.Background())
		}
		ticker := time.NewTimer(subscriptionRefreshMinutes())
		<-ticker.C
		ticker.Stop()
	}
}

// poolDomainMembers 生效池的域名直连成员（一等公民，cfnat -fixed 拨号时活解析）。
// 合并来源：显式 PROXY_DOMAIN_FORWARD > bestcf 优选域名表（快照 ForwardDomains）> 启用的优选域名。
// 上限 maxForwardDomains，去重保序，绝无预解析。
func (a *app) poolDomainMembers() []string {
	seen := map[string]struct{}{}
	domains := make([]string, 0, maxForwardDomains)
	add := func(domain string) {
		if !validCandidateHostname(domain) {
			return
		}
		if _, ok := seen[domain]; ok || len(domains) >= maxForwardDomains {
			return
		}
		seen[domain] = struct{}{}
		domains = append(domains, domain)
	}
	for _, domain := range envForwardDomains("PROXY_DOMAIN_FORWARD") {
		add(domain)
	}
	snapshot := a.proxyCandidateSnapshot()
	for _, domain := range snapshot.ForwardDomains {
		add(domain)
	}
	if envBool("PROXY_AUTO_DOMAINS", false) {
		for _, domain := range snapshot.PreferredDomains {
			add(domain)
		}
	}
	return domains
}

func runHealthcheck() {
	url := env("HEALTHCHECK_URL", "http://127.0.0.1:8080/api/health")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("healthcheck failed: %v", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("healthcheck bad status: %s", resp.Status)
		os.Exit(1)
	}
}

func (a *app) loadOptimizerSettings() {
	data, err := os.ReadFile(filepath.Join(a.dataDir, proxyOptimizerSettingsFile))
	if err != nil {
		return
	}
	var setting struct {
		Enabled bool `json:"enabled"`
	}
	if json.Unmarshal(data, &setting) != nil {
		return
	}
	a.optimizerMu.Lock()
	a.optimizerEnabled = setting.Enabled
	a.optimizerMu.Unlock()
}

func (a *app) saveOptimizerSettings(enabled bool) error {
	data, _ := json.Marshal(map[string]bool{"enabled": enabled})
	path := filepath.Join(a.dataDir, proxyOptimizerSettingsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
