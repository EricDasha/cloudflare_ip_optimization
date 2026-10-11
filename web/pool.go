package main

import (
	"context"
	"encoding/json"
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
	return pool
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
	a.cfnatCtlMu.Lock()
	defer a.cfnatCtlMu.Unlock()
	return a.applyPoolLocked(pool, current)
}

// applyPoolLocked 在已持有 cfnatCtlMu 时执行换池（踢人/手动采用等已持锁路径复用）。
func (a *app) applyPoolLocked(pool, current proxyActivePool) error {
	// 池成员 = 登基 IP + 优选域名（cfnat -fixed 原生收域名，拨号时活解析）。
	// Fallback 只留用户显式配置的 CFNAT_FALLBACK 兜底目标。
	fixedMembers := append([]string(nil), pool.IPs...)
	fixedMembers = append(fixedMembers, pool.Domains...)
	cfnatCfg := defaultCFnatConfig()
	cfnatCfg.Fixed = strings.Join(fixedMembers, ",")
	cfnatCfg.Fallback = strings.TrimSpace(env("CFNAT_FALLBACK", ""))
	rollbackFixed := append([]string(nil), current.IPs...)
	rollbackFixed = append(rollbackFixed, current.Domains...)
	rollbackCfg := defaultCFnatConfig()
	rollbackCfg.Fixed = strings.Join(rollbackFixed, ",")
	rollbackCfg.Fallback = strings.TrimSpace(env("CFNAT_FALLBACK", ""))
	if err := a.cfnat.stop(); err != nil {
		return fmt.Errorf("停止 cfnat: %w", err)
	}
	if err := a.cfnat.start(a.dataDir, cfnatCfg.args(), ""); err != nil {
		applyErr := fmt.Errorf("启动新 cfnat: %w", err)
		if rollbackErr := a.cfnat.start(a.dataDir, rollbackCfg.args(), ""); rollbackErr != nil {
			return fmt.Errorf("%w；回滚 cfnat 也失败: %v", applyErr, rollbackErr)
		}
		return applyErr
	}
	if err := a.saveProxyActivePool(pool); err != nil {
		commitErr := fmt.Errorf("保存生效池: %w", err)
		if stopErr := a.cfnat.stop(); stopErr != nil {
			return fmt.Errorf("%w；回滚前停止新 cfnat 失败: %v", commitErr, stopErr)
		}
		if rollbackErr := a.cfnat.start(a.dataDir, rollbackCfg.args(), ""); rollbackErr != nil {
			return fmt.Errorf("%w；回滚 cfnat 也失败: %v", commitErr, rollbackErr)
		}
		return commitErr
	}
	a.activeMu.Lock()
	a.activePool = pool
	a.activeMu.Unlock()
	if a.quality != nil && len(pool.IPs) > 0 {
		a.quality.seedActive(pool.IPs[0], pool.UpdatedAt)
	}
	log.Printf("proxy active pool applied: %s", strings.Join(fixedMembers, ","))
	return nil
}

func (a *app) setProxyPoolError(message string, results []proxyScanResult) {
	pool := a.proxyActivePoolSnapshot()
	pool.Error = message
	if results != nil {
		pool.Results = results
	}
	a.activeMu.Lock()
	a.activePool = pool
	a.activeMu.Unlock()
	log.Printf("proxy auto apply skipped: %s", message)
}

// activeHealthStat 记录生效池 IP 的被动健康：成功/失败计数与最近失败时间。
type activeHealthStat struct {
	Success   int
	Failures  int
	LastFail  time.Time
	LastCheck time.Time
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
	a.poolGuardMu.Unlock()
}

func (a *app) markPoolSwitched(now time.Time) {
	a.poolGuardMu.Lock()
	a.lastPoolSwitchAt = now
	a.poolGuardMu.Unlock()
}

// allowPoolSwitch 自动换池三道半闸：
// 1) 全局冷却未过 → 拒绝（残员豁免：池 < MinPool 时只走健康/数量闸，不空等）；
// 2) 现生效池全部健康（近 N 次无失败）→ 拒绝顶池；
// 3) 残员豁免：现池规模 < 自动换池最小数 → 放行（整池健康也不拦——残员补回优先）；
// 通过才允许换池。通过后调用方必须 markPoolSwitched。
func (a *app) allowPoolSwitch(now time.Time, reason string) bool {
	current := a.proxyActivePoolSnapshot()
	minPool := defaultProxyAutoConfig().MinPool
	// 残员豁免先行：池缩到最小数之下时，冷却不再是拒绝理由——补员优先于防抖。
	// 冷却日志照打（审计不断），但不再拦截。
	understaffed := len(current.IPs) < minPool
	cooldown := poolSwitchCooldown()
	a.poolGuardMu.Lock()
	last := a.lastPoolSwitchAt
	a.poolGuardMu.Unlock()
	if !understaffed && !last.IsZero() && now.Sub(last) < cooldown {
		log.Printf("pool switch blocked by cooldown (%s ago < %s): %s", now.Sub(last).Round(time.Second), cooldown, reason)
		return false
	}
	if understaffed && !last.IsZero() && now.Sub(last) < cooldown {
		log.Printf("pool switch cooldown waived for understaffed pool (%d<%d IPs, %s ago < %s): %s", len(current.IPs), minPool, now.Sub(last).Round(time.Second), cooldown, reason)
	}
	if understaffed {
		return true
	}
	if len(current.IPs) > 0 && a.activePoolHealthy(current.IPs) {
		log.Printf("pool switch blocked: active pool healthy, keep working IPs (%s)", reason)
		return false
	}
	return true
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
	a.poolGuardMu.Unlock()
	cooldown := poolSwitchCooldown()
	next := ""
	if !last.IsZero() {
		next = last.Add(cooldown).Format(time.RFC3339)
	}
	return map[string]any{
		"cooldownMinutes": int(cooldown.Minutes()),
		"lastSwitchAt":    last,
		"nextAllowedAt":   next,
		"healthWindowMin": int(activeHealthWindow().Minutes()),
	}
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
	need := cfg.PoolSize - len(current.IPs)
	if need <= 0 {
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
	merged := append([]string(nil), current.IPs...)
	added := make([]string, 0, need)
	for _, ip := range fast {
		if containsString(merged, ip) {
			continue
		}
		merged = append(merged, ip)
		added = append(added, ip)
		if len(merged) >= cfg.PoolSize {
			break
		}
	}
	if len(added) == 0 {
		log.Printf("kick refill: no new passes (%d tested), pool stays %d IPs", len(pending), len(current.IPs))
		return
	}
	now := time.Now()
	pool := current
	pool.UpdatedAt = now
	pool.IPs = merged
	pool.Results = mergePoolResults(merged, current.Results, results)
	if err := a.applyProxyPool(pool, current); err != nil {
		log.Printf("kick refill apply failed: %v", err)
		return
	}
	a.markPoolSwitched(now)
	log.Printf("kick refill: +%d IPs -> pool %d IPs", len(added), len(merged))
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

// proxyForwardDomains 返回 cfnat 兜底转发域名（-fallback）。
// 优选域名仅转发、不解析不探测：PROXY_DOMAIN_FORWARD 显式指定时优先；
// 否则取用户启用的优选域名前 maxForwardDomains 个。数据面拨号时按当前 DNS 解析。
// 兜底只在主池全部拨号失败时尝试，绝不参与 IP 探测与换池。
func (a *app) proxyForwardDomains() []string {
	configured := envForwardDomains("PROXY_DOMAIN_FORWARD")
	if len(configured) > 0 {
		return configured
	}
	if !envBool("PROXY_AUTO_DOMAINS", false) {
		return nil
	}
	snapshot := a.proxyCandidateSnapshot()
	domains := make([]string, 0, len(snapshot.PreferredDomains))
	for _, domain := range snapshot.PreferredDomains {
		if !validCandidateHostname(domain) {
			continue
		}
		domains = append(domains, domain)
		if len(domains) >= maxForwardDomains {
			break
		}
	}
	return domains
}
