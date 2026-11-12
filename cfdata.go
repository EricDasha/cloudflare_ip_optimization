//go:build ignore

// 独立 main 程序：只用 `go build cfdata.go` 单独构建（见 scripts/build-dist.ps1）。
// 理由同 cfnat.go：本目录是多个单文件 main 的集合，不可作为 ./... 包编译。
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var stdinReader = bufio.NewReader(os.Stdin)

// ----------------------- 数据类型定义 -----------------------

type DataCenterInfo struct {
	DataCenter string
	City       string
	IPCount    int
	MinLatency int // 毫秒
}

type ScanResult struct {
	IP          string
	DataCenter  string
	Region      string
	City        string
	LatencyStr  string
	TCPDuration time.Duration
}

type TestResult struct {
	IP         string
	MinLatency time.Duration
	MaxLatency time.Duration
	AvgLatency time.Duration
	LossRate   float64
}

type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

// ----------------------- 主程序入口 -----------------------

func main() {
	// 定义命令行参数
	scanThreads := flag.Int("scan", 100, "扫描阶段最大并发数")
	testThreads := flag.Int("test", 50, "详细测试阶段最大并发数")
	portFlag := flag.Int("port", 443, "详细测试使用的端口")
	delay := flag.Int("delay", 500, "延迟阈值，单位毫秒")
	updateFlag := flag.Bool("update", false, "非交互模式：强制更新 ip.csv")
	ipTypeFlag := flag.Int("iptype", 4, "非交互模式：测试 IPv4 或 IPv6 (4/6)")
	dataCenterFlag := flag.String("dc", "", "非交互模式：选择数据中心；留空表示全部")
	autoMode := flag.Bool("auto", false, "非交互模式：不提示，使用参数或默认值")
	noWait := flag.Bool("no-wait", false, "执行结束后不等待回车")
	flag.Parse()

	// 检查 ip.csv 是否存在
	_, err := os.Stat("ip.csv")
	updateChoice := "n"
	if err == nil {
		if *autoMode {
			if *updateFlag {
				updateChoice = "y"
			}
		} else {
			// 文件存在时提示用户是否更新
			for {
				fmt.Print("检测到 ip.csv 文件已存在，是否更新数据？(y/n, 默认n): ")
				input, err := readLine()
				if err != nil {
					fmt.Println("读取输入错误，使用默认 n:", err)
					input = "n"
				}
				if input == "" {
					input = "n"
				}
				input = strings.ToLower(strings.TrimSpace(input))
				if input == "y" || input == "n" {
					updateChoice = input
					break
				}
				fmt.Println("输入无效，请输入 y 或 n！")
			}
		}
	} else {
		// 文件不存在，直接更新数据
		updateChoice = "y"
	}

	if updateChoice == "y" {
		// 用户选择更新数据，询问测试 IPv4 或 IPv6
		ipType := *ipTypeFlag
		if ipType != 4 && ipType != 6 {
			ipType = 4
		}
		if !*autoMode {
			for {
				fmt.Print("请选择测试IPv4还是IPv6 (输入4或6，直接回车默认4): ")
				input, err := readLine()
				if err != nil {
					fmt.Println("读取输入错误，使用默认 IPv4:", err)
					input = ""
				}
				if input == "" {
					ipType = 4
					break
				}
				ipType, err = strconv.Atoi(input)
				if err != nil || (ipType != 4 && ipType != 6) {
					fmt.Println("输入无效，请输入4或6！")
					continue
				}
				break
			}
		}
		fmt.Printf("你选择的是: IPv%d\n", ipType)
		runIPScan(ipType, *scanThreads)
	}

	// 从 ip.csv 中读取数据中心信息，供用户选择
	selectedDC, ipList := selectDataCenterFromCSV(*dataCenterFlag, !*autoMode)
	if len(ipList) == 0 {
		fmt.Println("未找到IP地址，程序退出。")
		return
	}

	// 将IP列表写入 ip.txt（便于查看）
	err = writeIPsToFile("ip.txt", ipList)
	if err != nil {
		fmt.Println("写入 ip.txt 文件失败:", err)
	} else {
		if selectedDC == "" {
			fmt.Println("已将所有数据中心的IP地址写入 ip.txt")
		} else {
			fmt.Printf("已将数据中心 '%s' 的IP地址写入 ip.txt\n", selectedDC)
		}
	}

	// 对选中的IP列表做详细延迟和丢包测试，
	// 并写入结果CSV，同时输出横向柱状图（比例均基于 ip.txt 中的 IP 数量）
	runDetailedTest(ipList, selectedDC, *portFlag, *delay, *testThreads)

	// 修改退出逻辑：按 CTRL+C 或 回车键退出程序
	if *noWait || *autoMode {
		return
	}
	fmt.Println("\n程序执行结束，请按 CTRL+C 或 回车键退出程序。")
	exitChan := make(chan struct{})
	go func() {
		// 必须复用全局 stdinReader：另建一个 bufio.Reader 会各建
		// 4096 字节缓冲，先读的那个可能把回车吞进自己缓冲里，另
		// 一个永远等不到，只能 Ctrl+C 退出。
		_, _ = stdinReader.ReadString('\n')
		exitChan <- struct{}{}
	}()
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	select {
	case <-signalChan:
	case <-exitChan:
	}
}

// ----------------------- 功能模块 -----------------------

// runIPScan 根据用户选择的IPv4/IPv6，从指定URL获取CIDR列表、生成随机IP，然后扫描测试数据中心信息，最终写入 ip.csv
// clampThreads 把用户传的并发数钳到 [1,1000]，越界时打印提示并回落。
func clampThreads(n int, flagName string) int {
	if n < 1 {
		fmt.Printf("-%s %d 无效，并发数钳到 1\n", flagName, n)
		return 1
	}
	if n > 1000 {
		fmt.Printf("-%s %d 过大，并发数钳到 1000\n", flagName, n)
		return 1000
	}
	return n
}

func runIPScan(ipType int, scanMaxThreads int) {
	// -scan 0 或负数会让 make(chan,<=0) 后第一次发送即永久阻塞。
	scanMaxThreads = clampThreads(scanMaxThreads, "scan")
	var filename, url string
	if ipType == 6 {
		filename = "ips-v6.txt"
		url = "https://www.baipiao.eu.org/cloudflare/ips-v6"
	} else {
		filename = "ips-v4.txt"
		url = "https://www.baipiao.eu.org/cloudflare/ips-v4"
	}

	var content string
	var err error
	if _, err = os.Stat(filename); os.IsNotExist(err) {
		fmt.Printf("文件 %s 不存在，正在从 URL %s 下载数据\n", filename, url)
		content, err = getURLContent(url)
		if err != nil {
			fmt.Println("获取 URL 内容出错:", err)
			return
		}
		err = saveToFile(filename, content)
		if err != nil {
			fmt.Println("保存文件出错:", err)
			return
		}
	} else {
		content, err = getFileContent(filename)
		if err != nil {
			fmt.Println("读取本地文件出错:", err)
			return
		}
	}

	ipList := parseIPList(content)
	if ipType == 6 {
		ipList = getRandomIPv6s(ipList)
	} else {
		ipList = getRandomIPv4s(ipList)
	}

	var locations []location
	if _, err := os.Stat("locations.json"); os.IsNotExist(err) {
		fmt.Println("本地 locations.json 不存在，正在从 https://www.baipiao.eu.org/cloudflare/locations 下载")
		resp, err := httpGetClient.Get("https://www.baipiao.eu.org/cloudflare/locations")
		if err != nil {
			fmt.Printf("无法下载 locations.json: %v\n", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Printf("下载 locations.json 失败，状态码: %d\n", resp.StatusCode)
			return
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, downloadMaxBytes+1))
		if err != nil {
			fmt.Printf("无法读取响应体: %v\n", err)
			return
		}
		if len(body) > downloadMaxBytes {
			fmt.Printf("locations.json 超过 %d 字节上限，拒绝解析与落盘\n", downloadMaxBytes)
			return
		}
		err = json.Unmarshal(body, &locations)
		if err != nil {
			fmt.Printf("无法解析JSON: %v\n", err)
			return
		}
		err = saveToFile("locations.json", string(body))
		if err != nil {
			fmt.Printf("保存 locations.json 失败: %v\n", err)
			return
		}
	} else {
		fmt.Println("本地 locations.json 已存在，无需重新下载")
		file, err := os.Open("locations.json")
		if err != nil {
			fmt.Printf("无法打开 locations.json: %v\n", err)
			return
		}
		defer file.Close()
		body, err := io.ReadAll(file)
		if err != nil {
			fmt.Printf("读取 locations.json 失败: %v\n", err)
			return
		}
		err = json.Unmarshal(body, &locations)
		if err != nil {
			fmt.Printf("解析 locations.json 失败: %v\n", err)
			return
		}
	}

	locationMap := make(map[string]location)
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}

	var wg sync.WaitGroup
	wg.Add(len(ipList))
	resultChan := make(chan ScanResult, len(ipList))
	thread := make(chan struct{}, scanMaxThreads)
	var count int32
	total := len(ipList)

	for _, ip := range ipList {
		thread <- struct{}{}
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				current := atomic.AddInt32(&count, 1)
				percentage := float64(current) / float64(total) * 100
				fmt.Printf("扫描进度: %d/%d (%.2f%%)\r", current, total, percentage)
				if int(current) == total {
					fmt.Println("")
				}
			}()

			dialer := &net.Dialer{Timeout: 1 * time.Second}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, "80"))
			if err != nil {
				return
			}
			defer conn.Close()
			tcpDuration := time.Since(start)

			requestURL := "http://" + net.JoinHostPort(ip, "80")
			req, err := http.NewRequest("GET", requestURL, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Close = true

			conn.SetDeadline(time.Now().Add(2 * time.Second))
			if err := req.Write(conn); err != nil {
				return
			}

			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			cfRay := strings.TrimSpace(resp.Header.Get("CF-RAY"))
			if cfRay == "" {
				return
			}

			parts := strings.Split(cfRay, "-")
			if len(parts) < 2 {
				return
			}

			dataCenter := strings.TrimSpace(parts[len(parts)-1])
			if dataCenter == "" {
				return
			}

			loc, ok := locationMap[dataCenter]
			if ok {
				fmt.Printf("有效IP: %s, 数据中心: %s, 地区: %s, 城市: %s, 延迟: %d ms\n", ip, dataCenter, loc.Region, loc.City, tcpDuration.Milliseconds())
				resultChan <- ScanResult{IP: ip, DataCenter: dataCenter, Region: loc.Region, City: loc.City, LatencyStr: fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), TCPDuration: tcpDuration}
			} else {
				fmt.Printf("有效IP: %s, 数据中心: %s, 未知位置信息, 延迟: %d ms\n", ip, dataCenter, tcpDuration.Milliseconds())
				resultChan <- ScanResult{IP: ip, DataCenter: dataCenter, Region: "", City: "", LatencyStr: fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), TCPDuration: tcpDuration}
			}
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	if len(resultChan) == 0 {
		fmt.Println("未发现有效IP，程序退出。")
		os.Exit(1)
	}

	var results []ScanResult
	for r := range resultChan {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].TCPDuration < results[j].TCPDuration
	})

	file, err := os.Create("ip.csv")
	if err != nil {
		fmt.Printf("无法创建 ip.csv 文件: %v\n", err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	writer.Write([]string{"IP地址", "数据中心", "地区", "城市", "网络延迟"})
	for _, res := range results {
		writer.Write([]string{res.IP, res.DataCenter, res.Region, res.City, res.LatencyStr})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		fmt.Printf("写入 ip.csv 失败: %v\n", err)
		return
	}
	fmt.Println("扫描完成，ip.csv生成成功。")
}

func selectDataCenterFromCSV(preselectedDC string, interactive bool) (string, []string) {
	file, err := os.Open("ip.csv")
	if err != nil {
		fmt.Println("无法打开 ip.csv:", err)
		return "", nil
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		fmt.Println("读取 ip.csv 失败:", err)
		return "", nil
	}

	// 统计数据中心信息
	dataCenters := make(map[string]DataCenterInfo)
	for i, record := range records {
		if i == 0 {
			continue
		}
		if len(record) < 5 {
			continue
		}
		dc := record[1]
		city := record[3]
		latencyStr := record[4]
		latency := 0
		fmt.Sscanf(latencyStr, "%d", &latency)
		if info, exists := dataCenters[dc]; exists {
			info.IPCount++
			if latency < info.MinLatency {
				info.MinLatency = latency
			}
			dataCenters[dc] = info
		} else {
			dataCenters[dc] = DataCenterInfo{DataCenter: dc, City: city, IPCount: 1, MinLatency: latency}
		}
	}

	fmt.Println("请选择数据中心：")
	for _, info := range dataCenters {
		fmt.Printf("%s (%s) (IP数量: %d, 最低延迟: %d ms)\n", info.DataCenter, info.City, info.IPCount, info.MinLatency)
	}
	var selectedDC string
	preselectedDC = strings.TrimSpace(preselectedDC)
	if preselectedDC != "" || !interactive {
		if preselectedDC == "" {
			selectedDC = ""
		} else {
			for dc := range dataCenters {
				if strings.EqualFold(dc, preselectedDC) {
					selectedDC = dc
					break
				}
			}
			if selectedDC == "" {
				fmt.Printf("指定数据中心无效: %s，程序退出。\n", preselectedDC)
				return "", nil
			}
		}
	} else {
		for {
			fmt.Print("请输入数据中心名称（直接回车提取所有）：")
			input, err := readLine()
			if err != nil {
				fmt.Println("读取输入失败，默认提取所有:", err)
				selectedDC = ""
				break
			}
			input = strings.TrimSpace(input)
			if input == "" {
				selectedDC = ""
				break
			}
			found := false
			for dc := range dataCenters {
				if strings.EqualFold(dc, input) {
					selectedDC = dc
					found = true
					break
				}
			}
			if !found {
				fmt.Println("输入数据中心无效，请重新输入。")
				continue
			}
			break
		}
	}

	// 根据选择提取IP列表
	var ipList []string
	if selectedDC == "" {
		fmt.Println("提取所有数据中心的IP地址...")
		for i, record := range records {
			if i == 0 {
				continue
			}
			if len(record) >= 1 {
				ipList = append(ipList, record[0])
			}
		}
	} else {
		fmt.Printf("提取数据中心 '%s' 的IP地址...\n", selectedDC)
		for i, record := range records {
			if i == 0 {
				continue
			}
			if len(record) >= 2 && record[1] == selectedDC {
				ipList = append(ipList, record[0])
			}
		}
	}
	return selectedDC, ipList
}

// writeIPsToFile 将IP地址写入指定文本文件
func writeIPsToFile(filename string, ips []string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, ip := range ips {
		_, err := file.WriteString(ip + "\n")
		if err != nil {
			return err
		}
	}
	return nil
}

// runDetailedTest 对选中的IP列表进行详细测试（10次TCP连接测试443端口），统计延迟和丢包率，结果写入CSV文件
// 同时增加对测试结果的统计分析，并输出横向柱状图（基于 ip.txt 中的 IP 总数）
// safeDCFilename 把数据中心名洗成安全文件名：只留字母数字 - _，其余
// 字符变下划线。DC 名来自 ip.csv 第 2 列（外部数据），不洗的话
// DC="../../x" 会让 os.Create 写到工作目录之外，DC="result" 会
// 覆盖 result.csv。
func safeDCFilename(dc string) string {
	var b strings.Builder
	for _, r := range dc {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		return "result.csv"
	}
	return name + ".csv"
}

func runDetailedTest(ipList []string, selectedDC string, port int, delay int, testThreads int) {
	// -test 0 或负数同理死锁。
	testThreads = clampThreads(testThreads, "test")
	var wg sync.WaitGroup
	wg.Add(len(ipList))
	resultChan := make(chan TestResult, len(ipList))
	thread := make(chan struct{}, testThreads)
	var count int32
	total := len(ipList)
	for _, ip := range ipList {
		thread <- struct{}{}
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				current := atomic.AddInt32(&count, 1)
				perc := float64(current) / float64(total) * 100
				fmt.Printf("详细测试进度: %d/%d (%.2f%%)\r", current, total, perc)
				if int(current) == total {
					fmt.Println("")
				}
			}()
			dialer := &net.Dialer{
				Timeout: time.Duration(delay) * time.Millisecond,
			}
			successCount := 0
			totalLatency := time.Duration(0)
			minLatency := time.Duration(math.MaxInt64)
			maxLatency := time.Duration(0)
			// 进行10次测试
			for i := 0; i < 10; i++ {
				start := time.Now()
				conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
				if err != nil {
					continue
				}
				latency := time.Since(start)
				if latency > time.Duration(delay)*time.Millisecond {
					conn.Close()
					continue
				}
				successCount++
				totalLatency += latency
				if latency < minLatency {
					minLatency = latency
				}
				if latency > maxLatency {
					maxLatency = latency
				}
				conn.Close()
			}
			if successCount > 0 {
				avgLatency := totalLatency / time.Duration(successCount)
				lossRate := float64(10-successCount) / 10.0
				fmt.Printf("有效IP %s : 最小 %d ms, 最大 %d ms, 平均 %d ms, 丢包 %.2f%%\n",
					ip, minLatency.Milliseconds(), maxLatency.Milliseconds(), avgLatency.Milliseconds(), lossRate*100)
				resultChan <- TestResult{
					IP:         ip,
					MinLatency: minLatency,
					MaxLatency: maxLatency,
					AvgLatency: avgLatency,
					LossRate:   lossRate,
				}
			} else {
				// 未获得有效测试的 IP 视为丢包率 100%
				fmt.Printf("无效IP %s, 丢包率100%%, 已丢弃\n", ip)
			}
		}(ip)
	}
	wg.Wait()
	close(resultChan)

	// 如果没有成功测试的IP，不进行分析
	if len(resultChan) == 0 {
		fmt.Println("详细测试：未发现有效IP")
		return
	}
	var results []TestResult
	for r := range resultChan {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].LossRate == results[j].LossRate {
			return results[i].AvgLatency < results[j].AvgLatency
		}
		return results[i].LossRate < results[j].LossRate
	})

	outFilename := "result.csv"
	if selectedDC != "" {
		outFilename = safeDCFilename(selectedDC)
	}
	file, err := os.Create(outFilename)
	if err != nil {
		fmt.Println("无法创建结果文件:", err)
		return
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	writer.Write([]string{"IP地址", "最小延迟(ms)", "最大延迟(ms)", "平均延迟(ms)", "丢包率(%)"})
	for _, res := range results {
		writer.Write([]string{
			res.IP,
			fmt.Sprintf("%d", res.MinLatency.Milliseconds()),
			fmt.Sprintf("%d", res.MaxLatency.Milliseconds()),
			fmt.Sprintf("%d", res.AvgLatency.Milliseconds()),
			fmt.Sprintf("%d", int(res.LossRate*100)),
		})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		fmt.Println("写入结果文件时出现错误:", err)
		return
	}
	fmt.Printf("详细测试结束，结果已写入文件: %s\n", outFilename)

	// 调用分析函数，对详细测试结果进行统计分析，并输出横向柱状图（基于 ip.txt 中的 IP 总数）
	analyzeResults(results, total)
}

// analyzeResults 只输出横向柱状图
// 根据传入的测试结果以及总测试 IP 数量（ip.txt中），将成功测试的结果按丢包率桶分组（每10%一桶），
// 未成功测试的 IP 数量视为丢包率 100%
func analyzeResults(results []TestResult, totalIPs int) {
	// 对成功测试结果分桶（0%,10%,...90%）
	groups := make(map[int][]TestResult)
	for _, res := range results {
		// 按整数百分比取桶，如 5~9%归入0%，10~19%归入10%
		bucket := (int(res.LossRate*100) / 10) * 10
		groups[bucket] = append(groups[bucket], res)
	}
	// 未成功测试的 IP 数量视为 100% 丢包
	unsuccessfulCount := totalIPs - len(results)

	// 输出横向柱状图（固定 0%,10%,...,100% 桶）
	fmt.Println("------ 横向柱状图 （丢包率占比） ------")
	maxBarWidth := 50
	for bucket := 0; bucket <= 100; bucket += 10 {
		var count int
		var avgLatStr string
		if bucket == 100 {
			count = unsuccessfulCount
			avgLatStr = "N/A"
		} else {
			count = len(groups[bucket])
			if count > 0 {
				var totalLatency int64 = 0
				for _, res := range groups[bucket] {
					totalLatency += res.MinLatency.Milliseconds()
				}
				avgLat := float64(totalLatency) / float64(count)
				avgLatStr = fmt.Sprintf("%.0f ms", avgLat)
			} else {
				avgLatStr = "N/A"
			}
		}
		proportion := float64(count) / float64(totalIPs) * 100.0
		barLength := int(proportion / 100 * float64(maxBarWidth))
		bar := strings.Repeat("#", barLength)
		fmt.Printf("丢包率 %3d%% |%-50s| (%.2f%%, %d个, 平均延迟: %s)\n", bucket, bar, proportion, count, avgLatStr)
	}
}

// ----------------------- 工具函数 -----------------------

// readLine 从标准输入读取一行数据（去除换行符）
func readLine() (string, error) {
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// getURLContent 根据指定URL下载内容
// httpGetClient 给裸 http.Get 加 10 秒超时：远端挂住时不再无限等。
var httpGetClient = &http.Client{Timeout: 10 * time.Second}

// downloadMaxBytes 是 ips/locations 类下载的上限：预期文件仅 KB 级，
// 10 秒超时内远端被攻陷/DNS 劫持返回超大体时，无上限的 ReadAll 会把
// 数百 MB 灌进内存（还会落盘）。超限即报错，不解析、不落盘。
const downloadMaxBytes = 2 << 20

func getURLContent(url string) (string, error) {
	resp, err := httpGetClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP请求失败，状态码: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, downloadMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > downloadMaxBytes {
		return "", fmt.Errorf("下载内容超过 %d 字节上限", downloadMaxBytes)
	}
	return string(body), nil
}

// getFileContent 从本地读取指定文件的内容
func getFileContent(filename string) (string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// saveToFile 将内容保存到指定文件中
func saveToFile(filename, content string) error {
	return os.WriteFile(filename, []byte(content), 0644)
}

// parseIPList 按行解析文本内容，返回非空行组成的字符串切片
func parseIPList(content string) []string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	var ipList []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			ipList = append(ipList, line)
		}
	}
	return ipList
}

// getRandomIPv4s 从类似 "xxx.xxx.xxx.xxx/24" 的CIDR中随机生成一个IPv4地址（只替换最后一段）。
// 非法输入直接跳过：旧写法只按 "." 切分，不校验数字合法性，且末段可取
// .0（网络地址）与 .255（广播地址）。
func getRandomIPv4s(ipList []string) []string {
	rand.Seed(time.Now().UnixNano())
	var randomIPs []string
	for _, subnet := range ipList {
		_, ipnet, err := net.ParseCIDR(strings.TrimSpace(subnet))
		if err != nil {
			continue
		}
		ones, bits := ipnet.Mask.Size()
		if bits != 32 || ones != 24 {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		out := make(net.IP, net.IPv4len)
		copy(out, ip4)
		// 主机段取 1..254，避开 .0 网络地址与 .255 广播地址。
		out[3] = byte(1 + rand.Intn(254))
		randomIPs = append(randomIPs, out.String())
	}
	return randomIPs
}

// getRandomIPv6s 从类似 "xxxx:xxxx:xxxx::/48" 的CIDR中随机生成一个IPv6地址（保留前三组）。
// 旧写法按 ":" 切分，`2400::/48` 这类压缩写法会切出空段，拼出 `2400:::`
// 非法地址；非十六进制段也不校验。
func getRandomIPv6s(ipList []string) []string {
	rand.Seed(time.Now().UnixNano())
	var randomIPs []string
	for _, subnet := range ipList {
		_, ipnet, err := net.ParseCIDR(strings.TrimSpace(subnet))
		if err != nil {
			continue
		}
		ones, bits := ipnet.Mask.Size()
		if bits != 128 || ones != 48 {
			continue
		}
		ip16 := ipnet.IP.To16()
		if ip16 == nil {
			continue
		}
		out := make(net.IP, net.IPv6len)
		copy(out, ip16)
		// 生成后 80 位随机数据（第 6 字节起）。
		for i := 6; i < net.IPv6len; i++ {
			out[i] = byte(rand.Intn(256))
		}
		randomIPs = append(randomIPs, out.String())
	}
	return randomIPs
}
