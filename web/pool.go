package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type proxyActivePool struct {
	UpdatedAt time.Time         `json:"updatedAt,omitempty"`
	Host      string            `json:"host,omitempty"`
	Path      string            `json:"path,omitempty"`
	IPs       []string          `json:"ips"`
	Domains   []string          `json:"domains,omitempty"`
	Results   []proxyScanResult `json:"results,omitempty"`
	Error     string            `json:"error,omitempty"`
	// ErrorAt 是 Error 的写入时间。以前只有自由文本，下游只能靠正则从中文句子里
	// 抠「下轮放行时间」来判断这条报错是不是历史残留（见 scripts/cf_inspect.py）。
	// 加时间戳后，粘滞字段终于能自证新旧。
	ErrorAt time.Time `json:"errorAt,omitempty"`

	// MemberChannels 记录每个在位 IP 入池时的来源渠道（user/cfdata/subscription/bestcf）。
	// 候选快照每 6 小时整体换一份，且健康池会长期保留旧成员，所以「当前候选名单里
	// 有没有它」不能反推历史来源——不落这份映射，旧成员会被整批记成「手动」。
	MemberChannels map[string]string `json:"memberChannels,omitempty"`
	// Disabled 是用户显式关闭的成员（IP 或域名）：仍在位、仍可体检，但不进
	// cfnat -fixed，绝不参与转发。为空即「全开」。
	Disabled []string `json:"disabled,omitempty"`
	// Priority 是「顶到最上」的成员：扶持登基自动入列，也可由用户手动置顶。
	// 它们排在 -fixed 最前，锁位开启时是唯一被转发的集合。
	Priority []string `json:"priority,omitempty"`
	// Lock 为真时 cfnat 只对 Priority 成员转发——用户亲手决定谁承载流量。
	Lock bool `json:"lock,omitempty"`
}

func (a *app) loadProxyActivePool() {
	data, err := os.ReadFile(filepath.Join(a.dataDir, proxyActivePoolFile))
	if err != nil {
		return
	}
	var pool proxyActivePool
	if json.Unmarshal(data, &pool) != nil {
		return
	}
	valid := make([]string, 0, len(pool.IPs))
	for _, raw := range pool.IPs {
		if ip := net.ParseIP(raw); isPublicIPv4(ip) {
			valid = append(valid, ip.To4().String())
		}
	}
	pool.IPs = valid
	validDomains := make([]string, 0, len(pool.Domains))
	for _, domain := range pool.Domains {
		if validCandidateHostname(domain) {
			validDomains = append(validDomains, domain)
		}
	}
	pool.Domains = validDomains
	a.activeMu.Lock()
	a.activePool = pool
	a.activeMu.Unlock()
}

func (a *app) saveProxyActivePool(pool proxyActivePool) error {
	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, proxyActivePoolFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *app) proxyActivePoolSnapshot() proxyActivePool {
	a.activeMu.RLock()
	defer a.activeMu.RUnlock()
	pool := a.activePool
	pool.IPs = append([]string(nil), pool.IPs...)
	pool.Domains = append([]string(nil), pool.Domains...)
	pool.Results = append([]proxyScanResult(nil), pool.Results...)
	if pool.MemberChannels != nil {
		channels := make(map[string]string, len(pool.MemberChannels))
		for ip, channel := range pool.MemberChannels {
			channels[ip] = channel
		}
		pool.MemberChannels = channels
	}
	return pool
}

// reconcileMemberChannels 维护「在位成员 → 入池来源」映射。
//
// 以入池时为准：已在映射里的成员不改写（哪怕当前候选名单把他算给了别人）；
// 新成员先查本轮候选快照，查不到就只能是手动扶持/手动固定，记 user。
// 只在 applyPoolLocked 这一个提交点调用，所有换池入口天然共用。
func (a *app) reconcileMemberChannels(pool, current proxyActivePool) map[string]string {
	if len(pool.IPs) == 0 {
		return nil
	}
	snapshot := a.proxyCandidateSnapshot().SourceByIP
	// 旧版本落盘的池没有 MemberChannels，用上次扫描留下的 SourceRank 补一次：
	// 它比「当前候选名单」更接近入池时的真相。
	legacyRank := make(map[string]int, len(current.Results))
	for _, result := range current.Results {
		legacyRank[result.IP] = result.SourceRank
	}
	out := make(map[string]string, len(pool.IPs))
	for _, ip := range pool.IPs {
		if channel := current.MemberChannels[ip]; channel != "" {
			out[ip] = channel
			continue
		}
		if rank, ok := legacyRank[ip]; ok && rank >= 0 && rank < len(proxyCandidateSourceOrder) {
			out[ip] = proxyCandidateSourceOrder[rank]
			continue
		}
		if channel := snapshot[ip]; channel != "" {
			out[ip] = channel
			continue
		}
		out[ip] = "user"
	}
	return out
}
func (a *app) importedPreferredDomains() []string {
	// 从 v2rayn 导出的常见优选域名固定列表，用于候选池与兜底
	return []string{"bestcf.030101.xyz", "cdn.2020111.xyz", "cdns.doon.eu.org", "cf.0sm.com", "cf.877771.xyz", "cf.877774.xyz", "cf.900501.xyz", "cfip.1323123.xyz", "cfip.cfcdn.vip", "cfip.xxxxxxxx.tk", "cloudflare.182682.xyz", "cloudflare-dl.byoip.top", "cloudflare-ip.mofashi.ltd", "fn.130519.xyz", "freeyx.cloudflare88.eu.org", "nrt.xxxxxxxx.nyc.mn", "nrtcfdns.zone.id", "saas.sin.fan", "tencentapp.cn", "xn--b6gac.eu.org", "777.ai7777777.xyz", "store.ubi.com", "serviceshub.samsclub.com", "www.allianz.com", "www.decathlon.com", "www.asda.com", "www.shopify.com", "cookiebot.com", "bluehost.com", "nexusmods.com", "glassdoor.com", "www.jimdo.com", "openai.com", "www.sage.com", "www.speedtest.net", "investor.apple.com", "markmonitor.com", "store.epicgames.com", "mycareer.verizon.com", "onetrust.com", "www.affirm.com", "www.zendesk.com", "www.doordash.com", "digitalocean.com", "ringcentral.com", "chrono24.com", "www.wto.org", "www.mskcc.org", "cdn.jsdelivr.net", "www.broadcom.com", "www.nestle.com", "homecare.stryker.com", "www.emerson.com", "visit.honeywell.com", "polestar.com", "auctions.ihg.com.cn", "woodsbagot.com", "www.transunion.hk", "www.hongkongairport.com", "deepin.org", "www.police.uk", "www.leics.police.uk", "china.mfa.gov.ua", "www.visa.cn", "visa.com", "www.tpg.com", "www.wilshire.com", "engage.cloudflareclient.com", "cloudflare-eth.com", "cloudflare.dev", "cloudflare.net", "r2.dev", "pages.dev", "cdnjs.com", "cloudflare-ech.com", "static.cloudflareinsights.com", "ln.edu.hk", "www.ntu.edu.sg", "www.ox.ac.uk", "columbia.edu", "www.udacity.com", "for.edu.sg", "www.researchgate.net", "skk.moe", "cf.090227.xyz", "cf.3666888.xyz", "ct.877774.xyz", "ct.cloudflare.byoip.top", "cu.877774.xyz", "cu.cloudflare.byoip.top", "cmcc.877774.xyz", "cm.cloudflare.byoip.top"}
}

// lockTimeout 自旋等待锁至多 d：手动操作排队接力后台任务，不再立即吃 429 闭门羹。
// 全局锁序规范：refreshMu → proxyScanMu → cfnatCtlMu，任何交叉持锁都是死锁隐患。
func lockTimeout(mu *sync.Mutex, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if mu.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (a *app) applyProxyPool(pool, current proxyActivePool) error {
	// 来源映射在取 cfnat 锁之前结算：这里只读候选快照，不去碰候选锁与
	// cfnat 锁的交叉顺序。
	pool.MemberChannels = a.reconcileMemberChannels(pool, current)
	a.cfnatCtlMu.Lock()
	defer a.cfnatCtlMu.Unlock()
	return a.applyPoolLocked(pool, current)
}

// errPoolRebaseAborted 表示 rebase 闭包判定本轮不必提交
// （例如换入目标已在位、缺口已补满），不是错误，调用方直接返回。
var errPoolRebaseAborted = errors.New("pool rebase aborted")

// errPoolChangedDuringSubmit 表示提交窗口内已有别的路径换池：
// 调用方应放弃本轮（不重试、不覆盖），下一轮再按新池重算。
var errPoolChangedDuringSubmit = errors.New("pool changed during submit")

// applyPoolRebased 把「基于旧快照算出的改动」合到最新快照上再提交。
//
// 后台长探测（调度器数分钟、补位 6 分钟、reconcile 排队数分钟）期间，
// 验活/revalidate/关闭席等路径随时可能换池；直接拿旧快照提交会把
// 并发改动覆盖掉，甚至复活刚剔的死成员。rebase 在 cfnatCtlMu 下收到
// 最新快照，返回要提交的池；返回 ok=false 即本轮作罢，不提交。
//
// 提交分三步，缺一不可：
//  1. 锁内重取快照并算出要提交的池；
//  2. 解锁后在锁外结算来源映射（只读候选快照，不与 cfnatCtlMu 交叉持锁，
//     见 applyProxyPool 的注释）；
//  3. 再取锁并 CAS：窗口内若已有别的路径换池（内存池 UpdatedAt 变了），
//     放弃本轮——宁可少换一次，也不拿旧基底覆盖新池。
//     UpdatedAt 能当版本号用，是因为每次成功提交都会把它写成当下时间
//     （见各提交点的 pool.UpdatedAt = now 与 applyPoolLocked 的内存写回）；
//     只改报错的 setProxyPoolError 不动它，但那不改成员，不影响 CAS。
func (a *app) applyPoolRebased(rebase func(fresh proxyActivePool) (proxyActivePool, bool)) error {
	if !lockTimeout(&a.cfnatCtlMu, 10*time.Second) {
		return errors.New("pool control busy")
	}
	fresh := a.proxyActivePoolSnapshot()
	pool, ok := rebase(fresh)
	a.cfnatCtlMu.Unlock()
	if !ok {
		return errPoolRebaseAborted
	}
	pool.MemberChannels = a.reconcileMemberChannels(pool, fresh)
	if !lockTimeout(&a.cfnatCtlMu, 10*time.Second) {
		return errors.New("pool control busy")
	}
	defer a.cfnatCtlMu.Unlock()
	latest := a.proxyActivePoolSnapshot()
	if !latest.UpdatedAt.Equal(fresh.UpdatedAt) {
		return errPoolChangedDuringSubmit
	}
	return a.applyPoolLocked(pool, latest)
}

// planFixedMembers 决定 cfnat -fixed 的成员与顺序：
//  1. 置顶（扶持登基）成员在最前——「顶到最上进行转发」；
//  2. 其余未关闭成员随后；
//  3. Lock 开启时只保留置顶成员，杜绝劣质成员分走流量。
//
// 返回空集合即报错：绝不用空 -fixed 重启 cfnat。
func planFixedMembers(pool proxyActivePool) ([]string, []string, error) {
	disabled := make(map[string]bool, len(pool.Disabled))
	for _, m := range pool.Disabled {
		disabled[m] = true
	}
	all := make([]string, 0, len(pool.IPs)+len(pool.Domains))
	all = append(all, pool.IPs...)
	all = append(all, pool.Domains...)
	exists := make(map[string]bool, len(all))
	for _, m := range all {
		exists[m] = true
	}

	priority := make([]string, 0, len(pool.Priority))
	seen := make(map[string]bool, len(all))
	for _, m := range pool.Priority {
		if !exists[m] || disabled[m] || seen[m] {
			continue
		}
		seen[m] = true
		priority = append(priority, m)
	}
	fixed := append([]string(nil), priority...)
	if !pool.Lock {
		for _, m := range all {
			if disabled[m] || seen[m] {
				continue
			}
			seen[m] = true
			fixed = append(fixed, m)
		}
	}
	if len(fixed) == 0 {
		if pool.Lock {
			return nil, nil, fmt.Errorf("锁位已开启，但没有任何可转发的扶持登基成员")
		}
		return nil, nil, fmt.Errorf("没有可转发的成员：生效池成员已全部关闭")
	}
	return fixed, priority, nil
}

// disabledMemberSet 返回用户显式关闭的成员集合。空集合表示全开。
func disabledMemberSet(pool proxyActivePool) map[string]bool {
	if len(pool.Disabled) == 0 {
		return nil
	}
	set := make(map[string]bool, len(pool.Disabled))
	for _, member := range pool.Disabled {
		set[member] = true
	}
	return set
}

// forwardingIPs 返回真正参与转发的 IP（剔除用户关闭的成员）。
//
// 为什么单列一个函数：换池闸、健康判定、救援判死以前都直接读 pool.IPs，
// 而被关闭的成员虽然还在池里，却根本不进 cfnat -fixed。拿它们去算
// 「整组是不是全死了」「池子健不健康」「人手够不够」，得出的结论与用户
// 实际感受到的转发质量无关：
//   - 一个健康的关闭成员会把健康闸顶住，让本该换掉的坏线换不出去；
//   - 一个死掉的关闭成员会参与「整组已死」判定，但救它没有任何意义。
//
// Lock（只转置顶）是用户主动设定的转发范围，语义上确实只转 Priority；
// 但那是另一套独立开关，改动它会连带改变锁位用户的冷却行为，不在本次
// 修复范围内。这里只处理 Disabled 这一个明确的「不转发」信号。
func forwardingIPs(pool proxyActivePool) []string {
	disabled := disabledMemberSet(pool)
	if len(disabled) == 0 {
		return append([]string(nil), pool.IPs...)
	}
	out := make([]string, 0, len(pool.IPs))
	for _, ip := range pool.IPs {
		if !disabled[ip] {
			out = append(out, ip)
		}
	}
	return out
}

// forwardingMembers 返回真正参与转发的成员（IP + 域名，剔除 Disabled）。
func forwardingMembers(pool proxyActivePool) []string {
	disabled := disabledMemberSet(pool)
	out := make([]string, 0, len(pool.IPs)+len(pool.Domains))
	for _, ip := range pool.IPs {
		if !disabled[ip] {
			out = append(out, ip)
		}
	}
	for _, domain := range pool.Domains {
		if !disabled[domain] {
			out = append(out, domain)
		}
	}
	return out
}

// pruneDisabledMissingIPs 清掉 Disabled 里已经不在生效池的 IP。
//
// Disabled 的语义是「在位但被用户关闭」。全量重挑、手动采用、调度换人都会
// 整批替换 IP，却把 Disabled 原样复制过去，于是早已离池的 IP 会永远挂着关闭
// 标记：既不转发、也不体检（体检只认在位成员），页面上还一直显示成关闭成员。
// 2026-11-10 生产上就攒了 5 个这样的幽灵，关闭席看起来有 6 个成员，实际只剩
// 1 个域名还在池里。
//
// 只处理 IP：域名闸门会临时把域名移出 Domains，保留关闭标记才能防止它恢复
// 之后被自动放回转发。域名若真被移出池，由域名链自己收尾。
func pruneDisabledMissingIPs(pool proxyActivePool) []string {
	if len(pool.Disabled) == 0 {
		return nil
	}
	out := make([]string, 0, len(pool.Disabled))
	for _, member := range pool.Disabled {
		if net.ParseIP(member) != nil && !containsString(pool.IPs, member) {
			continue
		}
		out = append(out, member)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// carryPinnedIntoRebuild 把用户置顶的成员带进「全量重挑」后的新池。
//
// 自动换池只负责换掉不合格的成员，不能顺手把用户点名要的 IP/域名冲掉。
// 只保留现池里仍存在的置顶成员：它们若已死，在位验活会按同一套规则剔除，
// 这里不复活、也不延长寿命。
func carryPinnedIntoRebuild(ips, domains []string, current proxyActivePool) ([]string, []string) {
	for _, member := range current.Priority {
		if net.ParseIP(member) != nil {
			if containsString(current.IPs, member) && !containsString(ips, member) {
				ips = append(ips, member)
			}
			continue
		}
		if containsString(current.Domains, member) && !containsString(domains, member) {
			domains = append(domains, member)
		}
	}
	return ips, domains
}

// admittedPoolDomains 把落盘生效池里的域名过一遍质量闸。
// 返回新切片，不修改入参；被拦下的域名仍留在体检名单上（见 domainProbeTargets），
// 所以它们的成绩一旦好转就会在下一轮换池时自动回池。
//
// guaranteed 是用户亲手固定的成员（Priority）。它们**不过闸**：用户在页面上
// 明确指定「就用这个域名」时，系统再有意见也不该背着人把它删掉。这与
// PROXY_DOMAIN_FORWARD 的语义一致——显式指定视为运营者担保。此前扶持登基
// 收下的域名若体检为 dead/unstable，会被这里静默过滤，而接口回报仍是
// 「已登基」，属于报喜不报忧。
func (a *app) admittedPoolDomains(domains, guaranteed []string) []string {
	if len(domains) == 0 {
		return domains
	}
	pinned := make(map[string]bool, len(guaranteed))
	for _, member := range guaranteed {
		pinned[member] = true
	}
	admitted := make([]string, 0, len(domains))
	for _, domain := range domains {
		if pinned[domain] {
			if !a.domainUsableForForwarding(domain) {
				log.Printf("domain gate: %s 体检未通过，但被用户固定，按担保放行", domain)
			}
			admitted = append(admitted, domain)
			continue
		}
		if a.domainUsableForForwarding(domain) {
			admitted = append(admitted, domain)
		}
	}
	return admitted
}

// gatePoolDomains 对生效池里的域名过质量闸，并保留「闸门只能筛目标、不能把
// 服务拦死」这一条兜底。
//
// 单靠 admittedPoolDomains 会出现这种形态：IP 侧全灭或全被关闭，域名又都被体检
// 判坏，planFixedMembers 拿到空集合直接报错，于是「池里有域名、1234 上一个目标
// 都没有」。兜底只在「确实再也没有别的目标」时放行，体检结论仍留在账本上，
// 下一轮照常复检。
func (a *app) gatePoolDomains(pool proxyActivePool) []string {
	original := pool.Domains
	admitted := a.admittedPoolDomains(original, pool.Priority)
	if len(admitted) > 0 || len(original) == 0 {
		return admitted
	}
	if len(forwardingIPs(pool)) > 0 {
		return admitted
	}
	log.Printf("domain gate DEGRADED: 无可用 IP 且域名全部未通过体检，按兜底放行 %d 个域名", len(original))
	return original
}

// applyPoolLocked 在已持有 cfnatCtlMu 时执行换池（踢人/手动采用等已持锁路径复用）。
func (a *app) applyPoolLocked(pool, current proxyActivePool) error {
	// 换池成功，历史报错随之作废——pool 是按值传的本地副本，这里清掉
	// 只影响本次要落盘的那一份；失败路径不落盘，报错仍由调用方另行记录。
	// 旧实现只清 poolBlock（内存态），Error/ErrorAt 从不清除：手动换池
	// 成功后 GUI 仍挂着上一轮的拦截原因。
	pool.Error = ""
	pool.ErrorAt = time.Time{}
	// 落盘的生效池可能带着体检已判坏的域名（闸门上线前写入的历史遗留）。
	// 启动、手动采用、立即优选都走这里，若不过闸，重启就是把坏域名重新塞回
	// -fixed 的通道——闸门只在建池时生效、在重启时失效，等于没有。
	pool.Domains = a.gatePoolDomains(pool)
	// 关闭标记只对在位成员有意义：整批换 IP 时原样复制 Disabled 会留下
	// 「已经离池、却永远显示为关闭」的幽灵，体检也不会再碰它们。
	pool.Disabled = pruneDisabledMissingIPs(pool)
	// 池成员 = 登基 IP + 优选域名（cfnat -fixed 原生收域名，拨号时活解析）。
	// Disabled 成员不进 -fixed；Priority 成员排最前；Lock 时只留 Priority。
	fixedMembers, priorityMembers, err := planFixedMembers(pool)
	if err != nil {
		return err
	}
	cfnatCfg := defaultCFnatConfig()
	cfnatCfg.Fixed = strings.Join(fixedMembers, ",")
	cfnatCfg.Priority = strings.Join(priorityMembers, ",")
	cfnatCfg.Fallback = strings.TrimSpace(env("CFNAT_FALLBACK", ""))
	// 回滚目标是「换池前正在跑的那一份」，不是「用默认参数重算一份」：
	// 现跑进程可能经 /api/cfnat/start 定制过 Addr/Port/Num/Sticky，
	// 默认配置会把它们全丢掉。status().Args 是上一次成功启动的参数
	// （start() 失败不改写它），直接复用就是最忠实的回滚。
	// 只有从未成功启动过（无运行参数可复用）时，才用默认配置 +
	// planFixedMembers(current) 重算；若连这份规划都做不出来（全员
	// 关闭/锁位无置顶），宁可不回滚、如实报错，也不用原样 IP+域名
	// 把已关闭的成员重新投入转发。
	rollbackArgs := a.cfnat.status().Args
	if len(rollbackArgs) == 0 {
		if fixed, priority, err := planFixedMembers(current); err == nil {
			rollbackCfg := defaultCFnatConfig()
			rollbackCfg.Fixed = strings.Join(fixed, ",")
			rollbackCfg.Priority = strings.Join(priority, ",")
			rollbackCfg.Fallback = strings.TrimSpace(env("CFNAT_FALLBACK", ""))
			rollbackArgs = rollbackCfg.args()
		}
	}
	// 同参不重启：自动换池、在位验活剔人、域名体检回池三条后台路径各自重算
	// -fixed，同一轮里经常得出与正在运行的进程逐字相同的参数（生产日志里
	// 16:21:12/16:21:16 连续三次 applied 的 -fixed 完全相同，每次都是一次
	// stop()+start()）。对一模一样的参数重启 cfnat，唯一效果是把在途连接全部
	// 掐断，然后拨向同一批目标。只有「进程确实在跑 + 参数逐字相同」才走落盘
	// 通道；进程已退出时必须重建，否则池子改了却没人转发。
	if shouldSkipCFnatRestart(a.cfnat.status(), cfnatCfg.args()) {
		if err := a.commitPoolWithoutRestart(pool); err != nil {
			return fmt.Errorf("保存生效池: %w", err)
		}
		// 参数未变但池记录变了（成员通道、体检结论等），状态要与 apply 路径一致。
		a.poolGuardMu.Lock()
		a.poolBlock, a.poolBlockAt, a.poolBlockBy = poolSwitchVerdict{}, time.Time{}, ""
		a.poolGuardMu.Unlock()
		if a.quality != nil {
			if forwarding := forwardingIPs(pool); len(forwarding) > 0 {
				a.quality.seedActive(forwarding[0], pool.UpdatedAt)
			}
		}
		a.pruneActiveHealth(forwardingIPs(pool))
		log.Printf("proxy active pool applied (unchanged, no restart): %s", strings.Join(fixedMembers, ","))
		return nil
	}
	if err := a.cfnat.stop(); err != nil {
		return fmt.Errorf("停止 cfnat: %w", err)
	}
	if err := a.cfnat.start(a.dataDir, cfnatCfg.args(), ""); err != nil {
		applyErr := fmt.Errorf("启动新 cfnat: %w", err)
		if len(rollbackArgs) == 0 {
			return fmt.Errorf("%w；无可回滚的运行配置（旧池规划也失败），cfnat 已停止", applyErr)
		}
		if rollbackErr := a.cfnat.start(a.dataDir, rollbackArgs, ""); rollbackErr != nil {
			return fmt.Errorf("%w；回滚 cfnat 也失败: %v", applyErr, rollbackErr)
		}
		return applyErr
	}
	// 落盘与内存写回在同一把 activeMu 下完成（见 commitPoolWithoutRestart
	// 的注释）：保存失败时内存不动、照旧回滚；成功时磁盘与内存同时换新，
	// 窗口内 setProxyPoolError 不可能用旧副本把新池盖掉。
	a.activeMu.Lock()
	saveErr := a.saveProxyActivePool(pool)
	if saveErr == nil {
		a.activePool = pool
	}
	a.activeMu.Unlock()
	if saveErr != nil {
		commitErr := fmt.Errorf("保存生效池: %w", saveErr)
		if stopErr := a.cfnat.stop(); stopErr != nil {
			return fmt.Errorf("%w；回滚前停止新 cfnat 失败: %v", commitErr, stopErr)
		}
		if len(rollbackArgs) == 0 {
			return fmt.Errorf("%w；无可回滚的运行配置（旧池规划也失败），cfnat 已停止", commitErr)
		}
		if rollbackErr := a.cfnat.start(a.dataDir, rollbackArgs, ""); rollbackErr != nil {
			return fmt.Errorf("%w；回滚 cfnat 也失败: %v", commitErr, rollbackErr)
		}
		return commitErr
	}
	// 换池成功，历史报错随之作废——粘滞字段不该继续挂着上一轮的拦截原因。
	a.poolGuardMu.Lock()
	a.poolBlock, a.poolBlockAt, a.poolBlockBy = poolSwitchVerdict{}, time.Time{}, ""
	a.poolGuardMu.Unlock()
	if a.quality != nil {
		// Active 必须指向真正转发的那一个：pool.IPs[0] 可能已被用户关闭，
		// 把它播种成 Active 会让调度器盯着一根不承载流量的线做比较。
		if forwarding := forwardingIPs(pool); len(forwarding) > 0 {
			a.quality.seedActive(forwarding[0], pool.UpdatedAt)
		}
	}
	// 换池成功才裁健康账：离池 IP 的旧记录本来就不会再被读到
	// （activePoolHealthy 只遍历当前成员），只占内存与 /api/status 体积。
	// 失败路径不裁——旧池仍在跑，它的健康记录还得留着。
	a.pruneActiveHealth(forwardingIPs(pool))
	log.Printf("proxy active pool applied: %s", strings.Join(fixedMembers, ","))
	return nil
}

// shouldSkipCFnatRestart 判定「这次换池能不能直接落盘、不重启 cfnat」。
//
// 判据只有一条：进程正跑着，且这次要用的参数与它启动时收到的逐字相同。
// 参数相同意味着转发目标、优先级、回退深度全部没变，重启只是把在途连接
// 掐断再拨回同一批目标。
//
// 进程没在跑时绝不能跳过：那些参数只存在于我们手里，不重启就等于池子改了
// 却没人转发。参数有任何差异（哪怕只是顺序）也一律重启，宁可多断一次，
// 不可让配置与实跑不一致。
func shouldSkipCFnatRestart(st processStatus, want []string) bool {
	return st.Running && sameCFnatArgs(st.Args, want)
}

// sameCFnatArgs 逐字比较两组 cfnat 命令行参数。用于判定「这次换池是否需要重启」。
// 顺序敏感：args() 的输出顺序固定，不做集合比较。
func sameCFnatArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// commitPoolWithoutRestart 落盘并替换内存中的生效池，但不重启 cfnat。
//
// 只用于「-fixed 集合没有变化」的变更：典型场景是剔除一个被用户关闭的成员，
// 它本来就不转发。为了删掉这样一条记录去重启 cfnat，会把正在转发的连接
// 全部打断一次，代价与收益完全不成比例。调用方必须先证明前后 -fixed 一致。
func (a *app) commitPoolWithoutRestart(pool proxyActivePool) error {
	pool.Disabled = pruneDisabledMissingIPs(pool)
	// 落盘与内存写回必须在同一把 activeMu 下完成：上一版先落盘、
	// 再上锁写内存，窗口内 setProxyPoolError 可能用旧副本把新池从
	// 磁盘上盖掉（反之亦然）。保存失败时内存不动，调用方按失败处理。
	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	if err := a.saveProxyActivePool(pool); err != nil {
		return err
	}
	a.activePool = pool
	return nil
}

func (a *app) setProxyPoolError(message string, results []proxyScanResult) {
	// 快照-改-写回是竞态：promote/select 在 cfnatCtlMu 下落池的瞬间若
	// 夹在快照与写回之间，刚落成的池会被这份旧快照原样盖掉；且只写内
	// 存不落盘，重启后报错消失，与 Error/ErrorAt 的持久化结构自相矛
	// 盾。改成在 activeMu 下直接读改写。注意不能拿 cfnatCtlMu：
	// revalidate 在持双锁（proxyScanMu + cfnatCtlMu）时调本函数，
	// 拿锁会死锁。
	//
	// 落盘必须在同一把 activeMu 下做：上一版「解锁后再落盘」留了窗口——
	// 解锁后、落盘前若有别的路径换池并落盘，这份旧副本会把新池从磁盘
	// 上盖掉（内存是对的，磁盘是旧的），重启后生效池静默回退到旧成员。
	// 持锁做文件 IO 会短暂挡住快照读，但池文件很小，且本函数的调用方
	// 都不持 activeMu，无死锁。
	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	a.activePool.Error = message
	a.activePool.ErrorAt = time.Now()
	if results != nil {
		a.activePool.Results = results
	}
	if err := a.saveProxyActivePool(a.activePool); err != nil {
		log.Printf("proxy pool error persist failed: %v", err)
	}
	log.Printf("proxy auto apply skipped: %s", message)
}

// probeTCPAlive 探测单个目标能否建立 TCP 连接。抽成变量便于测试注入，
// 避免单测真的去连外网。
var probeTCPAlive = func(ctx context.Context, addr string, timeout time.Duration) bool {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// verifyActivePoolAlive 探测当前生效池的存活情况，返回 (存活数, 总数)。
//
// 用途：建池失败时决定「保留旧池」是否合理。2026-10-22 事故形态是建池 0 通过，
// 而旧池唯一的 IP 早已 dial timeout，系统照样报「保留旧池」——等于把一具尸体
// 留在岗位上，对外还显示一切正常。保留之前必须先证明它活着。
//
// 用独立的 Background 而非调用方的 ctx：走到这里时上层 ctx 往往已经超时，
// 拿它去探测会立刻全灭，把「网络慢」误判成「旧池已死」。
func (a *app) verifyActivePoolAlive() (int, int) {
	pool := a.proxyActivePoolSnapshot()
	// 只验真正参与转发的成员：关闭的成员不进 -fixed，证明它们活着不能说明
	// 「旧池还能用」；反过来，它们死了也不该拖累保留旧池的判断。
	targets := forwardingMembers(pool)
	if len(targets) == 0 {
		return 0, 0
	}
	port := defaultProxyAutoConfig().Port
	if port <= 0 {
		port = 443
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	alive := 0
	for _, target := range targets {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			addr := net.JoinHostPort(host, fmt.Sprint(port))
			if probeTCPAlive(ctx, addr, 5*time.Second) {
				mu.Lock()
				alive++
				mu.Unlock()
			}
		}(target)
	}
	wg.Wait()
	return alive, len(targets)
}

// activeHealthStat 记录生效池 IP 的被动健康：成功/失败计数与最近失败时间。
type activeHealthStat struct {
	Success   int
	Failures  int
	LastFail  time.Time
	LastCheck time.Time
}

// poolSwitchVerdict 是一次换池闸判定的完整结果。
//
// 只返回 bool 的时代已经过去：调度器被拦时只打容器日志，前端照样弹「优选完成」，
// 属于静默吞单。现在把「放没放、为什么、下次什么时候放」一并交回调用方，
// 由它统一落进 /api/status。
type poolSwitchVerdict struct {
	Allowed bool
	// Reason 取值：rescue / understaffed / ok / cooldown / healthy / no_pool
	Reason string
	// Rescue 为真表示整池判死、跳过全部防抖放行。
	Rescue bool
	// NextAllowedAt 仅在 cooldown 拦截时非零。
	NextAllowedAt time.Time
}

// activePoolAllDead 报告现生效池是否「整组已死」：每个成员都达到连续失败阈值。
//
// 数据源是 throneProbe（在位验活），不是 quality 成绩册，也不是 activeHealth：
//   - throneProbe 按「成员」建模，每轮给池里每个 IP 各记一次连续失败数，
//     正好回答「整组是不是全死了」；quality 成绩册是按「单条线路」建模的，
//     ActiveIP 只有一个，对多成员池答不上来。
//   - activeHealth 是 scheduler 每轮顺手记的被动健康，探测失败与调度器
//     主动请下台走的是同一份计数，语义混在一起；救援判定要的是干净的
//     验活口径，故不用它。
//
// 空池不算「死」：没有成员就没有可救援对象，交给残员豁免处理。
func (a *app) activePoolAllDead(ips []string) (bool, int) {
	if len(ips) == 0 {
		return false, 0
	}
	threshold := throneProbeFailThreshold()
	a.throneProbeMu.Lock()
	defer a.throneProbeMu.Unlock()
	dead := 0
	for _, ip := range ips {
		st := a.throneProbe[ip]
		if st != nil && st.ConsecutiveFailures >= threshold {
			dead++
		}
	}
	return dead == len(ips), dead
}

// seedPoolGuardFromActive 启动时用持久化生效池时间初始化冷却锚点，
// 避免重启后立即被后台换池顶掉正在工作的 IP。
func (a *app) seedPoolGuardFromActive() {
	pool := a.proxyActivePoolSnapshot()
	if pool.UpdatedAt.IsZero() {
		return
	}
	a.poolGuardMu.Lock()
	a.lastPoolSwitchAt = pool.UpdatedAt
	a.lastAutoSwitchAt = pool.UpdatedAt
	a.poolGuardMu.Unlock()
}

// markPoolSwitched 记录「池子变了」的时间，只供状态展示与人工审计。
// 它在位验活剔人、用户扶持、手动采用都会走——这些都不是「自动换池」，
// 不能拿来当冷却锚点（见 markAutoSwitch）。
func (a *app) markPoolSwitched(now time.Time) {
	a.poolGuardMu.Lock()
	a.lastPoolSwitchAt = now
	// 换池成功即清除上一次的拦截原因：留着会让前端把历史拦截当成当前状态。
	a.poolBlock, a.poolBlockAt, a.poolBlockBy = poolSwitchVerdict{}, time.Time{}, ""
	a.poolGuardMu.Unlock()
}

// markAutoSwitch 记录「自动换池真的执行了」的时间，冷却闸只认这个锚点。
//
// 为什么必须和 markPoolSwitched 分开：冷却防的是「自动换池互相顶池」，
// 不是「维修动作」。旧实现所有路径共用一个时间戳，生产实测 6 小时里
// 在位验活剔人 10 次、用户手动供给 4 次，每次都把 30 分钟冷却重新拨满，
// 自动换池被挡回 21 次、0 次成功——系统退化成「只能靠人工粘 IP」。
//
// 用户显式动作（手动供给、扶持登基）也调这里：刚点完就被后台顶掉同样糟糕，
// 给用户的选择留一个冷却窗口。
func (a *app) markAutoSwitch(now time.Time) {
	a.poolGuardMu.Lock()
	a.lastPoolSwitchAt = now
	a.lastAutoSwitchAt = now
	a.poolBlock, a.poolBlockAt, a.poolBlockBy = poolSwitchVerdict{}, time.Time{}, ""
	a.poolGuardMu.Unlock()
}

// recordPoolBlock 记下「这次换池被谁、为什么拦下」。
// 三个换池入口（scheduler / auto-apply / 救援）统一走这里，别各自拼字符串。
func (a *app) recordPoolBlock(verdict poolSwitchVerdict, source string, now time.Time) {
	a.poolGuardMu.Lock()
	a.poolBlock, a.poolBlockAt, a.poolBlockBy = verdict, now, source
	a.poolGuardMu.Unlock()
}

// poolBlockStatus 把最近一次拦截整理成 /api/status 可直读的结构。
// blocked 为 false 表示当前没有生效的拦截（从未拦过，或换池成功后已清）。
func (a *app) poolBlockStatus() map[string]any {
	a.poolGuardMu.Lock()
	verdict, at, source := a.poolBlock, a.poolBlockAt, a.poolBlockBy
	a.poolGuardMu.Unlock()
	if verdict.Allowed || source == "" {
		return map[string]any{"blocked": false}
	}
	next := ""
	if !verdict.NextAllowedAt.IsZero() {
		next = verdict.NextAllowedAt.Format(time.RFC3339)
	}
	out := map[string]any{
		"blocked": true,
		"reason":  verdict.Reason,
		"source":  source,
		"at":      at,
	}
	if next != "" {
		out["nextAllowedAt"] = next
	}
	return out
}

// allowPoolSwitch 自动换池闸门，按顺序判定：
// 1) 整池判死救援：每个在位成员都达到连续失败阈值 → 放行，跳过冷却与健康闸；
// 2) 全局冷却未过 → 拒绝（残员豁免：池 < MinPool 时只走健康/数量闸，不空等）；
// 3) 残员豁免：现池规模 < 自动换池最小数 → 放行（整池健康也不拦——残员补回优先）；
// 4) 现生效池全部健康（近 N 次无失败）→ 拒绝顶池。
//
// 返回 poolSwitchVerdict 而非 bool：调用方要能把「为什么没换」写进 /api/status，
// 只打容器日志等于静默吞单（生产 17:41/17:53 两次"优选完成"假捷报即此）。
// 自动路径放行后必须 markAutoSwitch；维修动作（剔人/扶持/踢人）只记 markPoolSwitched，
// 不重拨自动冷却的时钟。
func (a *app) allowPoolSwitch(now time.Time, reason string) poolSwitchVerdict {
	current := a.proxyActivePoolSnapshot()
	minPool := defaultProxyAutoConfig().MinPool
	// 判死、健康、人手都只看真正参与转发的成员：被用户关闭的成员不进
	// -fixed，它们的死活与转发质量无关，不能拿来顶住闸门或冒充「整组已死」。
	forwarding := forwardingIPs(current)
	// 救援先行：整组已死时，冷却与健康闸都失去意义——它们防的是"瞎换"，
	// 而此刻不换就是断网。判死口径见 activePoolAllDead。
	if allDead, deadCount := a.activePoolAllDead(forwarding); allDead {
		log.Printf("pool switch RESCUE: all %d members reached failure threshold, bypassing cooldown/health gate (%s)", deadCount, reason)
		return poolSwitchVerdict{Allowed: true, Reason: "rescue", Rescue: true}
	}
	// 残员豁免：池缩到最小数之下时，冷却不再是拒绝理由——补员优先于防抖。
	// 冷却日志照打（审计不断），但不再拦截。
	understaffed := len(forwarding) < minPool
	cooldown := poolSwitchCooldown()
	a.poolGuardMu.Lock()
	last := a.lastPoolSwitchAt
	autoLast := a.lastAutoSwitchAt
	if autoLast.IsZero() {
		// 兼容旧账本：没有自动锚点时退回到任意池变更的时间，宁可多等一轮。
		autoLast = last
	}
	a.poolGuardMu.Unlock()
	if !understaffed && !autoLast.IsZero() && now.Sub(autoLast) < cooldown {
		log.Printf("pool switch blocked by cooldown (%s ago < %s): %s", now.Sub(autoLast).Round(time.Second), cooldown, reason)
		return poolSwitchVerdict{Reason: "cooldown", NextAllowedAt: autoLast.Add(cooldown)}
	}
	if understaffed && !autoLast.IsZero() && now.Sub(autoLast) < cooldown {
		log.Printf("pool switch cooldown waived for understaffed pool (%d<%d forwarding IPs, %s ago < %s): %s", len(forwarding), minPool, now.Sub(autoLast).Round(time.Second), cooldown, reason)
	}
	if understaffed {
		return poolSwitchVerdict{Allowed: true, Reason: "understaffed"}
	}
	if len(forwarding) > 0 && a.activePoolHealthy(forwarding) {
		log.Printf("pool switch blocked: active pool healthy, keep working IPs (%s)", reason)
		return poolSwitchVerdict{Reason: "healthy"}
	}
	return poolSwitchVerdict{Allowed: true, Reason: "ok"}
}

// activePoolHealthy 现生效池健康即“全部 IP 近期无失败记录”。
// 无健康记录的 IP 视为未知（不阻拦换池），只有“有成功且无近期失败”才算健康票。
func (a *app) activePoolHealthy(ips []string) bool {
	window := activeHealthWindow()
	now := time.Now()
	a.activeHealthMu.Lock()
	defer a.activeHealthMu.Unlock()
	healthy := 0
	for _, ip := range ips {
		st := a.activeHealth[ip]
		if st == nil || st.Success == 0 {
			continue
		}
		if st.LastCheck.IsZero() || now.Sub(st.LastCheck) > window {
			continue
		}
		if !st.LastFail.IsZero() && now.Sub(st.LastFail) < window {
			continue
		}
		healthy++
	}
	return healthy > 0 && healthy == len(ips)
}

func (a *app) poolGuardStatus() map[string]any {
	a.poolGuardMu.Lock()
	last := a.lastPoolSwitchAt
	autoLast := a.lastAutoSwitchAt
	a.poolGuardMu.Unlock()
	cooldown := poolSwitchCooldown()
	next := ""
	if !autoLast.IsZero() {
		next = autoLast.Add(cooldown).Format(time.RFC3339)
	}
	return map[string]any{
		"cooldownMinutes":  int(cooldown.Minutes()),
		"lastSwitchAt":     last,
		"lastAutoSwitchAt": autoLast,
		"nextAllowedAt":    next,
		"healthWindowMin":  int(activeHealthWindow().Minutes()),
	}
}

// pruneActiveHealth 把被动健康账裁到当前转发成员，避免池子轮换后旧 IP
// 的记录无限堆积。旧记录本来也不会被 activePoolHealthy 读到（它只遍历
// 当前成员），这里纯粹是控制内存与 /api/status 的体积。
func (a *app) pruneActiveHealth(keep []string) {
	keepSet := make(map[string]bool, len(keep))
	for _, ip := range keep {
		keepSet[ip] = true
	}
	a.activeHealthMu.Lock()
	for ip := range a.activeHealth {
		if !keepSet[ip] {
			delete(a.activeHealth, ip)
		}
	}
	a.activeHealthMu.Unlock()
}

func (a *app) recordActiveHealth(ip string, success bool, now time.Time) {
	a.activeHealthMu.Lock()
	defer a.activeHealthMu.Unlock()
	st := a.activeHealth[ip]
	if st == nil {
		st = &activeHealthStat{}
		a.activeHealth[ip] = st
	}
	st.LastCheck = now
	if success {
		st.Success++
	} else {
		st.Failures++
		st.LastFail = now
	}
}

func sameIPSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, ip := range a {
		seen[ip]++
	}
	for _, ip := range b {
		seen[ip]--
		if seen[ip] < 0 {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// replacePoolMember keeps the rest of a working pool in service while a
// scheduler promotion replaces one member. A missing FromIP can happen after
// a restart or manual kick; in that case replace the first member rather than
// collapsing the whole pool to a single address.
func replacePoolMember(current []string, from, to string) []string {
	pool := append([]string(nil), current...)
	if len(pool) == 0 {
		return []string{to}
	}
	for i, ip := range pool {
		if ip == from {
			pool[i] = to
			return pool
		}
	}
	pool[0] = to
	return pool
}

// mergePoolSwaps 把病号换成替补，返回新池成员与处置账。
//   - 非病号原样保留；
//   - 有替补的病号换成替补；无替补的病号按 keepUnresolved 处置：
//     为真时暂留（验活：池子缩水比留一个病号更糟），为假时直接摘掉
//     （关闭席：弱成员本来就不转发，摘掉不影响转发）；
//   - 替补已在池里（考试期间别的路径把它换进来了）或已在本轮新池里
//     （两个病号共用一个替补）时不再重复进池，病号仍摘掉——
//     planFixedMembers 虽会对 -fixed 去重，但落盘池的成员计数与
//     转发统计都会因重复 IP 算错。
//
// removed 是被摘掉的病号（含已由在位替补接替的）；swapped 是
// "病号→替补" 的记录；held 是暂留的病号。
func mergePoolSwaps(ips []string, victims map[string]bool, replacements map[string]string, keepUnresolved bool) (next, removed, swapped, held []string) {
	next = make([]string, 0, len(ips))
	removed = make([]string, 0, len(victims))
	swapped = make([]string, 0, len(victims))
	held = make([]string, 0, len(victims))
	for _, ip := range ips {
		if !victims[ip] {
			next = append(next, ip)
			continue
		}
		replacement, ok := replacements[ip]
		if !ok {
			if keepUnresolved {
				// 无合格替补：不删。池子缩水（5→4→3）比留一个病号更糟，
				// 而且越缩越容易触发「整组已死」。留下来并在账上标清理由，
				// 页面能看见。
				next = append(next, ip)
				held = append(held, ip)
			} else {
				removed = append(removed, ip)
			}
			continue
		}
		if containsString(ips, replacement) || containsString(next, replacement) {
			removed = append(removed, ip)
			swapped = append(swapped, ip+"→"+replacement+"（已在位）")
			continue
		}
		next = append(next, replacement)
		removed = append(removed, ip)
		swapped = append(swapped, ip+"→"+replacement)
	}
	return next, removed, swapped, held
}

func mergePoolResults(ips []string, current, fresh []proxyScanResult) []proxyScanResult {
	byIP := make(map[string]proxyScanResult, len(current)+len(fresh))
	for _, result := range current {
		byIP[result.IP] = result
	}
	for _, result := range fresh {
		byIP[result.IP] = result
	}
	merged := make([]proxyScanResult, 0, len(ips))
	for _, ip := range ips {
		if result, ok := byIP[ip]; ok {
			merged = append(merged, result)
		}
	}
	return merged
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// worstPoolMember 挑在位池最差成员：优先拿质量记录里平均延迟最高者，
// 无记录者视为最差（末位保护：绝不动第一个成员）。
func (a *app) worstPoolMember(ips []string) string {
	if len(ips) == 0 {
		return ""
	}
	if len(ips) == 1 {
		return ips[0]
	}
	scores := make(map[string]float64, len(ips))
	if a.quality != nil {
		snapshot := a.quality.snapshot()
		if records, ok := snapshot["records"].([]qualityRecord); ok {
			for _, rec := range records {
				if rec.AverageLatencyMs > 0 {
					scores[rec.IP] = rec.AverageLatencyMs
				}
			}
		}
	}
	worst := ips[len(ips)-1]
	worstLatency := scores[worst]
	for _, ip := range ips[1:] {
		if scores[ip] > worstLatency {
			worst = ip
			worstLatency = scores[ip]
		}
	}
	return worst
}

// refillGap 踢人补位的缺口：按实际转发的成员算，锁位时不补。
func refillGap(pool proxyActivePool, poolSize int) int {
	if pool.Lock {
		return 0
	}
	return poolSize - len(forwardingIPs(pool))
}

// refillPoolAfterKick 踢人后的自动补位终审：从候选缓存考试新 IP，
// 保序并入现有在位成员，把池补回 PoolSize 目标（5~15）。
// 手动踢除的延续语义：不走 allowPoolSwitch 冷却（markPoolSwitched 由本路径自行记录）。
func (a *app) refillPoolAfterKick() {
	cfg := defaultProxyAutoConfig()
	if !cfg.Enabled || cfg.Host == "" || cfg.Path == "" {
		return
	}
	var template map[string]any
	if cfg.VLESS.Enabled {
		loaded, err := loadVLESSOutboundTemplate(cfg.VLESS.TemplatePath)
		if err != nil {
			log.Printf("kick refill skipped: %v", err)
			return
		}
		template = loaded
	}
	// 锁被占时排队等待而非静默让位：补位是一次性 goroutine，让位即永久丢失
	// （调度器只顶换不增员，静默让位后池永远回不满）。最多等 3 分钟。
	if !lockTimeout(&a.proxyScanMu, 3*time.Minute) {
		log.Printf("kick refill abandoned: proxy scan busy for 3m")
		return
	}
	defer a.proxyScanMu.Unlock()
	current := a.proxyActivePoolSnapshot()
	// 缺口按实际转发的成员算：Disabled 成员占着 IPs 名额却不转发，
	// 按 len(IPs) 算会得出 need=0，池永远补不满。锁位模式下补位无权
	// 改动池子：补进来的 IP 会进 pool.IPs 但不进 -fixed，账面与转发
	// 事实分叉，直接退出，等用户解锁再说。
	need := refillGap(current, cfg.PoolSize)
	if need <= 0 {
		if current.Lock {
			log.Printf("kick refill skipped: pool lock enabled")
		}
		return
	}
	snapshot := a.proxyCandidateSnapshot()
	active := make(map[string]struct{}, len(current.IPs))
	for _, ip := range current.IPs {
		active[ip] = struct{}{}
	}
	// 候选取 3 倍余量（WS 通过率折扣），VLESS 探测数控制在 need*3 内。
	pending := make([]string, 0, need*3)
	for _, ip := range snapshot.IPs {
		if len(pending) >= need*3 {
			break
		}
		if _, ok := active[ip]; ok {
			continue
		}
		pending = append(pending, ip)
	}
	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	results := scanProxyWebSockets(ctx, pending, cfg)
	applyCandidateSourcePriority(results, snapshot.SourceByIP)
	passed := make([]string, 0, need)
	// 进池延迟上限：踢人补位也不能塞慢 IP。旧实现只看 WS 通不通，
	// 于是补位反复塞进 1 秒级 IP，粘性设备随即卡死。
	ceiling := poolEntryMaxLatencyMs()
	if cfg.VLESS.Enabled {
		passed = probeVLESSPool(ctx, results, cfg.Port, min(cfg.VLESS.MaxCandidates, need*3), cfg.VLESS, template)
	} else {
		for _, result := range results {
			if result.Error == "" {
				passed = append(passed, result.IP)
			}
		}
	}
	// 只保留延迟达标的候选；全都不达标则宁可让池子短一点也不塞废物。
	fast := make([]string, 0, len(passed))
	slowRejected := 0
	for idx, ip := range passed {
		latency := int64(0)
		if idx < len(results) {
			for _, r := range results {
				if r.IP == ip {
					latency = r.Latency
					break
				}
			}
		}
		if latency > ceiling {
			slowRejected++
			continue
		}
		fast = append(fast, ip)
	}
	if slowRejected > 0 {
		log.Printf("kick refill rejected %d slow candidates (WS latency > %dms)", slowRejected, ceiling)
	}
	if len(fast) == 0 {
		log.Printf("kick refill aborted: no candidate under the %dms entry ceiling", ceiling)
		return
	}
	// 机房软偏好：延迟硬闸滤出的快名单再按偏好机房分区重排——补位先填
	// 偏好机房，数量不足时其余机房照常补位（软语义，永不因偏好让池短缺）。
	fast = orderPassedByColoPreference(fast, results, poolColoPreferenceSet())
	// 提交前在锁内重取快照再合：6 分钟考试窗口内验活或 reconcile
	// 可能已换池；拿开考前的旧快照提交会把已剔成员带回池。缺口按
	// 新快照重算，已在位的补位候选直接跳过（不重复进池）。
	var addedCount, mergedCount int
	if err := a.applyPoolRebased(func(fresh proxyActivePool) (proxyActivePool, bool) {
		if fresh.Lock {
			log.Printf("kick refill skipped: pool lock enabled")
			return proxyActivePool{}, false
		}
		needNow := refillGap(fresh, cfg.PoolSize)
		if needNow <= 0 {
			return proxyActivePool{}, false
		}
		merged := append([]string(nil), fresh.IPs...)
		added := make([]string, 0, needNow)
		for _, ip := range fast {
			if containsString(merged, ip) {
				continue
			}
			merged = append(merged, ip)
			added = append(added, ip)
			// 按新增数封顶：merged 含 Disabled 成员，按 len(merged) 封顶会
			// 在有已关闭成员时少补（与 need 的算法同病）。
			if len(added) >= needNow {
				break
			}
		}
		if len(added) == 0 {
			log.Printf("kick refill: no new passes (%d tested), pool stays %d IPs", len(pending), len(fresh.IPs))
			return proxyActivePool{}, false
		}
		addedCount, mergedCount = len(added), len(merged)
		pool := fresh
		pool.UpdatedAt = time.Now()
		pool.IPs = merged
		pool.Results = mergePoolResults(merged, fresh.Results, results)
		return pool, true
	}); err != nil {
		if errors.Is(err, errPoolRebaseAborted) {
			return
		}
		if errors.Is(err, errPoolChangedDuringSubmit) {
			log.Printf("kick refill skipped: pool changed during submit")
			return
		}
		log.Printf("kick refill apply failed: %v", err)
		return
	}
	a.markPoolSwitched(time.Now())
	log.Printf("kick refill: +%d IPs -> pool %d IPs", addedCount, mergedCount)
}

// withoutPoolIP 从池中剔除指定 IP；返回 (剩余, 是否找到)。
func withoutPoolIP(ips []string, ip string) ([]string, bool) {
	remaining := make([]string, 0, len(ips))
	found := false
	for _, item := range ips {
		if item == ip {
			found = true
			continue
		}
		remaining = append(remaining, item)
	}
	return remaining, found
}

// removeCandidateIP 把 IP 从候选缓存除名并落盘（踢出皇位后防止立刻被回选）。
func (a *app) removeCandidateIP(ip string) {
	a.candidateMu.Lock()
	snapshot := a.candidates
	filtered := make([]string, 0, len(snapshot.IPs))
	for _, item := range snapshot.IPs {
		if item != ip {
			filtered = append(filtered, item)
		}
	}
	snapshot.IPs = filtered
	if snapshot.SourceByIP != nil {
		delete(snapshot.SourceByIP, ip)
	}
	a.candidates = snapshot
	a.candidateMu.Unlock()
	_ = a.saveProxyCandidateCache(snapshot)
}
