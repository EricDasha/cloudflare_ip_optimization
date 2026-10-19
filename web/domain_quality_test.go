package main

import (
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
