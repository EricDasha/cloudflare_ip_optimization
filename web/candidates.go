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
	"time"
)

// Candidate collection and result sorting must use the same order. The first
// source that supplies an IP owns its source label, so this order also defines
// which source wins when the same address appears in multiple feeds.
// proxyCandidateSourceOrder 候选供给层（考试优先序）：
// user > subscription > bestcf(全量优选)。
// official 官方段已砍（用户指令）；preferred 优选域名不解析供 IP——域名直接
// 升格为 -fixed 转发成员（拨号时活解析）；cfdata(baipiao 上游)已砍。
var proxyCandidateSourceOrder = []string{"user", "subscription", "bestcf"}

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
	// 自然落网外，无需新解析器。页面标注"更新异常"的源不收；源生死由
	// 维护者决定，全部走 HTTPS 白名单校验，单个失败只记 sourceErrors。
	"bestcf": {
		Name: "BestCF 全量优选 IP",
		URLs: []string{
			// bestcf.pages.dev 自托管镜像
			"https://bestcf.pages.dev/cmliu/all.txt",
			"https://bestcf.pages.dev/cmliu2/all.txt",
			"https://bestcf.pages.dev/wetest/ipv4.txt",
			"https://bestcf.pages.dev/uouin/all.txt",
			"https://bestcf.pages.dev/luoli/all.txt",
			"https://bestcf.pages.dev/lzj/all.txt",
			"https://bestcf.pages.dev/xinyitang3/ipv4.txt",
			"https://bestcf.pages.dev/cfyes/ipv4.txt",
			"https://bestcf.pages.dev/vvhan/ipv4.txt",
			"https://bestcf.pages.dev/tiancheng/all.txt",
			"https://bestcf.pages.dev/gslege/Cfxyz.txt",
			"https://bestcf.pages.dev/zhixuanwang/ipv4-onlyip.txt",
			"https://bestcf.pages.dev/s5gy/all.txt",
			"https://bestcf.pages.dev/nirevil/ipv4.txt",
			"https://bestcf.pages.dev/ircf/ipv4.txt",
			"https://bestcf.pages.dev/vps789/top100.txt",
			// GitHub 原始列表（页面 #yxym 收录的活跃维护者）
			"https://raw.githubusercontent.com/cmliu/WorkerVless2sub/main/addressesapi.txt",
			"https://raw.githubusercontent.com/ymyuuu/IPDB/main/BestCF/bestcfv4.txt",
			"https://raw.githubusercontent.com/yuanxiawan/cfipv4db/main/cfip.txt",
			"https://raw.githubusercontent.com/LancelotRar/best-cf-ips/main/best-cf-ip-collected.txt",
			"https://raw.githubusercontent.com/joname1/BestCFip/main/ipv4.txt",
			"https://raw.githubusercontent.com/ahang39/router/main/all.txt",
			"https://raw.githubusercontent.com/einsitang/my-fast-cf-ip/master/fastips.txt",
			"https://raw.githubusercontent.com/hubbylei/bestcf/main/bestcf.txt",
			"https://raw.githubusercontent.com/gshtwy/CF-DNS-Clone/main/wetest-cloudflare-v4.txt",
			"https://raw.githubusercontent.com/Senflare/Senflare-IP/main/IPlist-Pro.txt",
			"https://raw.githubusercontent.com/JieChaoCC/cf-ip-auto/main/data/ipapi.txt",
			// MJZ / SS / LZ（运营商分网维护）
			"https://cf.junzhen.qzz.io/best_ips.txt",
			"https://cf.junzhen.qzz.io/best_ips_bj.txt",
			"https://raw.githubusercontent.com/svip-s/cloudflare_ip/main/best_ips.txt",
			"https://raw.githubusercontent.com/love-ztm/cfip/main/best_ips.txt",
			"https://raw.githubusercontent.com/love-ztm/cfip/main/ubest_ips.txt",
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
	// 现行供给层：user > subscription > bestcf(社区优选表全量合并)。
	// preferred(优选域名)不解析供 IP——域名直接升格为 -fixed 转发成员；
	// official(官方段)与 cfdata(baipiao 上游)已按用户指令砍除。
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
	// subscription 层：≤600，且必须为 bestcf 保底额度让位——
	// 生产教训（sub 独占 1000/1000，其余层=0）不允许重演。
	reserved := bestcfCandidatesFloor
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
	// 官方段已砍（用户指令）：bestcf 全量列表 + 订阅已是充分供给。
	// bestcf 层：地板配额已由 sub 的让位计算保证，余量全归 bestcf 兜底。
	bestcfQuota := candidatePoolTotalCap - len(resolved)
	if bestcfQuota > 0 {
		bestcf, bestcfErrors, err := a.resolveProxySources(ctx, defaultProxyCandidateSourceIDs, bestcfQuota)
		sourceErrors = append(sourceErrors, bestcfErrors...)
		if err != nil {
			sourceErrors = append(sourceErrors, err.Error())
		}
		appendGroup("bestcf", bestcf, bestcfQuota)
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

func (a *app) resolveProxySources(parent context.Context, sourceIDs []string, remaining int) ([]string, []string, error) {
	if remaining <= 0 || len(sourceIDs) == 0 {
		return []string{}, []string{}, nil
	}
	if len(sourceIDs) > len(proxyCandidateSources) {
		return nil, nil, errors.New("候选源数量无效")
	}
	seenSources := make(map[string]struct{})
	ips := make([]string, 0, remaining)
	errorsFound := make([]string, 0)
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
			ctx, cancel := context.WithTimeout(parent, 5*time.Second)
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, domain)
			cancel()
			if err != nil {
				errorsFound = append(errorsFound, fmt.Sprintf("%s: %v", source.Name, err))
				a.recordSourceHealth("dns:"+domain, 0, err)
				continue
			}
			v4 := make([]string, 0, len(addresses))
			for _, address := range addresses {
				if ip := address.IP.To4(); ip != nil {
					v4 = append(v4, ip.String())
				}
			}
			a.recordSourceHealth("dns:"+domain, len(v4), nil)
			ips = append(ips, v4...)
			if len(ips) >= remaining {
				return ips, errorsFound, nil
			}
		}
		for _, sourceURL := range source.URLs {
			if len(ips) >= remaining {
				return ips, errorsFound, nil
			}
			found, err := fetchProxyCandidateURL(parent, sourceURL)
			// 逐 URL 记健康账：渠道审计与优选台心跳以此辨死源。
			a.recordSourceHealth(sourceURL, len(found), err)
			if err != nil {
				errorsFound = append(errorsFound, fmt.Sprintf("%s: %v", source.Name, err))
				continue
			}
			ips = append(ips, found...)
		}
	}
	return ips, errorsFound, nil
}
