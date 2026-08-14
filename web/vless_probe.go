package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	passed := make([]string, 0, poolSize)
	attempts := 0
	for i := range results {
		if len(passed) >= poolSize || attempts >= cfg.MaxCandidates {
			break
		}
		if results[i].Error != "" {
			continue
		}
		attempts++
		ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
		started := time.Now()
		err := probeVLESSCandidate(ctx, results[i].IP, candidatePort, cfg, template)
		cancel()
		results[i].DataLatency = time.Since(started).Milliseconds()
		if err != nil {
			results[i].Stage = "VLESS_FAIL"
			results[i].Error = err.Error()
			continue
		}
		results[i].Stage = "VLESS_PASS"
		passed = append(passed, results[i].IP)
	}
	return passed
}

func probeVLESSCandidate(ctx context.Context, candidateIP string, candidatePort int, cfg vlessProbeConfig, template map[string]any) error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("分配本地探针端口失败: %w", err)
	}
	inboundPort := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	probeConfig, err := buildSingBoxProbeConfig(template, candidateIP, candidatePort, inboundPort)
	if err != nil {
		return fmt.Errorf("生成 sing-box 配置失败: %w", err)
	}
	data, err := json.Marshal(probeConfig)
	if err != nil {
		return fmt.Errorf("生成 sing-box 配置失败: %w", err)
	}
	tempFile, err := os.CreateTemp(filepath.Dir(cfg.TemplatePath), ".vless-probe-*.json")
	if err != nil {
		return fmt.Errorf("创建 sing-box 临时配置失败: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if err := tempFile.Chmod(0600); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("保护 sing-box 临时配置失败: %w", err)
	}
	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("写入 sing-box 临时配置失败: %w", err)
	}
	closeErr := tempFile.Close()
	if closeErr != nil {
		return fmt.Errorf("关闭 sing-box 临时配置失败: %w", closeErr)
	}

	processCtx, stopProcess := context.WithCancel(ctx)
	defer stopProcess()
	cmd := exec.CommandContext(processCtx, cfg.Binary, "run", "-c", tempPath)
	cmd.Dir = filepath.Dir(cfg.TemplatePath)
	output := &logBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 sing-box 失败: %w", err)
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
			break
		}
		select {
		case processErr := <-done:
			processFinished = true
			return formatSingBoxExit(processErr, output, template)
		case <-ctx.Done():
			return errors.New("VLESS 探针启动超时")
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
		return fmt.Errorf("创建 VLESS 测试请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "cloudflare-tools-vless-probe")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("VLESS 数据面请求失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != cfg.ExpectedStatus {
		return fmt.Errorf("VLESS 数据面状态码 %d，预期 %d", resp.StatusCode, cfg.ExpectedStatus)
	}
	return nil
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
