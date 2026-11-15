package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// customSpeedURLUnhealthy 记录「显式配置的测速端点」是否在启动预检中失败。
//
// 预检只做一次直连 GET，不经过 VLESS 隧道，因此它证明的是「端点本身可达」，
// 不是「隧道到端点可达」。失败时回退到默认 OVH 定长文件，避免整轮测速
// 因为一个写错的 URL 全部记零；成功时保持用户配置不变。
var customSpeedURLUnhealthy atomic.Bool

// preflightSpeedEndpoint 用 Range 请求拉一小段，确认端点返回 2xx/206 且有字节。
//
// 不用 HEAD：部分镜像对 HEAD 返回 200 但 GET 才 403；也不读满整个文件，
// 4KB 足够区分「能下」和「返回空/错误页」。
func preflightSpeedEndpoint(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("测速端点为空的")
	}
	target := strings.ReplaceAll(raw, "{bytes}", "65536")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-4095")
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			DisableCompression:  true,
			ForceAttemptHTTP2:   false,
			TLSHandshakeTimeout: 4 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	n, err := io.CopyN(io.Discard, resp.Body, 4096)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if n == 0 {
		return errors.New("响应没有字节")
	}
	return nil
}

// preflightSpeedSources 在启动时检查显式配置的测速端点。
//
// 只检查用户显式设置的 PROXY_VLESS_SPEED_URL；默认 OVH 端点不预检——
// 默认值失败时没有更可靠的备选，日志里提示即可，不能把默认值也标坏。
func preflightSpeedSources(ctx context.Context) {
	custom := strings.TrimSpace(env("PROXY_VLESS_SPEED_URL", ""))
	if custom == "" {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := preflightSpeedEndpoint(checkCtx, custom); err != nil {
		customSpeedURLUnhealthy.Store(true)
		log.Printf("speed preflight: 自定义测速端点不可用（%v），回退到 %s", err, disabledBenchSpeedURLDefault)
		return
	}
	customSpeedURLUnhealthy.Store(false)
	log.Printf("speed preflight: 自定义测速端点可用")
}
