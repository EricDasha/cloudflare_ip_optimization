package main

import (
	"bufio"
	"encoding/csv"
	"errors"
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

// cfdataCandidateIPs 返回 result.csv 中排序最靠前、且落在 Cloudflare 段内的前 limit 个 IP。
//
// 这是整个候选体系里唯一「以本机网络为基准」的供给层：延迟与丢包是 cfdata
// 在这台 NAS 上实测的，而不是别人在别的运营商线路上扫出来的。因而它排在
// subscription 与 bestcf 之前。
func (a *app) cfdataCandidateIPs(limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := a.readCFdataResultRows()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, row := range rows {
		if len(out) >= limit {
			break
		}
		ip := net.ParseIP(row.IP)
		if !isCloudflareIPv4(ip) {
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
		return nil, errors.New("result.csv 中无可用 Cloudflare IP")
	}
	return out, nil
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
