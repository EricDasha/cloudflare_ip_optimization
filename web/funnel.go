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

// vlessSpeedProbeConfig 统一构造测速参数：预算、超时、及格线与提前放弃
// 阈值必须由排序和补测共用，否则同一批候选会出现两套口径。
func vlessSpeedProbeConfig(base vlessProbeConfig) (vlessProbeConfig, int) {
	speedCfg := base
	speedBytes := envInt("PROXY_VLESS_SPEED_BYTES", 2097152)
	if speedBytes < 1048576 || speedBytes > 33554432 {
		speedBytes = 2097152
	}
	speedTimeout := envInt("PROXY_VLESS_SPEED_TIMEOUT", 20)
	if speedTimeout < 5 || speedTimeout > 120 {
		speedTimeout = 20
	}
	speedCfg.TestURL = vlessSpeedTestURL(speedBytes)
	speedCfg.ExpectedStatus = http.StatusOK
	speedCfg.ReadLimit = int64(speedBytes)
	speedCfg.MinBytes = int64(speedBytes)
	speedCfg.Timeout = time.Duration(speedTimeout) * time.Second
	// 提前放弃阈值取参考值的 40%：远低于及格线的线路，读满 2MB 只是
	// 把时间预算耗光，结论不会变。观察期 3 秒由采样器负责。
	if minMbps := vlessMinDownloadMbps(); envBool("PROXY_VLESS_SPEED_EARLY_ABORT", true) && minMbps > 0 {
		speedCfg.EarlyAbortMbps = minMbps * 0.4
	}
	// 串行测速之间留间隔，避免连续 2MB 下载把同一条出口压出瞬时丢包。
	// 默认 250ms，可用 PROXY_VLESS_SPEED_INTERVAL_MS 调整（0 关闭）。
	intervalMs := envInt("PROXY_VLESS_SPEED_INTERVAL_MS", 250)
	if intervalMs < 0 || intervalMs > 2000 {
		intervalMs = 250
	}
	return speedCfg, intervalMs
}

func rankVLESSPassesBySpeed(parent context.Context, passed []string, results []proxyScanResult, port int, base vlessProbeConfig, template map[string]any) []string {
	return rankVLESSPassesBySpeedWithProbe(parent, passed, results, port, base, template, probeVLESSCandidateMetrics)
}

// rankVLESSPassesBySpeedWithHistory 与 rankVLESSPassesBySpeed 相同，但把
// 上一轮的在位成绩带进排序做 EWMA 平滑。历史只影响先后，不影响准入门槛：
// results[].DownloadMbps 与 dropBelowReferenceIPs 仍读本轮新鲜值。
func rankVLESSPassesBySpeedWithHistory(parent context.Context, passed []string, results []proxyScanResult, port int, base vlessProbeConfig, template map[string]any, history []proxyScanResult) []string {
	return rankVLESSPassesBySpeedWithProbeAndHistory(parent, passed, results, port, base, template, probeVLESSCandidateMetrics, history)
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
	return rankVLESSPassesBySpeedWithProbeAndHistory(parent, passed, results, port, base, template, probe, nil)
}

func rankVLESSPassesBySpeedWithProbeAndHistory(
	parent context.Context,
	passed []string,
	results []proxyScanResult,
	port int,
	base vlessProbeConfig,
	template map[string]any,
	probe func(context.Context, string, int, vlessProbeConfig, map[string]any) (vlessProbeMetrics, error),
	history []proxyScanResult,
) []string {
	if len(passed) == 0 {
		return passed
	}
	speedCfg, intervalMs := vlessSpeedProbeConfig(base)
	times := make(map[string]int64, len(passed))
	speeds := make(map[string]float64, len(passed))
	minMbps := vlessMinDownloadMbps()
	speedInterval := time.Duration(intervalMs) * time.Millisecond
	// 测速只对前 maxSpeedProbes 个候选执行：终审池上限本来就只有个位数，
	// 没必要为排序把每个候选都跑一遍 2MB 下载。上限默认与池宽对齐，
	// 否则池里总有几个成员零吞吐证据，排序只能退回握手延迟。
	maxSpeedProbes := len(passed)
	if limit := maxVLESSSpeedProbes(); maxSpeedProbes > limit {
		maxSpeedProbes = limit
	}
	verified := make([]string, 0, len(passed))
	for idx, ip := range passed {
		if idx < maxSpeedProbes {
			if idx > 0 && speedInterval > 0 {
				select {
				case <-parent.Done():
					return passed
				case <-time.After(speedInterval):
				}
			}
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
			//
			// 但成绩必须记账：低于参考值的也要写入 speeds。旧实现把「实测
			// 0.58 Mbps」和「从未测过」一起记成零分，慢 IP 随后靠低延迟
			// 排到最前——2026-11 掉速事故的成因。
			if err == nil {
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
						if metrics.EarlyAbort {
							results[i].Note += "，已提前放弃"
						}
					} else if metrics.EarlyAbort {
						results[i].Note = "测速提前放弃（样本已足，仅影响排序）"
					}
					break
				}
			}
		}
		verified = append(verified, ip)
	}
	passed = verified
	blendSpeedsWithHistory(speeds, history, ewmaRankingAlpha())
	logSpeedProbeSummary(speeds, maxSpeedProbes, minMbps)
	latencyCeiling := vlessLatencyCeilingMs()
	sort.SliceStable(passed, func(i, j int) bool {
		// 延迟天花板：吞吐排序只看速度，看不见往返时间。一个 4 Mbps / 720ms
		// 的入口下载能跑满，但每一次请求都要多等半秒——浏览、交互全是这个代价。
		// 2026-11-14 线上实证：cfdata 灌进池子的 4 个 IP 吞吐 3.4~4.3 Mbps、
		// 握手 550~722ms，而在位的订阅 IP 是 245~292ms，用户感受正是
		// 「速度没问题但很卡」。超线的无论多快都排到线内之后。
		leftLaggy := vlessLatencyLaggy(results, passed[i], latencyCeiling)
		rightLaggy := vlessLatencyLaggy(results, passed[j], latencyCeiling)
		if leftLaggy != rightLaggy {
			return !leftLaggy
		}
		leftBucket := speedBucket(speeds, passed[i], minMbps)
		rightBucket := speedBucket(speeds, passed[j], minMbps)
		if leftBucket != rightBucket {
			return leftBucket < rightBucket
		}
		leftSpeed, rightSpeed := speeds[passed[i]], speeds[passed[j]]
		if leftSpeed != rightSpeed {
			return leftSpeed > rightSpeed
		}
		leftRank := proxyResultSourceRank(results, passed[i])
		rightRank := proxyResultSourceRank(results, passed[j])
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

// fillMissingSpeedMetrics 给已经进入最终名单、但还没测过速的成员补一次。
//
// 排序阶段只测前 maxVLESSSpeedProbes 个候选，后面的候选仍可能因为片区
// 轮转、延迟闸或补位进池。没有吞吐证据的成员只能靠数据面延迟兜底，
// 容易把「延迟正常但带宽很窄」的线路放进来。补测只覆盖最终名单，
// 不扩大整轮扫描的测速总量。
//
// 返回是否真的补测过，调用方据此决定要不要重新跑一次劣质剔除。
func fillMissingSpeedMetrics(parent context.Context, passed []string, results []proxyScanResult, port int, base vlessProbeConfig, template map[string]any, probe func(context.Context, string, int, vlessProbeConfig, map[string]any) (vlessProbeMetrics, error)) bool {
	speedCfg, intervalMs := vlessSpeedProbeConfig(base)
	speedInterval := time.Duration(intervalMs) * time.Millisecond
	minMbps := vlessMinDownloadMbps()
	limit := maxVLESSSpeedProbes()
	probed := 0
	for _, ip := range passed {
		if probed >= limit {
			break
		}
		result := proxyScanResultForIP(results, ip)
		if result == nil || result.DownloadMbps > 0 {
			continue
		}
		// Note 以「测速」开头说明已经试过（成功记了成绩，失败记了原因）。
		// 失败不重试：同一条出口连续测两次不会变好，只会多耗流量。
		if strings.HasPrefix(result.Note, "测速") {
			continue
		}
		if probed > 0 && speedInterval > 0 {
			select {
			case <-parent.Done():
				return probed > 0
			case <-time.After(speedInterval):
			}
		}
		ctx, cancel := context.WithTimeout(parent, speedCfg.Timeout)
		metrics, err := probe(ctx, ip, port, speedCfg, template)
		cancel()
		if err == nil {
			result.DownloadMbps = metrics.Mbps
			if metrics.Mbps < minMbps {
				result.Note = fmt.Sprintf("测速 %.2f Mbps 低于参考值 %.2f Mbps（补测，仅影响排序）", metrics.Mbps, minMbps)
				if metrics.EarlyAbort {
					result.Note += "，已提前放弃"
				}
			} else if metrics.EarlyAbort {
				result.Note = "测速提前放弃（补测，样本已足）"
			} else {
				result.Note = fmt.Sprintf("测速 %.2f Mbps（补测）", metrics.Mbps)
			}
		} else {
			result.Note = "测速未完成（补测，仅影响排序）：" + err.Error()
		}
		probed++
	}
	if probed > 0 {
		log.Printf("vless speed fill: probed %d unmeasured finalists", probed)
	}
	return probed > 0
}

func proxyScanResultForIP(results []proxyScanResult, ip string) *proxyScanResult {
	for i := range results {
		if results[i].IP == ip {
			return &results[i]
		}
	}
	return nil
}

// vlessMinDownloadMbps 是「及格线」：低于它的 IP 排到未测成员之后，
// 并被 dropBelowReferenceIPs 挡在池外。它不再是一票否决的录取门槛，
// 只是排序与占座的分界线。
func vlessMinDownloadMbps() float64 {
	mbps := envFloat("PROXY_VLESS_MIN_DOWNLOAD_MBPS", 3)
	if mbps < 0 || mbps > 10000 {
		mbps = 3
	}
	return mbps
}

// ewmaRankingAlpha 是跨轮吞吐平滑系数。0 完全关闭；上限 0.9 防止历史权重过大。
func ewmaRankingAlpha() float64 {
	alpha := envFloat("PROXY_EWMA_ALPHA", 0.3)
	if alpha < 0 || alpha > 0.9 {
		return 0.3
	}
	return alpha
}

// blendSpeedsWithHistory 把上一轮的在位吞吐混进本轮排序权重。
//
// 只改 speeds（排序用），不改 results[].DownloadMbps（准入与展示用）。
// 结果被夹在本轮值的 [0.67, 1.5] 倍之间：历史最多解释 50% 的差距，
// 一个真实崩掉的 IP 不会被旧成绩长期托住。
func blendSpeedsWithHistory(speeds map[string]float64, history []proxyScanResult, alpha float64) {
	if len(speeds) == 0 || len(history) == 0 || alpha <= 0 {
		return
	}
	previous := make(map[string]float64, len(history))
	for _, result := range history {
		if result.DownloadMbps > 0 {
			previous[result.IP] = result.DownloadMbps
		}
	}
	for ip, fresh := range speeds {
		prior := previous[ip]
		if fresh <= 0 || prior <= 0 {
			continue
		}
		blended := alpha*fresh + (1-alpha)*prior
		if blended < fresh*0.67 {
			blended = fresh * 0.67
		}
		if blended > fresh*1.5 {
			blended = fresh * 1.5
		}
		speeds[ip] = blended
	}
}

// speedBucket 把测速结果分成三档，供排序使用：
//
//	0 = 实测及格（>= minMbps），1 = 未测（未知），2 = 实测不及格。
//
// 三档必须分开。旧实现只把及格成绩写进 speeds，于是「实测 0.58 Mbps」与
// 「从未测过」同为零分，慢 IP 反而靠握手延迟排到最前。未知不等于差，
// 但已知的差必须垫底。
func speedBucket(speeds map[string]float64, ip string, minMbps float64) int {
	v, ok := speeds[ip]
	if !ok {
		return 1
	}
	if v >= minMbps {
		return 0
	}
	return 2
}

// vlessLatencyCeilingMs 是排序用的「慢线」分界，不是入池硬闸。
//
// 入池硬闸是 poolEntryMaxLatencyMs（默认 800ms），它只回答「能不能进来」；
// 这条回答的是「进来之后排第几」。两者不能合并：硬闸调紧会让池子变窄，
// 而慢线成员在候选不够时仍然有用——只是不该优先占座。
//
// 默认 450ms 的依据是 2026-11-14 线上实测：在位订阅 IP 245~292ms，
// 灌进池子的 cfdata IP 550~722ms。旧的 800ms 硬闸把两批一起放行，
// 而吞吐排序只看速度，于是 4 Mbps/720ms 排到了未测速的 250ms 前面。
func vlessLatencyCeilingMs() int64 {
	ms := int64(envInt("PROXY_VLESS_MAX_LATENCY_MS", 450))
	if ms < 0 || ms > 5000 {
		ms = 450
	}
	return ms
}

// vlessLatencyLaggy 报告某成员握手是否慢到不该优先占座。
// results 里查不到成绩时按「不慢」处理：未知不等于差，不能因为没数据就判死。
func vlessLatencyLaggy(results []proxyScanResult, ip string, ceiling int64) bool {
	if ceiling <= 0 {
		return false
	}
	for _, result := range results {
		if result.IP == ip {
			return result.Latency > 0 && result.Latency > ceiling
		}
	}
	return false
}

// partitionLaggyLast 把超过延迟天花板的成员稳定地挪到末尾，组内顺序不变。
// 只分区不淘汰：候选不够时慢线成员照样能用，只是不再优先占座。
func partitionLaggyLast(passed []string, results []proxyScanResult, ceiling int64) []string {
	if ceiling <= 0 || len(passed) < 2 {
		return passed
	}
	out := make([]string, 0, len(passed))
	laggy := make([]string, 0, len(passed))
	for _, ip := range passed {
		if vlessLatencyLaggy(results, ip, ceiling) {
			laggy = append(laggy, ip)
			continue
		}
		out = append(out, ip)
	}
	return append(out, laggy...)
}

// logSpeedProbeSummary 每轮打一行测速汇总。
// 生产日志此前没有任何速度数字，排查「为什么慢 IP 会进池」只能靠翻状态文件。
func logSpeedProbeSummary(speeds map[string]float64, probed int, minMbps float64) {
	if probed == 0 {
		return
	}
	if len(speeds) == 0 {
		log.Printf("vless speed probes: 0/%d measured (all failed)", probed)
		return
	}
	vals := make([]float64, 0, len(speeds))
	below := 0
	for _, v := range speeds {
		vals = append(vals, v)
		if v < minMbps {
			below++
		}
	}
	sort.Float64s(vals)
	log.Printf("vless speed probes: %d/%d measured, %d below %.1f Mbps, median %.2f Mbps",
		len(vals), probed, below, minMbps, vals[len(vals)/2])
}

// dropBelowReferenceIPs 把「实测吞吐低于参考值」的 IP 从入池名单里剔除。
//
// 只剔除有成绩且不及格的成员：未测（DownloadMbps==0 且不是测速失败）与
// 及格的都不受影响——未知不等于差。剔除后若剩余成员少于 minKeep，不是
// 把全部慢 IP 原样留下，而是按吞吐从高到低补足到 minKeep。
//
// 2026-11-15 修正：旧实现「不够 minKeep 就返回 passed」在生产上退化成
// 「一个都不剔」。当时池里 10 个 IP 只有 2 个过 3 Mbps，minKeep=3，于是
// 8 个 1.2~2.3 Mbps 的慢 IP 全被保留，cfnat 的 sticky 又把设备粘到其中
// 最慢的一个上。最小池要的是「至少 minKeep 个可用落点」，不是「保留所有
// 落点」——补足名额只需挑最快的几个慢 IP，其余仍应剔除。
func dropBelowReferenceIPs(passed []string, results []proxyScanResult, minKeep int, minMbps float64) []string {
	return dropBelowReferenceIPsWithDataCeiling(passed, results, minKeep, minMbps, vlessDataLatencyCeilingMs())
}

// vlessDataLatencyCeilingMs 是数据面延迟的天花板。
//
// 握手延迟只覆盖 TCP/TLS/WS 三段；VLESS 数据面还要经过 Worker 和 ProxyIP。
// 2026-11-16 线上实例：116.49.234.130 握手 232ms 合格，数据面 1618ms，且
// 因为测速没跑出成绩（DownloadMbps==0）被当成「未知」保留进池。用户感受
// 就是「能连、但每个请求都慢半拍」。超过天花板与吞吐不达标同属「低于
// 参考值」，可以被补位逻辑剔除；未测（DataLatency==0）仍然不判慢。
func vlessDataLatencyCeilingMs() int64 {
	ms := int64(envInt("PROXY_VLESS_DATA_LATENCY_CEILING_MS", 1200))
	if ms < 0 || ms > 10000 {
		ms = 1200
	}
	return ms
}

func dropBelowReferenceIPsWithDataCeiling(passed []string, results []proxyScanResult, minKeep int, minMbps float64, dataCeiling int64) []string {
	if len(passed) == 0 || (minMbps <= 0 && dataCeiling <= 0) {
		return passed
	}
	mbps := make(map[string]float64, len(results))
	dataLatency := make(map[string]int64, len(results))
	for _, r := range results {
		mbps[r.IP] = r.DownloadMbps
		dataLatency[r.IP] = r.DataLatency
	}
	isSlow := func(ip string) bool {
		if v := mbps[ip]; v > 0 && minMbps > 0 && v < minMbps {
			return true
		}
		return dataCeiling > 0 && dataLatency[ip] > dataCeiling
	}
	keep := make([]string, 0, len(passed))
	slow := make([]string, 0)
	for _, ip := range passed {
		if isSlow(ip) {
			slow = append(slow, ip)
			continue
		}
		keep = append(keep, ip)
	}
	if len(slow) == 0 {
		return passed
	}
	if minKeep < 1 {
		minKeep = 1
	}
	if len(keep) < minKeep {
		need := minKeep - len(keep)
		// 补位顺序：先吞吐不足但延迟正常的，再数据面延迟超线的。
		// 一个 15 Mbps / 1600ms 的入口下载很快，但每次交互都要多等一秒半，
		// 不该抢在 2 Mbps / 200ms 的成员前面占最后一个名额。
		ranked := append([]string(nil), slow...)
		sort.SliceStable(ranked, func(i, j int) bool {
			leftLaggy := dataCeiling > 0 && dataLatency[ranked[i]] > dataCeiling
			rightLaggy := dataCeiling > 0 && dataLatency[ranked[j]] > dataCeiling
			if leftLaggy != rightLaggy {
				return !leftLaggy
			}
			return mbps[ranked[i]] > mbps[ranked[j]]
		})
		if need > len(ranked) {
			need = len(ranked)
		}
		selected := make(map[string]struct{}, need)
		for _, ip := range ranked[:need] {
			selected[ip] = struct{}{}
		}
		out := make([]string, 0, len(keep)+need)
		for _, ip := range passed {
			if _, ok := selected[ip]; ok {
				out = append(out, ip)
				continue
			}
			if isSlow(ip) {
				continue
			}
			out = append(out, ip)
		}
		log.Printf("proxy auto apply: kept %d fastest below-reference IPs to reach min pool %d (dropped %d slower): %v",
			need, minKeep, len(slow)-need, ranked[need:])
		return out
	}
	log.Printf("proxy auto apply: dropped %d below-reference IPs (< %.1f Mbps or data latency > %dms): %v",
		len(slow), minMbps, dataCeiling, slow)
	return keep
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

func proxyResultDownloadMbps(results []proxyScanResult, ip string) float64 {
	for _, result := range results {
		if result.IP == ip {
			return result.DownloadMbps
		}
	}
	return 0
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

// poolColoCap 是单个 CF 机房在池内的软上限。
//
// 同一 colo 的多个 IP 共享同一套边缘容量，放 5 个进去不会让可用带宽
// 变成 5 倍，只会让「这个机房一挂，池子一起挂」的概率变大。guanfang-youxuan
// 用 capPerColo=3，这里沿用；0 表示关闭。
func poolColoCap() int {
	n := envInt("PROXY_POOL_COLO_CAP", 3)
	if n < 0 {
		n = 0
	}
	if n > 10 {
		n = 10
	}
	return n
}

// capPassedPerColo 按机房配额裁剪候选，并在跌破最小池时按原顺序回填。
//
// 回填是刻意的：配额是软约束，池子空掉比同机房多一个严重得多。无 colo
// 情报的候选不参与配额（未知不等于差）。
func capPassedPerColo(ips []string, results []proxyScanResult, limit, minKeep int) []string {
	if limit <= 0 || len(ips) == 0 {
		return ips
	}
	coloOf := make(map[string]string, len(ips))
	for _, result := range results {
		if result.Colo != "" {
			coloOf[result.IP] = result.Colo
		}
	}
	counts := make(map[string]int, 8)
	kept := make([]string, 0, len(ips))
	deferred := make([]string, 0)
	for _, ip := range ips {
		colo := coloOf[ip]
		if colo == "" || counts[colo] < limit {
			kept = append(kept, ip)
			if colo != "" {
				counts[colo]++
			}
			continue
		}
		deferred = append(deferred, ip)
	}
	if len(kept) < minKeep && len(deferred) > 0 {
		need := minKeep - len(kept)
		if need > len(deferred) {
			need = len(deferred)
		}
		kept = append(kept, deferred[:need]...)
		deferred = deferred[need:]
	}
	if len(deferred) > 0 {
		log.Printf("proxy auto apply: colo cap %d deferred %d candidates", limit, len(deferred))
	}
	return kept
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
// 准入（改动 B）：候选必须真的进过 VLESS 终审（Stage=VLESS_FAIL，
// DataLatency>0 说明探针跑完了）且失败归类不是 timeout——超时意味着
// 数据面 15 秒静默，最能说明这条出口此刻不通。
//
// 拒绝的三类：
//   - 纯 WS_PASS（未进 VLESS 终审）：WS 握手对数据面毫无预测力。实测：
//     13 个 VLESS_FAIL 的 WS 握手仅 228–423ms，数据面却卡满 15s 超时，
//     不能凭 WS 通过就进池；
//   - VLESS_FAIL 且 errorClass=timeout：数据面超时；
//   - WS_FAIL 或其他阶段失败：WS 阶段已失败。
//
// 注意：probeVLESSPool 里 DataLatency 是整次尝试的耗时（含 15s 超时），
// 不是「连上过」的证明——不能只凭 DataLatency>0 就放行，必须同时看
// errorClass。交接单原文「有 dataLatency 值（即真的连上过）」按字面
// 理解会把 15s 超时也放进来（实测证据里超时项的 dataLatency 全是
// 约 15005ms），此处按「已尝试 + 非超时失败」执行，并在 PR 说明里
// 标出这一处与原文措辞的出入。
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
		case result.Stage == "VLESS_FAIL" && result.DataLatency > 0 && result.ErrorClass != "" && result.ErrorClass != failureTimeout:
			adopted = append(adopted, result.IP)
		case result.Stage == "VLESS_FAIL" && result.ErrorClass == failureTimeout:
			rejected = append(rejected, result.IP+": VLESS 数据面超时")
		case result.Stage == "VLESS_FAIL":
			rejected = append(rejected, result.IP+": VLESS 失败原因未归类或无数据面耗时")
		case result.Stage == "WS_PASS":
			rejected = append(rejected, result.IP+": 仅 WS 通过，未验 VLESS 数据面")
		default:
			rejected = append(rejected, result.IP+": WS 未通过")
		}
	}
	return adopted, rejected
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
	// 后台漏斗与用户操作共用一把扫描锁；后台拿到的上下文可被用户操作取消。
	// 手动「立即优选」会经此路径：排队等待正在跑的考试，而不是拒绝。
	runCtx := parent
	if !manual {
		bgCtx, ok := a.backgroundScanContext()
		if !ok {
			log.Printf("proxy auto apply skipped: manual operation pending")
			return false
		}
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(bgCtx, autoApplyBudget)
		defer cancel()
	}
	if !lockTimeout(&a.proxyScanMu, 60*time.Second) {
		log.Printf("proxy auto apply skipped: another proxy scan still running after 60s")
		a.setProxyPoolError("已有优选在运行（等待 60 秒未结束），请稍后再试", nil)
		return
	}
	defer a.proxyScanMu.Unlock()
	kind := "background"
	if manual {
		kind = "optimize"
	}
	defer a.noteScanActivity(kind, "漏斗初筛")()
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
		prescreen := scanProxyIPs(runCtx, candidates, prescreenCfg)
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
	if !manual && runCtx.Err() != nil {
		log.Printf("proxy auto apply canceled before WS stage: %v", runCtx.Err())
		return false
	}
	a.setScanActivityPhase(kind, "WS 精筛", 0, len(shortlist))
	results := scanProxyWebSockets(runCtx, shortlist, cfg)
	applyCandidateSourcePriority(results, snapshot.SourceByIP)
	// WS 失败候选补一次 trace 诊断：只补 colo/备注，不改变 Stage 与 Error，
	// 也不参与准入。TLS 都没通的候选与「TLS 通了但 WS 路径不对」的候选
	// 从此能在状态页里分开看。
	attachTraceDiagnostics(runCtx, results, cfg)
	passed := make([]string, 0, cfg.PoolSize)
	finalStage := "WS"
	if cfg.VLESS.Enabled {
		finalStage = "VLESS"
		a.setScanActivityPhase(kind, "VLESS 终审", 0, len(results))
		passed = probeVLESSPool(runCtx, results, cfg.Port, min(cfg.VLESS.MaxCandidates, len(results)), cfg.VLESS, vlessTemplate)
		passed = rankVLESSPassesBySpeedWithHistory(runCtx, passed, results, cfg.Port, cfg.VLESS, vlessTemplate, a.proxyActivePoolSnapshot().Results)
		// 机房软偏好：Mbps 序排定后分区重排，截断时充裕取偏好、匮乏放行。
		passed = orderPassedByColoPreference(passed, results, poolColoPreferenceSet())
		// 片区降权：整片高失败率的段排到末尾（不淘汰，候选不够照样用）。
		passed = orderPassedBySegmentHealth(passed, results)
		passed = capPassedPerColo(passed, results, poolColoCap(), cfg.MinPool)
		passed = spreadBySegment(passed, cfg.PoolSize)
		// L7 复验：trace 明确不合格的候选不占座；探测失败一律放行。
		passed = filterTraceL7(runCtx, passed, results, cfg)
		// 劣质 IP 不占座：实测吞吐低于参考值或数据面延迟超线的成员从入池
		// 名单剔除。未测成员不受影响（未知不等于差）；剔除会跌破最小池时
		// 按「延迟正常优先、吞吐次之」补足。
		passed = dropBelowReferenceIPs(passed, results, cfg.MinPool, vlessMinDownloadMbps())
		// 排序阶段只测前 N 个；片区轮转/延迟闸/补位可能把未测成员带进
		// 最终名单。给这些成员补一次吞吐证据，再按同一口径剔除一次，
		// 避免「没有成绩」被当成「没有问题」。
		for round := 0; round < 2; round++ {
			if !fillMissingSpeedMetrics(runCtx, passed, results, cfg.Port, cfg.VLESS, vlessTemplate, probeVLESSCandidateMetrics) {
				break
			}
			passed = dropBelowReferenceIPs(passed, results, cfg.MinPool, vlessMinDownloadMbps())
		}
	} else {
		for _, result := range results {
			if result.Error == "" {
				passed = append(passed, result.IP)
			}
		}
		passed = orderPassedByColoPreference(passed, results, poolColoPreferenceSet())
		passed = orderPassedBySegmentHealth(passed, results)
		passed = capPassedPerColo(passed, results, poolColoCap(), cfg.MinPool)
		passed = spreadBySegment(passed, cfg.PoolSize)
		passed = filterTraceL7(runCtx, passed, results, cfg)
	}
	// 台账记终审结论：此前只有后台质量调度器和订阅库存探测记账，
	// 全量漏斗考过的 IP 在台账里长期显示 0/0，自动淘汰规则因此失真。
	a.observeFunnelResultsInLedger(results, cfg.VLESS.Enabled)
	// 排序层已经按慢线分过一次，但 orderPassedByColoPreference /
	// orderPassedBySegmentHealth / spreadBySegment 都会重排：段轮转是「每段各取
	// 一个」，一个独占某段的慢线成员会在第一轮就被取走，插到延迟更好但段更挤的
	// 成员前面。所以入池前必须再分一次——这里是最终顺序，之后只有硬闸。
	if ceiling := vlessLatencyCeilingMs(); ceiling > 0 {
		passed = partitionLaggyLast(passed, results, ceiling)
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
		// 旧池还活着时，降级采用未验数据面的候选是净损失：把已经过数据面
		// 验证的出口换成只过了 TCP/TLS/WS 的未知成员。2026-11-16 事故就是
		// 这条路把 6 个可用出口换成了 3 个 dial timeout 目标，cfnat 每个目标
		// 等 2 秒，客户端 6 秒后连接被关。降级只留给「旧池已死」这一种情况。
		if alive, total := a.verifyActivePoolAlive(); alive > 0 {
			a.setProxyPoolError(fmt.Sprintf("%s；旧池 %d/%d 存活，跳过降级并继续沿用", message, alive, total), results)
			return
		}
		// L3 降级：VLESS 数据面没验过，但 WS 101 已经通过 —— 说明 TCP/TLS/WS
		// 三层都通，失败点大概率在探测目标而非链路本身。池子空掉比用
		// 「未完成终审」的候选严重得多，故降级采用并明确标记。
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
			// 降级候选同样过 L7 复验：否则被硬条件拒掉的候选会从后门回到池里。
			if len(extra) > 0 {
				extra = filterTraceL7(runCtx, extra, results, cfg)
			}
			if len(extra) > 0 {
				passed = append(passed, extra...)
				message = fmt.Sprintf("%s；已降级采用 %d 个 VLESS 非超时失败的候选", message, len(extra))
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
	if !manual && runCtx.Err() != nil {
		// 用户操作已抢占：绝不能用开考前的快照把用户刚做的改动覆盖回去。
		log.Printf("proxy auto apply canceled before commit: %v", runCtx.Err())
		return false
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
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	attempts := wsProbeAttempts()
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					results[i] = proxyScanResult{
						IP:         ip,
						Stage:      "WS_FAIL",
						Error:      fmt.Sprintf("probe panic: %v", r),
						ErrorClass: "panic",
					}
				}
			}()
			latencies := make([]int64, 0, attempts)
			colo := ""
			var lastErr error
			for attempt := 0; attempt < attempts; attempt++ {
				if parent.Err() != nil {
					break
				}
				started := time.Now()
				ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.MaxLatency)*time.Millisecond)
				c, err := probeProxyWebSocket(ctx, ip, cfg)
				cancel()
				if err != nil {
					lastErr = err
					continue
				}
				latencies = append(latencies, time.Since(started).Milliseconds())
				if c != "" && colo == "" {
					colo = c
				}
			}
			if len(latencies) == 0 {
				results[i] = proxyScanResult{IP: ip, Stage: "WS_FAIL"}
				if lastErr != nil {
					results[i].Error = lastErr.Error()
					results[i].ErrorClass = classifyProbeError(lastErr)
				} else {
					results[i].Error = "WS 探测未执行"
					results[i].ErrorClass = "canceled"
				}
				return
			}
			avg, jitter := latencyAverageAndJitter(latencies)
			results[i] = proxyScanResult{
				IP:       ip,
				Latency:  avg,
				Colo:     colo,
				Stage:    "WS_PASS",
				LossRate: float64(attempts-len(latencies)) / float64(attempts),
				JitterMs: jitter,
			}
		}(i, ip)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if (results[i].Error == "") != (results[j].Error == "") {
			return results[i].Error == ""
		}
		// 先丢包，再按 20ms 分档，再看抖动，最后才比精确延迟。
		// 单次握手的 5ms 差值没有统计意义；3 次里丢 1 次才是真问题。
		if results[i].LossRate != results[j].LossRate {
			return results[i].LossRate < results[j].LossRate
		}
		leftBucket := results[i].Latency / 20
		rightBucket := results[j].Latency / 20
		if leftBucket != rightBucket {
			return leftBucket < rightBucket
		}
		if results[i].JitterMs != results[j].JitterMs {
			return results[i].JitterMs < results[j].JitterMs
		}
		return results[i].Latency < results[j].Latency
	})
	return results
}

// wsProbeAttempts 是 shortlist 上每个候选的 WS 握手次数。
//
// 只作用于 shortlist（默认 60 个），不碰 990 个初筛候选，所以不扩大
// 全量探测压力。3 次才能区分「偶发丢手」和「稳定可用」：1 次成功可能是
// 运气，3 次里丢 1 次在粘性转发下就是每三次断一次。
func wsProbeAttempts() int {
	n := envInt("PROXY_WS_PROBES", 3)
	if n < 1 {
		n = 1
	}
	if n > 3 {
		n = 3
	}
	return n
}

func latencyAverageAndJitter(samples []int64) (int64, int64) {
	if len(samples) == 0 {
		return 0, 0
	}
	var sum int64
	for _, sample := range samples {
		sum += sample
	}
	avg := sum / int64(len(samples))
	if avg < 1 {
		avg = 1
	}
	var deviation int64
	for _, sample := range samples {
		if sample > avg {
			deviation += sample - avg
		} else {
			deviation += avg - sample
		}
	}
	return avg, deviation / int64(len(samples))
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
	IP           string  `json:"ip"`
	Latency      int64   `json:"latency"`
	Colo         string  `json:"colo,omitempty"`
	DataLatency  int64   `json:"dataLatency,omitempty"`
	DownloadMbps float64 `json:"downloadMbps,omitempty"`
	SourceRank   int     `json:"sourceRank,omitempty"`
	// LossRate 是 shortlist 多次 WS 握手的失败比例（0~1），JitterMs 是
	// 成功样本的平均绝对偏差。单次握手看不出「偶发丢手」，而粘性转发下
	// 丢一次就是断一次，所以排序先看这两个再比延迟。
	LossRate float64 `json:"lossRate,omitempty"`
	JitterMs int64   `json:"jitterMs,omitempty"`
	Stage    string  `json:"stage,omitempty"`
	Error    string  `json:"error,omitempty"`
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
			sem <- struct{}{}
			defer func() { <-sem }()
			// 单个探测 panic 不能带走整轮扫描：CFData-WEB 的 bounded
			// worker 在每个 worker 里 recover，这里照做，并把 panic 记成
			// 该候选的失败原因，其余候选继续跑。
			defer func() {
				if r := recover(); r != nil {
					results[i] = proxyScanResult{
						IP:         ip,
						Stage:      "TLS_FAIL",
						Error:      fmt.Sprintf("probe panic: %v", r),
						ErrorClass: "panic",
					}
				}
			}()
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
