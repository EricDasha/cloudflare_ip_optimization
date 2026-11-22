package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// subDefaultLimit 是订阅默认发几个节点。
//
// 不发全部：客户端的 URL 测速是逐个连的，节点越多排序越慢，而排在前面的
// 本来就会被优先选中。四个足够留出「一个抽风还有备胎」的余量。
const subDefaultLimit = 4

// subMaxLimit 是 ?n= 能要到的上限，防止有人一口气把整池拉下来。
const subMaxLimit = 10

// handleSubscription 输出一份客户端可以直接订阅的节点清单（base64 的 vless 链接）。
//
// 为什么需要它：池子里的 IP 是会变的——2026-11-22 实测同一个 172.64.145.33
// 二十分钟内从 3.4MB/s 掉到 40KB/s，而写死在客户端的节点不会自己变。结果是
// 用户每次都要自己发现「又卡了」再喊人。让客户端定时来拉，换 IP 就变成后台的事。
//
// 只发 IP，不发域名，也不发段外地址：
//   - 域名型成员实测 p50 254ms，而且流量先落到别人的中转盒子上（正是要绕开的那类）；
//   - 段外地址是兜底用的，握手可能很快但吞吐可能只有 200KB/s（同一天实测）。
//
// 顺序沿用池子里的排位（优先级成员在前、官方段在前），客户端按这个顺序测速即可。
func (a *app) handleSubscription(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimSpace(os.Getenv("CF_SUB_UUID"))
	if uuid == "" {
		// 不把 UUID 写进仓库：这是凭据，属于 .env 的范畴。
		http.Error(w, "订阅未配置：请在容器环境变量里设置 CF_SUB_UUID", http.StatusServiceUnavailable)
		return
	}
	if want := strings.TrimSpace(os.Getenv("CF_SUB_TOKEN")); want != "" {
		if r.URL.Query().Get("token") != want {
			http.Error(w, "token 不正确", http.StatusForbidden)
			return
		}
	}

	pool := a.proxyActivePoolSnapshot()
	host := strings.TrimSpace(pool.Host)
	if host == "" {
		http.Error(w, "当前没有生效中的池子", http.StatusServiceUnavailable)
		return
	}
	limit := subDefaultLimit
	if raw := r.URL.Query().Get("n"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= subMaxLimit {
			limit = n
		}
	}

	disabled := make(map[string]struct{}, len(pool.Disabled))
	for _, v := range pool.Disabled {
		disabled[v] = struct{}{}
	}

	links := make([]string, 0, limit)
	// 路径对所有成员是同一个：池子给的是 cfnat 用的，缺 proxyip 就补上。
	path := pool.Path
	if override := strings.TrimSpace(os.Getenv("CF_SUB_PATH")); override != "" {
		path = override
	}
	path = withProxyIP(path, os.Getenv("CF_SUB_PROXYIP"))
	for _, raw := range pool.IPs {
		if len(links) >= limit {
			break
		}
		ip := net.ParseIP(raw)
		// 段外地址不发给客户端：它们不是真正的 CF 边缘，吞吐没有保证。
		if ip == nil || !isCloudflareIPv4(ip) {
			continue
		}
		if _, off := disabled[raw]; off {
			continue
		}
		links = append(links, buildVLESSLink(uuid, raw, host, path))
	}
	if len(links) == 0 {
		http.Error(w, "池子里没有可直接下发的官方段 IP", http.StatusServiceUnavailable)
		return
	}

	body := base64.StdEncoding.EncodeToString([]byte(strings.Join(links, "\n") + "\n"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// 客户端定期来拉，别让中间层缓存住旧池子。
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, body)
}

// withProxyIP 给路径补上 proxyip 段。
//
// 池子里的 Path 是 cfnat 转发用的（形如 /thread?ed=2560），里面通常没有
// proxyip=。但客户端直连边缘 IP 时，非 CF 的站点（openai 之类）必须靠 Worker
// 里的 proxyIP 出口才能出去——缺了它，浏览器一半站点打不开。已经带了的就不动。
func withProxyIP(path, proxyip string) string {
	proxyip = strings.TrimSpace(proxyip)
	if proxyip == "" || strings.Contains(path, "proxyip=") {
		return path
	}
	base, query, hasQuery := strings.Cut(path, "?")
	if base == "" {
		base = "/"
	}
	base = strings.TrimSuffix(base, "/") + "/proxyip=" + proxyip
	if hasQuery {
		return base + "?" + query
	}
	return base
}

// buildVLESSLink 拼一条 edgetunnel 认的 vless 链接。
//
// 参数与 Worker 的约定必须逐字对齐：path 里的 proxyip= 是给非 CF 目标用的
// 出口，security=tls 配 sni=Host，传输是 ws。
func buildVLESSLink(uuid, ip, host, path string) string {
	if path == "" {
		path = "/"
	}
	q := url.Values{}
	q.Set("encryption", "none")
	q.Set("security", "tls")
	q.Set("sni", host)
	q.Set("fp", "chrome")
	q.Set("type", "ws")
	q.Set("host", host)
	q.Set("path", path)
	q.Set("packetEncoding", "xudp")
	return fmt.Sprintf("vless://%s@%s:443?%s#CF-%s", uuid, ip, q.Encode(), ip)
}
