package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type proxyScanConfig struct {
	IPs          string   `json:"ips"`
	Subscription string   `json:"subscription"`
	Sources      []string `json:"sources"`
	Host         string   `json:"host"`
	Port         int      `json:"port"`
	Concurrency  int      `json:"concurrency"`
	MaxLatency   int      `json:"maxLatency"`
	Limit        int      `json:"limit"`
	TLS          bool     `json:"tls"`
}

func rankVLESSPassesBySpeed(parent context.Context, passed []string, results []proxyScanResult, port int, base vlessProbeConfig, template map[string]any) []string {
	return rankVLESSPassesBySpeedWithProbe(parent, passed, results, port, base, template, probeVLESSCandidateMetrics)
}

func rankVLESSPassesBySpeedWithProbe(
	parent context.Context,
	passed []string,
	results []proxyScanResult,
	port int,
	base vlessProbeConfig,
	template map[string]any,
	probe func(context.Context, string, int, vlessProbeConfig, map[string]any) (vlessProbeMetrics, error),
) []string {
	if len(passed) == 0 {
		return passed
	}
	speedCfg := base
	speedBytes := envInt("PROXY_VLESS_SPEED_BYTES", 2097152)
	if speedBytes < 1048576 || speedBytes > 33554432 {
		speedBytes = 2097152
	}
	speedTimeout := vlessSpeedTimeoutSeconds()
	speedCfg.TestURL = vlessSpeedTestURL(speedBytes)
	speedCfg.ExpectedStatus = http.StatusOK
	speedCfg.ReadLimit = int64(speedBytes)
	speedCfg.MinBytes = int64(speedBytes)
	speedCfg.Timeout = time.Duration(speedTimeout) * time.Second
	times := make(map[string]int64, len(passed))
	speeds := make(map[string]float64, len(passed))
	minMbps := envFloat("PROXY_VLESS_MIN_DOWNLOAD_MBPS", 3)
	if minMbps < 0 || minMbps > 10000 {
		minMbps = 3
	}
	// 测速只对前 maxSpeedProbes 个候选执行：终审池上限本来就只有个位数，
	// 没必要为排序把每个候选都跑一遍 2MB 下载。
	maxSpeedProbes := len(passed)
	if maxSpeedProbes > maxVLESSSpeedProbes {
		maxSpeedProbes = maxVLESSSpeedProbes
	}
	verified := make([]string, 0, len(passed))
	for idx, ip := range passed {
		if idx < maxSpeedProbes {
			ctx, cancel := context.WithTimeout(parent, speedCfg.Timeout)
			metrics, err := probe(ctx, ip, port, speedCfg, template)
			cancel()
			if err == nil {
				for i := range results {
					if results[i].IP == ip {
						results[i].DownloadMbps = metrics.Mbps
						break
					}
				}
			}
			// 软门槛：测速只用于排序与展示，不再一票否决。
			// 数据面（204）已通过的 IP 一律保留——否则国内网络很难稳定跑到
			// 20Mbps 门槛，终审会恒为 0 通过，池子永远换不出去。
			if err == nil && metrics.Mbps >= minMbps {
				times[ip] = metrics.Duration.Milliseconds()
				speeds[ip] = metrics.Mbps
			}
			for i := range results {
				if results[i].IP == ip {
					results[i].Stage = "VLESS_PASS"
					if err != nil {
						results[i].Note = "测速未完成（软门槛，仅影响排序）：" + err.Error()
					} else if metrics.Mbps < minMbps {
						results[i].Note = fmt.Sprintf("测速 %.2f Mbps 低于参考值 %.2f Mbps（软门槛，仅影响排序）", metrics.Mbps, minMbps)
					}
					break
				}
			}
		}
		verified = append(verified, ip)
	}
	passed = verified
	sort.SliceStable(passed, func(i, j int) bool {
		leftRank := proxyResultSourceRank(results, passed[i])
		rightRank := proxyResultSourceRank(results, passed[j])
		leftSpeed, rightSpeed := speeds[passed[i]], speeds[passed[j]]
		if leftSpeed != rightSpeed {
			return leftSpeed > rightSpeed
		}
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		left, lok := times[passed[i]]
		right, rok := times[passed[j]]
		if lok && rok && left != right {
			return left < right
		}
		return proxyResultLatency(results, passed[i]) < proxyResultLatency(results, passed[j])
	})
	return passed
}

func proxyResultLatency(results []proxyScanResult, ip string) int64 {
	for _, result := range results {
		if result.IP == ip {
			return result.Latency
		}
	}
	return int64(^uint64(0) >> 1)
}

func proxyResultSourceRank(results []proxyScanResult, ip string) int {
	for _, result := range results {
		if result.IP == ip {
			return result.SourceRank
		}
	}
	return 4
}

// orderPassedByColoPreference 把过考名单按机房偏好稳定分区重排：偏好机房在前，
// 区内保持原序（VLESS 路径的 Mbps 降序、WS 路径的来源+延迟序皆不破坏）。
// 只重排、不淘汰——名单长度永不因偏好而缩短；调用方截断时「充裕取偏好、
// 匮乏放行其余」的软语义自然成立。无 colo 情报的成员归非偏好区，绝不因此被拒。
// 偏好集为空（未配置）时恒等返回——默认行为与历史版本零差异。
func orderPassedByColoPreference(passed []string, results []proxyScanResult, prefer map[string]struct{}) []string {
	if len(prefer) == 0 || len(passed) < 2 {
		return passed
	}
	coloByIP := make(map[string]string, len(results))
	for _, r := range results {
		if r.Colo != "" {
			if _, ok := coloByIP[r.IP]; !ok {
				coloByIP[r.IP] = r.Colo
			}
		}
	}
	head := make([]string, 0, len(passed))
	tail := make([]string, 0, len(passed))
	for _, ip := range passed {
		if _, ok := prefer[coloByIP[ip]]; ok {
			head = append(head, ip)
		} else {
			tail = append(tail, ip)
		}
	}
	return append(head, tail...)
}

// spreadBySegment 在保持输入顺序（延迟/机房偏好已排好）的前提下限制单段占用，
// 提升段多样性。软配额：候选不足时按原顺序补回，宁可同段也不留空位。
//
// 2026-10-22 事故：池里只有 1 个 IP、1 个段，该段一挂全灭。段多样性是抗单点
// 故障的第一道防线，优先级高于"挑延迟最低的 3 个同段 IP"。
func spreadBySegment(ips []string, poolSize int) []string {
	if poolSize <= 0 {
		return ips
	}
	if len(ips) <= 1 {
		if len(ips) > poolSize {
			return ips[:poolSize]
		}
		return ips
	}
	// 按段分组，组内保持原顺序（延迟序），组顺序按各段首次出现位置。
	order := make([]string, 0, 4)
	groups := make(map[string][]string, 4)
	pos := make(map[string]int, len(ips))
	for i, ip := range ips {
		if _, ok := pos[ip]; !ok {
			pos[ip] = i
		}
		seg := segmentOf(net.ParseIP(ip))
		if seg == "" {
			// 只有非公网/非法地址会走到这里——它们本就进不了池，此处兜底防止
			// 空串把不同来源的无效条目混成一组。
			seg = "\x00invalid"
		}
		if _, ok := groups[seg]; !ok {
			order = append(order, seg)
		}
		groups[seg] = append(groups[seg], ip)
	}
	// 轮转取：每轮每组各取一个。顺序贪心会让排在前面的段吃满名额，
	// 后面的段永远进不来——那就退化成「没有段多样性」。
	out := make([]string, 0, poolSize)
	for round := 0; len(out) < poolSize; round++ {
		progressed := false
		for _, seg := range order {
			if len(out) >= poolSize {
				break
			}
			if round < len(groups[seg]) {
				out = append(out, groups[seg][round])
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	// 取完再按原序列位置排回：池成员的顺序就是 cfnat 的尝试顺序，
	// 延迟低的仍要排在前面。
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i]] < pos[out[j]] })
	return out
}

// admitPassedUnderCeiling 把已通过终审的名单按「入池延迟上限」再裁一遍。
// 顺序不变；results 里找不到成绩的成员保守保留（调用方给的 passed 本就来自 results）。
//
// 为什么要单独抽出来：手动供给、踢人补位、在位替补、自动全量重挑是四条入口，
// 旧实现只有前三条查 800ms，自动换池按 5000ms 放行——闸门各说各话，慢 IP
// 总能从最宽的那条缝里进来。
func admitPassedUnderCeiling(passed []string, results []proxyScanResult, ceiling int64) []string {
	if ceiling <= 0 || len(passed) == 0 {
		return passed
	}
	latency := make(map[string]int64, len(results))
	for _, result := range results {
		latency[result.IP] = result.Latency
	}
	out := make([]string, 0, len(passed))
	for _, ip := range passed {
		if ms, ok := latency[ip]; ok && ms > ceiling {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// degradedFromWSResults 从 WS 扫描结果里挑出可降级采用的候选，用于池子
// 将要空掉时的降级兜底。返回 (adopted, rejected)：rejected 是被拒绝的
// 候选及原因（"ip: 原因"），调用方打日志，不要静默丢弃。
//
// 语义边界：只补到 want 为止，绝不越界扩池——降级是为了不空池，
// 不是为了用未验证的候选把池子填满。
//
// 准入（第二轮审计收紧）：候选必须有明确的数据面成功证据
// （DataProven，见 vless_probe.go）：VLESS 终审里隧道收到过源站的
// HTTP 响应，才算「数据面真的通过」。只有这种失败才允许降级采用——
// 失败点在测速端点而不在隧道本身（例如状态码不对、下载字节不够），
// 候选本身是通的。
//
// 为什么不能按「非超时失败」放行（第一轮改动 B 的旧口径）：
//   - refused / unreachable 恰恰证明候选已死：443 没人听，或根本没路由。
//     把已死的地址推进池子，比空池更糟——转发过去必定失败；
//   - reset / protocol 在没收到任何响应前发生时，同样什么都没证明：
//     可能是对端在握手前就 RST，也可能是中间设备掐的；
//   - other 里混着探针基础设施失败（sing-box 起不来、模板错、端口分
//     配失败）——探针坏了，全员失败，旧口径会把整批候选照单全收；
//   - DataLatency 只是整次尝试的耗时（含 15 秒超时），实测超时项全是
//     约 15005ms，从来就不是「连上过」的证明。
//
// 结论：快速失败不比超时更可信——两者都只说明「这次没通」，
// 唯一可信的是隧道里真实流过源站响应的证据。
//
// 拒绝的情形：
//   - 纯 WS_PASS（未进 VLESS 终审）：WS 握手对数据面毫无预测力；
//   - VLESS_FAIL 但无 DataProven：不论 timeout / refused / reset /
//     protocol / other，一律拒绝并按归类写明原因；
//   - WS_FAIL 或其他阶段失败：WS 阶段已失败。
func degradedFromWSResults(results []proxyScanResult, existing []string, want int) (adopted, rejected []string) {
	if want <= len(existing) {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(existing))
	for _, ip := range existing {
		seen[ip] = struct{}{}
	}
	adopted = make([]string, 0, want-len(existing))
	for _, result := range results {
		if len(existing)+len(adopted) >= want {
			break
		}
		if _, ok := seen[result.IP]; ok {
			continue
		}
		seen[result.IP] = struct{}{}
		switch {
		case result.Stage == "VLESS_FAIL" && result.DataProven:
			adopted = append(adopted, result.IP)
		case result.Stage == "VLESS_FAIL":
			rejected = append(rejected, result.IP+": VLESS 失败（"+degradedRejectReason(result)+"），无数据面成功证据")
		case result.Stage == "skipped":
			rejected = append(rejected, result.IP+": 本轮未实际探测（预算耗尽跳过），不计入失败也不准入")
		case result.Stage == "WS_PASS":
			rejected = append(rejected, result.IP+": 仅 WS 通过，未验 VLESS 数据面")
		default:
			rejected = append(rejected, result.IP+": WS 未通过")
		}
	}
	return adopted, rejected
}

// degradedRejectReason 把 VLESS 失败的归类翻成人话，供拒绝日志使用。
// 只做展示，不参与准入判定——准入只认 DataProven。
func degradedRejectReason(result proxyScanResult) string {
	switch result.ErrorClass {
	case failureTimeout:
		return "数据面超时"
	case failureRefused:
		return "连接被拒，对端 443 无人监听"
	case failureUnreachable:
		return "无路由/网络不可达"
	case failureHandshake:
		return "TLS 握手失败"
	case failureProtocol:
		return "连接被重置/协议错误"
	case "":
		return "失败原因未归类"
	default:
		return "未归类失败（" + result.ErrorClass + "）"
	}
}

// autoApplyProxyPool 跑一轮全量重挑并换池。
//
// manual 为真表示用户显式点了「立即优选」：缴费的是用户自己的耐心，
// 因此不受后台防抖约束——既不查 30 分钟冷却，也不查「现池还健康就别动」
// 的健康闸。手动供给与扶持登基早就是这个口径，全量重挑没理由例外。
//
// PROXY_AUTO_APPLY 仍然必须启用：它决定这套代理考试参数与终审流程是否可用，
// 与「谁触发」无关。runOptimize 入口已就该开关给出明确报错，这里保留同一
// 判断作防御，避免将来新增调用方绕过入口。
//
// 返回值 applied 表示「本轮真的把池写下去了」（换池或同集合复核落盘）。
// 调用方不能用 UpdatedAt 变化来判断：在位探针可能在漏斗跑的几分钟里剔人，
// 那样会把探针的动作算成优选的战果，甚至把失败报成成功。
func (a *app) autoApplyProxyPool(parent context.Context, manual bool) (applied bool) {
	// 手动「立即优选」会经此路径：排队等待正在跑的考试，而不是拒绝。
	if !lockTimeout(&a.proxyScanMu, 60*time.Second) {
		log.Printf("proxy auto apply skipped: another proxy scan still running after 60s")
		a.setProxyPoolError("已有优选在运行（等待 60 秒未结束），请稍后再试", nil)
		return
	}
	defer a.proxyScanMu.Unlock()
	cfg := defaultProxyAutoConfig()
	if cfg.Host == "" || cfg.Path == "" {
		a.setProxyPoolError("PROXY_AUTO_HOST/PATH 未配置，无法优选", nil)
		return
	}
	if !cfg.Enabled {
		return
	}
	if strings.ContainsAny(cfg.Host, "/\\") || !strings.HasPrefix(cfg.Path, "/") {
		a.setProxyPoolError("PROXY_AUTO_HOST/PATH 格式非法，无法优选", nil)
		return
	}
	if cfg.Port < 1 || cfg.Port > 65535 || cfg.Concurrency < 1 || cfg.Concurrency > 100 || cfg.MaxLatency < 1 || cfg.MaxLatency > 30000 || cfg.PoolSize < 1 || cfg.PoolSize > 50 || cfg.MinPool < 1 || cfg.MinPool > cfg.PoolSize {
		a.setProxyPoolError("候选/漏斗参数超出允许范围，无法优选", nil)
		return
	}
	var vlessTemplate map[string]any
	if cfg.VLESS.Enabled {
		if err := cfg.VLESS.validate(); err != nil {
			a.setProxyPoolError("VLESS 终审配置无效；保留旧池: "+err.Error(), nil)
			return
		}
		var err error
		vlessTemplate, err = loadVLESSOutboundTemplate(cfg.VLESS.TemplatePath)
		if err != nil {
			a.setProxyPoolError("VLESS 终审不可用；保留旧池: "+err.Error(), nil)
			return
		}
	}
	snapshot := a.proxyCandidateSnapshot()
	candidates := snapshot.IPs
	if len(candidates) == 0 {
		return
	}
	// ---- 三层漏斗 ----
	// 3层·初筛：全量候选做大规模 TCP/TLS RTT 测试（参考原项目 cfip/scan.go
	// 的 runRTTTest 分层思想），高并发一次过，按延迟排序取前 N。
	// 2层·精筛：仅对初筛短名单做 WS 101 精筛 + 可选 VLESS 数据面终审。
	// 1层·在位：active pool 粘性分发（cfnat -sticky）。
	// 初筛并发默认 100：CFST 实证经验——高并发探测会被运营商/CF 判为网络
	// 扫描而触发临时限制（症状：连续几轮通过数递减，停一阵自愈）。
	// 需要提速可调高，但出现递减症状时应回调（README 诊断指引）。
	prescreenConcurrency := envInt("PROXY_AUTO_PRESCREEN_CONCURRENCY", 100)
	if prescreenConcurrency < 10 || prescreenConcurrency > 1000 {
		prescreenConcurrency = 100
	}
	shortlistSize := envInt("PROXY_AUTO_SHORTLIST", 60)
	if shortlistSize < cfg.PoolSize || shortlistSize > 500 {
		shortlistSize = 60
	}
	shortlist := candidates
	if len(candidates) > shortlistSize {
		prescreenCfg := proxyScanConfig{
			Host:        cfg.Host,
			Port:        cfg.Port,
			Concurrency: prescreenConcurrency,
			MaxLatency:  cfg.MaxLatency,
			TLS:         true,
		}
		// scanProxyIPs 返回按 (通过, 延迟升序) 排列；直接取延迟达标的前 N。
		prescreen := scanProxyIPs(parent, candidates, prescreenCfg)
		shortlist = make([]string, 0, shortlistSize)
		for _, result := range prescreen {
			if result.Error != "" {
				continue
			}
			shortlist = append(shortlist, result.IP)
			if len(shortlist) >= shortlistSize {
				break
			}
		}
		log.Printf("tier3 prescreen: %d candidates -> %d shortlist (rtt<=%dms)", len(candidates), len(shortlist), cfg.MaxLatency)
	}
	results := scanProxyWebSockets(parent, shortlist, cfg)
	applyCandidateSourcePriority(results, snapshot.SourceByIP)
	passed := make([]string, 0, cfg.PoolSize)
	finalStage := "WS"
	if cfg.VLESS.Enabled {
		finalStage = "VLESS"
		passed = probeVLESSPool(parent, results, cfg.Port, min(cfg.VLESS.MaxCandidates, len(results)), cfg.VLESS, vlessTemplate)
		passed = rankVLESSPassesBySpeed(parent, passed, results, cfg.Port, cfg.VLESS, vlessTemplate)
		// 机房软偏好：Mbps 序排定后分区重排，截断时充裕取偏好、匮乏放行。
		passed = orderPassedByColoPreference(passed, results, poolColoPreferenceSet())
		// 片区降权：整片高失败率的段排到末尾（不淘汰，候选不够照样用）。
		passed = orderPassedBySegmentHealth(passed, results)
		passed = spreadBySegment(passed, cfg.PoolSize)
	} else {
		for _, result := range results {
			if result.Error == "" {
				passed = append(passed, result.IP)
			}
		}
		passed = orderPassedByColoPreference(passed, results, poolColoPreferenceSet())
		passed = orderPassedBySegmentHealth(passed, results)
		passed = spreadBySegment(passed, cfg.PoolSize)
	}
	// 入池延迟上限对「全量重挑」同样适用。旧实现只有踢人补位和在位替补查
	// 800ms，自动换池却按 PROXY_AUTO_MAX_LATENCY（现场 5000ms）放行，于是
	// 1.5~2.5 秒的 IP 也能以「WS_PASS」进池，粘性设备随即卡死。所有换池
	// 入口必须同一口径，否则闸门等于各自解释。
	if ceiling := poolEntryMaxLatencyMs(); len(passed) > 0 {
		before := len(passed)
		passed = admitPassedUnderCeiling(passed, results, ceiling)
		if dropped := before - len(passed); dropped > 0 {
			log.Printf("auto apply rejected %d candidates over the %dms entry ceiling", dropped, ceiling)
		}
	}
	// 考试结果的有效性就地复验：进池的必须是公网 IPv4 且属于 CF 边缘段。
	// 不用「候选快照有没有变」作判据——候选池是轮转采样队列，每轮重新排序，
	// 拿它的抖动去否定一个端到端验过的结论属于本末倒置（详见 filterPoolAdmissibleIPs）。
	if admitted, rejected := filterPoolAdmissibleIPs(passed); len(rejected) > 0 {
		log.Printf("proxy auto apply: dropped %d passed candidates failing pool gate: %v", len(rejected), rejected)
		passed = admitted
	}
	if len(passed) < cfg.MinPool {
		message := fmt.Sprintf("%s 终审仅 %d 个通过，低于最小池 %d", finalStage, len(passed), cfg.MinPool)
		// L3 降级：VLESS 终审没全过，但个别候选在终审里已证明数据面
		// 真的通过（隧道里收到过源站响应，只是测速端点/字节数没达标）。
		// 池子空掉比用「有证据但不完美」的候选严重得多，故降级采用并
		// 明确标记。没有数据面证据的失败一律不采——凑数进池比空池更糟。
		if cfg.VLESS.Enabled {
			extra, rejected := degradedFromWSResults(results, passed, cfg.MinPool)
			if len(rejected) > 0 {
				log.Printf("proxy pool DEGRADED: 拒绝降级 %d 个候选: %v", len(rejected), rejected)
			}
			// 降级候选同样要过入池上限与准入闸：直接 append 会把
			// WS 2~5 秒的慢 IP 经后门塞进粘性转发的池子——800ms 上限
			// 的注释写明「所有换池入口必须同一口径」。
			if ceiling := poolEntryMaxLatencyMs(); len(extra) > 0 {
				before := len(extra)
				extra = admitPassedUnderCeiling(extra, results, ceiling)
				if dropped := before - len(extra); dropped > 0 {
					log.Printf("proxy pool DEGRADED: 拒绝降级 %d 个候选（超过 %dms 入池上限）", dropped, ceiling)
				}
			}
			if admitted, rejected := filterPoolAdmissibleIPs(extra); len(rejected) > 0 {
				log.Printf("proxy pool DEGRADED: 拒绝降级 %d 个候选（未通过准入闸）: %v", len(rejected), rejected)
				extra = admitted
			}
			if len(extra) > 0 {
				passed = append(passed, extra...)
				message = fmt.Sprintf("%s；已降级采用 %d 个有数据面成功证据的 VLESS 失败候选", message, len(extra))
				log.Printf("proxy pool DEGRADED: %s", message)
			}
		}
		if len(passed) < cfg.MinPool {
			// 保留旧池之前先证明它活着，否则等于把一具尸体留在岗位上。
			// 2026-10-22 事故：建池 0 通过，旧池唯一 IP 早已 dial timeout，
			// 系统照样报「保留旧池」，对外看起来一切正常。
			alive, total := a.verifyActivePoolAlive()
			if alive > 0 {
				a.setProxyPoolError(fmt.Sprintf("%s；旧池 %d/%d 存活，继续沿用", message, alive, total), results)
				return
			}
			a.setProxyPoolError(fmt.Sprintf("%s；旧池 %d 个成员全部不可达，已无法沿用", message, total), results)
			log.Printf("proxy pool CRITICAL: 建池失败且旧池全灭（0/%d 可达），需要人工介入", total)
			return
		}
	}
	current := a.proxyActivePoolSnapshot()
	// 锁位模式是用户亲手点名「只转发这些」，自动重挑无权改动。
	// 手动优选期间用户仍可能临时打开锁（开跑前的预检来得及，提交前这一刻
	// 却未必），所以这里也要留下原因；后台周期任务则不写，避免每轮都把
	// 「池已锁定」刷成最新错误、盖掉真正的故障信息。
	if current.Lock {
		if manual {
			a.setProxyPoolError("生效池已锁定（只转固定的），本轮未改动；先解锁再优选", nil)
		}
		log.Printf("proxy auto apply skipped: pool lock enabled, user-pinned members only")
		return
	}
	domains := a.poolDomainMembers()
	passed, domains = carryPinnedIntoRebuild(passed, domains, current)
	now := time.Now()
	pool := proxyActivePool{
		UpdatedAt: now, Host: cfg.Host, Path: cfg.Path,
		IPs: passed, Domains: domains, Results: results,
		Priority: append([]string(nil), current.Priority...),
		Disabled: append([]string(nil), current.Disabled...),
	}
	if sameIPSet(current.IPs, passed) && sameStringSet(current.Domains, domains) {
		// 同池复验也要走统一落点：直接保存会绕过域名质量闸、
		// planFixedMembers 校验、Disabled 清理、成员通道对齐与 poolBlock
		// 清除。参数未变时 applyPoolLocked 走不重启通道（只落盘+播种），
		// 不掐断在途连接。
		if err := a.applyProxyPool(pool, current); err != nil {
			a.setProxyPoolError("复验后落池失败，保留旧池: "+err.Error(), results)
			return false
		}
		log.Printf("proxy active pool reverified: %d IPs", len(passed))
		return true
	}
	// 自动全量终审与 scheduler 共用全局冷却，避免互相顶池；手动点
	// 「立即优选」是明确指令，按手动口径放行，不受冷却与健康闸约束。
	// 池子若一字未变，上面已经提前返回，不会为了放行白白重拨 cfnat。
	verdict := poolSwitchVerdict{Allowed: true, Reason: "manual"}
	if !manual {
		verdict = a.allowPoolSwitch(now, "auto-apply")
	}
	if !verdict.Allowed {
		a.recordPoolBlock(verdict, "auto-apply", now)
		var reason string
		if !verdict.NextAllowedAt.IsZero() {
			reason = fmt.Sprintf("自动换池被换池闸拦截（%s），下轮放行约 %s；保留旧池", verdict.Reason, verdict.NextAllowedAt.Format(time.RFC3339))
		} else {
			reason = fmt.Sprintf("自动换池被换池闸拦截（%s）；保留旧池", verdict.Reason)
		}
		a.setProxyPoolError(reason, results)
		return
	}
	if err := a.applyProxyPool(pool, current); err != nil {
		a.setProxyPoolError("自动换池失败，保留旧池: "+err.Error(), results)
		return false
	}
	a.markAutoSwitch(now)
	return true
}

func scanProxyWebSockets(parent context.Context, ips []string, cfg proxyAutoConfig) []proxyScanResult {
	results := make([]proxyScanResult, len(ips))
	// 防御性钳制：不是所有调用方都走 defaultProxyAutoConfig（手动采用
	// 的 gate.Concurrency 直接取请求值），0 会让 sem 变成 cap 为 0 的
	// channel、全部 goroutine 永久阻塞，负数直接 panic。钳到 [1,100]。
	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 100 {
		concurrency = 100
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-parent.Done():
				// 预算耗尽时还没排上槽位：根本没开始测，不得记成超时失败。
				// 此前这里死等槽位，拿到槽后 DialContext 立即报 deadline
				// exceeded，与真实超时无法区分——订阅 WS 粗筛 batch 400、
				// wsTimeout 60 秒时约 280 条尾部候选每轮被记一次失败，
				// 3 次即判 fail。skipped 由各调用方跳过，不入失败账。
				results[i] = proxyScanResult{IP: ip, Stage: "skipped", Error: "等待探测槽位时预算耗尽，未开始探测", ErrorClass: failureTimeout}
				return
			}
			started := time.Now()
			ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.MaxLatency)*time.Millisecond)
			defer cancel()
			colo, err := probeProxyWebSocket(ctx, ip, cfg)
			latency := time.Since(started).Milliseconds()
			results[i] = proxyScanResult{IP: ip, Latency: latency, Colo: colo, Stage: "WS_PASS"}
			if err != nil {
				results[i].Stage = "WS_FAIL"
				results[i].Error = err.Error()
				results[i].ErrorClass = classifyProbeError(err)
			}
		}(i, ip)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if (results[i].Error == "") != (results[j].Error == "") {
			return results[i].Error == ""
		}
		return results[i].Latency < results[j].Latency
	})
	return results
}

// measurePoolEntryLatency 三次 WS 握手取最优延迟（毫秒）、机房码与失败次数。
// 股神/CFST 多发采样思想：丢手即质量——3 测 2 失者不该与 3 测全通同等待遇，
// 失败数交由调用方记录与展示（入池闸只认「全败」与「超上限」两道硬伤）。
// 三次全败时 best=0（调用方以此判「不通」）；colo 取任一次成功握手的机房码。
func measurePoolEntryLatency(ctx context.Context, ip string, cfg proxyAutoConfig) (int64, string, int) {
	best := int64(0)
	colo := ""
	fails := 0
	for range 3 {
		started := time.Now()
		probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		c, err := probeProxyWebSocket(probeCtx, ip, cfg)
		cancel()
		if err != nil {
			fails++
			continue
		}
		if c != "" && colo == "" {
			colo = c
		}
		if ms := time.Since(started).Milliseconds(); best == 0 || ms < best {
			best = ms
		}
	}
	return best, colo, fails
}

// coloPattern 三字 IATA 机房码。CFST 同源手法（XIU2/CloudflareSpeedTest
// task/httping.go）：CF-RAY 的射线 ID 段恒为小写十六进制，全串里唯一的
// 三连大写字母即机房码（如 8f3b2c1d4e5f6a7b-HKG）。
var coloPattern = regexp.MustCompile(`[A-Z]{3}`)

// coloFromCFRay 从 CF-RAY 响应头值提取机房码；空头或无匹配返回空串。
// 空串语义为「无机房情报」——软偏好闸把它当非偏好处理，永不阻塞入池。
func coloFromCFRay(ray string) string {
	return coloPattern.FindString(strings.TrimSpace(ray))
}

// probeTLSConfig 抽成变量便于测试注入（默认用系统根证书校验 SNI）。
var probeTLSConfig = func(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

func probeProxyWebSocket(ctx context.Context, ip string, cfg proxyAutoConfig) (string, error) {
	dialer := &net.Dialer{}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
	if err != nil {
		return "", err
	}
	defer raw.Close()
	// 握手后的读写是裸读：对端完成 TLS 握手后装死，req.Write /
	// ReadResponse 会无限等下去，goroutine 与信号量槽位一起被占住，
	// MaxLatency 形同虚设。DialContext/HandshakeContext 只管建连，
	// 不管建连后的读写——建连成功就按 ctx 的 deadline 兜底，无
	// deadline 时用 cfg.MaxLatency 算一个。
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	} else if cfg.MaxLatency > 0 {
		_ = raw.SetDeadline(time.Now().Add(time.Duration(cfg.MaxLatency) * time.Millisecond))
	}
	tlsConn := tls.Client(raw, probeTLSConfig(cfg.Host))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return "", err
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+cfg.Host+cfg.Path, nil)
	if err != nil {
		return "", err
	}
	req.Host = cfg.Host
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("User-Agent", "cloudflare-tools-auto-probe")
	if err := req.Write(tlsConn); err != nil {
		return "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return "", fmt.Errorf("WS 状态码 %d", resp.StatusCode)
	}
	return coloFromCFRay(resp.Header.Get("CF-RAY")), nil
}

type proxyScanResult struct {
	IP          string `json:"ip"`
	Latency     int64  `json:"latency"`
	Colo        string `json:"colo,omitempty"`
	DataLatency int64  `json:"dataLatency,omitempty"`
	// DataProven 表示本次 VLESS 探测有明确的数据面成功证据（隧道里收到
	// 了源站的 HTTP 响应，见 vless_probe.go 的 Proven）。只有它能证明
	// 候选的数据面真的通过——DataLatency 只是尝试耗时，ErrorClass 只是
	// 失败归类。降级准入只认这一位。
	DataProven   bool    `json:"dataProven,omitempty"`
	DownloadMbps float64 `json:"downloadMbps,omitempty"`
	SourceRank   int     `json:"sourceRank,omitempty"`
	Stage        string  `json:"stage,omitempty"`
	Error        string  `json:"error,omitempty"`
	// ErrorClass 是 Error 的归类（timeout/refused/unreachable/handshake/protocol/other）。
	// 原始报错文本随版本平台变化，直接统计没法用；归类后才能看出「整片被掐」
	// 与「个别地址没了」的区别。
	ErrorClass string `json:"errorClass,omitempty"`
	Note       string `json:"note,omitempty"`
}

func normalizeProxyScan(c proxyScanConfig) (proxyScanConfig, error) {
	auto := defaultProxyAutoConfig()
	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" {
		// 全自动化：手动优选未填 SNI 时直接用 PROXY_AUTO_HOST，用户无需理解协议细节。
		c.Host = auto.Host
	}
	if c.Host == "" {
		return c, errors.New("SNI/Host 未配置（请在 .env 设置 PROXY_AUTO_HOST）")
	}
	if c.Port == 0 {
		c.Port = auto.Port
	}
	if c.Port < 1 || c.Port > 65535 {
		return c, errors.New("端口必须在 1-65535")
	}
	if c.Concurrency == 0 {
		c.Concurrency = 50
	}
	if c.Concurrency < 1 || c.Concurrency > 500 {
		return c, errors.New("并发必须在 1-500")
	}
	if c.MaxLatency == 0 {
		c.MaxLatency = 300
	}
	if c.MaxLatency < 1 || c.MaxLatency > 10000 {
		return c, errors.New("延迟上限必须在 1-10000 ms")
	}
	if c.Limit == 0 {
		c.Limit = 100
	}
	if c.Limit < 1 || c.Limit > 10000 {
		return c, errors.New("扫描数量必须在 1-10000")
	}
	return c, nil
}

func scanProxyIPs(parent context.Context, ips []string, cfg proxyScanConfig) []proxyScanResult {
	results := make([]proxyScanResult, len(ips))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-parent.Done():
				// 与 scanProxyWebSockets 同款：预算耗尽还没排上槽位即
				// skipped，不记 TLS_FAIL。初筛只取 Error=="" 的进短名单，
				// skipped 本轮不进名单，但不被当成失败候选。
				results[i] = proxyScanResult{IP: ip, Stage: "skipped", Error: "等待探测槽位时预算耗尽，未开始探测", ErrorClass: failureTimeout}
				return
			}
			started := time.Now()
			timeout := time.Duration(cfg.MaxLatency+1000) * time.Millisecond
			if timeout < 2*time.Second {
				timeout = 2 * time.Second
			}
			dialer := &net.Dialer{Timeout: timeout}
			ctx, cancel := context.WithTimeout(parent, timeout)
			defer cancel()
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
			if err == nil && cfg.TLS {
				tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
				err = tlsConn.HandshakeContext(ctx)
			}
			if conn != nil {
				_ = conn.Close()
			}
			latency := time.Since(started).Milliseconds()
			results[i] = proxyScanResult{IP: ip, Latency: latency, Stage: "TLS_PASS"}
			if err != nil {
				results[i].Stage = "TLS_FAIL"
				results[i].Error = err.Error()
				results[i].ErrorClass = classifyProbeError(err)
				return
			}
			if latency > int64(cfg.MaxLatency) {
				results[i].Error = fmt.Sprintf("超过延迟上限 %d ms", cfg.MaxLatency)
			}
		}(i, ip)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if (results[i].Error == "") != (results[j].Error == "") {
			return results[i].Error == ""
		}
		return results[i].Latency < results[j].Latency
	})
	return results
}
