package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxVLESSProbeTemplateBytes = 1 << 20

type vlessProbeConfig struct {
	Enabled        bool
	Binary         string
	TemplatePath   string
	TestURL        string
	ExpectedStatus int
	Timeout        time.Duration
	MaxCandidates  int
	ReadLimit      int64
	MinBytes       int64
}

// defaultVLESSSpeedHost 是 VLESS 测速的默认下载主机（Cloudflare 官方测速端点）。
const defaultVLESSSpeedHost = "speed.cloudflare.com"

// vlessSpeedPartialFloor 测速时间预算耗尽时的最低诚实样本量：低于此值读到的
// 字节不足以判断速率（可能只是握手回包），仍按失败处理。
const vlessSpeedPartialFloor = 64 * 1024

// speedSamplerSlice 分片采样周期：片内字节计入 slices，供 EWMA 平滑。
const speedSamplerSlice = 250 * time.Millisecond

type vlessProbeMetrics struct {
	Bytes    int64
	Duration time.Duration
	Mbps     float64
	// Proven 表示本次探测有明确的数据面成功证据：隧道里收到了源站的
	// HTTP 响应（见 vlessResponseProvesDataPlane）。只有它能证明候选
	// 的数据面真的通过——DataLatency 只是整次尝试的耗时（含 15 秒超时），
	// ErrorClass 也只是失败归类，两者都不能当「连上过」的证明。
	// 降级准入（degradedFromWSResults）只认这一位。
	Proven bool
}

// vlessResponseProvesDataPlane 判断一次经 VLESS 隧道收到的 HTTP 响应
// 是否证明数据面真的通过。纯函数，便于单测钉死边界。
//
// 证明成立：
//   - 状态码等于预期状态：sing-box 不会凭空拼出调用方要的那个状态码；
//   - 其他非代理错误状态（如 200/403/404）：请求确实穿过隧道抵达了源站，
//     源站回了真实的 HTTP 状态——状态码不对是测速端点的问题，不是隧道的问题。
//
// 不算证明：
//   - 502/503/504：sing-box 的 HTTP 入站（mixed inbound）在出站拨号失败
//     时会就地合成 502 之类的错误响应，请求根本没进隧道。只凭这类状态码
//     就认「通了」，等于把探针坏了也当成候选好了。
//   - 无响应（statusCode==0）：client.Do 直接报错，没收到任何响应头。
func vlessResponseProvesDataPlane(statusCode, expectedStatus int) bool {
	if statusCode <= 0 {
		return false
	}
	if statusCode == expectedStatus {
		// 预期状态本身若就是代理错误码，同样不可信——配置里不该这么写，
		// 但防一手：sing-box 合成的 502 正好撞上预期 502 时不能算数。
		return expectedStatus != http.StatusBadGateway &&
			expectedStatus != http.StatusServiceUnavailable &&
			expectedStatus != http.StatusGatewayTimeout
	}
	switch statusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return false
	}
	return true
}

// speedSampler 分片采样读取器：边读边按固定时间片记字节账。
// 意义：CDN 突发尖峰会被 EWMA 衰减，持续速率主导排序（CFST 同源思想）。
type speedSampler struct {
	r        io.Reader
	slices   []int64
	deadline time.Time
	current  int64
}

func newSpeedSampler(r io.Reader) *speedSampler {
	return &speedSampler{r: r, deadline: time.Now().Add(speedSamplerSlice)}
}

func (s *speedSampler) copyTo(dst io.Writer) (int64, error) {
	buf := make([]byte, 16*1024)
	var total int64
	for {
		n, err := s.r.Read(buf)
		if n > 0 {
			written, werr := dst.Write(buf[:n])
			total += int64(written)
			s.current += int64(written)
			if werr != nil {
				return total, werr
			}
		}
		if err != nil {
			if s.current > 0 {
				s.slices = append(s.slices, s.current)
			}
			// EOF 是读尽而非读败——io.Copy 同义：正常收尾不报错。
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
		if now := time.Now(); now.After(s.deadline) {
			s.slices = append(s.slices, s.current)
			s.current = 0
			s.deadline = now.Add(speedSamplerSlice)
		}
	}
}

// ewmaMbpsFromSlices 对每时间片字节数做指数加权移动平均（α=0.3）：
// 瞁发尖峰按 (1-α)^k 衰减，持续速率主导结果。片数 < 4 时返回 0，
// 由调用方回退到「总量/总时长」的朴素均值。
func ewmaMbpsFromSlices(slices []int64, sliceDur time.Duration) float64 {
	if len(slices) < 4 || sliceDur <= 0 {
		return 0
	}
	const alpha = 0.3
	var ew float64
	for _, b := range slices {
		ew = alpha*float64(b) + (1-alpha)*ew
	}
	return ew * 8 / sliceDur.Seconds() / 1_000_000
}

// vlessSpeedTestURL 决定 VLESS 数据面测速的下载端点。
//
// 默认 speed.cloudflare.com：从生产容器直连时可达（实测约 5.5 Mbps），
// 但经 sing-box 隧道（VLESS→WS→TLS→目标）常在 20s 预算内跑不完而超时。
// 故支持 PROXY_VLESS_SPEED_URL 覆盖；其中 {bytes} 会被替换为实际请求字节数。
func vlessSpeedTestURL(size int) string {
	candidate := strings.TrimSpace(env("PROXY_VLESS_SPEED_URL", ""))
	if candidate == "" {
		candidate = "https://" + defaultVLESSSpeedHost + "/__down?bytes={bytes}"
	}
	if !strings.Contains(candidate, "{bytes}") {
		return candidate
	}
	return strings.ReplaceAll(candidate, "{bytes}", strconv.Itoa(size))
}

func defaultVLESSProbeConfig() vlessProbeConfig {
	timeoutSeconds := envInt("PROXY_VLESS_TIMEOUT", 15)
	if timeoutSeconds < 3 || timeoutSeconds > 60 {
		timeoutSeconds = 15
	}
	return vlessProbeConfig{
		Enabled:        envBool("PROXY_VLESS_PROBE", false),
		Binary:         env("SING_BOX_BIN", "/usr/local/bin/sing-box"),
		TemplatePath:   env("PROXY_VLESS_TEMPLATE", filepath.Join(env("DATA_DIR", defaultDataDir), "vless-probe-outbound.json")),
		TestURL:        env("PROXY_VLESS_TEST_URL", "https://www.gstatic.com/generate_204"),
		ExpectedStatus: envInt("PROXY_VLESS_EXPECT_STATUS", http.StatusNoContent),
		Timeout:        time.Duration(timeoutSeconds) * time.Second,
		MaxCandidates:  envInt("PROXY_VLESS_MAX_CANDIDATES", 20),
		ReadLimit:      4096,
	}
}

func (c vlessProbeConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Binary) == "" || strings.TrimSpace(c.TemplatePath) == "" {
		return errors.New("sing-box 路径和 VLESS 模板不能为空")
	}
	testURL, err := url.Parse(c.TestURL)
	if err != nil || testURL.Hostname() == "" || (testURL.Scheme != "http" && testURL.Scheme != "https") {
		return errors.New("VLESS 测试 URL 必须是有效的 HTTP/HTTPS 地址")
	}
	if c.ExpectedStatus < 100 || c.ExpectedStatus > 599 {
		return errors.New("VLESS 预期状态码必须在 100-599")
	}
	if c.Timeout < 3*time.Second || c.Timeout > 60*time.Second {
		return errors.New("VLESS 超时必须在 3-60 秒")
	}
	if c.MaxCandidates < 1 || c.MaxCandidates > 100 {
		return errors.New("VLESS 单轮候选数必须在 1-100")
	}
	return nil
}

func loadVLESSOutboundTemplate(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 VLESS 模板失败: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxVLESSProbeTemplateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 VLESS 模板失败: %w", err)
	}
	if len(data) > maxVLESSProbeTemplateBytes {
		return nil, errors.New("VLESS 模板超过 1 MiB")
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("解析 VLESS 模板失败: %w", err)
	}
	outbound := root
	if rawOutbounds, ok := root["outbounds"].([]any); ok {
		outbound = nil
		for _, raw := range rawOutbounds {
			candidate, ok := raw.(map[string]any)
			if ok && stringValue(candidate["type"]) == "vless" {
				outbound = candidate
				break
			}
		}
		if outbound == nil {
			return nil, errors.New("模板中没有 VLESS outbound")
		}
	}
	if err := validateVLESSOutbound(outbound); err != nil {
		return nil, err
	}
	return cloneJSONMap(outbound)
}

func validateVLESSOutbound(outbound map[string]any) error {
	if stringValue(outbound["type"]) != "vless" {
		return errors.New("模板必须是 VLESS outbound 或包含 VLESS outbound 的 sing-box 配置")
	}
	if !looksLikeUUID(stringValue(outbound["uuid"])) {
		return errors.New("VLESS UUID 格式无效")
	}
	tlsConfig, ok := outbound["tls"].(map[string]any)
	if !ok || !boolValue(tlsConfig["enabled"]) || strings.TrimSpace(stringValue(tlsConfig["server_name"])) == "" {
		return errors.New("VLESS 模板必须启用 TLS 并提供 server_name")
	}
	transport, ok := outbound["transport"].(map[string]any)
	if !ok || stringValue(transport["type"]) != "ws" {
		return errors.New("VLESS 模板 transport 必须是 WebSocket")
	}
	path := stringValue(transport["path"])
	if !strings.HasPrefix(path, "/") {
		return errors.New("VLESS WebSocket path 必须以 / 开头")
	}
	headers, ok := transport["headers"].(map[string]any)
	if !ok || strings.TrimSpace(stringValue(headers["Host"])) == "" {
		return errors.New("VLESS WebSocket headers.Host 不能为空")
	}
	return nil
}

func cloneJSONMap(source map[string]any) (map[string]any, error) {
	data, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	var cloned map[string]any
	if err := json.Unmarshal(data, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func buildSingBoxProbeConfig(template map[string]any, candidateIP string, candidatePort, inboundPort int) (map[string]any, error) {
	outbound, err := cloneJSONMap(template)
	if err != nil {
		return nil, err
	}
	outbound["tag"] = "vless-probe"
	outbound["server"] = candidateIP
	outbound["server_port"] = candidatePort
	return map[string]any{
		"log": map[string]any{
			"level":     "error",
			"timestamp": true,
		},
		"inbounds": []any{map[string]any{
			"type":        "mixed",
			"tag":         "probe-in",
			"listen":      "127.0.0.1",
			"listen_port": inboundPort,
		}},
		"outbounds": []any{outbound},
		"route": map[string]any{
			"final": "vless-probe",
		},
	}, nil
}

func probeVLESSPool(parent context.Context, results []proxyScanResult, candidatePort, poolSize int, cfg vlessProbeConfig, template map[string]any) []string {
	return probeVLESSPoolWithProbe(parent, results, candidatePort, poolSize, cfg, template, probeVLESSCandidate)
}

// probeVLESSPoolWithProbe 与 probeVLESSPool 相同，但探针函数可注入，
// 便于单测钉死「数据面证据如何进结果、降级如何认」而不必真的起 sing-box。
// isLocalProbeError 识别「探针自己坏了」而不是「候选坏了」。
//
// 本地故障包括：端口分配/配置生成/临时文件/sing-box 启动失败、
// 绑定冲突 3 次重试耗尽、sing-box 迟迟起不来（启动超时）、
// sing-box 中途死亡后本地代理拨号被拒（错误里出现 127.0.0.1——
// 探针的 HTTP 入口永远只监听 127.0.0.1，对端的拒绝不会长这样）。
//
// 这类失败记到候选头上是误判：并发抢端口或 sing-box 崩一次，整批候选
// 各背一次失败，3 轮即进 lineFailed；第一轮只修了「重试成功不记账」，
// 耗尽分支仍归因。注意裸 "connection refused" 不算本地：只有带
// 127.0.0.1 的才是拨本地入口被拒，对端的拒绝由 sing-box 在隧道内
// 合成 502/503/504 响应，不会以 dial 错误出现。
func isLocalProbeError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "分配本地探针端口失败"),
		strings.Contains(msg, "生成 sing-box 配置失败"),
		strings.Contains(msg, "创建 sing-box 临时配置失败"),
		strings.Contains(msg, "写入 sing-box 临时配置失败"),
		strings.Contains(msg, "关闭 sing-box 临时配置失败"),
		strings.Contains(msg, "保护 sing-box 临时配置失败"),
		strings.Contains(msg, "启动 sing-box 失败"),
		strings.Contains(msg, "sing-box 提前退出"),
		strings.Contains(msg, "vless 探针启动超时"),
		strings.Contains(msg, "address already in use"),
		strings.Contains(msg, "127.0.0.1"):
		return true
	}
	return false
}

// vlessSpeedTimeoutSeconds 是 VLESS 测速单条超时（秒），与
// rankVLESSPassesBySpeed 共用同一钳制，避免两处各写各的范围。
func vlessSpeedTimeoutSeconds() int {
	timeout := envInt("PROXY_VLESS_SPEED_TIMEOUT", 20)
	if timeout < 5 || timeout > 120 {
		timeout = 20
	}
	return timeout
}

// vlessReviewBudget 按可配工作量算 VLESS 终审预算：探针（单轮候选数×
// 单条超时）+ 测速（最多 6 个×测速超时）+ 60 秒余量，下限 10 分钟、
// 上限 30 分钟。此前终审固定 10 分钟，但工作量可配：batch 50×15 秒
// =750 秒、Timeout 60 秒×12=720 秒、漏斗 MaxCandidates 60×15 秒
// =900 秒，默认值（20×15+6×20=420 秒）刚好够用，属「默认够、允许值
// 不够」——尾部候选没验却被记失败进质量账。
func vlessReviewBudget(cfg vlessProbeConfig) time.Duration {
	maxCandidates := cfg.MaxCandidates
	if maxCandidates < 1 {
		maxCandidates = 1
	}
	if maxCandidates > 100 {
		maxCandidates = 100
	}
	timeout := cfg.Timeout
	if timeout < 3*time.Second {
		timeout = 3 * time.Second
	}
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	need := time.Duration(maxCandidates)*timeout +
		time.Duration(maxVLESSSpeedProbes)*time.Duration(vlessSpeedTimeoutSeconds())*time.Second +
		time.Minute
	if need < 10*time.Minute {
		need = 10 * time.Minute
	}
	if need > 30*time.Minute {
		need = 30 * time.Minute
	}
	return need
}

func probeVLESSPoolWithProbe(parent context.Context, results []proxyScanResult, candidatePort, poolSize int, cfg vlessProbeConfig, template map[string]any, probe func(context.Context, string, int, vlessProbeConfig, map[string]any) (vlessProbeMetrics, error)) []string {
	// 入口钳制：validate() 只在漏斗路径调用，踢人补位与调度器不调；
	// MaxCandidates≤0 时 attempts>=0 首轮即成立、零次尝试，补位/调度
	// 静默无替补；Timeout≤0 时每条探针 ctx 立即过期、全部记失败。
	// 订阅段的超时/预算另在 subscription.go 里钳制。
	maxCandidates := cfg.MaxCandidates
	if maxCandidates < 1 {
		maxCandidates = 1
	}
	if maxCandidates > 100 {
		maxCandidates = 100
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	order := vlessProbeOrder(results)
	attempted := make(map[int]struct{}, len(order))
	attempts := 0
	for _, i := range order {
		if attempts >= maxCandidates {
			break
		}
		if parent.Err() != nil {
			// 父预算已到期：这一条及之后都没验，不得记失败。
			results[i].Stage = "skipped"
			results[i].Error = "VLESS 终审预算耗尽，未开始探测"
			results[i].ErrorClass = failureTimeout
			continue
		}
		attempts++
		attempted[i] = struct{}{}
		ctx, cancel := context.WithTimeout(parent, timeout)
		started := time.Now()
		metrics, err := probe(ctx, results[i].IP, candidatePort, cfg, template)
		cancel()
		results[i].DataLatency = time.Since(started).Milliseconds()
		// 数据面证据随 metrics 回来：失败时也要记——降级准入只认它。
		// 此前 probeVLESSCandidate 只返回 error，metrics 直接丢掉，
		// 失败候选有没有真的通根本无从知道，降级只能靠「非超时」瞎猜。
		results[i].DataProven = metrics.Proven
		if err != nil {
			if parent.Err() != nil {
				// 本条正在跑时父预算到期：不是候选死了，是预算没了。
				results[i].Stage = "skipped"
				results[i].Error = "VLESS 终审预算耗尽，探测被截断"
				results[i].ErrorClass = failureTimeout
				continue
			}
			if isLocalProbeError(err) {
				// 探针自己坏了（端口冲突耗尽、sing-box 提前退出等）：
				// 不是候选的问题，记 skipped，不进质量账、不进降级。
				// 降级准入已只认 DataProven（本轮 P1），这里再把账也摘掉，
				// 否则整批候选各背一次连败。
				results[i].Stage = "skipped"
				results[i].Error = err.Error()
				results[i].ErrorClass = failureLocal
				continue
			}
			results[i].Stage = "VLESS_FAIL"
			results[i].Error = err.Error()
			// 失败必须归类：降级逻辑（degradedFromWSResults）靠 ErrorClass
			// 区分「数据面超时」与「快速失败」；此前这里从不写 ErrorClass，
			// VLESS_FAIL 的 errorClass 一律为空，降级无从判断。
			results[i].ErrorClass = classifyProbeError(err)
			continue
		}
		results[i].Stage = "VLESS_PASS"
	}
	// 名额已满没轮上的也不是失败：本轮只验 N 个，剩下的根本没测过，
	// 不得进质量账（调度器 observe 会跳过 skipped）。
	for _, i := range order {
		if _, ok := attempted[i]; !ok && results[i].Stage == "WS_PASS" {
			results[i].Stage = "skipped"
			results[i].Error = "本轮 VLESS 终审名额已满，未开始探测"
			results[i].ErrorClass = failureTimeout
		}
	}
	return fastestVLESSPasses(results, poolSize)
}

func vlessProbeOrder(results []proxyScanResult) []int {
	order := make([]int, 0, len(results))
	for i := range results {
		if results[i].Error == "" {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if results[order[i]].SourceRank != results[order[j]].SourceRank {
			return results[order[i]].SourceRank < results[order[j]].SourceRank
		}
		return results[order[i]].Latency < results[order[j]].Latency
	})
	return order
}

func fastestVLESSPasses(results []proxyScanResult, poolSize int) []string {
	passed := make([]int, 0, poolSize)
	for i := range results {
		if results[i].Stage == "VLESS_PASS" {
			passed = append(passed, i)
		}
	}
	sort.SliceStable(passed, func(i, j int) bool {
		if results[passed[i]].SourceRank != results[passed[j]].SourceRank {
			return results[passed[i]].SourceRank < results[passed[j]].SourceRank
		}
		if results[passed[i]].DataLatency == results[passed[j]].DataLatency {
			return results[passed[i]].Latency < results[passed[j]].Latency
		}
		return results[passed[i]].DataLatency < results[passed[j]].DataLatency
	})
	if len(passed) > poolSize {
		passed = passed[:poolSize]
	}
	result := make([]string, len(passed))
	for i, index := range passed {
		result[i] = results[index].IP
	}
	return result
}

func probeVLESSCandidate(ctx context.Context, candidateIP string, candidatePort int, cfg vlessProbeConfig, template map[string]any) (vlessProbeMetrics, error) {
	return probeVLESSCandidateMetrics(ctx, candidateIP, candidatePort, cfg, template)
}

// maxVLESSProbePortAttempts 探针端口被抢占时换端口重试的次数上限。
const maxVLESSProbePortAttempts = 3

// allocProbePort 向内核要一个空闲本地端口。注意拿到的瞬间就已释放，
// 调用方必须尽快让 sing-box 绑上去，窗口期内别的进程可能抢走——
// 因此 probeVLESSCandidateMetrics 在绑定冲突时换端口重试。
func allocProbePort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port, nil
}

// isBindConflictError 识别「端口被抢占」导致的 sing-box 启动失败：
// 进程退出前输出里带有 bind address already in use。其它启动失败
// （模板错、二进制缺失）重试换端口也没用，直接返回。
func isBindConflictError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

// probeVLESSCandidateMetrics 起 sing-box 探针测一次 VLESS 数据面。
// 端口分配与绑定之间存在 TOCTOU 窗口（先 Listen 取端口、Close 释放，
// 再起 sing-box 去绑）：窗口期内端口被别的进程或并发探针抢走时，
// sing-box 绑失败，本次失败会被记到被测 IP 头上。绑定冲突时换一个
// 新分配的端口重试，最多 3 次。
func probeVLESSCandidateMetrics(ctx context.Context, candidateIP string, candidatePort int, cfg vlessProbeConfig, template map[string]any) (vlessProbeMetrics, error) {
	metrics := vlessProbeMetrics{}
	var lastErr error
	for attempt := 0; attempt < maxVLESSProbePortAttempts; attempt++ {
		if ctx.Err() != nil {
			return metrics, ctx.Err()
		}
		inboundPort, err := allocProbePort()
		if err != nil {
			return metrics, fmt.Errorf("分配本地探针端口失败: %w", err)
		}
		metrics, lastErr = probeVLESSCandidateMetricsOnce(ctx, candidateIP, candidatePort, cfg, template, inboundPort)
		if lastErr == nil || !isBindConflictError(lastErr) {
			return metrics, lastErr
		}
		log.Printf("VLESS probe port %d taken, retrying with a fresh port (attempt %d/%d)", inboundPort, attempt+1, maxVLESSProbePortAttempts)
	}
	return metrics, lastErr
}

func probeVLESSCandidateMetricsOnce(ctx context.Context, candidateIP string, candidatePort int, cfg vlessProbeConfig, template map[string]any, inboundPort int) (vlessProbeMetrics, error) {
	metrics := vlessProbeMetrics{}
	probeConfig, err := buildSingBoxProbeConfig(template, candidateIP, candidatePort, inboundPort)
	if err != nil {
		return metrics, fmt.Errorf("生成 sing-box 配置失败: %w", err)
	}
	data, err := json.Marshal(probeConfig)
	if err != nil {
		return metrics, fmt.Errorf("生成 sing-box 配置失败: %w", err)
	}
	tempFile, err := os.CreateTemp(filepath.Dir(cfg.TemplatePath), ".vless-probe-*.json")
	if err != nil {
		return metrics, fmt.Errorf("创建 sing-box 临时配置失败: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if err := tempFile.Chmod(0600); err != nil {
		_ = tempFile.Close()
		return metrics, fmt.Errorf("保护 sing-box 临时配置失败: %w", err)
	}
	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return metrics, fmt.Errorf("写入 sing-box 临时配置失败: %w", err)
	}
	closeErr := tempFile.Close()
	if closeErr != nil {
		return metrics, fmt.Errorf("关闭 sing-box 临时配置失败: %w", closeErr)
	}

	processCtx, stopProcess := context.WithCancel(ctx)
	defer stopProcess()
	cmd := exec.CommandContext(processCtx, cfg.Binary, "run", "-c", tempPath)
	cmd.Dir = filepath.Dir(cfg.TemplatePath)
	output := &logBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return metrics, fmt.Errorf("启动 sing-box 失败: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	processFinished := false
	defer func() {
		stopProcess()
		if processFinished {
			return
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(inboundPort))
	for {
		conn, dialErr := net.DialTimeout("tcp4", address, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			// 拨通只说明「这个端口有人监听」，不说明是自己的 sing-box：
			// 并发探针撞端口时，输家的 sing-box 绑定失败已退出，但拨号
			// 恰好打通赢家的监听端口，直接 break 就会借赢家的隧道测——
			// 输家的候选可能借赢家的隧道拿到通过，或背上赢家的错误。
			// 拨通后先非阻塞确认本进程还没退出，退出了按绑定冲突
			// 返回（调用方换端口重试）。
			select {
			case processErr := <-done:
				processFinished = true
				return metrics, formatSingBoxExit(processErr, output, template)
			default:
			}
			break
		}
		select {
		case processErr := <-done:
			processFinished = true
			return metrics, formatSingBoxExit(processErr, output, template)
		case <-ctx.Done():
			return metrics, errors.New("VLESS 探针启动超时")
		case <-time.After(50 * time.Millisecond):
		}
	}

	proxyURL, _ := url.Parse("http://" + address)
	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.TestURL, nil)
	if err != nil {
		return metrics, fmt.Errorf("创建 VLESS 测试请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "cloudflare-tools-vless-probe")
	resp, err := client.Do(req)
	if err != nil {
		return metrics, fmt.Errorf("VLESS 数据面请求失败: %w", err)
	}
	defer resp.Body.Close()
	// 响应头到了，就按 vlessResponseProvesDataPlane 判定数据面证据：
	// 状态码不对（403 之类）也算——请求穿过隧道抵达了源站；只有
	// sing-box 就地合成的 502/503/504 不算。此后所有失败返回
	// （状态码不对、读不满、读一半断开）都带着这一位，降级准入认它。
	metrics.Proven = vlessResponseProvesDataPlane(resp.StatusCode, cfg.ExpectedStatus)
	// 状态码先验：403/404 的错误体不必读满预算才判失败，且读超时会
	// 先于状态码返回、把报错原因写错。
	if resp.StatusCode != cfg.ExpectedStatus {
		return metrics, fmt.Errorf("VLESS 数据面状态码 %d，预期 %d", resp.StatusCode, cfg.ExpectedStatus)
	}
	started := time.Now()
	sampler := newSpeedSampler(io.LimitReader(resp.Body, cfg.ReadLimit))
	n, readErr := sampler.copyTo(io.Discard)
	metrics.Bytes = n
	metrics.Duration = time.Since(started)
	if metrics.Duration <= 0 {
		metrics.Duration = time.Millisecond
	}
	if ewma := ewmaMbpsFromSlices(sampler.slices, speedSamplerSlice); ewma > 0 {
		metrics.Mbps = ewma
	} else {
		metrics.Mbps = float64(n*8) / metrics.Duration.Seconds() / 1_000_000
	}
	if readErr != nil {
		// 时间预算耗尽而样本已足：这是「慢」不是「坏」——按实读字节给
		// 诚实低速分。旧实现一票归零，800KB/20s 的慢 IP 与死 IP 同等待遇，
		// 吞吐轨因此对慢线全盲（生产实测痛点）。
		if ctx.Err() != nil && n >= vlessSpeedPartialFloor {
			return metrics, nil
		}
		return metrics, fmt.Errorf("读取 VLESS 测试响应失败: %w", readErr)
	}
	if metrics.Bytes < cfg.MinBytes {
		return metrics, fmt.Errorf("VLESS 下载数据不足: %d/%d bytes", metrics.Bytes, cfg.MinBytes)
	}
	return metrics, nil
}

func formatSingBoxExit(processErr error, output *logBuffer, template map[string]any) error {
	detail := strings.Join(output.lines(4), " ")
	detail = sanitizeProbeMessage(detail, template)
	if len(detail) > 400 {
		detail = detail[:400]
	}
	if detail != "" {
		return fmt.Errorf("sing-box 提前退出: %s", detail)
	}
	if processErr != nil {
		return fmt.Errorf("sing-box 提前退出: %v", processErr)
	}
	return errors.New("sing-box 提前退出")
}

func sanitizeProbeMessage(message string, template map[string]any) string {
	secrets := []string{stringValue(template["uuid"]), stringValue(template["server"])}
	if tlsConfig, ok := template["tls"].(map[string]any); ok {
		secrets = append(secrets, stringValue(tlsConfig["server_name"]))
	}
	if transport, ok := template["transport"].(map[string]any); ok {
		secrets = append(secrets, stringValue(transport["path"]))
		if headers, ok := transport["headers"].(map[string]any); ok {
			secrets = append(secrets, stringValue(headers["Host"]))
		}
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return strings.TrimSpace(message)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func boolValue(value any) bool {
	flag, _ := value.(bool)
	return flag
}

func looksLikeUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}
