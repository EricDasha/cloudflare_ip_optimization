package main

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"syscall"
)

// 失败归类：把千变万化的底层报错收敛成有限的几类。
//
// 为什么需要它：候选池只知道「通/不通」，于是同一个段里的 IP 成批死掉时，
// 系统看不出这是「整片被掐」还是「个别地址没了」，只能一个个盲试。
// 归类之后，片区级统计才有意义（见 segmentFailureStats）。
//
// 判定顺序：先认类型（net.Error 超时、syscall 错误码、tls 类型错误），
// 字符串匹配只做兜底——Go 的报错文本随版本与平台变化，只靠文本会误归类。
const (
	failureNone        = ""            // 成功
	failureTimeout     = "timeout"     // 超时 / 静默丢包：最常见于整片被掐
	failureRefused     = "refused"     // 对方主动拒绝：地址没了或端口关了
	failureUnreachable = "unreachable" // 无路由 / 网络不可达
	failureHandshake   = "handshake"   // TLS 握手失败：对方不是目标服务或证书不对
	failureProtocol    = "protocol"    // 连上了但协议不对：WS 状态码非 101、提前 EOF
	failureOther       = "other"       // 归不了类
)

// classifyProbeError 把探测错误归入有限类别。nil 返回 failureNone。
func classifyProbeError(err error) string {
	if err == nil {
		return failureNone
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return failureTimeout
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return failureRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return failureUnreachable
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return failureProtocol
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return failureHandshake
	}
	// 兜底：文本匹配。注意大小写与常见变体，宁可归 other 也不乱归类。
	// 中文「超时」也要认：探针自己产的报错是中文（VLESS 探针启动超时），
	// 只认英文会把它归成 other，改动 B 的降级防线（ErrorClass != timeout
	// 才准入）就被绕过——超时类失败照样进降级池。
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "超时"):
		return failureTimeout
	case strings.Contains(msg, "connection refused"):
		return failureRefused
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "network is unreachable"):
		return failureUnreachable
	case strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"), strings.Contains(msg, "handshake"):
		return failureHandshake
	case strings.Contains(msg, "ws 状态码"), strings.Contains(msg, "eof"), strings.Contains(msg, "connection reset"):
		return failureProtocol
	}
	return failureOther
}

// segmentFailureStat 是单个 CF 段在一轮探测里的成绩。
type segmentFailureStat struct {
	Total   int
	Failed  int
	Classes map[string]int
}

// segmentFailureStats 按段汇总一轮探测结果。
// 非公网地址（segmentOf 返回空）不进统计——它们本来就不该出现在候选池里。
// 官方段外的公网地址按 /16 归组后照常参与，见 segmentOf 的说明。
func segmentFailureStats(results []proxyScanResult) map[string]segmentFailureStat {
	out := make(map[string]segmentFailureStat, 4)
	for _, result := range results {
		seg := segmentOf(net.ParseIP(result.IP))
		if seg == "" {
			continue
		}
		st := out[seg]
		if st.Classes == nil {
			st.Classes = make(map[string]int, 2)
		}
		st.Total++
		if result.Error != "" {
			st.Failed++
			class := result.ErrorClass
			if class == "" {
				class = failureOther
			}
			st.Classes[class]++
		}
		out[seg] = st
	}
	return out
}

// demotedSegments 返回本轮应当降权的段：失败率过高且样本足够。
//
// 降权不是拉黑：只把该段的候选排到名单末尾（见 orderPassedBySegmentHealth），
// 候选不够时照样会被采用。2026-10-22 的教训是「闸门拦得太狠会把服务拦死」，
// 所以这里既要求最小样本量（一两个候选失败 50% 说明不了问题），
// 也只在单轮内生效——下一轮样本滚动覆盖，片区恢复就自动解除，无需人工重置。
func demotedSegments(results []proxyScanResult) map[string]struct{} {
	stats := segmentFailureStats(results)
	out := make(map[string]struct{}, len(stats))
	for seg, st := range stats {
		if st.Total < segmentDemoteMinSamples {
			continue
		}
		if float64(st.Failed)/float64(st.Total) >= segmentDemoteFailureRatio {
			out[seg] = struct{}{}
		}
	}
	return out
}

// orderPassedBySegmentHealth 把被降权段的候选稳定移到名单末尾。
// 输入顺序（Mbps / 延迟 / 机房偏好）在两组内部原样保留；名单长度不变。
func orderPassedBySegmentHealth(passed []string, results []proxyScanResult) []string {
	demoted := demotedSegments(results)
	if len(demoted) == 0 || len(passed) < 2 {
		return passed
	}
	head := make([]string, 0, len(passed))
	tail := make([]string, 0, len(passed))
	for _, ip := range passed {
		if _, ok := demoted[segmentOf(net.ParseIP(ip))]; ok {
			tail = append(tail, ip)
		} else {
			head = append(head, ip)
		}
	}
	return append(head, tail...)
}

// degradedSegmentDiversity 报告一次换人操作是否会让池的段数变少。
// 池成员顺序就是 cfnat 的尝试顺序，但段数决定的是抗单点能力——两者都要。
// 官方段外的公网成员按 /16 计入（见 segmentOf），非公网地址不计入，
// 避免测试地址干扰判断。
func degradedSegmentDiversity(before, after []string) bool {
	beforeSegs := poolSegmentStats(before)
	afterSegs := poolSegmentStats(after)
	return len(afterSegs) < len(beforeSegs)
}

// cloudflareIPv4CIDRs 是 Cloudflare 官方公布的 IPv4 边缘段（15 段）。
//
// 用途：段标签与统计——2026-11-05 起不再作为候选池准入闸（实测：
// 官方段外的反代 IP 同样可用，「像不像 Cloudflare 自己的地址」与
// 「能不能把流量送到 Cloudflare」并不等价，后者由真实考试判定）。
// 现仅用于：候选汇合时统计段外成员数（offEdgeCount）、池段分布
// （poolSegmentStats）与降级多样性判断。
var cloudflareIPv4CIDRs = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
}

var cloudflareIPv4Nets = mustParseCloudflareCIDRs(cloudflareIPv4CIDRs)

func mustParseCloudflareCIDRs(values []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic("invalid Cloudflare IPv4 CIDR: " + value)
		}
		nets = append(nets, network)
	}
	return nets
}

// isCloudflareIPv4 报告 ip 是否落在 Cloudflare 官方 IPv4 边缘段内。
// 非 IPv4（含 nil、IPv6）一律 false：入口池只收 IPv4。
func isCloudflareIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	for _, network := range cloudflareIPv4Nets {
		if network.Contains(v4) {
			return true
		}
	}
	return false
}

// segmentOf 返回 IP 所属片区的标识，用于建池时的段多样性配额。
//
// 官方段内返回官方 CIDR（如 "104.16.0.0/13"）；官方段外的公网地址返回 /16
// 归组（如 "43.159.0.0/16"）。非公网地址（私网、回环、非法输入）仍返回空串——
// 它们进不了池，也不该污染段统计。
//
// 为什么非官方段必须归组：2026-11-05 起官方段从准入闸降级为标签，非官方段的
// 反代 IP 可以正常进池（线上实测 45 个成员里 30 个属于此类，转发正常）。但段
// 多样性的整套逻辑（spreadBySegment / degradedSegmentDiversity / 片区降权）
// 都靠 segmentOf 分组，若这些地址返回空串就会被排除在计数之外——池里 30 个
// 成员对防单点保护完全隐形，而所有指标照常显示正常。这类漏洞不会报错，
// 只会让「一个片区全挂」的风险悄悄回来。
//
// /16 是一个刻意的折中：比 /13 细、比 /24 粗。同一家反代商常在一个 /16 内
// 连续分配地址，用 /16 分组能把它们认作同一片区；再细就会把真正分散的地址
// 误判成独立片区，反而失去保护意义。
func segmentOf(ip net.IP) string {
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	for _, network := range cloudflareIPv4Nets {
		if network.Contains(v4) {
			return network.String()
		}
	}
	if !isPublicIPv4(ip) {
		return ""
	}
	return (&net.IPNet{IP: v4.Mask(net.CIDRMask(16, 32)), Mask: net.CIDRMask(16, 32)}).String()
}

// poolSegmentStats 统计一组 IP 的段分布，返回 段标识 -> 个数。
func poolSegmentStats(ips []string) map[string]int {
	out := make(map[string]int, 4)
	for _, raw := range ips {
		if seg := segmentOf(net.ParseIP(raw)); seg != "" {
			out[seg]++
		}
	}
	return out
}
