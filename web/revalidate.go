package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 在位成员实弹验证（立即优选的「暂停—验—剔」流程）
// ============================================================================
//
// 由来：原来的「立即优选」是先到外面捞一批新候选、跑三层漏斗、再换池。
// 它回答的是「有没有更好的」，却从来不回答「现在占着皇位的这些，到底还通不通」。
// 于是一个坏掉的成员可以一直坐在 -fixed 里分走流量，直到在位验活连续三次
// 摸不到它才下台——而三次探测按默认 10 分钟一轮，就是半小时。
//
// 现在的流程反过来：先暂停转发，把当下正在承载流量的 IP 与域名逐个拉到
// 真实数据面上验一遍，不合格的当场剔除，然后恢复转发。用户点这个按钮时的
// 真实诉求通常是「现在很卡，帮我清理一下」，而不是「去外面找新的」。
//
// 三条硬约束：
//  1. 绝不踢空。全灭说明是验证通路本身坏了（模板失效、出口不通），
//     此时保留原池并明确报错，不能把用户的池清成零。
//  2. 暂停必须有兜底。cfnat 停掉以后无论走哪条返回路径，都要把它重新拉起来。
//  3. 全程持 cfnatCtlMu。换池、踢人、登基、在位验活都排在这把锁后面，
//     验证期间它们要么等，要么快速失败——绝不能有人半路把池改了。

const (
	// revalidateIPBudget 验 IP 的总时间上限。WS 握手单条约 2 秒，
	// 并发 4，池子按 5~10 个算，60 秒足够，超时即判本轮失败。
	revalidateIPBudget = 60 * time.Second
	// revalidateDomainBudget 验域名的总时间上限。域名要跑 VLESS 下载，
	// 单条约 12 秒、并发 3，26 个域名约两分钟。
	revalidateDomainBudget = 240 * time.Second
	// revalidateDomainConcurrency 同时验几个域名。与定时体检保持一致（3），
	// 压满会把 NAS 打满、把每个探针都拖成超时。
	revalidateDomainConcurrency = 3
)

// revalidateMemberResult 单个成员的验证结论，用于回报给前端。
type revalidateMemberResult struct {
	Member     string `json:"member"`
	Kind       string `json:"kind"` // ip / domain
	OK         bool   `json:"ok"`
	LatencyMs  int64  `json:"latencyMs,omitempty"`
	Colo       string `json:"colo,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorClass string `json:"errorClass,omitempty"`
}

// revalidateReport 一次「暂停—验—剔」的完整结果。
type revalidateReport struct {
	StartedAt  time.Time                `json:"startedAt"`
	FinishedAt time.Time                `json:"finishedAt"`
	Paused     bool                     `json:"paused"`
	Tested     int                      `json:"tested"`
	Passed     int                      `json:"passed"`
	Removed    []string                 `json:"removed"`
	Kept       []string                 `json:"kept"`
	Members    []revalidateMemberResult `json:"members"`
	// Aborted 为真表示没有应用任何改动（例如全灭、或池为空）。
	Aborted bool   `json:"aborted,omitempty"`
	Note    string `json:"note,omitempty"`
}

// revalidateState 是正在进行的验证的实时进度，供 /api/status 观测。
// 没有它，前端点了按钮之后要干等一到四分钟，看起来像卡死了。
type revalidateState struct {
	Running   bool
	StartedAt time.Time
	Phase     string
	Tested    int
	Total     int
	// Last 是上一轮验证的结果。前端点完按钮后轮询这里拿结论——
	// 一轮验证可能跑几分钟，用同步 HTTP 干等会让浏览器先超时。
	Last   *revalidateReport
	LastAt time.Time
}

// beginRevalidate 标记验证开始。同一时刻只允许一轮。
func (a *app) beginRevalidate(total int) bool {
	a.revalidateMu.Lock()
	defer a.revalidateMu.Unlock()
	if a.revalidate.Running {
		return false
	}
	a.revalidate = revalidateState{Running: true, StartedAt: time.Now(), Phase: "暂停转发", Total: total}
	return true
}

func (a *app) setRevalidatePhase(phase string, tested int) {
	a.revalidateMu.Lock()
	a.revalidate.Phase = phase
	a.revalidate.Tested = tested
	a.revalidateMu.Unlock()
}

func (a *app) endRevalidate(report revalidateReport) {
	a.revalidateMu.Lock()
	finished := report
	a.revalidate = revalidateState{Last: &finished, LastAt: time.Now()}
	a.revalidateMu.Unlock()
}

// revalidateStatus 给 /api/status 用的只读快照。
func (a *app) revalidateStatus() map[string]any {
	a.revalidateMu.Lock()
	defer a.revalidateMu.Unlock()
	state := a.revalidate
	out := map[string]any{
		"running": state.Running,
		"phase":   state.Phase,
		"tested":  state.Tested,
		"total":   state.Total,
	}
	if state.Running {
		out["startedAt"] = state.StartedAt
	}
	return out
}

// revalidateDetail 是给 /api/cfnat/revalidate 的完整快照：进度 + 上一轮报告。
//
// 报告只走这个专用接口，不进 /api/status。理由：status 是秒级轮询的，
// 把 30 条成员明细塞进去等于每次刷新都多传几 KB，而绝大多数时候没人看。
func (a *app) revalidateDetail() map[string]any {
	out := a.revalidateStatus()
	a.revalidateMu.Lock()
	last, lastAt := a.revalidate.Last, a.revalidate.LastAt
	a.revalidateMu.Unlock()
	if last != nil {
		out["last"] = last
		out["lastAt"] = lastAt
	}
	return out
}

// revalidateTargetCount 当前在位成员总数，用于进度显示。
func (a *app) revalidateTargetCount() int {
	pool := a.proxyActivePoolSnapshot()
	return len(pool.IPs) + len(pool.Domains)
}

// revalidateDependencies 是本流程的四个外部依赖：验 IP、验域名、暂停/恢复转发、落地新池。
//
// 抽成包级变量只为单测能伪造「全灭 / 部分失败 / 全通过 / 落地失败」这几种结局。
// 这些分支恰恰是最要紧的：绝不踢空、恢复转发不能漏。真去 fork 一个 cfnat、
// 真去拨号外网，单测既跑不稳也验证不了任何东西。
//
// 生产永远用下面这份默认实现。
type revalidateDependencies struct {
	checkIPs        func(a *app, ctx context.Context, ips []string, cfg proxyAutoConfig) []revalidateMemberResult
	checkDomains    func(a *app, ctx context.Context, domains []string, base int) []revalidateMemberResult
	stopForwarding  func(a *app) error
	startForwarding func(a *app, pool proxyActivePool) error
	applyPool       func(a *app, pool, current proxyActivePool) error
}

var revalidateDeps = revalidateDependencies{
	checkIPs: func(a *app, ctx context.Context, ips []string, cfg proxyAutoConfig) []revalidateMemberResult {
		if cfg.Host == "" || cfg.Path == "" {
			// 没有握手参数就无从验证 WS 入口，退化成 TCP 可达性。
			return a.revalidateIPsByTCP(ctx, ips, cfg.Port)
		}
		return a.revalidateIPsByWS(ctx, ips, cfg)
	},
	checkDomains: func(a *app, ctx context.Context, domains []string, base int) []revalidateMemberResult {
		return a.revalidateDomains(ctx, domains, base)
	},
	stopForwarding: func(a *app) error {
		return a.cfnat.stop()
	},
	startForwarding: func(a *app, pool proxyActivePool) error {
		// 已经在跑就当作恢复完成。走到这里的路径里有一条会撞上：落地新池
		// 失败时 applyPoolLocked 自己已经用旧池回滚重启过了，再启一次只会
		// 报「cfnat 已在运行」，把一个成功的回滚误报成「需人工介入」。
		if a.cfnat.status().Running {
			return nil
		}
		fixed, priority, err := planFixedMembers(pool)
		if err != nil {
			return err
		}
		cfnatCfg := defaultCFnatConfig()
		cfnatCfg.Fixed = strings.Join(fixed, ",")
		cfnatCfg.Priority = strings.Join(priority, ",")
		cfnatCfg.Fallback = strings.TrimSpace(env("CFNAT_FALLBACK", ""))
		return a.cfnat.start(a.dataDir, cfnatCfg.args(), "")
	},
	applyPool: func(a *app, pool, current proxyActivePool) error {
		return a.applyPoolLocked(pool, current)
	},
}

// revalidateActivePool 暂停转发，逐个验证在位成员，剔除不合格者，然后恢复。
//
// 返回报告而非 error：部分失败要能被看见（剔了谁、留了谁、为什么），
// 只有「整个流程没跑起来」才用 error。
//
// report 是具名返回值：defer 里要把它连同实时进度一起收尾——一轮验证
// 可能跑几分钟，前端靠轮询这里拿结论，不能等到 HTTP 响应才落地。
//
// 调用方必须先调 beginRevalidate 抢到席位；本函数只负责跑完并归还。
// 抢席位放在调用方而不是这里，是为了让 HTTP 入口在起协程之前就能分辨
// 「开跑了」和「已有一轮在跑」——否则用户连点两次都收到成功，第二次静默作废。
func (a *app) revalidateActivePool(parent context.Context) (report revalidateReport, err error) {
	current := a.proxyActivePoolSnapshot()
	report = revalidateReport{StartedAt: time.Now()}
	// 无论从哪条路返回都要把席位还回去，否则页面会永远显示「验证中」。
	// 顺便把 error 也写进报告：前端只轮询 report，看不到 error 返回值，
	// 一个「跑不起来」的失败如果只留在 HTTP 日志里，页面上就是「验证完成」。
	defer func() {
		if report.FinishedAt.IsZero() {
			report.FinishedAt = time.Now()
		}
		if err != nil && report.Note == "" {
			report.Aborted = true
			report.Note = err.Error()
		}
		a.endRevalidate(report)
	}()

	total := len(current.IPs) + len(current.Domains)
	if total == 0 {
		return report, errors.New("在位池是空的，没有可验证的对象")
	}

	// 与「立即优选」共用同一把扫描锁：两者都要占用探测预算，
	// 同时跑只会互相把对方的探针拖成超时。
	if !lockTimeout(&a.proxyScanMu, 60*time.Second) {
		return report, errors.New("已有优选或验证在运行，请稍后再试")
	}
	defer a.proxyScanMu.Unlock()

	cfg := defaultProxyAutoConfig()
	if cfg.Port <= 0 {
		cfg.Port = 443
	}

	// ---- 暂停转发 ----
	// 持 cfnatCtlMu 直到整个流程结束：验证期间任何换池路径都必须排队，
	// 否则会出现「正在验的池」和「实际生效的池」不是同一个的荒唐局面。
	if !lockTimeout(&a.cfnatCtlMu, 30*time.Second) {
		return report, errors.New("换池操作正在运行，请稍后再试")
	}
	defer a.cfnatCtlMu.Unlock()

	if err := revalidateDeps.stopForwarding(a); err != nil {
		return report, fmt.Errorf("暂停转发失败: %w", err)
	}
	report.Paused = true
	log.Printf("revalidate: forwarding paused, testing %d IPs + %d domains", len(current.IPs), len(current.Domains))

	// 无论后面走哪条路，都必须把 cfnat 拉回来。用 defer 而不是在每个
	// return 前排一遍——漏一处就变成「点了按钮，转发永久停摆」。
	// restarted 在 applyPoolLocked 成功后置位（它自己已经把 cfnat 用新池
	// 拉起来了），避免这里再用旧池重启一遍把它覆盖掉。
	restarted := false
	restart := func(pool proxyActivePool) error {
		return revalidateDeps.startForwarding(a, pool)
	}
	defer func() {
		if restarted {
			return
		}
		if rerr := restart(current); rerr != nil {
			// 恢复失败是最严重的一种：转发停着，用户还不知道。
			log.Printf("revalidate: FAILED to restart forwarding: %v", rerr)
			a.setProxyPoolError("在位验证后恢复转发失败，需人工介入: "+rerr.Error(), nil)
			report.Aborted = true
			report.Note = "恢复转发失败，需人工介入: " + rerr.Error()
		}
	}()

	// ---- 验 IP：WS 101 握手 ----
	var ipResults []revalidateMemberResult
	if len(current.IPs) > 0 {
		a.setRevalidatePhase("验证 IP", 0)
		if cfg.Host == "" || cfg.Path == "" {
			// 没有握手参数就无从验证，宁可一人不剔，也不误杀。
			report.Note = "缺 PROXY_AUTO_HOST/PATH，IP 只做了 TCP 可达性检查"
		}
		ipResults = revalidateDeps.checkIPs(a, parent, current.IPs, cfg)
	}

	// ---- 验域名：TCP + VLESS 下载 ----
	var domainResults []revalidateMemberResult
	if len(current.Domains) > 0 {
		a.setRevalidatePhase("验证域名", len(ipResults))
		domainResults = revalidateDeps.checkDomains(a, parent, current.Domains, len(ipResults))
	}
	all := append(ipResults, domainResults...)
	report.Members = all
	report.Tested = len(all)
	report.FinishedAt = time.Now()

	removed := make([]string, 0)
	kept := make([]string, 0)
	for _, r := range all {
		if r.OK {
			report.Passed++
			kept = append(kept, r.Member)
		} else {
			removed = append(removed, r.Member)
		}
	}

	// ---- 绝不踢空 ----
	// 全部不合格意味着「验证通路本身坏了」的概率远大于「所有成员同时死」。
	// 这时候把池清空是最糟的选择：用户既没网，也没得看。
	if len(kept) == 0 {
		report.Aborted = true
		report.Removed = nil
		report.Note = "全部成员都未通过验证，疑似验证通路本身异常；已保留原池未作改动"
		log.Printf("revalidate: all %d members failed, keeping pool untouched (suspicious)", len(all))
		if err := restart(current); err != nil {
			return report, fmt.Errorf("恢复转发失败: %w", err)
		}
		restarted = true
		return report, nil
	}
	report.Removed = removed
	report.Kept = kept

	if len(removed) == 0 {
		report.Note = "全部通过，未剔除任何成员"
		if err := restart(current); err != nil {
			return report, fmt.Errorf("恢复转发失败: %w", err)
		}
		restarted = true
		log.Printf("revalidate: all %d members passed, forwarding resumed", len(all))
		return report, nil
	}

	// ---- 应用新池 ----
	a.setRevalidatePhase("应用结果", len(all))
	pool := current
	pool.UpdatedAt = time.Now()
	pool.IPs = keepOnly(current.IPs, kept)
	pool.Domains = keepOnly(current.Domains, kept)
	pool.Results = keepResults(current.Results, removed)
	// 被剔除的成员也要从 Disabled / Priority 里清掉，否则它们会以
	// 「已关闭成员」的身份继续留在池定义里，下次重启又被读回来。
	pool.Disabled = keepOnly(current.Disabled, kept)
	pool.Priority = keepOnly(current.Priority, kept)

	// applyPoolLocked 会先 stop（已是停的，空操作）再 start，
	// 顺带把新池落盘。这里不直接调 start：落盘失败要能回滚。
	if err := revalidateDeps.applyPool(a, pool, current); err != nil {
		report.Aborted = true
		report.Note = "应用新池失败，已回滚到原池: " + err.Error()
		log.Printf("revalidate: apply failed: %v", err)
		if rerr := restart(current); rerr != nil {
			return report, fmt.Errorf("应用新池失败（%v），且恢复原池转发也失败: %w", err, rerr)
		}
		restarted = true
		return report, nil
	}
	restarted = true // applyPoolLocked 已经用新池把 cfnat 拉起来了
	a.markPoolSwitched(pool.UpdatedAt)
	log.Printf("revalidate: removed %v, %d IPs + %d domains remain", removed, len(pool.IPs), len(pool.Domains))
	report.Note = fmt.Sprintf("剔除 %d 个，保留 %d 个", len(removed), len(kept))
	return report, nil
}

// revalidateIPsByWS 用 WS 101 握手验证一组 IP，这就是 cfnat 实际走的入口。
func (a *app) revalidateIPsByWS(parent context.Context, ips []string, cfg proxyAutoConfig) []revalidateMemberResult {
	cfg.Concurrency = 4
	if cfg.MaxLatency <= 0 || cfg.MaxLatency > 3000 {
		cfg.MaxLatency = 2000
	}
	ctx, cancel := context.WithTimeout(parent, revalidateIPBudget)
	defer cancel()
	results := scanProxyWebSockets(ctx, ips, cfg)
	out := make([]revalidateMemberResult, 0, len(results))
	for i, r := range results {
		out = append(out, revalidateMemberResult{
			Member:     r.IP,
			Kind:       "ip",
			OK:         r.Error == "",
			LatencyMs:  r.Latency,
			Colo:       r.Colo,
			Stage:      "ws",
			Error:      r.Error,
			ErrorClass: r.ErrorClass,
		})
		a.setRevalidatePhase("验证 IP", i+1)
	}
	return out
}

// revalidateIPsByTCP 在缺 WS 参数时退化成 TCP 可达性检查。
// 这判不死「能连上但不是 CF 入口」的地址，所以只在降级路径上用，且报告里注明。
func (a *app) revalidateIPsByTCP(parent context.Context, ips []string, port int) []revalidateMemberResult {
	ctx, cancel := context.WithTimeout(parent, revalidateIPBudget)
	defer cancel()
	out := make([]revalidateMemberResult, 0, len(ips))
	for i, ip := range ips {
		started := time.Now()
		ok := probeTCPAlive(ctx, net.JoinHostPort(ip, fmt.Sprint(port)), 5*time.Second)
		r := revalidateMemberResult{
			Member: ip, Kind: "ip", OK: ok, Stage: "tcp",
			LatencyMs: time.Since(started).Milliseconds(),
		}
		if !ok {
			r.Error = "TCP 不可达"
			r.ErrorClass = failureUnreachable
		}
		out = append(out, r)
		a.setRevalidatePhase("验证 IP", i+1)
	}
	return out
}

// revalidateDomains 验证一组域名。base 是已经验完的 IP 数，用于让进度
// 连续递增——不从 0 起算的话，第一个域名验完会把 tested 从 6 打回 1，
// 页面上的「3/31」会当着用户的面往回跳。
//
// 两级：先 TCP:443，不通即判死；通了再跑 VLESS 短时下载。只做 TCP 会漏掉
// 「能连上但不是我们要找的入口」这类域名——那正是 `mfa.gov.ua` 之流
// 最常见的形态，TCP 一直是通的，流量却根本到不了节点。
//
// 单条失败要复验一次再判死。实测一轮 26 个域名里掉了 3 个，其中
// `cfsaas.080112.xyz` 在上一次定时体检里还是 successRate=0.8 的健康成员——
// 并发 3 压满时偶发超时是常态，一次不过就踢人等于把好成员当垃圾清掉。
func (a *app) revalidateDomains(parent context.Context, domains []string, base int) []revalidateMemberResult {
	speedCfg, template, speedErr := a.domainSpeedProbeConfig()
	speedEnabled := speedErr == nil
	if !speedEnabled {
		log.Printf("revalidate: domain speed probe unavailable, degrading to TCP-only: %v", speedErr)
	}
	ctx, cancel := context.WithTimeout(parent, revalidateDomainBudget)
	defer cancel()

	out := make([]revalidateMemberResult, len(domains))
	sem := make(chan struct{}, revalidateDomainConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for i, domain := range domains {
		wg.Add(1)
		go func(i int, domain string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out[i] = revalidateMemberResult{Member: domain, Kind: "domain", Stage: "skipped", Error: "验证预算用尽", ErrorClass: failureTimeout}
				return
			}
			res := a.revalidateOneDomain(ctx, domain, speedCfg, template, speedEnabled)
			if !res.OK && ctx.Err() == nil && revalidateWorthRetry(res.ErrorClass) {
				// 只在「看起来是偶发」的类别上复验。超时与提前断流在并发压满时
				// 很常见，一次不过就踢人等于把好成员当垃圾清掉；而「拒绝」「握手
				// 失败」是对方的确定回答，复验只是白等一个超时预算。
				if again := a.revalidateOneDomain(ctx, domain, speedCfg, template, speedEnabled); again.OK {
					again.Stage += "+retry"
					res = again
				} else {
					res.Stage += "+retry"
				}
			}
			out[i] = res
			mu.Lock()
			done++
			a.setRevalidatePhase("验证域名", base+done)
			mu.Unlock()
		}(i, domain)
	}
	wg.Wait()
	return out
}

func (a *app) revalidateOneDomain(ctx context.Context, domain string, cfg vlessProbeConfig, template map[string]any, speedEnabled bool) revalidateMemberResult {
	out := revalidateMemberResult{Member: domain, Kind: "domain"}
	started := time.Now()
	dialer := net.Dialer{Timeout: domainProbeTimeout()}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(domain, "443"))
	out.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		out.Stage = "tcp"
		out.Error = err.Error()
		out.ErrorClass = classifyProbeError(err)
		return out
	}
	_ = conn.Close()

	if !speedEnabled {
		// 降级：只报可达性，不因测速通路坏掉而判死。
		out.OK = true
		out.Stage = "tcp-only"
		return out
	}
	probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	metrics, err := probeVLESSCandidateMetrics(probeCtx, domain, 443, cfg, template)
	if err != nil {
		out.Stage = "vless"
		out.Error = err.Error()
		out.ErrorClass = classifyProbeError(err)
		return out
	}
	out.OK = true
	out.Stage = "vless"
	out.LatencyMs = metrics.Duration.Milliseconds()
	return out
}

// revalidateWorthRetry 判断一个失败类别是否值得复验一次。
//
// 值得：超时、提前断流、归不了类的——这些在并发压满、瞬时丢包时很常见，
// 复验一次能分清「偶发」和「真死」。
// 不值得：拒绝、无路由、握手失败——那是对方的确定回答，复验只是白等一个
// 超时预算，还拖慢整轮。
func revalidateWorthRetry(class string) bool {
	switch class {
	case failureTimeout, failureProtocol, failureOther, failureNone:
		return true
	}
	return false
}

// keepOnly 返回 items 中也在 keep 里的成员，保持原顺序。
func keepOnly(items, keep []string) []string {
	if len(items) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		allowed[k] = struct{}{}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := allowed[item]; ok {
			out = append(out, item)
		}
	}
	return out
}

// keepResults 去掉被剔除成员的扫描成绩，保留其余记录原样。
func keepResults(results []proxyScanResult, removed []string) []proxyScanResult {
	if len(results) == 0 || len(removed) == 0 {
		return results
	}
	gone := make(map[string]struct{}, len(removed))
	for _, m := range removed {
		gone[m] = struct{}{}
	}
	out := make([]proxyScanResult, 0, len(results))
	for _, r := range results {
		if _, ok := gone[r.IP]; ok {
			continue
		}
		out = append(out, r)
	}
	return out
}
