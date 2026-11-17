package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	errLedgerBadRequest = errors.New("source ledger: 参数不合法")
	errLedgerNotFound   = errors.New("source ledger: 台账中没有这个地址")
)

// sourceLedgerFile 是「优选来源台账」的落盘文件。
//
// 为什么要有这本账：候选池里的 IP 每轮都在换，但没人记得住某个地址是
// 什么时候第一次出现的、被考过几次、有没有一次真的把流量送出去过。结果是
// 一批第三方 VPS 地址年复一年地挂在别人的 txt 里，每轮都被抓进来、每轮
// 都考不过、每轮又照样占着候选名额——看起来供给很充足，实际全是死重。
//
// 这本账把「见过几次 / 考过几次 / 通过几次 / 内容多久没变」记成可查的事实，
// 淘汰规则据此判断，而不是靠人拍脑袋。规则本身也落盘，页面可改，改完即生效。
const sourceLedgerFile = "proxy-source-ledger.json"

const sourceLedgerVersion = 1

// maxLedgerEntries 台账条数上限。超了按最后出现时间淘汰最旧的——台账是
// 判断依据，不是历史博物馆，保留最近活跃的那批就够了。
const maxLedgerEntries = 5000

// staleRule 是「长时间无变化且从未通过」的淘汰规则，全部字段热可改。
//
// 默认值刻意保守：观察期 7 天、至少 12 轮、至少考 3 次。宁可晚几天才剔，
// 也不要因为一次抖动把能用的地址误杀——误杀的代价是池子变小，误留的代价
// 只是多占一个名额。
type staleRule struct {
	Enabled bool `json:"enabled"`
	// GraceHours 观察期。从第一次见到该 IP 起算，不满这个时长一律不动。
	GraceHours int `json:"graceHours"`
	// MinRounds 至少被候选刷新看到过多少轮。一轮 = 一次候选池刷新。
	MinRounds int `json:"minRounds"`
	// MinProbes 至少被真实考试考过多少次（WS 握手或 VLESS 数据面）。
	MinProbes int `json:"minProbes"`
	// NonCFOnly 只淘汰不在 Cloudflare 官方段内的地址。官方段内的地址即使
	// 从没考过，也是 CF 自己的边缘，不该按「第三方冒充」处理。
	NonCFOnly bool `json:"nonCfOnly"`
	// NeverPassedOnly 只淘汰一次都没通过过的地址。通过过一次就说明它真的
	// 能把流量送到 CF，此后偶发失败属于正常波动。
	NeverPassedOnly bool `json:"neverPassedOnly"`
	// AutoExclude 命中规则时是否直接从候选池排除。关掉则只标记、不排除，
	// 用来先观察一段时间规则会误伤多少地址。
	AutoExclude bool `json:"autoExclude"`
	// SourceStaleRounds 单个来源的清单内容连续多少轮完全没变，就标记为
	// 「疑似停更」。这只是提示，不参与淘汰——停更的源仍然可能一直好用。
	SourceStaleRounds int `json:"sourceStaleRounds"`
}

// defaultStaleRule 默认规则。7 天 / 12 轮 / 3 次，与 6 小时一轮的刷新节奏
// 相配：12 轮 ≈ 3 天，7 天观察期足够覆盖一次完整的周末波动。
func defaultStaleRule() staleRule {
	return staleRule{
		Enabled:           true,
		GraceHours:        24 * 7,
		MinRounds:         12,
		MinProbes:         3,
		NonCFOnly:         true,
		NeverPassedOnly:   true,
		AutoExclude:       true,
		SourceStaleRounds: 28, // 28 轮 × 6h ≈ 7 天
	}
}

// clamp 把规则收进安全区间。页面可以随便填，落盘前必须归一：
// 观察期太短会误杀，太长等于没规则；轮次/次数同理。
func (r staleRule) clamp() staleRule {
	if r.GraceHours < 1 {
		r.GraceHours = 1
	}
	if r.GraceHours > 24*90 {
		r.GraceHours = 24 * 90
	}
	if r.MinRounds < 1 {
		r.MinRounds = 1
	}
	if r.MinRounds > 1000 {
		r.MinRounds = 1000
	}
	if r.MinProbes < 1 {
		r.MinProbes = 1
	}
	if r.MinProbes > 100 {
		r.MinProbes = 100
	}
	if r.SourceStaleRounds < 2 {
		r.SourceStaleRounds = 2
	}
	if r.SourceStaleRounds > 5000 {
		r.SourceStaleRounds = 5000
	}
	return r
}

// ipLedgerEntry 是单个 IP 的观察记录。
type ipLedgerEntry struct {
	IP        string    `json:"ip"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	// Rounds 该 IP 出现在候选刷新结果里的轮数。
	Rounds int `json:"rounds"`
	// ProbeCount / PassCount 真实考试次数与通过次数。
	ProbeCount int       `json:"probeCount"`
	PassCount  int       `json:"passCount"`
	LastPassAt time.Time `json:"lastPassAt,omitempty"`
	// LastProbeAt 最近一次考试时间；LastVerdict 为 pass / fail。
	LastProbeAt time.Time `json:"lastProbeAt,omitempty"`
	LastVerdict string    `json:"lastVerdict,omitempty"`
	// VerdictChangedAt 判定最后一次翻转的时间。长时间不翻转 + 从未通过，
	// 正是「这个地址一直没变、也一直没用」的证据。
	VerdictChangedAt time.Time `json:"verdictChangedAt,omitempty"`
	// CFEdge 是否落在 Cloudflare 官方段内（只作标签与规则输入）。
	CFEdge bool `json:"cfEdge"`
	// Sources 供出该 IP 的渠道标识（去重、保序、限长）。
	Sources []string `json:"sources,omitempty"`
	// BestMbps 历史最好吞吐，用于展示与人工判断。
	BestMbps float64 `json:"bestMbps,omitempty"`
	// EWMAMbps 按轮平滑的吞吐。换池排序把它当历史值，避免单轮抖动
	// 直接改写一个稳定成员的名次。系数复用 PROXY_EWMA_ALPHA。
	EWMAMbps float64 `json:"ewmaMbps,omitempty"`
	// LastMbps 最近一次成功的实测吞吐，用来判断账本是否过时。
	LastMbps float64 `json:"lastMbps,omitempty"`
	// FailStreak 连续考试失败次数。成功清零；连续失败很多次的地址
	// 不再拿账本历史替它说话，让本轮鲜值决定排序。
	FailStreak int `json:"failStreak,omitempty"`

	Excluded      bool      `json:"excluded,omitempty"`
	ExcludedAt    time.Time `json:"excludedAt,omitempty"`
	ExcludeReason string    `json:"excludeReason,omitempty"`
	// Manual 人工放行。置位后自动规则不再碰它——人已经拍过板，
	// 规则不该下一轮又把它剔掉，否则「放行」按钮等于没用。
	Manual bool `json:"manual,omitempty"`
}

// sourceLedgerStat 是单个来源（bestcf 的某个 URL、订阅的某个维护者、
// 域名表的某个地址）的供给质量账。
type sourceLedgerStat struct {
	Key     string `json:"key"`
	Channel string `json:"channel"` // subscription / bestcf / domain
	Kind    string `json:"kind"`    // ip / domain

	Fetches    int       `json:"fetches"`
	Successes  int       `json:"successes"`
	LastCount  int       `json:"lastCount"`
	LastSeenAt time.Time `json:"lastSeenAt,omitempty"`
	LastError  string    `json:"lastError,omitempty"`

	// Fingerprint 是最近一次清单内容的摘要；内容不变则 UnchangedRounds 递增。
	Fingerprint     string    `json:"fingerprint,omitempty"`
	LastChangedAt   time.Time `json:"lastChangedAt,omitempty"`
	UnchangedRounds int       `json:"unchangedRounds,omitempty"`
}

type sourceLedgerData struct {
	Version   int                          `json:"version"`
	UpdatedAt time.Time                    `json:"updatedAt"`
	Rules     staleRule                    `json:"rules"`
	IPs       map[string]*ipLedgerEntry    `json:"ips"`
	Sources   map[string]*sourceLedgerStat `json:"sources"`
}

// loadSourceLedger 启动时载入台账；文件不存在则用默认规则起一本空账。
func (a *app) loadSourceLedger() {
	data := sourceLedgerData{
		Version: sourceLedgerVersion,
		Rules:   defaultStaleRule(),
		IPs:     make(map[string]*ipLedgerEntry),
		Sources: make(map[string]*sourceLedgerStat),
	}
	if raw, err := os.ReadFile(filepath.Join(a.dataDir, sourceLedgerFile)); err == nil {
		var stored sourceLedgerData
		if json.Unmarshal(raw, &stored) == nil {
			data.Rules = stored.Rules.clamp()
			if stored.IPs != nil {
				data.IPs = stored.IPs
			}
			if stored.Sources != nil {
				data.Sources = stored.Sources
			}
			if stored.UpdatedAt.After(data.UpdatedAt) {
				data.UpdatedAt = stored.UpdatedAt
			}
		} else {
			// 文件损坏不得静默吞掉：带着空账本启动，淘汰规则等于从零开始
			// 重新观察，至少要留下痕迹。
			log.Printf("source ledger: %s 解析失败，以空账启动", sourceLedgerFile)
		}
	}
	a.ledgerMu.Lock()
	a.ledger = data
	a.ledgerMu.Unlock()
}

// saveSourceLedgerLocked 原子落盘。调用方须持 ledgerMu。
func (a *app) saveSourceLedgerLocked() error {
	a.ledger.Version = sourceLedgerVersion
	a.ledger.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(a.ledger, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, sourceLedgerFile)
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return err
	}
	a.ledgerLastSave = time.Now()
	return nil
}

// ledgerSaveThrottled 在写操作后按节流落盘：考试结果每 5 分钟就有一批，
// 每次都写盘没有意义；但也不能只在刷新时写，否则进程重启会丢掉一整天的
// 考试计数。两分钟一次是折中。
func (a *app) ledgerSaveThrottled() {
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	if time.Since(a.ledgerLastSave) < 2*time.Minute {
		return
	}
	if err := a.saveSourceLedgerLocked(); err != nil {
		log.Printf("source ledger save: %v", err)
	}
}

// ledgerEntryLocked 取或建一条 IP 记录。调用方须持 ledgerMu。
func (a *app) ledgerEntryLocked(ip string, now time.Time) *ipLedgerEntry {
	entry, ok := a.ledger.IPs[ip]
	if !ok {
		entry = &ipLedgerEntry{IP: ip, FirstSeen: now}
		a.ledger.IPs[ip] = entry
	}
	entry.LastSeen = now
	return entry
}

// observeCandidateRound 记一轮候选刷新：哪些 IP 被看到、由哪些渠道供出。
//
// 只在「本轮真的抓到了候选」时调用。一轮里同一个 IP 被多个源供出时，
// sourceByIP 只会保留第一个认领的渠道；这是既有语义，台账沿用，
// 不额外展开——展开会让「来源」变成一个永远在变的长列表，反而不好读。
func (a *app) observeCandidateRound(sourceByIP map[string]string) {
	if len(sourceByIP) == 0 {
		return
	}
	now := time.Now()
	a.ledgerMu.Lock()
	for raw, source := range sourceByIP {
		ip := net.ParseIP(raw)
		if !isPublicIPv4(ip) {
			continue
		}
		key := ip.To4().String()
		entry := a.ledgerEntryLocked(key, now)
		entry.Rounds++
		entry.CFEdge = isCloudflareIPv4(ip)
		entry.Sources = appendLedgerSource(entry.Sources, source)
	}
	a.pruneLedgerLocked(now)
	a.ledgerMu.Unlock()
	a.ledgerSaveThrottled()
}

// appendLedgerSource 保序去重追加，最多保留 6 个来源。
func appendLedgerSource(list []string, value string) []string {
	if value == "" {
		return list
	}
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	if len(list) >= 6 {
		return list
	}
	return append(list, value)
}

// ledgerObserveProbe 记一次真实考试结果。
//
// ok 的语义是「这个地址真的把流量送到了目标」——WS 握手通过、VLESS 数据面
// 拿到字节。仅仅 TCP 能连不算，那正是第三方 VPS 冒充 CF 边缘时最容易通过
// 的一关。
func (a *app) ledgerObserveProbe(ip string, ok bool, mbps float64) {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if !isPublicIPv4(parsed) {
		return
	}
	key := parsed.To4().String()
	now := time.Now()
	a.ledgerMu.Lock()
	entry := a.ledgerEntryLocked(key, now)
	entry.ProbeCount++
	entry.LastProbeAt = now
	verdict := "fail"
	if ok {
		verdict = "pass"
		entry.PassCount++
		entry.LastPassAt = now
		entry.FailStreak = 0
		if mbps > entry.BestMbps {
			entry.BestMbps = mbps
		}
		// 吞吐只在真测到字节时更新：WS 通过但没有数据面成绩（mbps=0）
		// 不该把账本里的历史吞吐抹成 0。
		if mbps > 0 {
			entry.LastMbps = mbps
			if alpha := ewmaRankingAlpha(); alpha > 0 && entry.EWMAMbps > 0 {
				entry.EWMAMbps = alpha*mbps + (1-alpha)*entry.EWMAMbps
			} else {
				entry.EWMAMbps = mbps
			}
		}
		// 通过过的地址不再受「从未通过」规则约束，顺手解除自动排除，
		// 让它重新参与竞争。人工排除（Manual）不受影响。
		if entry.Excluded && !entry.Manual {
			entry.Excluded = false
			entry.ExcludedAt = time.Time{}
			entry.ExcludeReason = ""
		}
	} else {
		entry.FailStreak++
	}
	if entry.LastVerdict != verdict {
		entry.LastVerdict = verdict
		entry.VerdictChangedAt = now
	}
	a.ledgerMu.Unlock()
	a.ledgerSaveThrottled()
}

// observeFunnelResultsInLedger 把一轮漏斗的真实考试结论写进来源台账。
//
// 只记「有结论」的结果：VLESS 启用时，被预算截断、根本没进数据面的
// WS_PASS 不记账（未知不等于通过，也不等于失败）；WS_FAIL / VLESS_FAIL /
// VLESS_PASS 都是确定结论。VLESS 未启用时 WS 结论就是终审结论。
func (a *app) observeFunnelResultsInLedger(results []proxyScanResult, vlessEnabled bool) {
	for _, result := range results {
		switch result.Stage {
		case "VLESS_PASS":
			a.ledgerObserveProbe(result.IP, true, result.DownloadMbps)
		case "VLESS_FAIL", "WS_FAIL":
			a.ledgerObserveProbe(result.IP, false, 0)
		default:
			if !vlessEnabled {
				a.ledgerObserveProbe(result.IP, result.Error == "", result.DownloadMbps)
			}
		}
	}
}

// ledgerExcluded 报告某 IP 是否已被台账排除。候选刷新逐条调用，
// 命中即跳过，不再浪费配额与探针预算。
func (a *app) ledgerExcluded(ip string) bool {
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	entry, ok := a.ledger.IPs[ip]
	return ok && entry.Excluded
}

// evaluateStale 执行一次淘汰判定，返回本轮新排除的数量。
//
// 判据（全部满足才排除）：
//   - 规则启用；
//   - 未被人为放行；
//   - 非 CF 官方段（NonCFOnly 时）；
//   - 从未通过真实考试（NeverPassedOnly 时）；
//   - 首次出现已超过观察期；
//   - 被看到轮数、被考次数均达阈值。
func (a *app) evaluateStale(now time.Time) int {
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	rule := a.ledger.Rules
	if !rule.Enabled || !rule.AutoExclude {
		return 0
	}
	grace := time.Duration(rule.GraceHours) * time.Hour
	marked := 0
	for _, entry := range a.ledger.IPs {
		if entry.Excluded || entry.Manual {
			continue
		}
		if rule.NonCFOnly && entry.CFEdge {
			continue
		}
		if rule.NeverPassedOnly && entry.PassCount > 0 {
			continue
		}
		if now.Sub(entry.FirstSeen) < grace {
			continue
		}
		if entry.Rounds < rule.MinRounds || entry.ProbeCount < rule.MinProbes {
			continue
		}
		entry.Excluded = true
		entry.ExcludedAt = now
		entry.ExcludeReason = "长时间无变化且从未通过真实考试"
		marked++
	}
	if marked > 0 {
		if err := a.saveSourceLedgerLocked(); err != nil {
			log.Printf("source ledger save after eviction: %v", err)
		}
		log.Printf("source ledger: 排除 %d 个长期未通过地址（观察期 %dh / %d 轮 / %d 次）",
			marked, rule.GraceHours, rule.MinRounds, rule.MinProbes)
	}
	return marked
}

// pruneLedgerLocked 控制台账规模：超上限时按最后出现时间淘汰最旧的。
func (a *app) pruneLedgerLocked(now time.Time) {
	if len(a.ledger.IPs) <= maxLedgerEntries {
		return
	}
	type victim struct {
		ip   string
		seen time.Time
	}
	victims := make([]victim, 0, len(a.ledger.IPs))
	for ip, entry := range a.ledger.IPs {
		victims = append(victims, victim{ip: ip, seen: entry.LastSeen})
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].seen.Before(victims[j].seen) })
	drop := len(victims) - maxLedgerEntries
	for i := 0; i < drop; i++ {
		delete(a.ledger.IPs, victims[i].ip)
	}
}

// recordSourceContent 记一次来源抓取的清单指纹。
//
// 用途是回答一个具体问题：「这个源多久没变过了？」——一个第三方 txt 连续
// 几十轮吐出完全相同的地址、而那些地址又从来没考过，基本可以断定它已经
// 不再维护，只是在重复旧数据。
func (a *app) recordSourceContent(key, channel, kind string, values []string, fetchErr error) {
	if key == "" {
		return
	}
	now := time.Now()
	fingerprint := fingerprintList(values)
	a.ledgerMu.Lock()
	if a.ledger.Sources == nil {
		a.ledger.Sources = make(map[string]*sourceLedgerStat)
	}
	stat, ok := a.ledger.Sources[key]
	if !ok {
		stat = &sourceLedgerStat{Key: key, Channel: channel, Kind: kind}
		a.ledger.Sources[key] = stat
	}
	stat.Fetches++
	stat.LastSeenAt = now
	if fetchErr != nil {
		stat.LastError = fetchErr.Error()
		a.ledgerMu.Unlock()
		a.ledgerSaveThrottled()
		return
	}
	stat.Successes++
	stat.LastError = ""
	stat.LastCount = len(values)
	if stat.Fingerprint == "" {
		stat.Fingerprint = fingerprint
		stat.LastChangedAt = now
		stat.UnchangedRounds = 0
	} else if stat.Fingerprint != fingerprint {
		stat.Fingerprint = fingerprint
		stat.LastChangedAt = now
		stat.UnchangedRounds = 0
	} else {
		stat.UnchangedRounds++
	}
	a.ledgerMu.Unlock()
	a.ledgerSaveThrottled()
}

// fingerprintList 对清单内容取短摘要。顺序敏感是刻意的：这些表基本按
// 排名排列，重排本身就是「有人在维护」的信号。
func fingerprintList(values []string) string {
	if len(values) == 0 {
		return ""
	}
	h := sha1.New()
	for _, value := range values {
		h.Write([]byte(value))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// sourceLedgerSnapshot 供接口输出的台账快照。
func (a *app) sourceLedgerSnapshot(filter string, limit int) map[string]any {
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rule := a.ledger.Rules
	entries := make([]map[string]any, 0, limit)
	excluded, stale := 0, 0
	ips := make([]*ipLedgerEntry, 0, len(a.ledger.IPs))
	for _, entry := range a.ledger.IPs {
		ips = append(ips, entry)
	}
	sort.Slice(ips, func(i, j int) bool { return ips[i].LastSeen.After(ips[j].LastSeen) })
	for _, entry := range ips {
		if entry.Excluded {
			excluded++
		}
		switch filter {
		case "excluded":
			if !entry.Excluded {
				continue
			}
		case "never-passed":
			if entry.PassCount > 0 {
				continue
			}
		}
		if len(entries) >= limit {
			continue
		}
		entries = append(entries, map[string]any{
			"ip":               entry.IP,
			"firstSeen":        entry.FirstSeen,
			"lastSeen":         entry.LastSeen,
			"rounds":           entry.Rounds,
			"probeCount":       entry.ProbeCount,
			"passCount":        entry.PassCount,
			"lastPassAt":       entry.LastPassAt,
			"lastVerdict":      entry.LastVerdict,
			"cfEdge":           entry.CFEdge,
			"sources":          entry.Sources,
			"bestMbps":         entry.BestMbps,
			"excluded":         entry.Excluded,
			"excludedAt":       entry.ExcludedAt,
			"excludeReason":    entry.ExcludeReason,
			"manual":           entry.Manual,
			"observedHours":    round1(time.Since(entry.FirstSeen).Hours()),
			"verdictChangedAt": entry.VerdictChangedAt,
		})
	}
	sources := make([]map[string]any, 0, len(a.ledger.Sources))
	keys := make([]string, 0, len(a.ledger.Sources))
	for key := range a.ledger.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		stat := a.ledger.Sources[key]
		fresh := stat.UnchangedRounds < rule.SourceStaleRounds
		if !fresh {
			stale++
		}
		sources = append(sources, map[string]any{
			"key":             stat.Key,
			"channel":         stat.Channel,
			"kind":            stat.Kind,
			"fetches":         stat.Fetches,
			"successes":       stat.Successes,
			"lastCount":       stat.LastCount,
			"lastSeenAt":      stat.LastSeenAt,
			"lastError":       stat.LastError,
			"lastChangedAt":   stat.LastChangedAt,
			"unchangedRounds": stat.UnchangedRounds,
			"stale":           !fresh,
		})
	}
	return map[string]any{
		"updatedAt": a.ledger.UpdatedAt,
		"rules":     rule,
		"counts": map[string]any{
			"tracked":  len(a.ledger.IPs),
			"excluded": excluded,
			"sources":  len(a.ledger.Sources),
			"stale":    stale,
		},
		"ips":     entries,
		"sources": sources,
	}
}

// round1 保留一位小数，避免接口里出现 12.345678901 这种噪声。
func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// updateSourceLedger 处理页面对台账的操作。所有动作在同一个锁内完成，
// 校验不过就不落盘，不留半改状态。
//
// 动作：
//
//	rules    改淘汰规则（整份覆盖后 clamp）
//	exclude  人工排除某个 IP
//	allow    人工放行某个 IP（置 Manual，自动规则不再碰）
//	forget   从台账删掉某个 IP
//	evaluate 立即执行一次淘汰判定
//	clear    清空全部排除标记
func (a *app) updateSourceLedger(action string, rules *staleRule, ip string) (map[string]any, error) {
	// evaluate 自己会拿 ledgerMu，必须在加锁之前分派。放在 switch 里先
	// Unlock 再 return，会撞上函数顶部那个 defer Unlock——重复解锁直接 panic。
	if action == "evaluate" {
		marked := a.evaluateStale(time.Now())
		return map[string]any{"evaluated": true, "newlyExcluded": marked}, nil
	}
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	key := strings.TrimSpace(ip)
	if key != "" {
		if parsed := net.ParseIP(key); parsed != nil && parsed.To4() != nil {
			key = parsed.To4().String()
		}
	}
	switch action {
	case "rules":
		if rules == nil {
			return nil, errLedgerBadRequest
		}
		a.ledger.Rules = rules.clamp()
	case "exclude", "allow", "forget":
		if key == "" {
			return nil, errLedgerBadRequest
		}
		entry, ok := a.ledger.IPs[key]
		if !ok {
			return nil, errLedgerNotFound
		}
		switch action {
		case "exclude":
			entry.Excluded = true
			entry.ExcludedAt = time.Now()
			entry.ExcludeReason = "人工排除"
			entry.Manual = false
		case "allow":
			entry.Excluded = false
			entry.ExcludedAt = time.Time{}
			entry.ExcludeReason = ""
			entry.Manual = true
		case "forget":
			delete(a.ledger.IPs, key)
		}
	case "clear":
		for _, entry := range a.ledger.IPs {
			if entry.Manual {
				continue
			}
			entry.Excluded = false
			entry.ExcludedAt = time.Time{}
			entry.ExcludeReason = ""
		}
	default:
		return nil, errLedgerBadRequest
	}
	if err := a.saveSourceLedgerLocked(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "action": action, "ip": key}, nil
}

// ledgerThroughputSnapshot 返回 IP → 历史最好吞吐(Mbps) 的只读快照。
//
// 只收 BestMbps>0 的记录：没测过和测出来是 0 是两件事，混在一起会让调用方
// 无法区分「未知」与「已知为零」。调用方按「不在表里 = 没测过」处理。
func (a *app) ledgerThroughputSnapshot() map[string]float64 {
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	if len(a.ledger.IPs) == 0 {
		return nil
	}
	out := make(map[string]float64, len(a.ledger.IPs))
	for ip, entry := range a.ledger.IPs {
		if entry == nil || entry.BestMbps <= 0 {
			continue
		}
		out[ip] = entry.BestMbps
	}
	return out
}

// ledgerRankingHistory 把账本里的历史吞吐整理成排名用的 history。
//
// 只放「真的通过过、最近没连续失败、并且有吞吐记录」的地址：账本是参考，
// 不是免死金牌。连续失败 3 次以上就不再拿历史替它说话，让本轮鲜值决定。
func (a *app) ledgerRankingHistory() []proxyScanResult {
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	if len(a.ledger.IPs) == 0 {
		return nil
	}
	type item struct {
		ip   string
		at   time.Time
		mbps float64
	}
	items := make([]item, 0, len(a.ledger.IPs))
	for ip, entry := range a.ledger.IPs {
		if entry == nil || entry.PassCount == 0 || entry.FailStreak >= 3 || entry.EWMAMbps <= 0 {
			continue
		}
		items = append(items, item{ip: ip, at: entry.LastSeen, mbps: entry.EWMAMbps})
	}
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	if len(items) > 200 {
		items = items[:200]
	}
	out := make([]proxyScanResult, 0, len(items))
	for _, it := range items {
		out = append(out, proxyScanResult{IP: it.ip, DownloadMbps: it.mbps})
	}
	return out
}

// poolLedgerMember 是 /api/status 里暴露的单成员摘要。
type poolLedgerMember struct {
	IP          string    `json:"ip"`
	ProbeCount  int       `json:"probeCount"`
	PassCount   int       `json:"passCount"`
	SuccessRate float64   `json:"successRate"`
	EWMAMbps    float64   `json:"ewmaMbps,omitempty"`
	LastMbps    float64   `json:"lastMbps,omitempty"`
	FailStreak  int       `json:"failStreak,omitempty"`
	LastSeen    time.Time `json:"lastSeen"`
}

type poolLedgerSummaryData struct {
	UpdatedAt time.Time          `json:"updatedAt"`
	Total     int                `json:"total"`
	Members   []poolLedgerMember `json:"members"`
}

// poolLedgerSummary 返回最近 limit 个成员的摘要，供 /api/status 使用。
// 全量可能上千条，不能整本塞进状态接口。
func (a *app) poolLedgerSummary(limit int) poolLedgerSummaryData {
	if limit <= 0 {
		limit = 50
	}
	a.ledgerMu.Lock()
	defer a.ledgerMu.Unlock()
	out := poolLedgerSummaryData{UpdatedAt: a.ledger.UpdatedAt, Total: len(a.ledger.IPs)}
	if len(a.ledger.IPs) == 0 {
		return out
	}
	members := make([]poolLedgerMember, 0, len(a.ledger.IPs))
	for ip, entry := range a.ledger.IPs {
		if entry == nil {
			continue
		}
		rate := 0.0
		if entry.ProbeCount > 0 {
			rate = float64(entry.PassCount) / float64(entry.ProbeCount)
		}
		members = append(members, poolLedgerMember{
			IP: ip, ProbeCount: entry.ProbeCount, PassCount: entry.PassCount,
			SuccessRate: rate, EWMAMbps: entry.EWMAMbps, LastMbps: entry.LastMbps,
			FailStreak: entry.FailStreak, LastSeen: entry.LastSeen,
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].LastSeen.After(members[j].LastSeen) })
	if len(members) > limit {
		members = members[:limit]
	}
	out.Members = members
	return out
}

// handleSourceLedger 是台账的读写口。
//
// GET  ?filter=all|excluded|never-passed&limit=N  读账
// POST {action, rules?, ip?}                      改规则 / 排除 / 放行 / 遗忘 / 立即判定
//
// 之所以做成一条接口而不是拆成五六条：台账是一个整体，读的时候永远要连
// 规则和统计一起看，写的时候也永远要立刻看到新规则下的结果。拆开只会让
// 页面连打五个请求再拼。
func (a *app) handleSourceLedger(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		writeJSON(w, a.sourceLedgerSnapshot(r.URL.Query().Get("filter"), limit))
	case http.MethodPost:
		var payload struct {
			Action string     `json:"action"`
			Rules  *staleRule `json:"rules"`
			IP     string     `json:"ip"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		result, err := a.updateSourceLedger(payload.Action, payload.Rules, payload.IP)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errLedgerNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, result)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
