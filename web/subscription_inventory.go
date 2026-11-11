package main

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// subscriptionInventoryFile 是「待考本子」的落盘文件。
//
// 为什么要有这一个文件：旧实现是「拉取→提取→立刻握手→只存通过的」一次跑完，
// 于是「这轮通过 141 个」这句话背后，一共提取了多少、扔了多少、多少根本没轮到
// 试，系统自己都不知道。改成分两段之后，本子记全部候选，池子只记考过的——
// 中间那段落差从此可见、可追、可复盘。
const subscriptionInventoryFile = "subscription-inventory.json"

// 本子的容量与淘汰。候选是滚动的，无限增长会把落盘和内存都拖垮。
const (
	// subscriptionInventoryCap 本子上限。超出时先淘汰「久未露面且连续失败」的。
	subscriptionInventoryCap = 6000
	// subscriptionInventoryStale 多久没再被任何维护者供出来，就可列入淘汰候选。
	subscriptionInventoryStale = 72 * time.Hour
	// subscriptionFetchHistoryPerMaintainer 每个维护者保留最近几次拉取记录。
	subscriptionFetchHistoryPerMaintainer = 20
)

// 本子里一条候选的状态。pending 是「装了还没考」，不是失败。
const (
	subStatePending = "pending"
	subStatePass    = "pass"
	subStateFail    = "fail"
)

// subEntry 是本子里的一条候选记录。以 IP 为主键，多个维护者供出同一个 IP
// 时合并为一条、来源记成列表——去重因此是结构性的，不是事后补的一道筛。
type subEntry struct {
	// Sources 供出这个 IP 的维护者域名（去重、保序）。
	Sources []string `json:"sources"`
	// Via 解析到这个 IP 的原始主机名（订阅里的域名节点）。仅作溯源，
	// 不参与考试——考试对象始终是 IP。
	Via []string `json:"via,omitempty"`

	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`

	// LastProbe 是最近一次考试时间，零值表示从未考过。
	LastProbe time.Time `json:"lastProbe,omitempty"`
	State     string    `json:"state"`

	Passes int `json:"passes"`
	Fails  int `json:"fails"`
	// ConsecutiveFails 连续失败次数。单次抖动不判死，连败才降级为 fail。
	ConsecutiveFails int `json:"consecutiveFails"`

	LastError  string `json:"lastError,omitempty"`
	ErrorClass string `json:"errorClass,omitempty"`
	// Stage 记录这条是通过哪一层考出来的：ws（握手）或 vless（真实数据面）。
	Stage     string  `json:"stage,omitempty"`
	LatencyMs int64   `json:"latencyMs,omitempty"`
	Mbps      float64 `json:"mbps,omitempty"`
	Colo      string  `json:"colo,omitempty"`
}

// subFetchRecord 是一个维护者的单次拉取留痕。旧实现只有累计计数，
// 「这一次到底成没成、要到多少」无从查证，排障只能靠猜。
type subFetchRecord struct {
	At        time.Time `json:"at"`
	OK        bool      `json:"ok"`
	Status    int       `json:"status,omitempty"`
	DurationMs int64    `json:"durationMs"`
	Extracted int       `json:"extracted"`
	Error     string    `json:"error,omitempty"`
}

type subscriptionInventory struct {
	UpdatedAt time.Time              `json:"updatedAt"`
	Entries   map[string]*subEntry   `json:"entries"`
	Fetches   map[string][]subFetchRecord `json:"fetches,omitempty"`
	// LastHarvestAt / LastProbeAt 分开记：收一次和考一次是两件独立的事，
	// 合成一个时间戳就又会回到「看不出卡在哪一段」的老问题。
	LastHarvestAt  time.Time `json:"lastHarvestAt,omitempty"`
	LastHarvestErr string    `json:"lastHarvestErr,omitempty"`
	LastProbeAt    time.Time `json:"lastProbeAt,omitempty"`
	LastProbeErr   string    `json:"lastProbeErr,omitempty"`
	LastHarvested  int       `json:"lastHarvested,omitempty"`
	LastProbed     int       `json:"lastProbed,omitempty"`
	LastPassed     int       `json:"lastPassed,omitempty"`
}

// loadSubscriptionInventory 启动时载入本子。
// 若本子不存在而旧的 subscription-pool.json 存在，则把旧池一次性搬进来
// （全部标为 pass），避免升级后第一次运行把已验证的候选全丢。
func (a *app) loadSubscriptionInventory() {
	inv := subscriptionInventory{Entries: make(map[string]*subEntry), Fetches: make(map[string][]subFetchRecord)}
	data, err := os.ReadFile(filepath.Join(a.dataDir, subscriptionInventoryFile))
	if err == nil {
		var stored subscriptionInventory
		if json.Unmarshal(data, &stored) == nil && stored.Entries != nil {
			inv = stored
			if inv.Fetches == nil {
				inv.Fetches = make(map[string][]subFetchRecord)
			}
		}
	}
	if len(inv.Entries) == 0 {
		inv = a.importLegacySubscriptionPool(inv)
	}
	now := time.Now()
	for ip, entry := range inv.Entries {
		if entry == nil || net.ParseIP(ip) == nil {
			delete(inv.Entries, ip)
			continue
		}
		if entry.State == "" {
			entry.State = subStatePending
		}
		if entry.FirstSeen.IsZero() {
			entry.FirstSeen = now
		}
		if entry.LastSeen.IsZero() {
			entry.LastSeen = entry.FirstSeen
		}
	}
	a.subMu.Lock()
	a.subInv = inv
	a.subMu.Unlock()
	if len(inv.Entries) > 0 {
		log.Printf("subscription inventory loaded: %d entries", len(inv.Entries))
	}
}

// importLegacySubscriptionPool 把旧版 subscription-pool.json 的 IP 搬进本子。
func (a *app) importLegacySubscriptionPool(inv subscriptionInventory) subscriptionInventory {
	data, err := os.ReadFile(filepath.Join(a.dataDir, subscriptionPoolFile))
	if err != nil {
		return inv
	}
	var stored struct {
		UpdatedAt time.Time         `json:"updatedAt"`
		IPs       []string          `json:"ips"`
		Sources   map[string]string `json:"sources"`
	}
	if json.Unmarshal(data, &stored) != nil {
		return inv
	}
	at := stored.UpdatedAt
	if at.IsZero() {
		at = time.Now()
	}
	imported := 0
	for _, raw := range stored.IPs {
		ip := net.ParseIP(raw)
		if ip == nil || !isPublicIPv4(ip) {
			continue
		}
		key := ip.To4().String()
		src := stored.Sources[key]
		if src == "" {
			src = "subscription"
		}
		inv.Entries[key] = &subEntry{
			Sources:   []string{src},
			FirstSeen: at,
			LastSeen:  at,
			State:     subStatePass,
			Passes:    1,
		}
		imported++
	}
	if imported > 0 {
		log.Printf("subscription inventory: imported %d IPs from legacy %s", imported, subscriptionPoolFile)
	}
	return inv
}

// saveSubscriptionInventoryLocked 落盘。调用方须持 subMu 写锁。
func (a *app) saveSubscriptionInventoryLocked() error {
	a.subInv.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(a.subInv, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, subscriptionInventoryFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mergeSubscriptionHarvest 把一轮采集结果并入本子。
// sources 是 ip -> 维护者域名的映射；via 是 ip -> 原始主机名（可为空）。
//
// 这一段刻意不做任何探测：收就是收，收完立刻落盘。考试是另一个循环的事。
// 拆开的直接收益是采集速度不再被考试预算绑死——旧实现里 240 秒的握手预算
// 决定了这一轮最多能收多少，收得再多也进不了本子。
func (a *app) mergeSubscriptionHarvest(sources, via map[string]string, records map[string]subFetchRecord) (added, refreshed int) {
	now := time.Now()
	a.subMu.Lock()
	defer a.subMu.Unlock()
	if a.subInv.Entries == nil {
		a.subInv.Entries = make(map[string]*subEntry)
	}
	if a.subInv.Fetches == nil {
		a.subInv.Fetches = make(map[string][]subFetchRecord)
	}
	for raw, maintainer := range sources {
		ip := net.ParseIP(raw)
		if ip == nil || !isPublicIPv4(ip) {
			continue
		}
		key := ip.To4().String()
		entry := a.subInv.Entries[key]
		if entry == nil {
			entry = &subEntry{FirstSeen: now, State: subStatePending}
			a.subInv.Entries[key] = entry
			added++
		} else {
			refreshed++
		}
		entry.LastSeen = now
		if maintainer != "" {
			entry.Sources = appendUniqueString(entry.Sources, maintainer)
		}
		if host := via[key]; host != "" {
			entry.Via = appendUniqueString(entry.Via, host)
		}
	}
	for maintainer, record := range records {
		history := a.subInv.Fetches[maintainer]
		history = append(history, record)
		if len(history) > subscriptionFetchHistoryPerMaintainer {
			history = history[len(history)-subscriptionFetchHistoryPerMaintainer:]
		}
		a.subInv.Fetches[maintainer] = history
	}
	a.subInv.LastHarvestAt = now
	return added, refreshed
}

func appendUniqueString(values []string, want string) []string {
	for _, v := range values {
		if v == want {
			return values
		}
	}
	return append(values, want)
}

// subscriptionProbeCandidates 挑出本轮该考的候选，按优先级排序。
//
// 优先级：从未考过的排在前面（先让新料过一遍水），其次是考过但已过期的
// （持续复核），最后才是失败待重试的。这一顺序保证本子不会因为一批顽固
// 失败者长期占住考试预算，而让新来的候选永远轮不到。
func (a *app) subscriptionProbeCandidates(limit int, recheckAfter, retryAfter time.Duration) []string {
	if limit <= 0 {
		return nil
	}
	now := time.Now()
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	type ranked struct {
		ip       string
		priority int
		age      time.Time
	}
	items := make([]ranked, 0, len(a.subInv.Entries))
	for ip, entry := range a.subInv.Entries {
		if entry == nil {
			continue
		}
		switch entry.State {
		case subStatePending, "":
			// 从没考过的最优先。但「排到队却没轮上」（WS 过了、真数据面预算用光）
			// 的也会留下 LastProbe：这类必须等一个重试周期再排，否则每轮都会
			// 重新拿同一批去敲 WS，预算全耗在重复劳动上。
			if entry.LastProbe.IsZero() {
				items = append(items, ranked{ip, 0, entry.FirstSeen})
			} else if now.Sub(entry.LastProbe) >= retryAfter {
				items = append(items, ranked{ip, 0, entry.LastProbe})
			}
		case subStatePass:
			if entry.LastProbe.IsZero() || now.Sub(entry.LastProbe) >= recheckAfter {
				items = append(items, ranked{ip, 1, entry.LastProbe})
			}
		case subStateFail:
			if entry.LastProbe.IsZero() || now.Sub(entry.LastProbe) >= retryAfter {
				items = append(items, ranked{ip, 2, entry.LastProbe})
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority < items[j].priority
		}
		return items[i].age.Before(items[j].age)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ip
	}
	return out
}

// recordSubscriptionProbe 把一条考试结果写回本子。调用方须持 subMu 写锁。
//
// 判死规则与在位验活一致：单次失败不算数，连续失败到阈值才降为 fail。
// 这样一次网络抖动不会把一条好线路从本子里打下去。
func (a *app) recordSubscriptionProbeLocked(ip string, result subscriptionProbeOutcome) {
	entry := a.subInv.Entries[ip]
	if entry == nil {
		return
	}
	entry.LastProbe = result.At
	if result.OK {
		entry.State = subStatePass
		entry.Stage = result.Stage
		entry.Passes++
		entry.ConsecutiveFails = 0
		entry.LastError = ""
		entry.ErrorClass = ""
		entry.LatencyMs = result.LatencyMs
		entry.Mbps = result.Mbps
		entry.Colo = result.Colo
		return
	}
	entry.Fails++
	entry.ConsecutiveFails++
	entry.LastError = result.Error
	entry.ErrorClass = result.ErrorClass
	if entry.ConsecutiveFails >= subscriptionEntryFailThreshold {
		entry.State = subStateFail
		entry.Mbps = 0
	}
}

// subscriptionEntryFailThreshold 连续失败多少次把一条候选判为 fail。
// 与在位验活同值（3）：同一套「单次抖动不换人」的判断标准，不另立规矩。
const subscriptionEntryFailThreshold = 3

type subscriptionProbeOutcome struct {
	At         time.Time
	OK         bool
	Stage      string
	LatencyMs  int64
	Mbps       float64
	Colo       string
	Error      string
	ErrorClass string
}

// pruneSubscriptionInventoryLocked 淘汰本子里不该再占位的条目。调用方须持写锁。
//
// 淘汰条件从严：既久未露面、又反复考不过。只满足一条的留下——
// 一个维护者暂时拉不到不代表 IP 死了，一条 IP 暂时不通也不代表不会恢复。
func (a *app) pruneSubscriptionInventoryLocked(now time.Time) int {
	removed := 0
	for ip, entry := range a.subInv.Entries {
		if entry == nil {
			delete(a.subInv.Entries, ip)
			removed++
			continue
		}
		stale := now.Sub(entry.LastSeen) > subscriptionInventoryStale
		dead := entry.State == subStateFail && entry.ConsecutiveFails >= subscriptionEntryFailThreshold*2
		if stale && dead {
			delete(a.subInv.Entries, ip)
			removed++
		}
	}
	if len(a.subInv.Entries) <= subscriptionInventoryCap {
		return removed
	}
	// 仍然超容：按「先淘汰最没用」的顺序削，pass 的最后动。
	type victim struct {
		ip       string
		state    string
		lastSeen time.Time
	}
	victims := make([]victim, 0, len(a.subInv.Entries))
	for ip, entry := range a.subInv.Entries {
		if entry == nil {
			continue
		}
		victims = append(victims, victim{ip, entry.State, entry.LastSeen})
	}
	sort.SliceStable(victims, func(i, j int) bool {
		rank := func(state string) int {
			switch state {
			case subStateFail:
				return 0
			case subStatePending, "":
				return 1
			default:
				return 2
			}
		}
		if rank(victims[i].state) != rank(victims[j].state) {
			return rank(victims[i].state) < rank(victims[j].state)
		}
		return victims[i].lastSeen.Before(victims[j].lastSeen)
	})
	for _, v := range victims {
		if len(a.subInv.Entries) <= subscriptionInventoryCap {
			break
		}
		delete(a.subInv.Entries, v.ip)
		removed++
	}
	return removed
}

// subscriptionPassedIPs 返回本子里考过的 IP，按质量降序。
// 这是候选汇合时 subscription 层真正的取用口径——不再是「存进池的那批」，
// 而是「本子里state=pass 的那批」，两者语义一致但后者带完整成绩。
func (a *app) subscriptionPassedIPs(remaining int) []string {
	if remaining <= 0 {
		return nil
	}
	a.subMu.RLock()
	type scored struct {
		ip    string
		mbps  float64
		lat   int64
		first time.Time
	}
	items := make([]scored, 0, len(a.subInv.Entries))
	for ip, entry := range a.subInv.Entries {
		if entry == nil || entry.State != subStatePass {
			continue
		}
		items = append(items, scored{ip, entry.Mbps, entry.LatencyMs, entry.FirstSeen})
	}
	a.subMu.RUnlock()
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].mbps != items[j].mbps {
			return items[i].mbps > items[j].mbps
		}
		if items[i].lat != items[j].lat {
			return items[i].lat < items[j].lat
		}
		return items[i].first.Before(items[j].first)
	})
	if len(items) > remaining {
		items = items[:remaining]
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ip
	}
	return out
}

// subscriptionInventorySummary 给页面看的总览：总数、各状态计数、失败分类分布。
//
// 刻意不含逐次拉取历史：这段会被 /api/status 每 3 秒拉一次，塞进 24 个维护者 ×
// 最近 20 次的记录纯属浪费。历史单独走 subscriptionFetchHistory()，只在订阅页
// 打开时取一次。
func (a *app) subscriptionInventorySummary() map[string]any {
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	counts := map[string]int{subStatePending: 0, subStatePass: 0, subStateFail: 0}
	bySource := make(map[string]int)
	classDist := make(map[string]int)
	for _, entry := range a.subInv.Entries {
		if entry == nil {
			continue
		}
		state := entry.State
		if state == "" {
			state = subStatePending
		}
		counts[state]++
		for _, src := range entry.Sources {
			bySource[src]++
		}
		if entry.ErrorClass != "" && state != subStatePass {
			classDist[entry.ErrorClass]++
		}
	}
	return map[string]any{
		"total":          len(a.subInv.Entries),
		"pending":        counts[subStatePending],
		"pass":           counts[subStatePass],
		"fail":           counts[subStateFail],
		"bySource":       bySource,
		"errorClasses":   classDist,
		"lastHarvestAt":  a.subInv.LastHarvestAt,
		"lastHarvestErr": a.subInv.LastHarvestErr,
		"lastHarvested":  a.subInv.LastHarvested,
		"lastProbeAt":    a.subInv.LastProbeAt,
		"lastProbeErr":   a.subInv.LastProbeErr,
		"lastProbed":     a.subInv.LastProbed,
		"lastPassed":     a.subInv.LastPassed,
		"cap":            subscriptionInventoryCap,
	}
}

// subscriptionFetchHistory 返回每个维护者的最近拉取记录与最后一条。
// 只给订阅页用，不进 /api/status。
func (a *app) subscriptionFetchHistory() (map[string][]subFetchRecord, map[string]subFetchRecord) {
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	history := make(map[string][]subFetchRecord, len(a.subInv.Fetches))
	last := make(map[string]subFetchRecord, len(a.subInv.Fetches))
	for k, v := range a.subInv.Fetches {
		copied := append([]subFetchRecord(nil), v...)
		history[k] = copied
		if len(copied) > 0 {
			last[k] = copied[len(copied)-1]
		}
	}
	return history, last
}

// subscriptionEntrySnapshot 返回单条记录副本，供接口按 IP 查询。
func (a *app) subscriptionEntrySnapshot(ip string) (subEntry, bool) {
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	entry := a.subInv.Entries[ip]
	if entry == nil {
		return subEntry{}, false
	}
	copied := *entry
	copied.Sources = append([]string(nil), entry.Sources...)
	copied.Via = append([]string(nil), entry.Via...)
	return copied, true
}

// subscriptionInventoryEntryView 是接口输出的单条记录（带 IP 字段）。
type subscriptionInventoryEntryView struct {
	IP string `json:"ip"`
	subEntry
}

// subscriptionInventoryEntries 按状态过滤并分页返回本子内容。
//
// 排序刻意固定为「最该看什么排前面」：pending 在前（还没结论的最需要关注），
// 同状态内按最近考试时间升序——最久没考的排最前，正是下一轮会先被挑中的。
func (a *app) subscriptionInventoryEntries(state string, limit int) []subscriptionInventoryEntryView {
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	items := make([]subscriptionInventoryEntryView, 0, len(a.subInv.Entries))
	for ip, entry := range a.subInv.Entries {
		if entry == nil {
			continue
		}
		entryState := entry.State
		if entryState == "" {
			entryState = subStatePending
		}
		if state != "" && state != "all" && entryState != state {
			continue
		}
		copied := *entry
		copied.Sources = append([]string(nil), entry.Sources...)
		copied.Via = append([]string(nil), entry.Via...)
		copied.State = entryState
		items = append(items, subscriptionInventoryEntryView{IP: ip, subEntry: copied})
	}
	sort.SliceStable(items, func(i, j int) bool {
		rank := func(s string) int {
			switch s {
			case subStatePending:
				return 0
			case subStateFail:
				return 1
			default:
				return 2
			}
		}
		if rank(items[i].State) != rank(items[j].State) {
			return rank(items[i].State) < rank(items[j].State)
		}
		return items[i].LastProbe.Before(items[j].LastProbe)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

// removeSubscriptionEntry 从本子里删掉一条。返回被删的 IP 与是否存在。
func (a *app) removeSubscriptionEntry(ip string) (string, bool) {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return "", false
	}
	key := parsed.To4()
	if key == nil {
		return "", false
	}
	a.subMu.Lock()
	defer a.subMu.Unlock()
	value := key.String()
	if _, ok := a.subInv.Entries[value]; !ok {
		return "", false
	}
	delete(a.subInv.Entries, value)
	if err := a.saveSubscriptionInventoryLocked(); err != nil {
		log.Printf("subscription inventory save failed: %v", err)
	}
	return value, true
}
