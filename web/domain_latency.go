package main

import (
	"context"
	"net"
	"sync"
	"time"
)

// 优选域名延迟探测（Domain Latency Probe）
//
// 由来：优选域名是 cfnat -fixed 的一等公民，拨号时按当前 DNS 活解析——设计上
// 「不解析成 IP 快照、不做域名级探测」，于是 UI 上只见域名、不见延迟，用户抱怨
// 「优选域名不知道延迟」。
//
// 本链补上这一针只读体检：对生效池里的每个域名做一次 TCP:443 拨号（DNS + 握手），
// 量出的耗时正对齐 cfnat 的「拨号耗时」。只量不改——绝不动池、绝不把域名解析成
// IP 快照，域名仍是活解析成员。关掉即停（PROXY_DOMAIN_PROBE_ENABLED）。

const (
	domainProbeDefaultIntervalSeconds = 300
	domainProbeMinIntervalSeconds     = 120
	domainProbeMaxIntervalSeconds     = 1800
	domainProbeDefaultTimeoutMs       = 3000
	domainProbeMinTimeoutMs           = 500
	domainProbeMaxTimeoutMs           = 10000
	domainProbeConcurrency            = 6
)

func domainProbeEnabled() bool { return envBool("PROXY_DOMAIN_PROBE_ENABLED", true) }

// domainProbeInterval 探测间隔，钳到 [120, 1800] 秒，越界回落 300。
func domainProbeInterval() time.Duration {
	seconds := envInt("PROXY_DOMAIN_PROBE_INTERVAL_SECONDS", domainProbeDefaultIntervalSeconds)
	if seconds < domainProbeMinIntervalSeconds || seconds > domainProbeMaxIntervalSeconds {
		seconds = domainProbeDefaultIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

// domainProbeTimeout 单次拨号超时，钳到 [500, 10000] 毫秒，越界回落 3000。
func domainProbeTimeout() time.Duration {
	ms := envInt("PROXY_DOMAIN_PROBE_TIMEOUT_MS", domainProbeDefaultTimeoutMs)
	if ms < domainProbeMinTimeoutMs || ms > domainProbeMaxTimeoutMs {
		ms = domainProbeDefaultTimeoutMs
	}
	return time.Duration(ms) * time.Millisecond
}

// domainLatency 单个优选域名的一次 TCP:443 拨号实况。
type domainLatency struct {
	Ms    int64     `json:"ms"`
	OK    bool      `json:"ok"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// probeActiveDomainLatencies 给生效池里的每个优选域名打一针 TCP:443，量出域名级
// 延迟（DNS + 握手）。只读体检，绝不动池。
func (a *app) probeActiveDomainLatencies(ctx context.Context) {
	domains := a.proxyActivePoolSnapshot().Domains
	if len(domains) == 0 {
		return
	}
	timeout := domainProbeTimeout()
	results := make(map[string]domainLatency, len(domains))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, domainProbeConcurrency)
	for _, domain := range domains {
		wg.Add(1)
		sem <- struct{}{}
		go func(domain string) {
			defer wg.Done()
			defer func() { <-sem }()
			dialer := net.Dialer{Timeout: timeout}
			started := time.Now()
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(domain, "443"))
			latency := time.Since(started).Milliseconds()
			rec := domainLatency{Ms: latency, OK: err == nil, At: time.Now()}
			if err != nil {
				rec.Error = err.Error()
			} else {
				_ = conn.Close()
			}
			mu.Lock()
			results[domain] = rec
			mu.Unlock()
		}(domain)
	}
	wg.Wait()
	a.domainLatencyMu.Lock()
	a.domainLatency = results
	a.domainLatencyLast = time.Now()
	a.domainLatencyMu.Unlock()
}

// runDomainLatencyProbeLoop 域名延迟探测主循环：启动后 30 秒首探，随后每 interval 一轮。
func (a *app) runDomainLatencyProbeLoop() {
	if !domainProbeEnabled() {
		return
	}
	initial := time.NewTimer(30 * time.Second)
	defer initial.Stop()
	<-initial.C
	for {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), domainProbeInterval())
			defer cancel()
			a.probeActiveDomainLatencies(ctx)
		}()
		timer := time.NewTimer(domainProbeInterval())
		<-timer.C
		timer.Stop()
	}
}

// domainLatencySnapshot 返回域名延迟快照的深拷贝。
func (a *app) domainLatencySnapshot() map[string]domainLatency {
	a.domainLatencyMu.Lock()
	defer a.domainLatencyMu.Unlock()
	out := make(map[string]domainLatency, len(a.domainLatency))
	for k, v := range a.domainLatency {
		out[k] = v
	}
	return out
}
