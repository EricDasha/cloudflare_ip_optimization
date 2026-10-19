package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func okSample(mbps float64, tcpMs int64) domainProbeSample {
	return domainProbeSample{At: time.Now(), OK: true, Stage: "speed", TCPMs: tcpMs, Mbps: mbps, Bytes: 512 * 1024}
}

func failSample(stage string) domainProbeSample {
	return domainProbeSample{At: time.Now(), OK: false, Stage: stage, Error: "boom"}
}

func TestDomainQualityRecalculateTesting(t *testing.T) {
	q := &domainQuality{Domain: "a.example.com", Samples: []domainProbeSample{okSample(10, 50)}}
	q.recalculate()
	if q.State != domainStateTesting {
		t.Fatalf("state = %s, want %s", q.State, domainStateTesting)
	}
}

func TestDomainQualityRecalculateHealthy(t *testing.T) {
	q := &domainQuality{Domain: "a.example.com", Samples: []domainProbeSample{
		okSample(10, 50), okSample(11, 52), okSample(10.5, 49), okSample(10.8, 51),
	}}
	q.recalculate()
	if q.State != domainStateHealthy {
		t.Fatalf("state = %s (success=%.2f jitter=%.2f), want healthy", q.State, q.SuccessRate, q.JitterRatio)
	}
	if q.SuccessRate != 1 {
		t.Fatalf("successRate = %v, want 1", q.SuccessRate)
	}
	if q.P50Mbps <= 0 || q.P95Mbps <= 0 {
		t.Fatalf("percentiles not computed: p50=%v p95=%v", q.P50Mbps, q.P95Mbps)
	}
}

func TestDomainQualityRecalculateDead(t *testing.T) {
	// 连续失败计数由 recordDomainProbeResults 维护，recalculate 只按它判定。
	q := &domainQuality{Domain: "dead.example.com", ConsecutiveFailures: 3, Samples: []domainProbeSample{
		okSample(10, 50), failSample("tcp"), failSample("tcp"), failSample("speed"),
	}}
	q.recalculate()
	if q.State != domainStateDead {
		t.Fatalf("state = %s (failures=%d), want dead", q.State, q.ConsecutiveFailures)
	}
	// 失败未达阈值时不能误杀。
	q.ConsecutiveFailures = 2
	q.recalculate()
	if q.State == domainStateDead {
		t.Fatalf("state = dead with failures=2, want not dead")
	}
}

func TestDomainQualityRecalculateUnstableByJitter(t *testing.T) {
	// P50=10, P95=19 → 抖动 1.9 > 1.8，虽全部成功仍判不稳。
	// 需要足够样本让 P95 索引落到尾部：10 个 10 + 2 个 19（共 12 个）时
	// P50=speeds[6]=10、P95=speeds[10]=19。
	samples := make([]domainProbeSample, 0, domainQualitySampleCap)
	for i := 0; i < 10; i++ {
		samples = append(samples, okSample(10, 50))
	}
	samples = append(samples, okSample(19, 50), okSample(19, 50))
	q := &domainQuality{Domain: "jitter.example.com", Samples: samples}
	q.recalculate()
	if q.State != domainStateUnstable {
		t.Fatalf("state = %s (p50=%.2f p95=%.2f jitter=%.2f), want unstable", q.State, q.P50Mbps, q.P95Mbps, q.JitterRatio)
	}
}

func TestDomainQualityRecalculateUnstableBySuccessRate(t *testing.T) {
	q := &domainQuality{Domain: "flaky.example.com", Samples: []domainProbeSample{
		okSample(10, 50), failSample("speed"), failSample("speed"), failSample("tcp"),
	}}
	q.ConsecutiveFailures = 1
	q.recalculate()
	if q.State != domainStateUnstable {
		t.Fatalf("state = %s (success=%.2f), want unstable", q.State, q.SuccessRate)
	}
	if q.SuccessRate != 0.25 {
		t.Fatalf("successRate = %v, want 0.25", q.SuccessRate)
	}
}

func TestDomainQualityLastSampleAndTCPMs(t *testing.T) {
	q := &domainQuality{Domain: "a.example.com", Samples: []domainProbeSample{
		okSample(10, 60), failSample("tcp"),
	}}
	if last, ok := q.lastSample(); !ok || last.OK {
		t.Fatalf("lastSample = %+v, want failed last sample", last)
	}
	if got := q.lastTCPMs(); got != 60 {
		t.Fatalf("lastTCPMs = %d, want 60 (most recent non-zero)", got)
	}
	if _, ok := (&domainQuality{}).lastSample(); ok {
		t.Fatal("empty quality should report no last sample")
	}
}

// ---------------------------------------------------------------------------
// 探针事故保险：整轮无人测出速度时不得累加连败
// ---------------------------------------------------------------------------

func newProbeTestApp(t *testing.T) *app {
	t.Helper()
	return &app{
		dataDir:       t.TempDir(),
		domainQuality: make(map[string]*domainQuality),
		domainLatency: make(map[string]domainLatency),
	}
}

func TestRecordDomainProbeSuppressesDeadWhenProbeBroken(t *testing.T) {
	a := newProbeTestApp(t)
	// 三个域名，TCP 都通、测速全失败 —— 更像测速通路坏了，而非三域名齐死。
	samples := map[string]domainProbeSample{
		"a.example.com": {OK: false, Stage: "speed", TCPMs: 30, Error: "boom"},
		"b.example.com": {OK: false, Stage: "speed", TCPMs: 35, Error: "boom"},
		"c.example.com": {OK: false, Stage: "speed", TCPMs: 40, Error: "boom"},
	}
	a.recordDomainProbeResults(samples)
	a.recordDomainProbeResults(samples)
	a.recordDomainProbeResults(samples)
	for domain, rec := range a.domainQuality {
		if rec.ConsecutiveFailures != 0 {
			t.Fatalf("%s consecutiveFailures = %d, want 0 (probe-broken round must not escalate)", domain, rec.ConsecutiveFailures)
		}
		if rec.State == domainStateDead {
			t.Fatalf("%s state = dead after probe-broken rounds; would blank the pool", domain)
		}
	}
	// UI 视图仍如实反映最近一轮不可达。
	if view := a.domainLatencySnapshot()["a.example.com"]; view.OK {
		t.Fatal("view.OK = true, want false (last round was unreachable)")
	}
}

func TestRecordDomainProbeCountsFailuresWhenPeerSucceeds(t *testing.T) {
	a := newProbeTestApp(t)
	// 同轮里 b 成功，则 a 的失败属于真实故障，照常计数。
	samples := map[string]domainProbeSample{
		"a.example.com": {OK: false, Stage: "speed", TCPMs: 30, Error: "boom"},
		"b.example.com": okSample(12, 25),
	}
	a.recordDomainProbeResults(samples)
	if got := a.domainQuality["a.example.com"].ConsecutiveFailures; got != 1 {
		t.Fatalf("a consecutiveFailures = %d, want 1", got)
	}
	if got := a.domainQuality["b.example.com"].ConsecutiveFailures; got != 0 {
		t.Fatalf("b consecutiveFailures = %d, want 0", got)
	}
}

func TestRecordDomainProbePrunesRemovedDomains(t *testing.T) {
	a := newProbeTestApp(t)
	a.recordDomainProbeResults(map[string]domainProbeSample{
		"a.example.com": okSample(10, 20),
		"b.example.com": okSample(10, 20),
	})
	a.recordDomainProbeResults(map[string]domainProbeSample{
		"a.example.com": okSample(10, 20),
	})
	if _, ok := a.domainQuality["b.example.com"]; ok {
		t.Fatal("removed domain still holds a quality record")
	}
}

// ---------------------------------------------------------------------------
// 测速端点：默认必须落在非 Cloudflare 目标上
//
// 2026-10-20 实测：这条 Worker+ProxyIP 链路对 CF 目标（speed.cloudflare.com、
// cp.cloudflare.com）零字节，对非 CF 目标（gstatic、proof.ovh.net）正常。
// 端点一旦回退成 CF，整套测速会静默归零，故用测试钉死。
// ---------------------------------------------------------------------------

func TestDomainSpeedProbeURL(t *testing.T) {
	t.Run("默认端点非 CF 且可被替换", func(t *testing.T) {
		t.Setenv("PROXY_DOMAIN_SPEED_URL", "")
		got := domainSpeedProbeURL(256 * 1024)
		if got != domainSpeedDefaultURL {
			t.Fatalf("got %q, want default %q", got, domainSpeedDefaultURL)
		}
		if strings.Contains(got, "cloudflare.com") {
			t.Fatalf("默认测速端点 %q 指向 Cloudflare，该链路对 CF 目标零字节", got)
		}
	})

	t.Run("{bytes} 占位符展开", func(t *testing.T) {
		t.Setenv("PROXY_DOMAIN_SPEED_URL", "https://example.net/__down?bytes={bytes}")
		if got, want := domainSpeedProbeURL(131072), "https://example.net/__down?bytes=131072"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("定长端点原样透传", func(t *testing.T) {
		t.Setenv("PROXY_DOMAIN_SPEED_URL", "https://example.net/10Mb.dat")
		if got, want := domainSpeedProbeURL(262144), "https://example.net/10Mb.dat"; got != want {
			t.Fatalf("got %q, want %q（定长端点不该被改写）", got, want)
		}
	})

	t.Run("空白值回落默认", func(t *testing.T) {
		t.Setenv("PROXY_DOMAIN_SPEED_URL", "   ")
		if got := domainSpeedProbeURL(1); got != domainSpeedDefaultURL {
			t.Fatalf("got %q, want default %q", got, domainSpeedDefaultURL)
		}
	})
}

// writeProbeTemplate 落一份校验通过的 VLESS 模板，供配置类测试复用。
func writeProbeTemplate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vless-probe-outbound.json")
	const tmpl = `{"type":"vless","server":"127.0.0.1","server_port":443,"uuid":"94892c52-199b-46fb-8552-41b91c00f9ee",` +
		`"tls":{"enabled":true,"server_name":"probe.example.com"},` +
		`"transport":{"type":"ws","path":"/probe","headers":{"Host":"probe.example.com"}}}`
	if err := os.WriteFile(path, []byte(tmpl), 0600); err != nil {
		t.Fatalf("写临时模板失败: %v", err)
	}
	return path
}

func TestDomainSpeedProbeConfigDefaults(t *testing.T) {
	t.Setenv("PROXY_VLESS_TEMPLATE", writeProbeTemplate(t))
	t.Setenv("PROXY_DOMAIN_SPEED_URL", "")
	t.Setenv("PROXY_DOMAIN_SPEED_BYTES", "")
	t.Setenv("PROXY_DOMAIN_SPEED_TIMEOUT_SECONDS", "")

	a := newProbeTestApp(t)
	cfg, template, err := a.domainSpeedProbeConfig()
	if err != nil {
		t.Fatalf("domainSpeedProbeConfig: %v", err)
	}
	if template == nil {
		t.Fatal("template = nil, want parsed outbound")
	}
	if cfg.TestURL != domainSpeedDefaultURL {
		t.Fatalf("TestURL = %q, want %q", cfg.TestURL, domainSpeedDefaultURL)
	}
	if cfg.ReadLimit != int64(domainSpeedDefaultBytes) {
		t.Fatalf("ReadLimit = %d, want %d", cfg.ReadLimit, domainSpeedDefaultBytes)
	}
	// 只读到上限即止：整文件 10 MB，不该把家宽烧在体检上。
	if cfg.ReadLimit > 512*1024 {
		t.Fatalf("ReadLimit = %d，超过 512 KiB 上限，单轮流量失控", cfg.ReadLimit)
	}
	if cfg.MinBytes != int64(domainQualityMinBytes) {
		t.Fatalf("MinBytes = %d, want %d", cfg.MinBytes, domainQualityMinBytes)
	}
	if cfg.Timeout != time.Duration(domainSpeedDefaultTimeoutSeconds)*time.Second {
		t.Fatalf("Timeout = %v, want %ds", cfg.Timeout, domainSpeedDefaultTimeoutSeconds)
	}
	if cfg.ExpectedStatus != 200 {
		t.Fatalf("ExpectedStatus = %d, want 200", cfg.ExpectedStatus)
	}
}

func TestDomainSpeedProbeConfigClamp(t *testing.T) {
	t.Setenv("PROXY_VLESS_TEMPLATE", writeProbeTemplate(t))
	t.Setenv("PROXY_DOMAIN_SPEED_BYTES", "1")             // 低于下限
	t.Setenv("PROXY_DOMAIN_SPEED_TIMEOUT_SECONDS", "999") // 高于上限

	a := newProbeTestApp(t)
	cfg, _, err := a.domainSpeedProbeConfig()
	if err != nil {
		t.Fatalf("domainSpeedProbeConfig: %v", err)
	}
	if cfg.ReadLimit != int64(domainSpeedDefaultBytes) || cfg.Timeout != time.Duration(domainSpeedDefaultTimeoutSeconds)*time.Second {
		t.Fatalf("越界值未回落默认: ReadLimit=%d Timeout=%v", cfg.ReadLimit, cfg.Timeout)
	}
}
