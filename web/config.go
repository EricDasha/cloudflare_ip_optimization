package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDataDir                = "/data"
	maxLogBytes                   = 512 * 1024
	proxyCandidateRefreshInterval = 6 * time.Hour
	proxyCandidateCacheFile       = "proxy-candidates.json"
	proxyActivePoolFile           = "proxy-active.json"
	proxyOptimizerSettingsFile    = "proxy-optimizer.json"
	subscriptionPoolFile          = "subscription-pool.json"
	maxPreferredDomains           = 64
	// maxForwardDomains 转发域名上限。
	//
	// 2026-10-22 修正：原值 12 是照着「A 阶段 3 IP + 12 域名」的经验拍的，
	// 但 domain/all.txt 实际有 77 个域名，其中 29 个解析到当前网络下可通的
	// CF 段（104.16/13、104.24/14、103.31/22、108.162.192/18），实测抽 10 个
	// 有 8 个 TLS 握手成功。卡在 12 等于把大半可用域名挡在门外。
	// 放宽到 30：既覆盖足够多的可通域名，又不至于让 cfnat 的 -fixed 参数过长。
	maxForwardDomains   = 30
	maxVLESSSpeedProbes = 6
	// minFallbackForwardDomains 质量闸的兜底下限。
	//
	// 2026-10-22 二次事故：上游链路故障 3 小时把 16 个优选域名全打成 unstable
	// （successRate 0.17~0.5，低于 domainSuccessFloor=0.6）。闸门上线后按规则
	// 全部拦下，转发池退化成「只剩 IP」，而 IP 侧又全部 dial timeout，1234 直接归零。
	// 拦人的目的是筛掉坏目标，不是把服务拦死：可用域名低于该下限时，按体检成功率
	// 降序放行若干个并标记降级，等账本自然恢复再收回兜底。
	minFallbackForwardDomains = 3
	// cfnat 单连接回退深度上限。cfnat 只在上一个目标拨号失败时才顺延，
	// 所以该值不放大并发，只是在整池抖动时把「一次超时=连接断开」变成
	// 「最多试 defaultCFnatNum 个目标」。
	defaultCFnatNum       = 3
	maxCFnatNum           = 8
	preferredSettingsFile = "proxy-preferred.json"
	// 候选池分层配额：优先级决定谁先进（同 IP 先到先得），配额保证单层不能
	// 吃光整个池（生产曾见 subscription 独占 1000/1000，official=0、proxy=0）。
	//
	// 现行供给序（user > cfdata > subscription > bestcf），依「与这条线路的
	// 相关性」排定：cfdata 在本机实测延迟与丢包，是唯一以我们自己的网络为
	// 基准的源；subscription 未校验 CF 段，最杂；bestcf 是别人扫的，但已按
	// CF 段闸清洗。配额之和恰为 candidatePoolTotalCap（100+250+300=650，
	// bestcf 吃剩余 350）。
	candidatePoolTotalCap     = 1000
	userCandidatesCap         = 100
	cfdataCandidatesCap       = 250
	subscriptionCandidatesCap = 300
	bestcfCandidatesReserve   = 350
	// 片区降权的最小样本量与失败率门槛。
	//
	// 为什么要有最小样本量：片区里只有一两个候选时，一个失败就是 50%~100%，
	// 直接降权会误伤。2026-10-22 的教训是闸门拦得太狠会把服务拦死，所以
	// 样本不足时宁可不判。降权只是排到名单末尾，不是拉黑，候选不够照样用。
	segmentDemoteMinSamples   = 5
	segmentDemoteFailureRatio = 0.8
)

type proxyAutoConfig struct {
	Enabled     bool
	Host        string
	Path        string
	Port        int
	Concurrency int
	MaxLatency  int
	PoolSize    int
	MinPool     int
	VLESS       vlessProbeConfig
}

func defaultProxyAutoConfig() proxyAutoConfig {
	return proxyAutoConfig{
		Enabled:     envBool("PROXY_AUTO_APPLY", false),
		Host:        env("PROXY_AUTO_HOST", ""),
		Path:        env("PROXY_AUTO_PATH", "/"),
		Port:        envInt("PROXY_AUTO_PORT", 443),
		Concurrency: envInt("PROXY_AUTO_CONCURRENCY", 20),
		MaxLatency:  envInt("PROXY_AUTO_MAX_LATENCY", 5000),
		PoolSize:    envInt("PROXY_AUTO_POOL_SIZE", 5),
		MinPool:     envInt("PROXY_AUTO_MIN_POOL", 3),
		VLESS:       defaultVLESSProbeConfig(),
	}
}

// poolEntryMaxLatencyMs 是「进池延迟上限」：任何 IP 想进生效池（手动顶替、
// 踢人补位、调度器换池），其 WS 握手延迟都必须低于此值。
//
// 由来：生产事故。池里混入 connect 1168ms 的 IP，设备粘性把 LAN 设备锁死
// 在它上面，YouTube 评论区直接加载不出来。WS 握手「通」不代表「快」——
// 换入前必须比速度。
//
// 参考实测（同一 SNI，NAS 现场）：好 IP connect 59~64ms / WS ~190ms；
// 官方 anycast 基线 connect 155ms；坏 IP connect 231ms（3.7x）/ 1168ms（18x）。
// 默认 800ms 明显高于基线、又远低于两个坏 IP。
func poolEntryMaxLatencyMs() int64 {
	ms := int64(envInt("PROXY_POOL_MAX_ENTRY_LATENCY_MS", 800))
	if ms < 50 || ms > 5000 {
		ms = 800
	}
	return ms
}

// throneProbeSlowMs 是「在位成员判慢」的阈值：握手通了，但耗时超过它就算慢。
//
// 为什么不复用进池那条 800ms：进池是「别把慢的放进来」，代价为零，卡得越紧越好；
// 请下台要重启一次 cfnat、断一次现有连接，必须留滞回余量。实测好 IP 的 WS 约
// 190ms，坏的 1650/1809ms，1500ms 落在中间的空档里，两边都不容易误伤。
func throneProbeSlowMs() int64 {
	ms := int64(envInt("PROXY_ACTIVE_PROBE_SLOW_MS", 1500))
	if ms < 300 || ms > 5000 {
		ms = 1500
	}
	return ms
}

// ============================================================================
// 关闭成员体检（Disabled Bench）
// ============================================================================
//
// 用户关闭一个成员，本身就是一条质量信号：多半是「用着卡」。关闭之后它不再
// 转发，正好是零成本的体检窗口——测握手延迟与下载速度，不行就扔，体质差但
// 还能用的拿去和后面的替补比，比不过就换掉。这套参数只服务这一件事。

const (
	disabledBenchDefaultIntervalSeconds = 900  // 15 分钟一轮
	disabledBenchMinIntervalSeconds     = 300  // 5 分钟
	disabledBenchMaxIntervalSeconds     = 3600 // 60 分钟
	// disabledBenchBatch 每轮最多体检几个关闭成员。每个 IP 可能要起一次
	// sing-box 做下载测速（最长 20s），一次全测会把单轮拖到几分钟。
	disabledBenchBatch = 3
	// disabledBenchFailThreshold 连续几轮失败才扔。单轮内还会对握手失败
	// 立即复测一次，所以这里 2 轮≈两轮体检，而不是「一次抖动就判死」。
	disabledBenchFailThreshold = 2
	// disabledBenchBusyRetry 是扫描锁被占用后的重试间隔。锁通常几秒内释放，
	// 没必要让刚关闭的成员等满一个 15 分钟周期才拿到成绩。
	disabledBenchBusyRetry = 2 * time.Minute
	// disabledBenchCandidateBatch 找替补时最多先摸几个候选的 WS。
	disabledBenchCandidateBatch = 6
	// disabledBenchMaxSpeedProbes 找替补时最多给几个候选做下载测速。
	disabledBenchMaxSpeedProbes = 3
)

func disabledBenchEnabled() bool {
	return envBool("PROXY_DISABLED_BENCH_ENABLED", true)
}

func disabledBenchInterval() time.Duration {
	seconds := envInt("PROXY_DISABLED_BENCH_INTERVAL_SECONDS", disabledBenchDefaultIntervalSeconds)
	if seconds < disabledBenchMinIntervalSeconds || seconds > disabledBenchMaxIntervalSeconds {
		seconds = disabledBenchDefaultIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

// disabledBenchMinMbps 是「体质差」的下限：下载速度低于它就算弱成员，
// 需要和替补比一比。默认 3 Mbps 与 PROXY_VLESS_MIN_DOWNLOAD_MBPS 同口径。
func disabledBenchMinMbps() float64 {
	mbps := envFloat("PROXY_DISABLED_BENCH_MIN_MBPS", 3)
	if mbps < 0 || mbps > 10000 {
		mbps = 3
	}
	return mbps
}

// poolSwitchCooldown 全局换池冷却：任何自动换池路径（scheduler/auto-apply）
// 两次换池之间至少间隔该时长，防止“人挤人”互相顶池。
// 用户手动采用不受冷却限制。
func poolSwitchCooldown() time.Duration {
	minutes := envInt("PROXY_POOL_SWITCH_COOLDOWN_MINUTES", 30)
	if minutes < 5 || minutes > 24*60 {
		minutes = 30
	}
	return time.Duration(minutes) * time.Minute
}

func activeHealthWindow() time.Duration {
	minutes := envInt("PROXY_ACTIVE_HEALTH_WINDOW_MINUTES", 60)
	if minutes < 10 || minutes > 24*60 {
		minutes = 60
	}
	return time.Duration(minutes) * time.Minute
}

// defaultSubscriptionConverter 订阅转换器基地址（token 来自 SECRET，非代码）。
// sub 参数为第三方维护者域名：?sub=<maintainer-host>。
func defaultSubscriptionConverter() string {
	return strings.TrimRight(strings.TrimSpace(env("PROXY_SUBSCRIPTION_CONVERTER", "https://vlesdy.trojanjd.dpdns.org/sub")), "/")
}

// subscriptionMaintainers 第三方订阅维护者域名清单（逗号/空白分隔）。
// 默认内置用户给出的 7 个维护者；环境变量可覆盖。
func subscriptionMaintainers() []string {
	raw := strings.TrimSpace(env("PROXY_SUBSCRIPTION_MAINTAINERS", "owo.o00o.ooo cm.soso.edu.kg zrf.zrf.me sub.keaeye.icu sub.mot.cloudns.biz sub.mia.xx.kg sub.lzjbaby.com sub.xdu.qzz.io"))
	seen := make(map[string]struct{})
	out := make([]string, 0, 16)
	for _, host := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		if host == "" || !validCandidateHostname(host) {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
		if len(out) >= 16 {
			break
		}
	}
	return out
}

func subscriptionToken() string {
	return strings.TrimSpace(env("PROXY_SUBSCRIPTION_TOKEN", ""))
}

func subscriptionRefreshEnabled() bool {
	return envBool("PROXY_SUBSCRIPTION_REFRESH_ENABLED", true)
}

func subscriptionRefreshMinutes() time.Duration {
	minutes := envInt("PROXY_SUBSCRIPTION_REFRESH_MINUTES", 360)
	if minutes < 30 || minutes > 24*60 {
		minutes = 360
	}
	return time.Duration(minutes) * time.Minute
}

// subscriptionProbeInterval 是「考」的节奏，与「收」分开。
//
// 拆开的意义：收的节奏由维护者上游决定（六小时一轮足够），而考的节奏只受
// 本机预算约束。合成一个周期，就等于让采集速度去迁就考试速度——旧实现正是
// 这样，收得再多也只能考那么点，剩下的连记录都不留。
// 默认 10 分钟一批，每批从本子里挑新/过期的候选。
func subscriptionProbeInterval() time.Duration {
	minutes := envInt("PROXY_SUBSCRIPTION_PROBE_MINUTES", 10)
	if minutes < 1 || minutes > 24*60 {
		minutes = 10
	}
	return time.Duration(minutes) * time.Minute
}

func subscriptionMaxHosts() int {
	return maxSubscriptionHostnames
}

func envForwardDomains(name string) []string {
	var domains []string
	for _, raw := range strings.FieldsFunc(env(name, ""), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		domain = strings.TrimPrefix(domain, "*.")
		if validCandidateHostname(domain) {
			domains = append(domains, domain)
		}
	}
	if len(domains) > maxForwardDomains {
		domains = domains[:maxForwardDomains]
	}
	return domains
}

// poolColoPreferenceSet 读 PROXY_POOL_COLO_PREFER（如 "HKG,SJC"）：逗号/空白/
// 分号分隔的 IATA 机房码，小写自动转大写，非三字码剔除。
// 空/未配置 = 禁用软偏好（行为与历史版本全同）。
// 语义（软偏好，非硬闸）：候选充裕时只取偏好机房；匮乏时其余机房补位；
// 永不拒收、永不缩短名单。调度器晋升（superiorTo）保持纯质量判定，不参与偏好。
func poolColoPreferenceSet() map[string]struct{} {
	raw := strings.TrimSpace(os.Getenv("PROXY_POOL_COLO_PREFER"))
	if raw == "" {
		return nil
	}
	prefer := make(map[string]struct{})
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == ';' || r == '，' || r == '；'
	}) {
		code := strings.ToUpper(strings.TrimSpace(field))
		if len(code) == 3 {
			prefer[code] = struct{}{}
		}
	}
	if len(prefer) == 0 {
		return nil
	}
	return prefer
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return def
	}
	return v
}

func envFloat(key string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64)
	if err != nil {
		return def
	}
	return v
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
