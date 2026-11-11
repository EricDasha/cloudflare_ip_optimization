//go:build ignore

// 独立 main 程序：只用 `go build cfnat.go` 单独构建（见 scripts/build-dist.ps1）。
// 本文件与 cfdata.go 都声明 func main，若不加此标签，`go build ./...` 会因
// main/location 重复声明而失败，也会连带编译到 x-tunnel.go 的缺失依赖。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	timeout     = 1 * time.Second // 超时时间
	maxDuration = 2 * time.Second // 最大持续时间
)

var (
	activeConnections int32 // 用于跟踪活跃连接的数量
	randomMu          sync.Mutex
	randomGenerator   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// maxClientAssignments 设备钉位表的上限。
//
// 家用局域网只有几台设备，这个上限唯一的用途是防止「服务被暴露到公网后被
// 当成粘性表塞满内存」。超过即整体作废重来——在那种场景下，粘性本身也没有意义。
const maxClientAssignments = 512

// IPManager 用于安全管理 IP 地址状态
type IPManager struct {
	mu            sync.Mutex
	ipAddresses   []string
	dispatchIndex int
	// assignments 把「客户端主机」钉到一个具体出口，实现一台设备一个上游。
	//
	// 存目标字符串而不是下标：换池时下标会整体挪位，按字符串保留能让出口仍在
	// 新池里的设备维持原映射，只有它真的被剔除了才重新分配。否则一次补位就会
	// 让局域网里所有设备同时换线，正在看的视频全部断流。
	assignments map[string]string
	// assignCursor 是分配新设备时的起始扫描位置，与 dispatchIndex 分开。
	// 两种分发方式互斥（-sticky 决定），但混在一次会干扰单测钉住的「粘性
	// 不推进轮转指针」这条约束，也会在两套逻辑并存时产生难以解释的偏移。
	assignCursor int
}

func NewIPManager() *IPManager {
	return &IPManager{assignments: map[string]string{}}
}

func (m *IPManager) SetIPAddresses(ips []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ipAddresses = ips
	m.dispatchIndex = 0
	m.assignCursor = 0
	if m.assignments == nil {
		m.assignments = map[string]string{}
		return
	}
	alive := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		alive[ip] = struct{}{}
	}
	for client, target := range m.assignments {
		if _, ok := alive[target]; !ok {
			delete(m.assignments, client)
		}
	}
}

func (m *IPManager) SetPriorityIPs(ips []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 保留旧配置接口以兼容已有 Compose/API；数据面不再按 priority
	// 加权，来源优先级只在控制面构建 active pool 时生效。
}

// nextTargets returns a round-robin slice for a new client connection.
func (m *IPManager) nextTargets(port, count int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if count <= 0 || len(m.ipAddresses) == 0 {
		return nil
	}
	if count > len(m.ipAddresses) {
		count = len(m.ipAddresses)
	}

	ips := make([]string, count)
	for i := range ips {
		ips[i] = m.ipAddresses[(m.dispatchIndex+i)%len(m.ipAddresses)]
	}
	m.dispatchIndex = (m.dispatchIndex + count) % len(m.ipAddresses)
	return generateTargets(ips, port)
}

// assignLocked 给一台设备分配一个独占的上游，返回它在 ipAddresses 中的下标。
//
// 为什么不用哈希：哈希只能保证「同一台设备每次落到同一个位置」，保证不了
// 「不同设备落到不同位置」。两台设备撞到同一个上游时，页面上看起来就是
// 「负载均衡没生效」，实际确实没生效——一个出口分给了两台设备，另一个闲置。
//
// 这里改成按到达顺序排队占位：新设备优先拿当前没人用的出口，全被占满才回头
// 复用。N 台设备配 N 个出口时，一定是各占一个，这才是用户要的「均衡」。
func (m *IPManager) assignLocked(clientHost string) int {
	n := len(m.ipAddresses)
	if n == 0 {
		return -1
	}
	if m.assignments == nil {
		m.assignments = map[string]string{}
	}
	// 表满只可能出现在「服务被暴露到公网」的场景；此时粘性已无意义，整体作废。
	if len(m.assignments) >= maxClientAssignments {
		m.assignments = map[string]string{}
	}
	// 已有钉位：出口还在池里就继续用。这是换池时设备不跳线的关键——
	// 补进来一个新成员不应该让所有正在看视频的设备同时换线。
	if target, ok := m.assignments[clientHost]; ok {
		for i, ip := range m.ipAddresses {
			if ip == target {
				return i
			}
		}
		delete(m.assignments, clientHost)
	}
	used := make(map[string]struct{}, len(m.assignments))
	for _, target := range m.assignments {
		used[target] = struct{}{}
	}
	pick := -1
	for offset := 0; offset < n; offset++ {
		idx := (m.assignCursor + offset) % n
		if _, taken := used[m.ipAddresses[idx]]; taken {
			continue
		}
		pick = idx
		break
	}
	if pick < 0 {
		// 设备比出口多，只能轮流复用；此时至少保证相邻设备不挤在同一个出口。
		pick = m.assignCursor % n
	}
	m.assignments[clientHost] = m.ipAddresses[pick]
	m.assignCursor = (pick + 1) % n
	return pick
}

// repinClient 把一台设备的钉位改到一个刚刚拨通的出口上。
//
// 用途：钉住的出口坏了但还留在池里（拨号超时、连接被拒），这条连接会按回退
// 顺序落到下一个成员上。若不改钉位，该设备的下一条连接又会回去撞同一堵墙，
// 表现为「这台设备一直卡，别的设备正常」。拨通的那个才是实证。
func (m *IPManager) repinClient(clientHost, target string) {
	if clientHost == "" || target == "" {
		return
	}
	if host, _, err := net.SplitHostPort(target); err == nil {
		target = host
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.assignments == nil {
		m.assignments = map[string]string{}
	}
	// 只钉在自家池成员上：兜底目标也走同一个拨号函数，把设备钉到池外
	// 成员上会让下一次分配立刻判它失效、白折腾一遍。
	known := false
	for _, ip := range m.ipAddresses {
		if ip == target {
			known = true
			break
		}
	}
	if !known {
		return
	}
	m.assignments[clientHost] = target
}

// nextTargetsForClient 按客户端来源分发：每台设备独占一个上游 IP
// （count>1 时其后目标按序排列，即拨号失败的回退顺序）。
// clientHost 为空时退化为严格轮转。
func (m *IPManager) nextTargetsForClient(clientHost string, port, count int) []string {
	if clientHost == "" {
		return m.nextTargets(port, count)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if count <= 0 || len(m.ipAddresses) == 0 {
		return nil
	}
	if count > len(m.ipAddresses) {
		count = len(m.ipAddresses)
	}
	start := m.assignLocked(clientHost)
	if start < 0 {
		return nil
	}
	ips := make([]string, count)
	for i := range ips {
		ips[i] = m.ipAddresses[(start+i)%len(m.ipAddresses)]
	}
	return generateTargets(ips, port)
}

type result struct {
	ip          string        // IP地址
	dataCenter  string        // 数据中心
	region      string        // 地区
	city        string        // 城市
	latency     string        // 延迟
	tcpDuration time.Duration // TCP请求延迟
}

type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

func main() {
	localAddr := flag.String("addr", "0.0.0.0:1234", "本地监听的 IP 和端口")
	code := flag.Int("code", 200, "HTTP/HTTPS 响应状态码")
	coloFilter := flag.String("colo", "", "筛选数据中心例如 HKG,SJC,LAX (多个数据中心用逗号隔开,留空则忽略匹配)")
	Delay := flag.Int("delay", 300, "有效延迟（毫秒），超过此延迟将断开连接")
	domain := flag.String("domain", "cloudflaremirrors.com/debian", "响应状态码检查的域名地址")
	fixedIPs := flag.String("fixed", "", "固定转发目标：IP 或优选域名，多个用逗号分隔；留空时自动扫描")
	fallbackIPs := flag.String("fallback", "", "兜底目标（IP 或优选域名），仅在固定转发目标拨号失败时尝试；多个用逗号分隔")
	priorityIPs := flag.String("priority", "", "兼容旧配置保留；数据面不再按优先 IP 加权")
	ipCount := flag.Int("ipnum", 20, "提取的有效IP数量")
	ipsType := flag.String("ips", "4", "指定生成IPv4还是IPv6地址 (4或6)")
	num := flag.Int("num", 1, "单连接拨号失败时按顺序尝试的目标数量")
	port := flag.Int("port", 443, "转发的目标端口")
	random := flag.Bool("random", true, "是否随机生成IP，如果为false，则从CIDR中拆分出所有IP")
	sticky := flag.Bool("sticky", true, "设备粘性：同一客户端 IP 恒定走同一上游，且不同设备尽量各占一个出口（拨号失败才顺延）")
	maxThreads := flag.Int("task", 100, "并发请求最大协程数")
	useTLS := flag.Bool("tls", true, "是否为 TLS 端口")

	flag.Parse()
	// 保留旧 flags，避免现有 Compose/Web 启动参数失效；固定池数据面不使用协议探针。
	_, _, _ = code, domain, useTLS

	// 创建 IP 管理器
	ipManager := NewIPManager()
	var fallbackManager *IPManager

	// 启动 TCP 监听
	listener, err := net.Listen("tcp", *localAddr)
	if err != nil {
		log.Fatalf("无法监听 %s: %v", *localAddr, err)
	}
	defer listener.Close()

	log.Printf("正在监听 %s 并转发到 %d 个目标地址，有效延迟：%d ms", *localAddr, *num, *Delay)

	for {
		startTime := time.Now()
		fixedMode := strings.TrimSpace(*fixedIPs) != ""
		fallbackMode := strings.TrimSpace(*fallbackIPs) != ""
		var results []result
		if fixedMode {
			targets, parseErr := parseFixedTargets(*fixedIPs)
			if parseErr != nil {
				log.Printf("固定转发目标配置无效: %v", parseErr)
				time.Sleep(3 * time.Second)
				continue
			}
			results = make([]result, 0, len(targets))
			for _, target := range targets {
				results = append(results, result{ip: target})
			}
			if priorities, err := parseFixedTargets(*priorityIPs); err == nil {
				ipManager.SetPriorityIPs(priorities)
			}
			if fallbackMode {
				fbTargets, fbErr := parseFixedTargets(*fallbackIPs)
				if fbErr != nil {
					log.Printf("兜底目标配置无效: %v", fbErr)
				} else {
					fallbackManager = NewIPManager()
					fallbackManager.SetIPAddresses(fbTargets)
				}
			}
			fixedIPs, fixedDomains := countFixedKinds(results)
			log.Printf("使用 %d 个固定转发目标（%d IP + %d 域名），跳过官方段扫描", len(results), fixedIPs, fixedDomains)
		} else {

			// 使用函数处理 locations.json，确保 defer 正确执行
			locations, err := loadLocations()
			if err != nil {
				log.Printf("加载位置信息失败: %v", err)
				time.Sleep(3 * time.Second)
				continue
			}

			locationMap := make(map[string]location)
			for _, loc := range locations {
				locationMap[loc.Iata] = loc
			}

			var url string
			var filename string

			// 使用 switch 替代 if-else
			switch *ipsType {
			case "6":
				filename = "ips-v6.txt"
				url = "https://www.baipiao.eu.org/cloudflare/ips-v6"
			case "4":
				filename = "ips-v4.txt"
				url = "https://www.baipiao.eu.org/cloudflare/ips-v4"
			default:
				fmt.Println("无效的IP类型。请使用 '4' 或 '6'")
				return
			}

			var content string

			// 检查本地是否有文件
			if _, err = os.Stat(filename); os.IsNotExist(err) {
				fmt.Printf("文件 %s 不存在，正在从 URL %s 下载数据\n", filename, url)
				content, err = getURLContent(url)
				if err != nil {
					fmt.Println("获取URL内容出错:", err)
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

			var ipList []string
			if *random {
				ipList = parseIPList(content)
				switch *ipsType {
				case "6":
					ipList = getRandomIPv6s(ipList)
				case "4":
					ipList = getRandomIPv4s(ipList)
				}
			} else {
				ipList, err = readIPs(filename)
				if err != nil {
					fmt.Println("读取IP出错:", err)
					return
				}
			}

			// 从生成的 IP 列表进行处理
			results = scanIPs(ipList, locationMap, *maxThreads)
		}

		if len(results) == 0 {
			fmt.Println("未发现有效IP")
			time.Sleep(3 * time.Second)
			continue
		}

		// 应用数据中心筛选
		if *coloFilter != "" && !fixedMode {
			results = applyColoFilter(results, *coloFilter)
			if len(results) == 0 {
				// 过滤后为空不能进 accept 循环：否则 SetIPAddresses(nil)
				// 后每个连接都无上游可转、且永远不再重扫。
				fmt.Println("数据中心筛选后无可用IP")
				time.Sleep(3 * time.Second)
				continue
			}
		}

		// 按 TCP 延迟排序。必须用 Stable：fixed 模式下全部 tcpDuration
		// 都是 0，不稳定排序会打乱用户指定的 -fixed 顺序，再经
		// capFixedTargets 截断，留谁全凭运气；扫描模式下延迟相同也
		// 应保持扫描顺序。
		sort.SliceStable(results, func(i, j int) bool {
			return results[i].tcpDuration < results[j].tcpDuration
		})

		// 只显示指定数量的 IP
		if len(results) > *ipCount {
			if fixedMode {
				results = capFixedTargets(results, *ipCount)
			} else {
				results = results[:*ipCount]
			}
		}

		fmt.Println("IP 地址 | 数据中心 | 地区 | 城市 | 延迟")
		for _, r := range results {
			fmt.Printf("%s | %s | %s | %s | %s\n", r.ip, r.dataCenter, r.region, r.city, r.latency)
		}

		if domains := len(results) - countIPTargets(results); domains > 0 {
			fmt.Printf("成功提取 %d 个有效IP + %d 个域名，耗时 %d秒\n", len(results)-domains, domains, time.Since(startTime)/time.Second)
		} else {
			fmt.Printf("成功提取 %d 个有效IP，耗时 %d秒\n", len(results), time.Since(startTime)/time.Second)
		}

		// 设置 IP 地址列表
		var ips []string
		for _, r := range results {
			ips = append(ips, r.ip)
		}
		ipManager.SetIPAddresses(ips)
		if priorities, err := parseFixedTargets(*priorityIPs); err == nil {
			ipManager.SetPriorityIPs(priorities)
		}

		// CFnat 数据面只负责在新 TCP 连接建立时轮换 IP。
		// 不再主动连接自身监听口做“健康检查”：该探针会制造空的 TLS/WS
		// 上游连接，GrainTCP/EdgeTunnel 可能将其视为异常会话并导致前端 -1。
		for {
			if tcpListener, ok := listener.(*net.TCPListener); ok {
				_ = tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
			}
			conn, err := listener.Accept()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if opErr, ok := err.(*net.OpError); ok && strings.Contains(opErr.Error(), "use of closed network connection") {
					return
				}
				log.Printf("接受连接时发生错误: %v", err)
				continue
			}

			clientAddr := conn.RemoteAddr().String()
			atomic.AddInt32(&activeConnections, 1)
			log.Printf("客户端来源: %s 连接建立，当前活跃连接数: %d", clientAddr, atomic.LoadInt32(&activeConnections))
			// 多目标参数仅作为“失败时按顺序回退”，不再并发竞速，
			// 从而保证一次前端逻辑连接只产生一个真实上游会话。
			// 粘性模式：每台设备独占一个上游；拨号失败按序回退不变。
			clientHost, _, _ := net.SplitHostPort(clientAddr)
			var primary []string
			if *sticky {
				primary = ipManager.nextTargetsForClient(clientHost, *port, *num)
			} else {
				primary = ipManager.nextTargets(*port, *num)
			}
			var fallback []string
			if fallbackManager != nil {
				fallback = fallbackManager.nextTargets(*port, 1)
			}
			// 只有粘性模式下才纠正钉位：轮转模式本来就不该有「设备记忆」。
			manager := ipManager
			if !*sticky {
				manager = nil
			}
			go handleConnection(conn, manager, clientHost, append(primary, fallback...), time.Duration(*Delay)*time.Millisecond)
		}
	}
}

// loadLocations 加载位置信息，使用函数封装确保 defer 正确执行
// httpGetClient 给裸 http.Get 加 10 秒超时：远端挂住时启动不再无限等。
var httpGetClient = &http.Client{Timeout: 10 * time.Second}

func loadLocations() ([]location, error) {
	var locations []location

	if _, err := os.Stat("locations.json"); os.IsNotExist(err) {
		fmt.Println("本地 locations.json 不存在\n正在从 https://www.baipiao.eu.org/cloudflare/locations 下载 locations.json")
		resp, err := httpGetClient.Get("https://www.baipiao.eu.org/cloudflare/locations")
		if err != nil {
			return nil, fmt.Errorf("无法从URL中获取JSON: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("下载 locations.json 失败，状态码: %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("无法读取响应体: %v", err)
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			return nil, fmt.Errorf("无法解析JSON: %v", err)
		}

		file, err := os.Create("locations.json")
		if err != nil {
			return nil, fmt.Errorf("无法创建文件: %v", err)
		}
		defer file.Close()

		_, err = file.Write(body)
		if err != nil {
			return nil, fmt.Errorf("无法写入文件: %v", err)
		}
	} else {
		file, err := os.Open("locations.json")
		if err != nil {
			return nil, fmt.Errorf("无法打开文件: %v", err)
		}
		defer file.Close()

		body, err := io.ReadAll(file)
		if err != nil {
			return nil, fmt.Errorf("无法读取文件: %v", err)
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			return nil, fmt.Errorf("无法解析JSON: %v", err)
		}
	}

	return locations, nil
}

// scanIPs 扫描 IP 列表并返回结果
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

func scanIPs(ipList []string, locationMap map[string]location, maxThreads int) []result {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []result

	// -task 0 或负数会让 make(chan,<=0) 后第一次发送即永久阻塞；
	// 上限 1000 防手滑把并发打爆。
	maxThreads = clampThreads(maxThreads, "task")
	thread := make(chan struct{}, maxThreads)

	var count int32
	total := len(ipList)

	for _, ip := range ipList {
		wg.Add(1)
		thread <- struct{}{}
		go func(ipAddr string) {
			defer func() {
				<-thread
				wg.Done()
				current := atomic.AddInt32(&count, 1)
				percentage := float64(current) / float64(total) * 100
				fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\r", current, total, percentage)
				if int(current) == total {
					fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\n", current, total, percentage)
				}
			}()

			dialer := &net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ipAddr, "80"))
			if err != nil {
				return
			}
			defer conn.Close()

			tcpDuration := time.Since(start)

			// 通过根路径响应头里的 CF-RAY 提取机房信息
			requestURL := "http://" + net.JoinHostPort(ipAddr, "80")
			req, err := http.NewRequest("GET", requestURL, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Close = true

			conn.SetDeadline(time.Now().Add(maxDuration))
			err = req.Write(conn)
			if err != nil {
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
			mu.Lock()
			if ok {
				fmt.Printf("发现有效IP %s 位置信息 %s 延迟 %d 毫秒\n", ipAddr, loc.City, tcpDuration.Milliseconds())
				results = append(results, result{ipAddr, dataCenter, loc.Region, loc.City, fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration})
			} else {
				fmt.Printf("发现有效IP %s 位置信息未知 延迟 %d 毫秒\n", ipAddr, tcpDuration.Milliseconds())
				results = append(results, result{ipAddr, dataCenter, "", "", fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration})
			}
			mu.Unlock()
		}(ip)
	}

	wg.Wait()
	return results
}

// 获取URL内容
func getURLContent(url string) (string, error) {
	resp, err := httpGetClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP请求失败，状态码: %d", resp.StatusCode)
	}

	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			content.WriteString(line + "\n")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	return content.String(), nil
}

// 从本地文件读取内容
func getFileContent(filename string) (string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// 将内容保存到本地文件
func saveToFile(filename, content string) error {
	return os.WriteFile(filename, []byte(content), 0644)
}

// 解析IP列表，跳过空行
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

// parseFixedTargets 解析固定转发目标列表，每个目标可以是 IP 或优选域名。
// 域名目标在每次新连接拨号时按当前 DNS 解析，天然跟随第三方优选 IP 轮换。
func parseFixedTargets(value string) ([]string, error) {
	seen := make(map[string]struct{})
	var targets []string
	for _, raw := range strings.Split(value, ",") {
		target := strings.TrimSpace(raw)
		if target == "" {
			continue
		}
		if parsed := net.ParseIP(target); parsed != nil {
			target = parsed.String()
		} else if !validForwardHostname(target) {
			return nil, fmt.Errorf("%q 不是有效 IP 或域名", target)
		}
		key := strings.ToLower(target)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("至少需要一个固定 IP 或域名")
	}
	return targets, nil
}

// countFixedKinds 分别统计固定目标里的 IP 与域名数量。
func countFixedKinds(results []result) (ips, domains int) {
	for _, r := range results {
		if net.ParseIP(r.ip) == nil {
			domains++
		} else {
			ips++
		}
	}
	return ips, domains
}

// countIPTargets 统计列表里真正的 IP 条数（域名不计）。
func countIPTargets(results []result) int {
	n := 0
	for _, r := range results {
		if net.ParseIP(r.ip) != nil {
			n++
		}
	}
	return n
}

// capFixedTargets 把 IP 数量限制在 limit 以内，域名全部保留。
//
// 固定模式下 -fixed 同时带 IP 和域名，-ipnum 的语义是「取多少个有效 IP」，
// 不是「整个目标列表最多几项」。旧实现直接 results[:ipCount]，排序对全为 0
// 的固定目标又不稳定，域名会被任意截掉——IP 池一空，域名也跟着消失。
func capFixedTargets(results []result, limit int) []result {
	if limit < 0 {
		limit = 0
	}
	out := make([]result, 0, len(results))
	ips := 0
	for _, r := range results {
		if net.ParseIP(r.ip) == nil {
			out = append(out, r)
			continue
		}
		if ips < limit {
			out = append(out, r)
			ips++
		}
	}
	return out
}

// validForwardHostname 校验可作为转发目标的域名：仅 ASCII、含点、逐标签合法。
// 纯数字加点的“残 IP”形式（如 1.2.3.999）一律拒绝，避免被误当域名。
func validForwardHostname(host string) bool {
	if len(host) < 4 || len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	if !strings.Contains(host, ".") || strings.ContainsAny(host, " /\\@\"':") {
		return false
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	numericOnly := true
	for _, char := range host {
		if char != '.' && (char < '0' || char > '9') {
			numericOnly = false
			break
		}
	}
	if numericOnly {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}

func nextRandomIntn(n int) int {
	randomMu.Lock()
	defer randomMu.Unlock()
	return randomGenerator.Intn(n)
}

// 从每个/24子网随机提取一个IPv4
func getRandomIPv4s(ipList []string) []string {
	var randomIPs []string
	for _, subnet := range ipList {
		// 跳过空行
		subnet = strings.TrimSpace(subnet)
		if subnet == "" {
			continue
		}
		baseIP := strings.TrimSuffix(subnet, "/24")
		octets := strings.Split(baseIP, ".")
		if len(octets) >= 4 {
			octets[3] = fmt.Sprintf("%d", nextRandomIntn(256))
			randomIP := strings.Join(octets, ".")
			randomIPs = append(randomIPs, randomIP)
		}
	}
	return randomIPs
}

// 从每个/48子网随机提取一个IPv6
func getRandomIPv6s(ipList []string) []string {
	var randomIPs []string
	for _, subnet := range ipList {
		// 跳过空行
		subnet = strings.TrimSpace(subnet)
		if subnet == "" {
			continue
		}
		baseIP := strings.TrimSuffix(subnet, "/48")
		sections := strings.Split(baseIP, ":")
		if len(sections) >= 3 {
			sections = sections[:3]
			for i := 3; i < 8; i++ {
				sections = append(sections, fmt.Sprintf("%x", nextRandomIntn(65536)))
			}
			randomIP := strings.Join(sections, ":")
			randomIPs = append(randomIPs, randomIP)
		}
	}
	return randomIPs
}

// applyColoFilter 按数据中心（逗号分隔，忽略大小写）筛选扫描结果。
func applyColoFilter(results []result, coloFilter string) []result {
	filters := strings.Split(coloFilter, ",")
	var filtered []result
	for _, r := range results {
		for _, filter := range filters {
			if strings.EqualFold(r.dataCenter, filter) {
				filtered = append(filtered, r)
				break
			}
		}
	}
	return filtered
}

// readIPs 展开上限：单个 CIDR 最多 65536 个地址（/16），整文件最多
// 262144 条。再大不是“候选列表”，是直接把内存和扫描时间撑爆——
// IPv6 /48 展开是 2^80 个地址，旧代码会在这个循环里跑到天荒地老。
const (
	maxCIDRExpansion = 65536
	maxReadIPsTotal  = 262144
)

// 从CIDR中拆分出所有IP
func readIPs(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var ips []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// 跳过空行
		if line == "" {
			continue
		}
		if strings.Contains(line, "/") {
			ipAddr, ipNet, err := net.ParseCIDR(line)
			if err != nil {
				return nil, err
			}
			// 先算再展开：掩码位数直接决定展开规模，IPv6 大前缀
			// 不经此检查会把循环跑到天荒地老。
			if ones, bits := ipNet.Mask.Size(); bits-ones > 16 {
				return nil, fmt.Errorf("CIDR %s 展开超过 %d 个地址", line, maxCIDRExpansion)
			}
			// 使用新变量避免遮蔽
			for currentIP := ipAddr.Mask(ipNet.Mask); ipNet.Contains(currentIP); incrementIP(currentIP) {
				ips = append(ips, currentIP.String())
			}
		} else {
			ips = append(ips, line)
		}
		if len(ips) > maxReadIPsTotal {
			return nil, fmt.Errorf("IP 文件展开超过 %d 条", maxReadIPsTotal)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return ips, nil
}

// 增加IP
func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func generateTargets(ips []string, port int) []string {
	targets := make([]string, len(ips))
	for i, ip := range ips {
		address := ip
		if strings.Contains(ip, ":") {
			address = fmt.Sprintf("[%s]", ip)
		}
		targets[i] = fmt.Sprintf("%s:%d", address, port)
	}
	return targets
}

// 处理客户端连接；仅在拨号失败时按顺序尝试下一个地址。
// manager/clientHost 用于钉位纠正：钉住的出口拨不通、回退到别的成员并拨通时，
// 把该设备的钉位改到真正拨通的那个，别让它下一条连接再去撞同一堵墙。
func handleConnection(conn net.Conn, manager *IPManager, clientHost string, forwardAddrs []string, delay time.Duration) {
	dialer := &net.Dialer{Timeout: delay}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		upstream, err := dialer.DialContext(ctx, network, addr)
		if err == nil && manager != nil {
			manager.repinClient(clientHost, addr)
		}
		return upstream, err
	}
	handleConnectionWithDial(conn, forwardAddrs, dial)
}

// handleConnectionWithDial 处理一条客户端连接：按顺序拨号，首个成功即用。
//
// 日志里带客户端地址是有意为之：控制台的「设备名册」靠这一行把「哪台设备
// 现在走哪个出口」对上号。cfnat 是四层哑管道，/proc 里只看到两个互不相干的
// socket，唯一能把它们连起来的证据就是这里打出来的这条记录。
func handleConnectionWithDial(conn net.Conn, forwardAddrs []string, dial func(context.Context, string, string) (net.Conn, error)) {
	clientHost, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	defer func() {
		clientAddr := conn.RemoteAddr().String()
		atomic.AddInt32(&activeConnections, -1)
		log.Printf("客户端来源: %s 连接关闭，当前活跃连接数: %d", clientAddr, atomic.LoadInt32(&activeConnections))
		conn.Close()
	}()

	if len(forwardAddrs) == 0 {
		log.Printf("未找到可用的转发地址")
		return
	}
	var lastErr error
	for _, target := range forwardAddrs {
		started := time.Now()
		upstream, err := dial(context.Background(), "tcp", target)
		if err != nil {
			lastErr = err
			log.Printf("连接到 %s 失败: %v", target, err)
			continue
		}
		log.Printf("选择转发连接: 客户端 %s，地址: %s，拨号耗时: %d ms", clientHost, target, time.Since(started).Milliseconds())
		pipeConnections(conn, upstream)
		return
	}
	log.Printf("未找到可用的转发连接: %v", lastErr)
}

// pipeIdleTimeout 任一方向无流量超过此时长即断开：半开/装死的连接
// 否则会永远占着两个 goroutine、两个 FD 与一个 activeConnections
// 计数。10 分钟——内层协议自带心跳，真空闲这么久就是死了。
// 变量而非常量，供测试收窄。
var pipeIdleTimeout = 10 * time.Minute

func pipeConnections(src, dst net.Conn) {
	var wg sync.WaitGroup
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			src.Close()
			dst.Close()
		})
	}

	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = copyWithIdleTimeout(src, dst, pipeIdleTimeout)
		closeBoth()
	}()

	go func() {
		defer wg.Done()
		_, _ = copyWithIdleTimeout(dst, src, pipeIdleTimeout)
		closeBoth()
	}()

	wg.Wait()
}

// copyWithIdleTimeout 从 src 拷到 dst，每次读前刷新 src 的读 deadline。
// 对端握手完就装死时，读超时即返回，不再无限占槽。
func copyWithIdleTimeout(dst io.Writer, src net.Conn, timeout time.Duration) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		if err := src.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return written, err
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			return written, er
		}
	}
}
