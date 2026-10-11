package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type scanRow struct {
	IP             string `json:"ip"`
	DataCenter     string `json:"dataCenter"`
	DataCenterZh   string `json:"dataCenterZh"`
	DataCenterName string `json:"dataCenterName"`
	Region         string `json:"region"`
	RegionZh       string `json:"regionZh"`
	RegionEn       string `json:"regionEn"`
	City           string `json:"city"`
	CityZh         string `json:"cityZh"`
	CityEn         string `json:"cityEn"`
	Latency        string `json:"latency"`
	LatencyMS      int    `json:"latencyMs"`
	Source         string `json:"source"`
}

type dataCenterSummary struct {
	DataCenter     string `json:"dataCenter"`
	DataCenterZh   string `json:"dataCenterZh"`
	DataCenterName string `json:"dataCenterName"`
	Region         string `json:"region"`
	RegionZh       string `json:"regionZh"`
	RegionEn       string `json:"regionEn"`
	City           string `json:"city"`
	CityZh         string `json:"cityZh"`
	CityEn         string `json:"cityEn"`
	IPCount        int    `json:"ipCount"`
	MinLatencyMS   int    `json:"minLatencyMs"`
}

type detailRow struct {
	IP           string `json:"ip"`
	MinLatencyMS int    `json:"minLatencyMs"`
	MaxLatencyMS int    `json:"maxLatencyMs"`
	AvgLatencyMS int    `json:"avgLatencyMs"`
	LossRate     int    `json:"lossRate"`
}

type detailFile struct {
	Name    string `json:"name"`
	Rows    int    `json:"rows"`
	ModTime string `json:"modTime"`
}

func (a *app) handleCFdataResults(w http.ResponseWriter, r *http.Request) {
	scanRows, dcRows, err := a.readScanRows()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	liveRows := a.liveScanRows()
	detailFiles := a.detailFiles()
	writeJSON(w, map[string]any{
		"ipListSource": ipListSource(),
		"liveRows":     liveRows,
		"scanRows":     scanRows,
		"dataCenters":  dcRows,
		"detailFiles":  detailFiles,
	})
}

func (a *app) handleCFdataDetail(w http.ResponseWriter, r *http.Request) {
	file := strings.TrimSpace(r.URL.Query().Get("file"))
	if file != "" {
		file = filepath.Base(file)
	}
	if file == "" {
		files := a.detailFiles()
		if len(files) == 0 {
			writeJSON(w, map[string]any{"file": "", "rows": []detailRow{}})
			return
		}
		file = files[0].Name
	}
	if file == "ip.csv" || strings.ToLower(filepath.Ext(file)) != ".csv" || file != filepath.Base(file) {
		http.Error(w, "detail csv denied", http.StatusBadRequest)
		return
	}
	rows, err := readDetailCSV(filepath.Join(a.dataDir, file))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"file": file, "rows": rows})
}

func (a *app) handleCFdataSpeed(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	parsed := net.ParseIP(ip)
	if parsed == nil {
		http.Error(w, "invalid ip", http.StatusBadRequest)
		return
	}
	if !a.knownResultIP(ip) {
		http.Error(w, "ip not in cfdata results", http.StatusBadRequest)
		return
	}
	size := queryInt(r, "bytes", 2_000_000)
	if size > 20_000_000 {
		size = 20_000_000
	}
	result, err := downloadSpeedViaCloudflareIP(r.Context(), ip, size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, result)
}

func (a *app) knownResultIP(ip string) bool {
	candidates := []string{filepath.Join(a.dataDir, "ip.csv")}
	for _, file := range a.detailFiles() {
		candidates = append(candidates, filepath.Join(a.dataDir, file.Name))
	}
	for _, path := range candidates {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		reader := csv.NewReader(bufio.NewReader(f))
		reader.FieldsPerRecord = -1
		records, err := reader.ReadAll()
		_ = f.Close()
		if err != nil {
			continue
		}
		for i, record := range records {
			if i == 0 || len(record) == 0 {
				continue
			}
			if strings.TrimSpace(record[0]) == ip {
				return true
			}
		}
	}
	return false
}

func downloadSpeedViaCloudflareIP(ctx context.Context, ip string, size int) (map[string]any, error) {
	host := defaultVLESSSpeedHost
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				port = "443"
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       15 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second}
	url := fmt.Sprintf("https://%s/__down?bytes=%d", host, size)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.Header.Set("User-Agent", "cloudflare-tools-web/1.0")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("speed endpoint status %d", resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, int64(size)))
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		elapsed = time.Millisecond
	}
	mbps := float64(n*8) / elapsed.Seconds() / 1_000_000
	return map[string]any{
		"ip":         ip,
		"bytes":      n,
		"durationMs": elapsed.Milliseconds(),
		"mbps":       mbps,
	}, nil
}

func (a *app) readScanRows() ([]scanRow, []dataCenterSummary, error) {
	path := filepath.Join(a.dataDir, "ip.csv")
	f, err := os.Open(path)
	if err != nil {
		return []scanRow{}, []dataCenterSummary{}, err
	}
	defer f.Close()

	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, nil, err
	}

	rows := make([]scanRow, 0, max(0, len(records)-1))
	summary := map[string]*dataCenterSummary{}
	for i, record := range records {
		if i == 0 || len(record) < 5 {
			continue
		}
		code := strings.TrimSpace(record[1])
		region := strings.TrimSpace(record[2])
		city := strings.TrimSpace(record[3])
		lat := strings.TrimSpace(record[4])
		latMS := parseIntPrefix(lat)
		names := decorateLocation(code, region, city)
		row := scanRow{
			IP:             strings.TrimSpace(record[0]),
			DataCenter:     code,
			DataCenterZh:   names.DataCenterZh,
			DataCenterName: names.DataCenterName,
			Region:         region,
			RegionZh:       names.RegionZh,
			RegionEn:       names.RegionEn,
			City:           city,
			CityZh:         names.CityZh,
			CityEn:         names.CityEn,
			Latency:        lat,
			LatencyMS:      latMS,
			Source:         "ip.csv",
		}
		rows = append(rows, row)
		if code == "" {
			continue
		}
		cur, ok := summary[code]
		if !ok {
			summary[code] = &dataCenterSummary{
				DataCenter:     code,
				DataCenterZh:   row.DataCenterZh,
				DataCenterName: row.DataCenterName,
				Region:         region,
				RegionZh:       row.RegionZh,
				RegionEn:       row.RegionEn,
				City:           city,
				CityZh:         row.CityZh,
				CityEn:         row.CityEn,
				IPCount:        1,
				MinLatencyMS:   latMS,
			}
			continue
		}
		cur.IPCount++
		if cur.MinLatencyMS == 0 || (latMS > 0 && latMS < cur.MinLatencyMS) {
			cur.MinLatencyMS = latMS
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LatencyMS < rows[j].LatencyMS })
	dcs := make([]dataCenterSummary, 0, len(summary))
	for _, v := range summary {
		dcs = append(dcs, *v)
	}
	sort.Slice(dcs, func(i, j int) bool {
		if dcs[i].MinLatencyMS == dcs[j].MinLatencyMS {
			return dcs[i].DataCenter < dcs[j].DataCenter
		}
		return dcs[i].MinLatencyMS < dcs[j].MinLatencyMS
	})
	return rows, dcs, nil
}

type decoratedLocation struct {
	DataCenterZh   string
	DataCenterName string
	RegionZh       string
	RegionEn       string
	CityZh         string
	CityEn         string
}

func decorateLocation(code, region, city string) decoratedLocation {
	code = strings.ToUpper(strings.TrimSpace(code))
	region = strings.TrimSpace(region)
	city = strings.TrimSpace(city)
	cityZh := cityZh(code, city)
	cityEn := cityEn(code, city)
	regionZh, regionEn := regionNames(region)
	name := code
	if cityZh != "" && cityEn != "" && cityZh != cityEn {
		name = code + " · " + cityZh + " / " + cityEn
	} else if cityZh != "" {
		name = code + " · " + cityZh
	}
	return decoratedLocation{
		DataCenterZh:   codeDisplay(code, city),
		DataCenterName: name,
		RegionZh:       regionZh,
		RegionEn:       regionEn,
		CityZh:         cityZh,
		CityEn:         cityEn,
	}
}

func (a *app) liveScanRows() []scanRow {
	lines := a.cfdata.logs.lines(2000)
	rows := make([]scanRow, 0, min(len(lines), 500))
	for _, line := range lines {
		row, ok := parseLiveScanLine(line)
		if ok {
			rows = append(rows, row)
		}
	}
	if len(rows) > 500 {
		rows = rows[len(rows)-500:]
	}
	return rows
}

func parseLiveScanLine(line string) (scanRow, bool) {
	if !strings.Contains(line, "有效IP:") || !strings.Contains(line, "延迟:") {
		return scanRow{}, false
	}
	parts := strings.Split(line, ",")
	if len(parts) < 2 {
		return scanRow{}, false
	}
	ipPart := strings.TrimSpace(parts[0])
	idx := strings.LastIndex(ipPart, "有效IP:")
	if idx < 0 {
		return scanRow{}, false
	}
	row := scanRow{IP: strings.TrimSpace(strings.TrimPrefix(ipPart[idx:], "有效IP:")), Source: "live"}
	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "数据中心:"):
			row.DataCenter = strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(part, "数据中心:")))
		case strings.HasPrefix(part, "地区:"):
			row.Region = strings.TrimSpace(strings.TrimPrefix(part, "地区:"))
		case strings.HasPrefix(part, "城市:"):
			row.City = strings.TrimSpace(strings.TrimPrefix(part, "城市:"))
		case strings.HasPrefix(part, "延迟:"):
			row.Latency = strings.TrimSpace(strings.TrimPrefix(part, "延迟:"))
			row.LatencyMS = parseIntPrefix(row.Latency)
		}
	}
	if row.DataCenter == "" || row.IP == "" {
		return scanRow{}, false
	}
	names := decorateLocation(row.DataCenter, row.Region, row.City)
	row.DataCenterZh = names.DataCenterZh
	row.DataCenterName = names.DataCenterName
	row.RegionZh = names.RegionZh
	row.RegionEn = names.RegionEn
	row.CityZh = names.CityZh
	row.CityEn = names.CityEn
	return row, true
}

func (a *app) detailFiles() []detailFile {
	entries, err := os.ReadDir(a.dataDir)
	if err != nil {
		return []detailFile{}
	}
	files := []detailFile{}
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".csv" || e.Name() == "ip.csv" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows, _ := countCSVRows(filepath.Join(a.dataDir, e.Name()))
		files = append(files, detailFile{Name: e.Name(), Rows: rows, ModTime: info.ModTime().Format(time.RFC3339)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime > files[j].ModTime })
	return files
}

func readDetailCSV(path string) ([]detailRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return []detailRow{}, err
	}
	defer f.Close()
	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	rows := make([]detailRow, 0, max(0, len(records)-1))
	for i, record := range records {
		if i == 0 || len(record) < 5 {
			continue
		}
		rows = append(rows, detailRow{
			IP:           strings.TrimSpace(record[0]),
			MinLatencyMS: atoi(strings.TrimSpace(record[1])),
			MaxLatencyMS: atoi(strings.TrimSpace(record[2])),
			AvgLatencyMS: atoi(strings.TrimSpace(record[3])),
			LossRate:     atoi(strings.TrimSpace(record[4])),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].LossRate == rows[j].LossRate {
			return rows[i].AvgLatencyMS < rows[j].AvgLatencyMS
		}
		return rows[i].LossRate < rows[j].LossRate
	})
	return rows, nil
}

func countCSVRows(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}
	return len(records) - 1, nil
}

func ipListSource() map[string]any {
	return map[string]any{
		"cacheFiles": []string{"ips-v4.txt", "ips-v6.txt", "locations.json"},
		"remote": map[string]string{
			"ipv4":      "https://www.baipiao.eu.org/cloudflare/ips-v4",
			"ipv6":      "https://www.baipiao.eu.org/cloudflare/ips-v6",
			"locations": "https://www.baipiao.eu.org/cloudflare/locations",
		},
		"behavior": "cfdata/cfnat 启动后优先读取工作目录本地缓存；缺少 ips-v4.txt/ips-v6.txt 或 locations.json 时才从远端下载并保存。",
	}
}

func parseIntPrefix(s string) int {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return 0
	}
	return atoi(fields[0])
}

func atoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func regionZh(region string) string {
	zh, _ := regionNames(region)
	return zh
}

func regionNames(region string) (string, string) {
	switch strings.TrimSpace(region) {
	case "Asia Pacific":
		return "亚太地区", "Asia Pacific"
	case "North America":
		return "北美洲", "North America"
	case "South America":
		return "南美洲", "South America"
	case "Europe":
		return "欧洲", "Europe"
	case "Middle East":
		return "中东", "Middle East"
	case "Africa":
		return "非洲", "Africa"
	case "Oceania":
		return "大洋洲", "Oceania"
	case "亚太", "亚太地区":
		return "亚太地区", "Asia Pacific"
	case "北美", "北美洲":
		return "北美洲", "North America"
	case "南美", "南美洲":
		return "南美洲", "South America"
	case "欧洲":
		return "欧洲", "Europe"
	case "中东":
		return "中东", "Middle East"
	case "非洲":
		return "非洲", "Africa"
	case "大洋洲":
		return "大洋洲", "Oceania"
	default:
		return region, region
	}
}

func codeDisplay(code, city string) string {
	zh := cityZh(code, city)
	if zh == "" {
		return code
	}
	return code + " · " + zh
}

func cityZh(code, city string) string {
	if v := cityZhByCode[strings.ToUpper(strings.TrimSpace(code))]; v != "" {
		return v
	}
	if v := cityZhByName[strings.ToLower(strings.TrimSpace(city))]; v != "" {
		return v
	}
	return city
}

func cityEn(code, city string) string {
	if v := cityEnByCode[strings.ToUpper(strings.TrimSpace(code))]; v != "" {
		return v
	}
	city = strings.TrimSpace(city)
	if v := cityEnByZh[city]; v != "" {
		return v
	}
	return city
}

var cityZhByCode = map[string]string{
	"HKG": "香港", "TPE": "台北", "KHH": "高雄", "NRT": "东京", "KIX": "大阪", "FUK": "福冈", "OKA": "那霸",
	"ICN": "首尔", "SIN": "新加坡", "BKK": "曼谷", "CNX": "清迈", "SGN": "胡志明市", "HAN": "河内", "DAD": "岘港",
	"KUL": "吉隆坡", "JHB": "新山", "KCH": "古晋", "MNL": "马尼拉", "CEB": "宿务", "CRK": "克拉克", "CGK": "雅加达",
	"DPS": "登巴萨", "DEL": "新德里", "BOM": "孟买", "BLR": "班加罗尔", "MAA": "金奈", "HYD": "海得拉巴",
	"LAX": "洛杉矶", "SJC": "圣何塞", "SFO": "旧金山", "SEA": "西雅图", "ORD": "芝加哥", "DFW": "达拉斯",
	"IAD": "阿什本", "EWR": "纽瓦克", "JFK": "纽约", "BOS": "波士顿", "MIA": "迈阿密", "DEN": "丹佛",
	"ATL": "亚特兰大", "PHX": "凤凰城", "LAS": "拉斯维加斯", "PDX": "波特兰", "SLC": "盐湖城", "HNL": "檀香山",
	"LHR": "伦敦", "MAN": "曼彻斯特", "CDG": "巴黎", "AMS": "阿姆斯特丹", "FRA": "法兰克福", "MUC": "慕尼黑",
	"DUS": "杜塞尔多夫", "HAM": "汉堡", "TXL": "柏林", "MAD": "马德里", "BCN": "巴塞罗那", "LIS": "里斯本",
	"FCO": "罗马", "MXP": "米兰", "ZRH": "苏黎世", "GVA": "日内瓦", "VIE": "维也纳", "PRG": "布拉格",
	"WAW": "华沙", "CPH": "哥本哈根", "ARN": "斯德哥尔摩", "HEL": "赫尔辛基", "OSL": "奥斯陆", "DUB": "都柏林",
	"SYD": "悉尼", "MEL": "墨尔本", "BNE": "布里斯班", "PER": "珀斯", "AKL": "奥克兰", "CHC": "基督城",
	"DXB": "迪拜", "DOH": "多哈", "TLV": "特拉维夫", "AMM": "安曼", "JED": "吉达", "RUH": "利雅得",
	"JNB": "约翰内斯堡", "CPT": "开普敦", "CAI": "开罗", "NBO": "内罗毕", "LOS": "拉各斯", "GRU": "圣保罗",
	"GIG": "里约热内卢", "SCL": "圣地亚哥", "BOG": "波哥大", "LIM": "利马", "MEX": "墨西哥城", "YYZ": "多伦多",
	"YVR": "温哥华", "YUL": "蒙特利尔",
}

var cityEnByCode = map[string]string{
	"HKG": "Hong Kong", "TPE": "Taipei", "KHH": "Kaohsiung City", "NRT": "Tokyo", "KIX": "Osaka", "FUK": "Fukuoka", "OKA": "Naha",
	"ICN": "Seoul", "SIN": "Singapore", "BKK": "Bangkok", "CNX": "Chiang Mai", "SGN": "Ho Chi Minh City", "HAN": "Hanoi", "DAD": "Da Nang",
	"KUL": "Kuala Lumpur", "JHB": "Johor Bahru", "KCH": "Kuching", "MNL": "Manila", "CEB": "Cebu", "CRK": "Tarlac City", "CGK": "Jakarta",
	"DPS": "Denpasar", "DEL": "New Delhi", "BOM": "Mumbai", "BLR": "Bangalore", "MAA": "Chennai", "HYD": "Hyderabad",
	"LAX": "Los Angeles", "SJC": "San Jose", "SFO": "San Francisco", "SEA": "Seattle", "ORD": "Chicago", "DFW": "Dallas",
	"IAD": "Ashburn", "EWR": "Newark", "JFK": "New York", "BOS": "Boston", "MIA": "Miami", "DEN": "Denver",
	"ATL": "Atlanta", "PHX": "Phoenix", "LAS": "Las Vegas", "PDX": "Portland", "SLC": "Salt Lake City", "HNL": "Honolulu",
	"LHR": "London", "MAN": "Manchester", "CDG": "Paris", "AMS": "Amsterdam", "FRA": "Frankfurt", "MUC": "Munich",
	"DUS": "Düsseldorf", "HAM": "Hamburg", "TXL": "Berlin", "MAD": "Madrid", "BCN": "Barcelona", "LIS": "Lisbon",
	"FCO": "Rome", "MXP": "Milan", "ZRH": "Zurich", "GVA": "Geneva", "VIE": "Vienna", "PRG": "Prague",
	"WAW": "Warsaw", "CPH": "Copenhagen", "ARN": "Stockholm", "HEL": "Helsinki", "OSL": "Oslo", "DUB": "Dublin",
	"SYD": "Sydney", "MEL": "Melbourne", "BNE": "Brisbane", "PER": "Perth", "AKL": "Auckland", "CHC": "Christchurch",
	"DXB": "Dubai", "DOH": "Doha", "TLV": "Tel Aviv", "AMM": "Amman", "JED": "Jeddah", "RUH": "Riyadh",
	"JNB": "Johannesburg", "CPT": "Cape Town", "CAI": "Cairo", "NBO": "Nairobi", "LOS": "Lagos", "GRU": "São Paulo",
	"GIG": "Rio de Janeiro", "SCL": "Santiago", "BOG": "Bogota", "LIM": "Lima", "MEX": "Mexico City", "YYZ": "Toronto",
	"YVR": "Vancouver", "YUL": "Montréal",
}

var cityZhByName = map[string]string{
	"hong kong": "香港", "tokyo": "东京", "osaka": "大阪", "seoul": "首尔", "singapore": "新加坡", "taipei": "台北",
	"los angeles": "洛杉矶", "san jose": "圣何塞", "san francisco": "旧金山", "seattle": "西雅图", "chicago": "芝加哥",
	"london": "伦敦", "paris": "巴黎", "frankfurt": "法兰克福", "amsterdam": "阿姆斯特丹", "sydney": "悉尼",
	"melbourne": "墨尔本", "dubai": "迪拜", "toronto": "多伦多", "vancouver": "温哥华", "mexico city": "墨西哥城",
}

var cityEnByZh = map[string]string{
	"香港": "Hong Kong", "台北": "Taipei", "高雄": "Kaohsiung City", "东京": "Tokyo", "大阪": "Osaka", "福冈": "Fukuoka",
	"首尔": "Seoul", "新加坡": "Singapore", "曼谷": "Bangkok", "胡志明市": "Ho Chi Minh City", "河内": "Hanoi",
	"洛杉矶": "Los Angeles", "圣何塞": "San Jose", "旧金山": "San Francisco", "西雅图": "Seattle", "芝加哥": "Chicago",
	"达拉斯": "Dallas", "丹佛": "Denver", "伦敦": "London", "巴黎": "Paris", "法兰克福": "Frankfurt",
	"阿姆斯特丹": "Amsterdam", "马德里": "Madrid", "悉尼": "Sydney", "多伦多": "Toronto",
}
