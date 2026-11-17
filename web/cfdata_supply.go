package main

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// cfdataResultFile 是 cfdata 扫描的输出文件名。
//
// 必须与 ip.csv 区分开：ip.csv 是「待扫描清单」（IP地址,数据中心,地区,城市,网络延迟），
// result.csv 才是「本轮实测成绩」（IP地址,最小延迟,最大延迟,平均延迟,丢包率）。
// 历史上 readScanRows() 读的是 ip.csv，等于拿上一轮的输入冒充这一轮的结果——
// 供给层因此长期在用陈旧数据。候选供给只认 result.csv。
const cfdataResultFile = "result.csv"

type cfdataResultRow struct {
	IP         string
	MinLatency int
	MaxLatency int
	AvgLatency int
	PacketLoss float64
}

// jitter 返回延迟抖动幅度（最大减最小）。同一平均延迟下，抖动小的更适合做
// 稳定入口——短时下载最怕的就是这种忽快忽慢。
func (r cfdataResultRow) jitter() int {
	if r.MaxLatency > r.MinLatency {
		return r.MaxLatency - r.MinLatency
	}
	return 0
}

// readCFdataResultRows 解析 cfdata 的 result.csv。
// 表头：IP地址,最小延迟(ms),最大延迟(ms),平均延迟(ms),丢包率(%)
func (a *app) readCFdataResultRows() ([]cfdataResultRow, error) {
	path := filepath.Join(a.dataDir, cfdataResultFile)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	rows := make([]cfdataResultRow, 0, len(records))
	for i, record := range records {
		if i == 0 || len(record) < 5 {
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(record[0]))
		if ip == nil || ip.To4() == nil {
			continue
		}
		rows = append(rows, cfdataResultRow{
			IP:         ip.To4().String(),
			MinLatency: parseIntPrefix(record[1]),
			MaxLatency: parseIntPrefix(record[2]),
			AvgLatency: parseIntPrefix(record[3]),
			PacketLoss: parseFloatPrefix(record[4]),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		// 零丢包优先：丢包是入口 IP 的一票否决项，短时下载对丢包极其敏感。
		if (left.PacketLoss == 0) != (right.PacketLoss == 0) {
			return left.PacketLoss == 0
		}
		if left.PacketLoss != right.PacketLoss {
			return left.PacketLoss < right.PacketLoss
		}
		if left.AvgLatency != right.AvgLatency {
			return left.AvgLatency < right.AvgLatency
		}
		return left.jitter() < right.jitter()
	})
	return rows, nil
}

// cfdataCandidateIPs 返回 result.csv 中排序最靠前的前 limit 个公网 IPv4。
//
// 注意：只要求公网 IPv4，不再过官方段闸（2026-11-05 实测：非官方段
// 反代同样可用，段只作为标签统计，不作准入条件）。
//
// 这是整个候选体系里唯一「以本机网络为基准」的供给层：延迟与丢包是 cfdata
// 在这台 NAS 上实测的，而不是别人在别的运营商线路上扫出来的。因而它排在
// subscription 与 bestcf 之前。
//
// 但 cfdata 只会做 CloudflareST 三步里的第一步：它测的是 TCP 握手到最近
// Cloudflare 边缘的往返时间，然后按延迟排序。2026-11-13 实测证明这一步和
// 隧道吞吐几乎不相关——它挑出的 250 个候选里，有成绩的那批是 1~3 Mbps、
// 握手 900~1500ms，而在位成员是 9~10 Mbps。所以这里补上第三步：凡是台账里
// 已经有真实吞吐成绩的，一律按成绩重排；没成绩的才退回延迟。见 sortCFdataRows。
func (a *app) cfdataCandidateIPs(limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := a.readCFdataResultRows()
	if err != nil {
		return nil, err
	}
	coloByIP := a.cfdataColoByIP()
	a.sortCFdataRows(rows, coloByIP)
	out := make([]string, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, row := range rows {
		if len(out) >= limit {
			break
		}
		ip := net.ParseIP(row.IP)
		// 只要求公网 IPv4：cfdata 是本机亲手测的延迟与丢包，比任何地址表都准，
		// 不需要再用官方段二次过滤（2026-11-05 实测：非官方段反代同样可用）。
		if !isPublicIPv4(ip) {
			continue
		}
		key := ip.To4().String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	if len(out) == 0 {
		return nil, errors.New("result.csv 中无可用公网 IPv4")
	}
	logCFdataColoSpread(out, coloByIP)
	return out, nil
}

// logCFdataColoSpread 打出这一批入选 IP 的机房分布。
//
// 2026-11-13 实测：cfdata 按延迟挑出的前 250 个里 FRA 117、LAX 85、SIN 42、
// NRT 4、SJC 2——全是最远的几个机房。这条日志是判断「延迟排序是不是又把我们
// 送到远端拥塞线路」的唯一依据，所以和分档统计一起留在每轮刷新里。
func logCFdataColoSpread(ips []string, coloByIP map[string]string) {
	if len(coloByIP) == 0 {
		return
	}
	counts := make(map[string]int, 8)
	for _, ip := range ips {
		colo := coloByIP[ip]
		if colo == "" {
			colo = "?"
		}
		counts[colo]++
	}
	type pair struct {
		colo string
		n    int
	}
	list := make([]pair, 0, len(counts))
	for colo, n := range counts {
		list = append(list, pair{colo, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].colo < list[j].colo
	})
	parts := make([]string, 0, len(list))
	for _, p := range list {
		parts = append(parts, fmt.Sprintf("%s=%d", p.colo, p.n))
	}
	log.Printf("cfdata colo spread: %s", strings.Join(parts, " "))
}

// cfdataSpeedTier 把一条扫描结果分档。数字小的先出线。
//
//	0 实测及格   —— 真跑过数据面，速度达标
//	1 没测过     —— cfdata 只给了延迟，吞吐未知
//	2 实测不及格 —— 真跑过，但慢
//	3 丢包过高   —— CloudflareST 的 -tlr，入口 IP 的一票否决项
//
// 为什么「没测过」要排在「实测不及格」前面：不知道不等于坏。一个从没被
// 考过的地址还有机会，一个已经被证明只有 1 Mbps 的地址没有。反过来排会让
// 已知的坏地址永远占着名额，而它们恰恰是最该被挤掉的。
func cfdataSpeedTier(mbps, minMbps, loss, maxLoss float64) int {
	if maxLoss > 0 && loss > maxLoss {
		return 3
	}
	switch {
	case mbps <= 0:
		return 1
	case mbps >= minMbps:
		return 0
	default:
		return 2
	}
}

// sortCFdataRows 在 readCFdataResultRows 的延迟基线之上，按真实吞吐重排。
//
// 基线（零丢包 → 低延迟 → 低抖动）仍然保留：它只在同一档内做次序，不再
// 决定谁排在最前。这样做的理由是实测数据：cfdata 按延迟挑出来的前 250 个，
// 吞吐中位不到 2 Mbps，而在位成员是 9~10 Mbps，两者毫无相关性。
func (a *app) sortCFdataRows(rows []cfdataResultRow, coloByIP map[string]string) {
	mbps := a.ledgerThroughputSnapshot()
	prior := cfdataColoThroughput(coloByIP, mbps)
	minMbps := vlessMinDownloadMbps()
	maxLoss := float64(envInt("CFDATA_MAX_LOSS_PCT", 20))
	tiers := [4]int{}
	for _, row := range rows {
		tiers[cfdataSpeedTier(mbps[row.IP], minMbps, row.PacketLoss, maxLoss)]++
	}
	// 每轮打一行分档：这是「cfdata 到底有没有变好」的唯一观测口。
	// 只看延迟的时候，这一行永远显示 unknown 占满——那正是问题本身。
	log.Printf("cfdata ranking: %d rows, measured-good=%d unknown=%d measured-slow=%d lossy=%d, colo priors=%d",
		len(rows), tiers[0], tiers[1], tiers[2], tiers[3], len(prior))
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		lm, rm := mbps[left.IP], mbps[right.IP]
		lt := cfdataSpeedTier(lm, minMbps, left.PacketLoss, maxLoss)
		rt := cfdataSpeedTier(rm, minMbps, right.PacketLoss, maxLoss)
		if lt != rt {
			return lt < rt
		}
		// 有成绩的两档按成绩降序；没成绩的没有成绩可比。
		if lm != rm {
			return lm > rm
		}
		// 同档内先看丢包：丢包是入口 IP 的一票否决项，短时下载对它极其敏感。
		if (left.PacketLoss == 0) != (right.PacketLoss == 0) {
			return left.PacketLoss == 0
		}
		if left.PacketLoss != right.PacketLoss {
			return left.PacketLoss < right.PacketLoss
		}
		// 再看得票：这个机房历史上出过多少条能跑满的线路。样本不足三个
		// 机房不给先验——一两条好线是个别 IP 的运气，不是机房的性质。
		// 「没有先验」不等于「先验为零」：拿 0 去比会把没测过的机房判成最差，
		// 于是已知很差的机房反而排在未知机房前面。只在双方都有先验时比大小，
		// 只有一方有时按「好机房优先、差机房靠后」处理。
		lp, lok := prior[coloByIP[left.IP]]
		rp, rok := prior[coloByIP[right.IP]]
		if lok != rok {
			if lok {
				return lp >= minMbps
			}
			return rp < minMbps
		}
		if lok && lp != rp {
			return lp > rp
		}
		if left.AvgLatency != right.AvgLatency {
			return left.AvgLatency < right.AvgLatency
		}
		return left.jitter() < right.jitter()
	})
}

// cfdataColoByIP 从 ip.csv 取出每个 IP 落在哪个数据中心。
//
// result.csv 只有成绩没有机房，ip.csv 只有机房没有成绩，两张表靠 IP 关联。
// 读不到就返回空表——机房先验只是锦上添花，不该让它拖垮整条供给链。
func (a *app) cfdataColoByIP() map[string]string {
	path := filepath.Join(a.dataDir, "ip.csv")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(records))
	for i, record := range records {
		if i == 0 || len(record) < 2 {
			continue
		}
		ip := strings.TrimSpace(record[0])
		colo := strings.ToUpper(strings.TrimSpace(record[1]))
		if ip == "" || colo == "" {
			continue
		}
		out[ip] = colo
	}
	return out
}

// cfdataColoThroughput 算出每个机房的历史吞吐中位数。
//
// 只要样本够三条才算数：一个机房的好坏是统计性质，一两条好线可能只是
// 那台机器离得近。
func cfdataColoThroughput(coloByIP map[string]string, mbps map[string]float64) map[string]float64 {
	if len(coloByIP) == 0 || len(mbps) == 0 {
		return nil
	}
	samples := make(map[string][]float64, 16)
	for ip, value := range mbps {
		if value <= 0 {
			continue
		}
		colo, ok := coloByIP[ip]
		if !ok {
			continue
		}
		samples[colo] = append(samples[colo], value)
	}
	out := make(map[string]float64, len(samples))
	for colo, values := range samples {
		if len(values) < 3 {
			continue
		}
		sort.Float64s(values)
		out[colo] = values[len(values)/2]
	}
	return out
}

// parseFloatPrefix 取字符串开头的十进制数（容忍尾随单位/百分号）。
func parseFloatPrefix(value string) float64 {
	s := strings.TrimSpace(value)
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0
	}
	return v
}
