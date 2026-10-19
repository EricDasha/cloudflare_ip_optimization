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
	preferredDomainSourceURL      = "https://cf.090227.xyz/"
	maxPreferredDomains           = 64
	maxForwardDomains             = 12
	maxVLESSSpeedProbes           = 6
	preferredSettingsFile         = "proxy-preferred.json"
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
