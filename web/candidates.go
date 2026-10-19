package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// proxyCandidateSourceOrder 候选供给层（考试优先序）：
// user > cfdata > subscription > bestcf(社区优选)。
//
// 排序依据不是「谁给得多」，而是「谁与这条线路相关」：
//   - user：人工拍板，最高优先。
//   - cfdata：在本机（NAS）实测延迟与丢包，是唯一以我们自己网络为基准的源。
//   - subscription：未校验 CF 段的机场订阅，原料最杂，故靠后。
//   - bestcf：别人在自己的线路上扫出来的社区表，已按 CF 段闸清洗。
//
// official 官方段已砍（用户指令）；preferred 优选域名不解析供 IP——域名直接
// 升格为 -fixed 转发成员（拨号时活解析）。
var proxyCandidateSourceOrder = []string{"user", "cfdata", "subscription", "bestcf"}

var errProxyCandidatesBusy = errors.New("候选源刷新正在运行")

// preferredDomainPattern 匹配 090227 优选目录页面中服务端渲染的域名卡片。
// 页面为 GBK/UTF-8 双拼，但 copyDomain 参数是 ASCII，可在原始字节上直接匹配。
var preferredDomainPattern = regexp.MustCompile(`copyDomain\(\s*['"]([a-zA-Z0-9][a-zA-Z0-9.\-]*)['"]\s*\)`)

type proxyCandidateSource struct {
	Name    string
	Domains []string
	URLs    []string
}

// sourceHealthStat 单一候选源的健康账。逐源（bestcf 的每个 URL、订阅的每个
// 维护者、user/域名表各一席）记抓取次数、成功次数、累计 IP 数与最近一次
// 成败——渠道审计与优选台心跳都以此为准，死源一眼可辨。
type sourceHealthStat struct {
	Fetches     int       `json:"fetches"`
	Successes   int       `json:"successes"`
	IPs         int       `json:"ips"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastFailed  time.Time `json:"lastFailed,omitempty"`
}

// recordSourceHealth 记一笔渠道健康账。key 为源标识（URL、维护者域名或
// 逻辑层名）；ips 为本次抓到的公网 IPv4 数；err 非 nil 即为败笔。
func (a *app) recordSourceHealth(key string, ips int, err error) {
	if key == "" {
		return
	}
	a.sourceHealthMu.Lock()
	defer a.sourceHealthMu.Unlock()
	stat, ok := a.sourceHealth[key]
	if !ok {
		stat = &sourceHealthStat{}
		a.sourceHealth[key] = stat
	}
	stat.Fetches++
	stat.IPs += ips
	if err != nil {
		stat.LastError = err.Error()
		stat.LastFailed = time.Now()
		return
	}
	stat.Successes++
	stat.LastSuccess = time.Now()
	if ips > 0 {
		stat.LastError = ""
	}
}

// sourceHealthSnapshot 导出健康账深拷贝（/api/status 之 sources 节）。
func (a *app) sourceHealthSnapshot() map[string]*sourceHealthStat {
	a.sourceHealthMu.Lock()
	defer a.sourceHealthMu.Unlock()
	out := make(map[string]*sourceHealthStat, len(a.sourceHealth))
	for key, stat := range a.sourceHealth {
		copyStat := *stat
		out[key] = &copyStat
	}
	return out
}

type proxyCandidateSnapshot struct {
	UpdatedAt        time.Time         `json:"updatedAt,omitempty"`
	NextRefresh      time.Time         `json:"nextRefresh,omitempty"`
	Sources          []string          `json:"sources"`
	IPs              []string          `json:"ips"`
	SourceByIP       map[string]string `json:"sourceByIp,omitempty"`
	PreferredDomains []string          `json:"preferredDomains,omitempty"`
	ForwardDomains   []string          `json:"forwardDomains,omitempty"`
	Errors           []string          `json:"errors"`
}

var proxyCandidateSources = map[string]proxyCandidateSource{
	// 社区候选源已停用：IP 来源改为「用户输入 + 订阅 + CFdata 筛选推送 + 优选域名」。
	// "zhaobo": {Name: "Zhaobo 聚合池", Domains: []string{"proxyip.zhaobo.org"}},
	// "william": {
	// 	Name:    "William 台湾/韩国",
	// 	Domains: []string{"tw.william.us.ci", "kr.william.us.ci"},
	// },
	// "euorg": {Name: "EU.org 社区池", Domains: []string{"cdn.xn--b6gac.eu.org"}},
	// "cmliussss-proxyip": {
	// 	Name: "CMLiussss ProxyIP",
	// 	Domains: []string{
	// 		"ProxyIP.CMLiussss.net", "ProxyIP.HK.CMLiussss.net", "ProxyIP.SG.CMLiussss.net",
	// 		"ProxyIP.JP.CMLiussss.net", "ProxyIP.KR.CMLiussss.net", "ProxyIP.IN.CMLiussss.net",
	// 		"ProxyIP.GB.CMLiussss.net", "ProxyIP.FR.CMLiussss.net", "ProxyIP.DE.CMLiussss.net",
	// 		"ProxyIP.NL.CMLiussss.net", "ProxyIP.SE.CMLiussss.net", "ProxyIP.FI.CMLiussss.net",
	// 		"ProxyIP.PL.CMLiussss.net", "ProxyIP.RU.CMLiussss.net", "ProxyIP.CH.CMLiussss.net",
	// 		"ProxyIP.LV.CMLiussss.net", "ProxyIP.US.CMLiussss.net", "ProxyIP.CA.CMLiussss.net",
	// 	},
	// },
	"090227": {
		Name: "090227 优选域名与 API",
		Domains: []string{
			"cf.090227.xyz", "youxuan.cf.090227.xyz", "123.cf.090227.xyz",
			"www.visa.cn", "mfa.gov.ua", "www.shopify.com", "store.ubi.com",
			"staticdelivery.nexusmods.com", "tencentapp.cn", "cloudflare-dl.byoip.top",
			"cf.877774.xyz", "saas.sin.fan", "bestcf.030101.xyz", "cloudflare.182682.xyz",
			"ipdb.api.030101.xyz", "wetest.vip", "ip.164746.xyz",
		},
		URLs: []string{
			"https://cf.090227.xyz/",
			"https://cf.090227.xyz/ct?ips=6",
			"https://cf.090227.xyz/cu",
			"https://cf.090227.xyz/cmcc?ips=8",
		},
	},
	// bestcf 导航（https://bestcf.pages.dev/#yxym）全量优选 IP 列表：
	// `IP:端口#备注` 格式，extractPublicIPv4 按非数字点切分后端口与备注
	// 自然落网外，无需新解析器。
	//
	// 本名单于 2026-10-20 在 NAS 上逐源实测（curl + CF 段比对），只保留
	// 「CF 段命中率 ≥80% 且产出 ≥4 个」的源。被剔除的两类，记在这里以免
	// 后人再捡回来：
	//   1) ProxyIP 冒充优选：lzj(0/193)、cmliu(0/32)、luoli(0/31)、
	//      xinyitang3(0/18) —— 全是第三方 VPS（阿里云/M247/OVH/DO），
	//      不是 Cloudflare 边缘，进了入口池只会白烧探针。
	//   2) 域名表或低产：vps789/top100.txt(101 行全是域名)、
	//      domain/all.txt(域名表，另走 fetchBestcfForwardDomains)、
	//      zhixuanwang(2 个)、JieChaoCC(2 个)、tiancheng(7/58)。
	//   3) NAS 侧不可达：ymyuuu/IPDB、LancelotRar、joname1 —— 本地能开，
	//      但 NAS 到 GitHub raw 超时，保留只会每轮空耗 25 秒。
	"bestcf": {
		Name: "BestCF 全量优选 IP",
		URLs: []string{
			// bestcf.pages.dev 自托管镜像（NAS 侧稳定可达）
			"https://bestcf.pages.dev/s5gy/all.txt",         // 116/217
			"https://bestcf.pages.dev/uouin/all.txt",        // 42/42
			"https://bestcf.pages.dev/nirevil/ipv4.txt",     // 35/37
			"https://bestcf.pages.dev/vvhan/ipv4.txt",       // 32/32
			"https://bestcf.pages.dev/wetest/ipv4.txt",      // 17/17
			"https://bestcf.pages.dev/cfyes/ipv4.txt",       // 17/17
			"https://bestcf.pages.dev/ircf/ipv4.txt",        // 15/15
			"https://bestcf.pages.dev/cmliu2/all.txt",       // 12/15
			"https://bestcf.pages.dev/gslege/Cfxyz.txt",     // 4/4
			// GitHub 原始列表（NAS 实测可达的维护者）
			"https://raw.githubusercontent.com/Senflare/Senflare-IP/main/IPlist-Pro.txt",       // 29/29
			"https://raw.githubusercontent.com/einsitang/my-fast-cf-ip/master/fastips.txt",     // 20/20
			"https://raw.githubusercontent.com/yuanxiawan/cfipv4db/main/cfip.txt",              // 15/16
			"https://raw.githubusercontent.com/gshtwy/CF-DNS-Clone/main/wetest-cloudflare-v4.txt", // 14/14
			"https://raw.githubusercontent.com/hubbylei/bestcf/main/bestcf.txt",                // 10/10
			"https://raw.githubusercontent.com/ahang39/router/main/all.txt",                    // 7/7
		},
	},
}

var defaultProxyCandidateSourceIDs = []string{
	// 供给 = user > subscription > bestcf 全量优选（090227 并入 bestcf 层）。
	"bestcf",
}

// bestcfForwardDomainURL 是 bestcf 优选域名全量列表（`域名:端口#备注` 格式）。
// 域名作为一等公民进池（cfnat -fixed 拨号时活解析），绝不预解析成 IP。
const bestcfForwardDomainURL = "https://bestcf.pages.dev/domain/all.txt"

func applyCandidateSourcePriority(results []proxyScanResult, sourceByIP map[string]string) {
	if len(sourceByIP) == 0 {
		return
	}
	// 来源优先级决定"什么时候拿它来测试"，不决定最终性能排名。
	// 现行供给层：user > cfdata(本机实测) > subscription > bestcf(社区优选表)。
	// preferred(优选域名)不解析供 IP——域名直接升格为 -fixed 转发成员；
	// official(官方段)已按用户指令砍除。
	rank := func(ip string) int {
		for rank, source := range proxyCandidateSourceOrder {
			if sourceByIP[ip] == source {
				return rank
			}
		}
		return len(proxyCandidateSourceOrder)
	}
	for i := range results {
		results[i].SourceRank = rank(results[i].IP)
	}
	sort.SliceStable(results, func(i, j int) bool {
		leftOK, rightOK := results[i].Error == "", results[j].Error == ""
		if leftOK != rightOK {
			return leftOK
		}
		leftRank, rightRank := results[i].SourceRank, results[j].SourceRank
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return results[i].Latency < results[j].Latency
	})
}

func (a *app) refreshProxyCandidates(parent context.Context) (bool, error) {
	// 手动触发的刷新排队等待（后台周期偶发重叠不再闭门羹）；后台自调用同样受益。
	if !lockTimeout(&a.refreshMu, 60*time.Second) {
		return false, errProxyCandidatesBusy
	}
	defer a.refreshMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	resolved := make([]string, 0, candidatePoolTotalCap)
	sourceByIP := make(map[string]string)
	sourceErrors := []string{}
	nonCloudflare := 0
	// appendGroup 按层配额灌入：quota 是本层最多进入候选池的数量，
	// 总池上限 candidatePoolTotalCap 兜底。优先级仍由「先灌者先得」实现
	// （同 IP 先到先得，见 sourceByIP 认领），配额保证单层吃不光全池。
	appendGroup := func(source string, values []string, quota int) {
		if quota <= 0 {
			return
		}
		added := 0
		for _, raw := range values {
			if len(resolved) >= candidatePoolTotalCap || added >= quota {
				return
			}
			ip := net.ParseIP(strings.TrimSpace(raw))
			if !isPublicIPv4(ip) {
				continue
			}
			// CF 段闸：入口 IP 必须是 Cloudflare 边缘。source 里混进来的
			// 第三方 VPS（ProxyIP）到此为止，不进候选、不占探针。
			if !isCloudflareIPv4(ip) {
				nonCloudflare++
				continue
			}
			key := ip.To4().String()
			if _, ok := sourceByIP[key]; ok {
				continue
			}
			sourceByIP[key] = source
			resolved = append(resolved, key)
			added++
		}
	}
	// user 层：最高优先，小上限全收。
	userCandidates := strings.FieldsFunc(env("PROXY_USER_CANDIDATES", ""), func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	a.recordSourceHealth("user:env", len(userCandidates), nil)
	appendGroup("user", userCandidates, userCandidatesCap)
	// cfdata 层：唯一以本机网络为基准的供给。读 result.csv（本轮实测成绩，
	// 含丢包率），零丢包优先、低延迟次之、低抖动再次之。排第二是因为它的
	// 延迟和丢包是这台 NAS 亲手测的，而 bestcf/subscription 是别人测的。
	cfdataQuota := cfdataCandidatesCap
	if remaining := candidatePoolTotalCap - len(resolved); remaining < cfdataQuota {
		cfdataQuota = remaining
	}
	if cfdataQuota > 0 {
		cfdataIPs, cfdataErr := a.cfdataCandidateIPs(cfdataQuota)
		a.recordSourceHealth("cfdata:"+cfdataResultFile, len(cfdataIPs), cfdataErr)
		if cfdataErr != nil {
			sourceErrors = append(sourceErrors, "cfdata: "+cfdataErr.Error())
		}
		appendGroup("cfdata", cfdataIPs, cfdataQuota)
	}
	// subscription 层：≤300，且必须为 bestcf 预留额度让位——
	// 生产教训（sub 独占 1000/1000，其余层=0）不允许重演。
	reserved := bestcfCandidatesReserve
	subQuota := subscriptionCandidatesCap
	if remaining := candidatePoolTotalCap - len(resolved) - reserved; remaining < subQuota {
		subQuota = remaining
	}
	if subQuota < 0 {
		subQuota = 0
	}
	subPool := a.subscriptionPoolIPs(subQuota)
	a.recordSourceHealth("subscription:pool", len(subPool), nil)
	appendGroup("subscription", subPool, subQuota)
	// 优选域名不再解析供 IP：域名直接升格为 -fixed 转发成员（applyPoolLocked），
	// 拨号时按当前 DNS 活解析、天然跟随上游刷新——无需快照、无需考试。
	// bestcf 层：余量全归它兜底（前面几层若给不满，配额顺延至此）。
	bestcfQuota := candidatePoolTotalCap - len(resolved)
	if bestcfQuota > 0 {
		bestcf, bestcfErrors, err := a.resolveProxySources(ctx, defaultProxyCandidateSourceIDs, bestcfQuota)
		sourceErrors = append(sourceErrors, bestcfErrors...)
		if err != nil {
			sourceErrors = append(sourceErrors, err.Error())
		}
		appendGroup("bestcf", bestcf, bestcfQuota)
	}
	if nonCloudflare > 0 {
		sourceErrors = append(sourceErrors, fmt.Sprintf("CF 段闸剔除 %d 个非边缘 IP（ProxyIP 冒充优选）", nonCloudflare))
	}
	// bestcf 优选域名表：域名作为一等公民进池，绝不解析成 IP。
	// 拉取失败只记 error，不影响 IP 候选。
	forwardDomains := a.fetchBestcfForwardDomains(ctx)
	if len(forwardDomains) == 0 {
		sourceErrors = append(sourceErrors, "BestCF 优选域名表: 本次未获取到域名")
	}
	seen := make(map[string]struct{})
	ips := make([]string, 0, len(resolved))
	for _, raw := range resolved {
		if len(ips) >= candidatePoolTotalCap {
			break
		}
		ip := net.ParseIP(raw)
		if !isPublicIPv4(ip) {
			continue
		}
		key := ip.To4().String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		ips = append(ips, key)
	}
	if len(ips) == 0 {
		if len(sourceErrors) == 0 {
			sourceErrors = append(sourceErrors, "候选源未返回公网 IPv4")
		}
		log.Printf("proxy candidate refresh failed: no public IPv4 candidates; errors=%v", sourceErrors)
		snapshot := a.proxyCandidateSnapshot()
		snapshot.NextRefresh = time.Now().Add(proxyCandidateRefreshInterval)
		snapshot.Errors = sourceErrors
		a.candidateMu.Lock()
		a.candidates = snapshot
		a.candidateMu.Unlock()
		return false, errors.New("候选源未返回公网 IPv4")
	}
	now := time.Now()
	snapshot := proxyCandidateSnapshot{
		UpdatedAt:        now,
		NextRefresh:      now.Add(proxyCandidateRefreshInterval),
		Sources:          append([]string(nil), proxyCandidateSourceOrder...),
		IPs:              ips,
		SourceByIP:       sourceByIP,
		PreferredDomains: a.enabledPreferredDomainsForCandidates(),
		ForwardDomains:   forwardDomains,
		Errors:           sourceErrors,
	}
	a.candidateMu.Lock()
	a.candidates = snapshot
	a.candidateMu.Unlock()
	if err := a.saveProxyCandidateCache(snapshot); err != nil {
		log.Printf("save proxy candidate cache: %v", err)
	}
	log.Printf("proxy candidate cache refreshed: %d IPv4, next=%s", len(ips), snapshot.NextRefresh.Format(time.RFC3339))
	return true, nil
}

func (a *app) proxyCandidateSnapshot() proxyCandidateSnapshot {
	a.candidateMu.RLock()
	defer a.candidateMu.RUnlock()
	snapshot := a.candidates
	snapshot.Sources = append([]string(nil), snapshot.Sources...)
	snapshot.IPs = append([]string(nil), snapshot.IPs...)
	if snapshot.SourceByIP != nil {
		copySource := make(map[string]string, len(snapshot.SourceByIP))
		for ip, source := range snapshot.SourceByIP {
			copySource[ip] = source
		}
		snapshot.SourceByIP = copySource
	}
	snapshot.Errors = append([]string(nil), snapshot.Errors...)
	return snapshot
}

func (a *app) loadProxyCandidateCache() {
	data, err := os.ReadFile(filepath.Join(a.dataDir, proxyCandidateCacheFile))
	if err != nil {
		return
	}
	var snapshot proxyCandidateSnapshot
	if json.Unmarshal(data, &snapshot) != nil || len(snapshot.IPs) == 0 {
		return
	}
	valid := snapshot.IPs[:0]
	for _, raw := range snapshot.IPs {
		if ip := net.ParseIP(raw); isPublicIPv4(ip) {
			valid = append(valid, ip.To4().String())
		}
	}
	snapshot.IPs = valid
	if snapshot.SourceByIP == nil {
		snapshot.SourceByIP = make(map[string]string, len(valid))
	}
	validSources := make(map[string]string, len(valid))
	for _, ip := range valid {
		if source := snapshot.SourceByIP[ip]; source != "" {
			validSources[ip] = source
		}
	}
	snapshot.SourceByIP = validSources
	a.candidateMu.Lock()
	a.candidates = snapshot
	a.candidateMu.Unlock()
}

func (a *app) saveProxyCandidateCache(snapshot proxyCandidateSnapshot) error {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, proxyCandidateCacheFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isPublicIPv4(ip net.IP) bool {
	return ip != nil && ip.To4() != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast()
}

const maxCandidateSourceBody = 512 * 1024

func extractPublicIPv4(text string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	add := func(raw string) {
		if ip := net.ParseIP(raw); isPublicIPv4(ip) {
			key := ip.To4().String()
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				result = append(result, key)
			}
		}
	}
	parseText := func(value string) {
		for _, token := range strings.FieldsFunc(value, func(r rune) bool {
			return !(r == '.' || r >= '0' && r <= '9')
		}) {
			add(token)
		}
	}
	parseText(text)
	for _, line := range strings.Fields(text) {
		compact := strings.TrimSpace(line)
		if len(compact) < 16 || len(compact) > maxCandidateSourceBody {
			continue
		}
		for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			decoded, err := encoding.DecodeString(compact)
			if err == nil {
				parseText(string(decoded))
				break
			}
		}
	}
	return result
}

func fetchHTTPSBody(parent context.Context, rawURL string) ([]byte, error) {
	target, err := url.Parse(rawURL)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" {
		return nil, errors.New("候选源 URL 必须是 HTTPS")
	}
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !strings.EqualFold(req.URL.Hostname(), target.Hostname()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(parent, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("HTTP 状态码 %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCandidateSourceBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxCandidateSourceBody {
		return nil, errors.New("候选源内容超过 512 KiB")
	}
	return body, nil
}

func fetchProxyCandidateURL(parent context.Context, rawURL string) ([]string, error) {
	body, err := fetchHTTPSBody(parent, rawURL)
	if err != nil {
		return nil, err
	}
	return extractPublicIPv4(string(body)), nil
}

// extractPreferredDomains 从 090227 优选目录页面提取服务端渲染的优选域名。
// copyDomain 参数是页面当前权威清单；泛域名说明 .cf.090227.xyz 补充根域名。
func extractPreferredDomains(html string) []string {
	seen := make(map[string]struct{})
	domains := make([]string, 0, 16)
	add := func(raw string) {
		domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		domain = strings.TrimPrefix(domain, "*.")
		if domain == "" || !validCandidateHostname(domain) {
			return
		}
		if _, ok := seen[domain]; ok || len(domains) >= maxPreferredDomains {
			return
		}
		seen[domain] = struct{}{}
		domains = append(domains, domain)
	}
	for _, match := range preferredDomainPattern.FindAllStringSubmatch(html, -1) {
		add(match[1])
	}
	// 页面泛域名说明：*.cf.090227.xyz / *.123.cf.090227.xyz 与根域名等效。
	for _, root := range []string{"*.cf.090227.xyz", "*.123.cf.090227.xyz"} {
		if strings.Contains(html, root) {
			add(root)
		}
	}
	return domains
}

// fetchBestcfForwardDomains 拉取 bestcf 优选域名迷你表并解析出域名列表。
// 输入格式 `域名:端口#备注`（与 bestcf.pages.dev/domain/mini.txt 一致），
// 也兼容裸域名。只留主机名，端口与备注全部剥去；绝不解析 DNS。
// 拉取成败与域名数入健康账（渠道审计可辨此表死活）。
func (a *app) fetchBestcfForwardDomains(ctx context.Context) []string {
	body, err := fetchHTTPSBody(ctx, bestcfForwardDomainURL)
	if err != nil {
		a.recordSourceHealth(bestcfForwardDomainURL, 0, err)
		return nil
	}
	domains := extractForwardDomainList(string(body))
	a.recordSourceHealth(bestcfForwardDomainURL, len(domains), nil)
	return domains
}

// extractForwardDomainList 从 `域名:端口#备注` / `IP:端口#备注` 混合文本中
// 提取可转发域名。IP 行天然被 validCandidateHostname 排除（它是 IP 校验的
// 反面：net.ParseIP 命中即弃）。
func extractForwardDomainList(text string) []string {
	seen := make(map[string]struct{})
	domains := make([]string, 0, maxForwardDomains)
	for _, line := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		entry := strings.TrimSpace(line)
		if entry == "" {
			continue
		}
		if at := strings.IndexAny(entry, "#＃"); at >= 0 {
			entry = strings.TrimSpace(entry[:at])
		}
		if host, _, err := net.SplitHostPort(entry); err == nil {
			entry = host
		} else if idx := strings.LastIndex(entry, ":"); idx >= 0 && !strings.Contains(entry, "]") {
			// 无方括号的 尾端口 形式（example.com:443）
			candidate := strings.TrimSpace(entry[:idx])
			if _, err := strconv.Atoi(strings.TrimSpace(entry[idx+1:])); err == nil {
				entry = candidate
			}
		}
		entry = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(entry)), ".")
		if entry == "" || net.ParseIP(entry) != nil || !validCandidateHostname(entry) {
			continue
		}
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		if len(domains) >= maxForwardDomains {
			break
		}
		domains = append(domains, entry)
	}
	return domains
}

// resolveProxySources 抓取指定候选源并合流，最多返回 remaining 个 Cloudflare IP。
//
// 两个刻意的设计：
//  1. 所有目标并发抓取。旧实现按 URL 顺序串行，前几个源（尤其 lzj 那种单源
//     193 个的）会先把配额吃满，后面的源永远轮不到——健康账里 34 个 URL
//     只有 11 个有记录，就是这个原因。并发之后每个源都有机会被看见。
//  2. 合流用 round-robin。小源耗尽后自然退出，大源继续贡献，既保证多样性，
//     又不会因为某个源排在前面就垄断配额。
func (a *app) resolveProxySources(parent context.Context, sourceIDs []string, remaining int) ([]string, []string, error) {
	if remaining <= 0 || len(sourceIDs) == 0 {
		return []string{}, []string{}, nil
	}
	if len(sourceIDs) > len(proxyCandidateSources) {
		return nil, nil, errors.New("候选源数量无效")
	}

	type fetchTarget struct {
		healthKey string // 健康账归集键
		domain    string // 非空则走 DNS 解析
		url       string // 非空则走 HTTPS 抓取
	}
	targets := make([]fetchTarget, 0, 32)
	seenSources := make(map[string]struct{})
	for _, id := range sourceIDs {
		source, ok := proxyCandidateSources[id]
		if !ok {
			return nil, nil, fmt.Errorf("未知候选源: %s", id)
		}
		if _, ok := seenSources[id]; ok {
			continue
		}
		seenSources[id] = struct{}{}
		for _, domain := range source.Domains {
			targets = append(targets, fetchTarget{healthKey: "dns:" + domain, domain: domain})
		}
		for _, sourceURL := range source.URLs {
			targets = append(targets, fetchTarget{healthKey: sourceURL, url: sourceURL})
		}
	}

	results := make([][]string, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if targets[i].domain != "" {
				ctx, cancel := context.WithTimeout(parent, 5*time.Second)
				addresses, err := net.DefaultResolver.LookupIPAddr(ctx, targets[i].domain)
				cancel()
				if err != nil {
					errs[i] = err
					return
				}
				v4 := make([]string, 0, len(addresses))
				for _, address := range addresses {
					if ip := address.IP.To4(); ip != nil {
						v4 = append(v4, ip.String())
					}
				}
				results[i] = v4
				return
			}
			found, err := fetchProxyCandidateURL(parent, targets[i].url)
			results[i] = found
			errs[i] = err
		}()
	}
	wg.Wait()

	// 逐目标记账：渠道审计与优选台心跳都以健康账辨死源。
	errorsFound := make([]string, 0)
	for i, target := range targets {
		a.recordSourceHealth(target.healthKey, len(results[i]), errs[i])
		if errs[i] != nil {
			errorsFound = append(errorsFound, fmt.Sprintf("%s: %v", target.healthKey, errs[i]))
		}
	}

	// round-robin 合流，只收 Cloudflare 边缘段内的地址。
	ips := make([]string, 0, remaining)
	seenIP := make(map[string]struct{}, remaining)
	cursor := make([]int, len(results))
	for len(ips) < remaining {
		progressed := false
		for i := range results {
			for cursor[i] < len(results[i]) {
				raw := results[i][cursor[i]]
				cursor[i]++
				ip := net.ParseIP(strings.TrimSpace(raw))
				if !isCloudflareIPv4(ip) {
					continue
				}
				key := ip.To4().String()
				if _, ok := seenIP[key]; ok {
					continue
				}
				seenIP[key] = struct{}{}
				ips = append(ips, key)
				progressed = true
				break
			}
			if len(ips) >= remaining {
				return ips, errorsFound, nil
			}
		}
		if !progressed {
			break
		}
	}
	return ips, errorsFound, nil
}
