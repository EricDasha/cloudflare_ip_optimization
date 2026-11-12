package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

var (
	// domainFallbackMu / domainFallbackLast 记录最近一次质量闸兜底放行的域名。
	// 非空即表示当前转发域名处于降级状态：闸门原本要拦，为了不空池才放行。
	// 供 /api/status 与巡检判断「现在是不是靠降级在撑」。
	domainFallbackMu   sync.Mutex
	domainFallbackLast []string
)

func setDomainFallback(domains []string) {
	domainFallbackMu.Lock()
	domainFallbackLast = append([]string(nil), domains...)
	domainFallbackMu.Unlock()
}

// domainFallbackSnapshot 返回最近一次 poolDomainMembers 兜底放行的域名副本。
func (a *app) domainFallbackSnapshot() []string {
	domainFallbackMu.Lock()
	defer domainFallbackMu.Unlock()
	return append([]string(nil), domainFallbackLast...)
}

type app struct {
	dataDir          string
	cfnat            *managedProcess
	cfdata           *managedProcess
	proxyScanMu      sync.Mutex
	cfnatCtlMu       sync.Mutex
	candidateMu      sync.RWMutex
	refreshMu        sync.Mutex
	candidates       proxyCandidateSnapshot
	activeMu         sync.RWMutex
	activePool       proxyActivePool
	poolGuardMu      sync.Mutex
	lastPoolSwitchAt time.Time
	// lastAutoSwitchAt 是「自动换池真的执行过」的锚点，冷却闸只认它。
	// lastPoolSwitchAt 仍记录所有池变更（剔人、扶持、手动采用），只供展示。
	lastAutoSwitchAt time.Time
	// poolBlock 记录最近一次「换池闸拒绝」的结构化原因，供 /api/status 直读。
	// 与 proxyActivePool.Error（自由文本、粘滞）互补：这个字段带时间、带来源、
	// 带下次放行时间，巡检不必再从中文句子里抠时间戳。
	poolBlock          poolSwitchVerdict
	poolBlockAt        time.Time
	poolBlockBy        string
	activeHealthMu     sync.Mutex
	activeHealth       map[string]*activeHealthStat
	optimizerMu        sync.RWMutex
	optimizerEnabled   bool
	optimizerCursor    int
	optimizerLastRun   time.Time
	optimizerLastError string
	// 订阅器状态：「收」与「考」是两条独立管线，各自一把锁、各自一套时间戳，
	// 免得一个卡住时看不出卡在哪一段；配置另用一把锁，改配置不该等考试。
	subMu              sync.RWMutex
	subInv             subscriptionInventory
	subHarvestMu       sync.Mutex
	subProbeMu         sync.Mutex
	subCfgMu           sync.Mutex
	subSettings        subscriptionSettings
	subSweepMu         sync.Mutex
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
	// speedPathMu / speedPath 记录「测速这件事本身」还能不能做。
	// 与域名成绩账分开：成绩是每个域名的，通路是整条隧道+端点的，
	// 混在一起会出现「十二个域名同时变差」这种无法归因的结论。
	speedPathMu     sync.Mutex
	speedPath       speedPathStat
	domainQualityMu sync.Mutex
	domainQuality   map[string]*domainQuality
	sourceHealthMu  sync.Mutex
	sourceHealth    map[string]*sourceHealthStat
	throneProbeMu   sync.Mutex
	throneProbe     map[string]*throneProbeStat
	throneLastProbe time.Time
	throneLastError string
	// thronePinLoss 记录「原固定成员因判死被解除固定并下线」，供页面提示。
	// 只保留最近几条：这是给人看的告示，不是审计全账。
	thronePinLoss []throneLostPin
	// disabledBench* 是「被用户关闭的成员」的体检账。关闭不等于安全，也不
	// 等于判死——停止转发正好是零成本考试窗口。详见 disabled_bench.go。
	disabledBenchMu      sync.Mutex
	disabledBench        map[string]*disabledBenchStat
	disabledBenchWake    chan struct{}
	disabledBenchLast    time.Time
	disabledBenchLastErr string
	disabledBenchEvents  []disabledBenchEvent
	// domainQuarantine 是索引期域名黑名单：验活判死的域名在一段时间内不得
	// 自动回池，否则下一轮 reconcile 会立刻把它加回来，等于白剔一次。
	// 到期自动失效，不靠人工重置（2026-10-22 教训：闸门拦得太狠会拦死服务）。
	domainQuarantineMu sync.Mutex
	domainQuarantine   map[string]time.Time
	// actionMu 把「另一个动作是否在跑」的检查与「抢自己席位」合成一步。
	// revalidateMu / optimizeMu 只护各自的进度，分开是对的；但若入口先读
	// 对方状态、再抢自己席位，两个并发请求可以同时通过检查，然后一起跑。
	actionMu sync.Mutex
	// revalidateMu 保护「当前测试」的实时进度（暂停转发 → 验在位 → 剔坏）。
	// 单独一把锁，不与探测账共用：进度是秒级高频写的，帐是每轮一次。
	revalidateMu sync.Mutex
	revalidate   revalidateState
	// optimizeMu 保护「立即优选」的实时进度（刷新候选 → 三层漏斗 → 换池）。
	// 与 revalidate 分开：两个动作语义不同、可分别观测，但入口互斥，
	// 不允许同时跑——它们会争同一份探测预算与 cfnat 控制权。
	optimizeMu sync.Mutex
	optimize   optimizeState
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
		dataDir:           dataDir,
		cfnat:             newManagedProcess("cfnat", env("CFNAT_BIN", "/usr/local/bin/cfnat")),
		cfdata:            newManagedProcess("cfdata", env("CFDATA_BIN", "/usr/local/bin/cfdata")),
		optimizerEnabled:  envBool("PROXY_BACKGROUND_OPTIMIZER", true),
		activeHealth:      make(map[string]*activeHealthStat),
		sourceHealth:      make(map[string]*sourceHealthStat),
		throneProbe:       make(map[string]*throneProbeStat),
		disabledBench:     make(map[string]*disabledBenchStat),
		disabledBenchWake: make(chan struct{}, 1),
		domainQuarantine:  make(map[string]time.Time),
		domainLatency:     make(map[string]domainLatency),
		domainQuality:     make(map[string]*domainQuality),
	}
	a.loadProxyCandidateCache()
	a.loadProxyActivePool()
	a.loadOptimizerSettings()
	a.loadPreferredSettings()
	a.loadSubscriptionSettings()
	a.loadSubscriptionInventory()
	a.loadDomainQuality()
	a.seedPoolGuardFromActive()
	a.quality = newQualitySchedulerRuntime(a.dataDir)
	if pool := a.proxyActivePoolSnapshot(); len(pool.IPs) > 0 {
		// 只播种真正转发的成员：pool.IPs[0] 可能已被用户关闭。
		if forwarding := forwardingIPs(pool); len(forwarding) > 0 {
			a.quality.seedActive(forwarding[0], pool.UpdatedAt)
		}
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
	go a.runDisabledBenchLoop()
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
	// 在位成员实弹验证：暂停转发 → 逐个验 → 剔除不合格 → 恢复。
	// 「当前测试」按钮打这条。
	mux.HandleFunc("/api/cfnat/revalidate", a.handleRevalidate)
	// 全量重挑：刷新候选 → 三层漏斗 → 换池。「立即优选」按钮打这条。
	// 2026-11-05 改成体检时把这条能力整根拔掉了，现在补回来并与体检分家。
	mux.HandleFunc("/api/cfnat/optimize", a.handleOptimize)
	mux.HandleFunc("/api/cfnat/connections", a.handleCFnatConnections)
	mux.HandleFunc("/api/cfnat/upstreams", a.handleCFnatUpstreams)
	mux.HandleFunc("/api/cfnat/active/kick", a.handleActiveKick)
	mux.HandleFunc("/api/cfnat/active/endorse", a.handleActiveEndorse)
	mux.HandleFunc("/api/cfnat/active/select", a.handleActiveSelect)
	mux.HandleFunc("/api/cfnat/background-optimizer", a.handleBackgroundOptimizer)
	mux.HandleFunc("/api/cfnat/proxy-scan/apply", a.handleProxyScanApply)
	mux.HandleFunc("/api/cfnat/preferred", a.handlePreferredDomains)
	mux.HandleFunc("/api/subscriptions/refresh", a.handleSubscriptionRefresh)
	mux.HandleFunc("/api/subscriptions/settings", a.handleSubscriptionSettings)
	mux.HandleFunc("/api/subscriptions/inventory", a.handleSubscriptionInventory)
	mux.HandleFunc("/api/cfdata/run", a.handleCFdataRun)
	mux.HandleFunc("/api/cfdata/stop", a.handleCFdataStop)
	mux.HandleFunc("/api/cfdata/results", a.handleCFdataResults)
	mux.HandleFunc("/api/cfdata/detail", a.handleCFdataDetail)
	mux.HandleFunc("/api/cfdata/speed", a.handleCFdataSpeed)
	mux.HandleFunc("/api/cfdata/push-candidates", a.handleCFdataPushCandidates)
	mux.HandleFunc("/api/logs", a.handleLogs)
	mux.HandleFunc("/api/files", a.handleFiles)
	mux.HandleFunc("/api/file/", a.handleFileDownload)
	// 调试接口只在 -tags debugapi 的测试构建里注册；普通构建是空实现，
	// 连编译都不参与。/api/debug/ 在生产二进制里不存在。
	registerDebugRoutes(mux, a)
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

// autoApplyBudget 是漏斗（初筛 + WS 精筛 + VLESS 终审）的独立时间预算。
//
// 为什么必须独立：候选刷新会先把几乎整份请求预算吃掉（26 个源并发，
// 上限 90 秒）。若漏斗沿用刷新那一个 ctx，进到精筛时 deadline 早已到期，
// 每个拨号瞬间返回「dial tcp …: i/o timeout」，整轮 0 通过——日志看起来
// 像「全网断线」，实际是预算没了。线上实测：初筛还能跑（快），紧接着
// 60 个候选 2 秒内全超时，同一时刻在 NAS 上手工握手却是 101。
//
// 也不复用 HTTP 请求的 ctx：用户关掉页面就会把整轮考试连根取消。
const autoApplyBudget = 10 * time.Minute

// newAutoApplyContext 每次漏斗都开一份全新预算，与刷新/请求 ctx 彻底解耦。
// 预算按 VLESS 终审工作量算（见 vlessReviewBudget），下限 10 分钟：
// 固定 10 分钟在 batch/Timeout/MaxCandidates 取大值时不够，尾部候选
// 没验却被记失败进质量账。
func newAutoApplyContext() (context.Context, context.CancelFunc) {
	budget := vlessReviewBudget(defaultProxyAutoConfig().VLESS)
	if budget < autoApplyBudget {
		budget = autoApplyBudget
	}
	return context.WithTimeout(context.Background(), budget)
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
		refreshCtx, cancelRefresh := context.WithTimeout(context.Background(), 60*time.Second)
		ok, err := a.refreshProxyCandidates(refreshCtx)
		cancelRefresh()
		if err != nil {
			log.Printf("post-cfdata candidate refresh skipped: %v", err)
			return
		}
		if ok {
			applyCtx, cancelApply := newAutoApplyContext()
			a.autoApplyProxyPool(applyCtx, false)
			cancelApply()
		}
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
// healthProbeBudget 按成员数算在位健康探测预算：并发槽数×单条上限，
// 再留 1 秒余量，下限 5 秒。固定 5 秒时，成员一多尾部必然超时——
// 预算不够被当成「成员死了」，与 throne 的按需预算（按目标数算）
// 是同一笔账，不能只在一处算对。
func healthProbeBudget(memberCount, concurrency, maxLatencyMs int) time.Duration {
	if concurrency < 1 {
		concurrency = 1
	}
	if maxLatencyMs < 1 {
		maxLatencyMs = 1
	}
	rounds := (memberCount + concurrency - 1) / concurrency
	need := time.Duration(rounds)*time.Duration(maxLatencyMs)*time.Millisecond + time.Second
	if need < 5*time.Second {
		need = 5 * time.Second
	}
	return need
}

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
	// 只测真正转发的成员：Disabled 成员既不进 cfnat -fixed，还占预算槽位；
	// 预算按成员数算，不再固定 5 秒——14 个成员（并发 4、单条 1.5 秒）
	// 就要 6 秒，尾部成员此前每轮确定性地拿不到槽位被记假失败，
	// LastFail 每轮刷新，健康闸永久 fail-open。
	targets := forwardingIPs(current)
	if len(targets) > 0 {
		healthCtx, healthCancel := context.WithTimeout(parent, healthProbeBudget(len(targets), cfg.Concurrency, cfg.MaxLatency))
		activeResults := scanProxyWebSockets(healthCtx, targets, cfg)
		healthCancel()
		for _, result := range activeResults {
			if result.Stage == "skipped" {
				// 没排上槽位、根本没测：不得记成健康失败，否则尾部成员
				// 每轮被刷新 LastFail，健康闸永久 fail-open。
				continue
			}
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
		// VLESS 终审不得与 WS 初筛共用 45 秒预算：终审是串行的，每 IP
		// 15 秒超时，12 个一批最坏 180 秒 + 测速 6×20 秒，45 秒一到
		// 后面的尝试瞬间报 context 错误——不是 IP 死了，是预算没了，
		// 降级逻辑（改动 B）还会把这些假超时当真失败采纳。与漏斗同款：
		// 终审单独开一份按工作量算的预算（见 vlessReviewBudget），
		// 探测量不变，只是让它跑完；预算仍不够时没轮上的记 skipped，
		// 不入失败账。
		vlessCtx, vlessCancel := newAutoApplyContext()
		defer vlessCancel()
		passed = probeVLESSPool(vlessCtx, results, cfg.Port, len(results), cfg.VLESS, template)
		passed = rankVLESSPassesBySpeed(vlessCtx, passed, results, cfg.Port, cfg.VLESS, template)
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
		if result.Stage == "skipped" {
			// 没轮上探测（WS 排队预算耗尽，或 VLESS 终审预算耗尽）：
			// 不是失败，不得进质量账。否则尾部候选每轮被记一次
			// ConsecutiveFailures，3 次即进 lineFailed。
			continue
		}
		_, ok := passedSet[result.IP]
		a.quality.observe(result.IP, snapshot.SourceByIP[result.IP], result.DownloadMbps, result.Latency, ok, now)
	}

	// 决策：检查是否有 Standby IP 应该晋升为 Active
	decision := a.quality.decide(now)
	if decision.Event == switchNone || decision.ToIP == "" || !a.quality.apply {
		// 救援只代表「允许换」，不代表「有得换」：整池已死却没有合格替补时，
		// 决策层返回 NONE，链路就停在这里。此时必须留下明确记录，否则运维
		// 看到的是「调度器每轮正常结束」，实际上一个能用的 IP 都没有。
		if allDead, deadCount := a.activePoolAllDead(current.IPs); allDead {
			log.Printf("scheduler rescue stalled: all %d members dead but no eligible standby (event=%s reason=%s)", deadCount, decision.Event, decision.Reason)
			a.quality.setError(fmt.Sprintf("整池 %d 个成员均已判死，但候选池没有合格替补；需人工补 IP 或等待供给层恢复", deadCount))
		}
		return
	}

	// 执行换池：旧 Active → Standby，新 IP → Active。
	// 防顶池闸：同池跳过、整池判死救援、全局冷却、active 自检先行。
	// 被拦时把原因落进 poolBlock——调度器被拦若只打日志，前端仍显示「一切正常」。
	verdict := a.allowPoolSwitch(now, "scheduler")
	if !verdict.Allowed {
		a.recordPoolBlock(verdict, "scheduler", now)
		a.quality.setError("调度器换池被换池闸拦截: " + verdict.Reason)
		return
	}
	if verdict.Rescue {
		log.Printf("scheduler rescue lane open: %s -> %s", decision.FromIP, decision.ToIP)
	}
	if admitted, _ := filterPoolAdmissibleIPs([]string{decision.ToIP}); len(admitted) == 0 {
		log.Printf("scheduler pool switch skipped: target %s fails pool gate (not a public IPv4)", decision.ToIP)
		return
	}
	// 提交前在锁内重取快照再换：探测持续数分钟，期间验活/reconcile
	// 随时可能换池；拿开考前的旧快照提交会把并发改动覆盖掉，甚至
	// 复活刚剔的死成员（见 applyPoolRebased）。以下三道检查都在
	// 新快照上重做——旧快照上的结论已过期。
	if err := a.applyPoolRebased(func(fresh proxyActivePool) (proxyActivePool, bool) {
		if containsString(fresh.IPs, decision.ToIP) {
			log.Printf("scheduler pool switch skipped: target already in current pool")
			return proxyActivePool{}, false
		}
		if !containsString(fresh.IPs, decision.FromIP) {
			// 换出对象已不在位（验活/别路刚换过），决策前提已不存在：
			// 不能拿 replacePoolMember 去顶 fresh.IPs[0]，下一轮按新池重判。
			log.Printf("scheduler pool switch skipped: source %s no longer in pool", decision.FromIP)
			return proxyActivePool{}, false
		}
		// 片区多样性：调度器单独换一个成员时，若换完会让池的段数变少就拒绝。
		// spreadBySegment 只在全量重挑/踢人补位里跑过，这条路径此前完全绕过片区
		// 检查——池结构可以一轮一轮被换成同一段，而所有指标都显示正常。
		nextIPs := replacePoolMember(fresh.IPs, decision.FromIP, decision.ToIP)
		if degradedSegmentDiversity(fresh.IPs, nextIPs) {
			log.Printf("scheduler pool switch skipped: segment diversity would drop %v -> %v", poolSegmentStats(fresh.IPs), poolSegmentStats(nextIPs))
			a.quality.setError("调度器换池被片区多样性检查拦截：换入后段数减少")
			return proxyActivePool{}, false
		}
		pool := fresh
		pool.UpdatedAt, pool.Host, pool.Path = now, cfg.Host, cfg.Path
		pool.IPs = nextIPs
		pool.Results = mergePoolResults(pool.IPs, fresh.Results, results)
		return pool, true
	}); err != nil {
		if errors.Is(err, errPoolRebaseAborted) {
			return
		}
		if errors.Is(err, errPoolChangedDuringSubmit) {
			log.Printf("scheduler pool switch skipped: pool changed during submit, will retry next round")
			return
		}
		a.quality.setError("调度器换池失败: " + err.Error())
		return
	}
	a.quality.commitSwitch(decision, now)
	a.markAutoSwitch(now)
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
		// 漏斗另开预算：刷新会把调用方 ctx 的 deadline 吃光（见 autoApplyBudget）。
		applyCtx, cancel := newAutoApplyContext()
		defer cancel()
		a.autoApplyProxyPool(applyCtx, false)
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

// runSubscriptionSweepLoop 订阅器后台循环。
//
// 「收」和「考」拆成两条独立节奏：收按 6 小时一轮（跟随维护者上游的刷新频率），
// 考跑得更勤（默认 10 分钟一批），只从本子里挑还没考过或已经过期的。
// 旧实现把两者绑在一起，于是采集量被考试预算卡死——收得再多也只能考那么点，
// 剩下的当场丢弃。
func (a *app) runSubscriptionSweepLoop() {
	initial := time.NewTimer(60 * time.Second)
	defer initial.Stop()
	<-initial.C
	harvestTimer := time.NewTimer(0)
	probeTimer := time.NewTimer(0)
	defer harvestTimer.Stop()
	defer probeTimer.Stop()
	for {
		select {
		case <-harvestTimer.C:
			if subscriptionRefreshEnabled() {
				if _, err := a.harvestSubscriptions(context.Background()); err != nil {
					log.Printf("subscription harvest failed: %v", err)
				}
			}
			harvestTimer.Reset(subscriptionRefreshMinutes())
		case <-probeTimer.C:
			if subscriptionRefreshEnabled() {
				if passed, err := a.probeSubscriptions(context.Background()); err != nil {
					log.Printf("subscription probe skipped: %v", err)
				} else if passed > 0 {
					// 有新通过才有必要重建聚合快照；空手而归不惊动下游。
					go a.refreshAndApplyProxyPool(context.Background())
				}
			}
			probeTimer.Reset(subscriptionProbeInterval())
		}
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
	// 显式配置视为运营者本人担保，不参与质量闸。
	for _, domain := range envForwardDomains("PROXY_DOMAIN_FORWARD") {
		add(domain)
	}
	snapshot := a.proxyCandidateSnapshot()
	// 自动来源一律过质量闸：体检判 dead/unstable 的域名不再进 -fixed。
	// 生产事故的形态就是 12 个 successRate≈0.2 的域名留在轮转池里，
	// 每 4 次拨号有 3 次撞死目标，而 -num=1 时一次失败即断连。
	//
	// 观察期同在这道闸内判：在位验活刚把某个域名剔出去并压了 30 分钟观察期，
	// 若这里不看观察期，自动换池就会在下一轮把它整批放回 -fixed——剔除与回池
	// 各走各的口径，域名集合在两套算法之间来回摆动，每次摆动都是一次 cfnat 重启
	// （生产日志 3 域名 ↔ 25 域名反复横跳）。观察期只在自动来源生效，
	// PROXY_DOMAIN_FORWARD 是运营者担保，仍然放行。
	now := time.Now()
	for _, domain := range snapshot.ForwardDomains {
		if !a.domainUsableForForwarding(domain) {
			continue
		}
		if a.domainQuarantined(domain, now) {
			log.Printf("domain pool: %s 在观察期内，不进自动候选", domain)
			continue
		}
		add(domain)
	}
	if envBool("PROXY_AUTO_DOMAINS", false) {
		for _, domain := range snapshot.PreferredDomains {
			if !a.domainUsableForForwarding(domain) {
				continue
			}
			if a.domainQuarantined(domain, now) {
				log.Printf("domain pool: %s 在观察期内，不进自动候选", domain)
				continue
			}
			add(domain)
		}
	}
	// 兜底：闸门若把自动来源全拦下，转发池会退化成「只剩 IP」甚至空池，
	// 而 IP 侧一旦同时全灭，1234 就归零（2026-10-22 事故形态）。
	// 拦人的目的是筛掉坏目标，不是把服务拦死——只有拦到「一个都不剩」时才兜底，
	// 按体检成功率降序放行，并标记为降级，等账本自然恢复后自动收回。
	// 注意条件必须是 ==0 而不是 <下限：池里已有 2 个合格域名时不该再拉坏域名凑数。
	fallback := make([]string, 0, minFallbackForwardDomains)
	if len(domains) == 0 && len(snapshot.ForwardDomains) > 0 {
		before := len(domains)
		for _, domain := range a.topDomainsByQuality(snapshot.ForwardDomains, len(snapshot.ForwardDomains)) {
			if len(domains) >= minFallbackForwardDomains {
				break
			}
			// 隔离期是体检当场给的「别放回」verdict：兜底也不该把它拉
			// 回来，否则刚隔离的域名立刻回池、再被踢出，3 ↔ 25 域名横
			// 跳、每摆一次重启一次 cfnat。只放行未隔离的（哪怕它体检
			// 成绩差），兜底的本意就是「有总比没有强」。输入放宽到全部
			// 候选、仍按成功率降序取前 3 个未隔离的，避免前 3 里有隔离
			// 者时名额白白浪费。
			if a.domainQuarantined(domain, now) {
				continue
			}
			prev := len(domains)
			add(domain)
			if len(domains) > prev {
				fallback = append(fallback, domain)
			}
		}
		if len(fallback) > 0 {
			log.Printf("domain gate DEGRADED: 可用域名 %d 个低于下限 %d，兜底放行成功率最高者: %v",
				before, minFallbackForwardDomains, fallback)
		}
	}
	setDomainFallback(fallback)
	return domains
}

// topDomainsByQuality 从候选里按体检成功率降序取前 n 个，无记录者按 0 计。
// 只服务于质量闸兜底：闸门与兜底读同一份账本，避免「一边说它坏、一边拿它救命」
// 的语义漂移。
func (a *app) topDomainsByQuality(candidates []string, n int) []string {
	type scored struct {
		domain string
		rate   float64
	}
	list := make([]scored, 0, len(candidates))
	a.domainQualityMu.Lock()
	for _, domain := range candidates {
		var rate float64
		if rec := a.domainQuality[domain]; rec != nil {
			rate = rec.SuccessRate
		}
		list = append(list, scored{domain: domain, rate: rate})
	}
	a.domainQualityMu.Unlock()
	sort.SliceStable(list, func(i, j int) bool { return list[i].rate > list[j].rate })
	if n > len(list) {
		n = len(list)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, list[i].domain)
	}
	return out
}

// domainUsableForForwarding 判断自动来源的域名是否允许进入转发池。
// 语义是「没有证据证明它坏」即放行，而不是「必须先证明它好」：
// 新域名尚无体检记录（testing）时放行，判死或判不稳时拦下。
//
// 证据必须既存在又新鲜：关掉体检、或账本久未更新（进程停机数天）时，
// 拦人用的就是一纸旧账，宁可放行让它重新被考一次。
func (a *app) domainUsableForForwarding(domain string) bool {
	if !envBool("PROXY_DOMAIN_QUALITY_GATE", true) {
		return true
	}
	if !domainProbeEnabled() {
		return true
	}
	a.domainQualityMu.Lock()
	rec := a.domainQuality[domain]
	var state string
	var failures int
	var probedAt time.Time
	if rec != nil {
		state = rec.State
		failures = rec.ConsecutiveFailures
		probedAt = rec.LastProbeAt
	}
	a.domainQualityMu.Unlock()

	if state == "" {
		return true
	}
	if probedAt.IsZero() || time.Since(probedAt) > 3*domainProbeInterval() {
		return true
	}
	switch state {
	case domainStateDead, domainStateUnstable:
		log.Printf("domain gate: %s 体检状态 %s（连败 %d），不进转发池", domain, state, failures)
		return false
	}
	return true
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
