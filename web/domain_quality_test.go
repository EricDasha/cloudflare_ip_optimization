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
	// 同一批样本也要被算作「测速通路可疑」：Stage=speed 且 TCP 通、测速全零。
	st := a.speedPathStatus()
	if !st.Suspect || st.ZeroRounds != speedPathWarnRounds || st.TCPOK != 3 || st.SpeedOK != 0 {
		t.Fatalf("speedPath = %+v, want suspect with zeroRounds=%d tcpOk=3 speedOk=0", st, speedPathWarnRounds)
	}
}

// 模板不可用降级成 tcp-only 时，本轮根本没碰测速端点，不能算进 TCPOK，
// 否则三轮后会把故障指到上游端点头上。
func TestSpeedPathIgnoresTCPOnlyRounds(t *testing.T) {
	a := newProbeTestApp(t)
	samples := map[string]domainProbeSample{
		"a.example.com": {OK: true, Stage: "tcp-only", TCPMs: 30},
		"b.example.com": {OK: true, Stage: "tcp-only", TCPMs: 40},
	}
	for i := 0; i < speedPathWarnRounds+2; i++ {
		a.recordDomainProbeResults(samples)
	}
	st := a.speedPathStatus()
	if st.Suspect || st.ZeroRounds != 0 || st.TCPOK != 0 || st.SpeedOK != 0 {
		t.Fatalf("tcp-only 轮不应判端点可疑，实际 %+v", st)
	}
}

// 整轮无成绩时，失败样本不进入稳定性账：否则端点坏三轮，
// 好域名的成功率会被拖到门槛以下，告警还没响域名先被踢出转发。
func TestRecordDomainProbeKeepsSuccessRateOnBrokenRounds(t *testing.T) {
	a := newProbeTestApp(t)
	samples := map[string]domainProbeSample{
		"a.example.com": {OK: false, Stage: "speed", TCPMs: 30, Error: "boom"},
		"b.example.com": {OK: false, Stage: "speed", TCPMs: 35, Error: "boom"},
	}
	for i := 0; i < 6; i++ {
		a.recordDomainProbeResults(samples)
	}
	for domain, rec := range a.domainQuality {
		if len(rec.Samples) != 0 {
			t.Fatalf("%s samples = %d, want 0 (broken rounds must not enter the ledger)", domain, len(rec.Samples))
		}
		if rec.State == domainStateUnstable || rec.State == domainStateDead {
			t.Fatalf("%s state = %s, want untouched", domain, rec.State)
		}
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

// 改动 A：DNS 解析失败占大头且无一例测速成功时，判定为体检通路故障——
// 本轮失败样本不入账、不累计连败、状态不重算，且返回 true 让调用方跳过
// reconcile（池子成员数量不变）。
func TestRecordDomainProbePathBrokenOnDNSFailures(t *testing.T) {
	a := newProbeTestApp(t)
	samples := map[string]domainProbeSample{
		"a.example.com": failSample("dns"),
		"b.example.com": failSample("dns"),
		"c.example.com": failSample("dns"),
		"d.example.com": failSample("tcp"),
	}
	if broken := a.recordDomainProbeResults(samples); !broken {
		t.Fatal("4/4 解析+建连失败应判定为通路故障")
	}
	for domain, rec := range a.domainQuality {
		if len(rec.Samples) != 0 {
			t.Fatalf("%s samples = %d, want 0 (path-broken rounds must not enter the ledger)", domain, len(rec.Samples))
		}
		if rec.ConsecutiveFailures != 0 {
			t.Fatalf("%s consecutiveFailures = %d, want 0", domain, rec.ConsecutiveFailures)
		}
		if rec.State != "" {
			t.Fatalf("%s state = %q, want untouched empty state", domain, rec.State)
		}
	}
}

// 改动 A：失败占比低于阈值（<70%）时不是通路故障——有个别域名真死时，
// 它的失败仍要照常计数，否则坏域名永远清不掉。
func TestRecordDomainProbeNotBrokenBelowThreshold(t *testing.T) {
	a := newProbeTestApp(t)
	samples := map[string]domainProbeSample{
		"a.example.com": failSample("dns"),
		"b.example.com": failSample("tcp"),
		"c.example.com": okSample(12, 25),
		"d.example.com": okSample(10, 30),
	}
	if broken := a.recordDomainProbeResults(samples); broken {
		t.Fatal("2/4 解析+建连失败且有测速成功，不应判定为通路故障")
	}
}

// 改动 A：纯函数分账与 70% 阈值边界——恰好 7/10 失败判通路故障，6/10 不判。
func TestClassifyDomainProbeRoundThreshold(t *testing.T) {
	build := func(dns, tcp, speedFail int) map[string]domainProbeSample {
		samples := make(map[string]domainProbeSample, dns+tcp+speedFail)
		for i := 0; i < dns; i++ {
			samples["dns"+string(rune('a'+i))+".example.com"] = failSample("dns")
		}
		for i := 0; i < tcp; i++ {
			samples["tcp"+string(rune('a'+i))+".example.com"] = failSample("tcp")
		}
		for i := 0; i < speedFail; i++ {
			samples["spd"+string(rune('a'+i))+".example.com"] = failSample("speed")
		}
		return samples
	}
	stats := classifyDomainProbeRound(build(4, 3, 3))
	if stats.DNSFailed != 4 || stats.TCPFailed != 3 || stats.SpeedFailed != 3 || stats.Total != 10 {
		t.Fatalf("stats = %+v, want dns=4 tcp=3 speed=3 total=10", stats)
	}
	if !stats.pathBroken() {
		t.Fatal("7/10 解析+建连失败应判定为通路故障")
	}
	if stats := classifyDomainProbeRound(build(3, 3, 4)); stats.pathBroken() {
		t.Fatal("6/10 解析+建连失败不应判定为通路故障")
	}
	// 只要有一例测速成功，即使 9/10 解析失败也不是通路故障——
	// 同伴成功证明通路本身还活着。
	samples := build(9, 0, 0)
	samples["ok.example.com"] = okSample(12, 25)
	if stats := classifyDomainProbeRound(samples); stats.pathBroken() {
		t.Fatal("有测速成功时不应判定为通路故障")
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

// 改动 A 的补漏：通路故障轮失败的样本连账都不入——不建空记录，
// 已有记录的 LastProbeAt 也不被没测过的轮次刷新。旧实现先建记录再
// 跳过入账，账本凭空多出 state="" 的空记录，闸门对空状态一律放行。
func TestRecordDomainProbeBrokenRoundCreatesNoRecords(t *testing.T) {
	a := newProbeTestApp(t)
	old := freshRecord(domainStateHealthy)
	oldAt := time.Now().Add(-time.Hour)
	old.LastProbeAt = oldAt
	a.domainQuality["old.example.com"] = old
	samples := map[string]domainProbeSample{
		"a.example.com":   failSample("dns"),
		"b.example.com":   failSample("dns"),
		"c.example.com":   failSample("dns"),
		"old.example.com": failSample("tcp"),
	}
	if broken := a.recordDomainProbeResults(samples); !broken {
		t.Fatal("4/4 解析+建连失败应判定为通路故障")
	}
	if got := len(a.domainQuality); got != 1 {
		t.Fatalf("broken round ledger = %d records, want 1 (only pre-existing)", got)
	}
	if rec := a.domainQuality["old.example.com"]; rec == nil {
		t.Fatal("pre-existing record pruned by broken round")
	} else if !rec.LastProbeAt.Equal(oldAt) {
		t.Fatalf("LastProbeAt = %v, want untouched %v", rec.LastProbeAt, oldAt)
	} else if rec.State != domainStateHealthy {
		t.Fatalf("state = %q, want healthy", rec.State)
	}
}

// tcp-only 降级样本只报可达性：攒够 3 轮也不能判 healthy——
// 一次测速都没做过，凭 TCP 通就放行等于质量闸对它永久失明。
func TestRecalculateTCPOnlyNeverHealthy(t *testing.T) {
	q := &domainQuality{Domain: "x.example.com"}
	for i := 0; i < 5; i++ {
		q.Samples = append(q.Samples, domainProbeSample{At: time.Now(), OK: true, Stage: "tcp-only", TCPMs: 50})
		q.ConsecutiveSuccesses++
	}
	q.recalculate()
	if q.State != domainStateTesting {
		t.Fatalf("tcp-only state = %q, want testing", q.State)
	}
	// 一次真实测速后即可正常晋升。
	q.Samples = append(q.Samples, domainProbeSample{At: time.Now(), OK: true, Stage: "speed", TCPMs: 50, Mbps: 20})
	q.ConsecutiveSuccesses++
	q.recalculate()
	if q.State != domainStateHealthy {
		t.Fatalf("mixed state = %q, want healthy", q.State)
	}
}

// 账本损坏不得静默丢弃：文件改名备查，内存用空账本启动。
func TestLoadDomainQualityRenamesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, domainQualityFile), []byte("{not json"), 0600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	a := &app{dataDir: dir, domainQuality: make(map[string]*domainQuality)}
	a.loadDomainQuality()
	if _, err := os.Stat(filepath.Join(dir, domainQualityFile)); !os.IsNotExist(err) {
		t.Fatalf("corrupt file still in place")
	}
	entries, err := filepath.Glob(filepath.Join(dir, domainQualityFile+".corrupt-*"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("corrupt backups = %v, want 1", entries)
	}
	if len(a.domainQuality) != 0 {
		t.Fatalf("domainQuality = %d records, want 0", len(a.domainQuality))
	}
}

// 质量账本损坏时构造函数不得崩溃，空账本启动（失败原因已记日志）。
func TestNewQualitySchedulerRuntimeToleratesCorruptCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, proxyQualityFile), []byte("{not json"), 0600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	r := newQualitySchedulerRuntime(dir)
	if r == nil || r.data.ActiveIP != "" || len(r.data.Records) != 0 {
		t.Fatalf("corrupt cache produced non-empty runtime: %#v", r.data)
	}
}

// 空样本轮次不得动账本：末尾剪枝循环以 samples 为准，空 map 会把
// 整本账清空。生产路径无目标时不会调到这里，但防御不能缺。
func TestRecordDomainProbeEmptyRoundIsNoop(t *testing.T) {
	a := newProbeTestApp(t)
	old := freshRecord(domainStateHealthy)
	oldAt := time.Now().Add(-time.Hour)
	old.LastProbeAt = oldAt
	old.Samples = []domainProbeSample{okSample(10, 50), okSample(11, 52), okSample(10, 50)}
	a.domainQuality["old.example.com"] = old
	if broken := a.recordDomainProbeResults(map[string]domainProbeSample{}); broken {
		t.Fatal("空轮次不应判定为通路故障")
	}
	if broken := a.recordDomainProbeResults(nil); broken {
		t.Fatal("nil 轮次不应判定为通路故障")
	}
	rec := a.domainQuality["old.example.com"]
	if rec == nil {
		t.Fatal("空轮次清空了账本")
	}
	if len(rec.Samples) != 3 || !rec.LastProbeAt.Equal(oldAt) || rec.State != domainStateHealthy {
		t.Fatalf("空轮次改动了既有记录: %+v", rec)
	}
}

// 模板降级模式下的通路故障轮：8 个 DNS 失败 + 2 个 tcp-only 成功，
// 整轮都不入账——成功的 tcp-only 样本也不能建记录或刷新 LastProbeAt，
// 否则故障轮的结论会污染账本（新域名凭空建 state="" 的记录，
// 闸门对空状态一律放行）。
func TestRecordDomainProbeBrokenRoundSkipsOKSamplesToo(t *testing.T) {
	a := newProbeTestApp(t)
	old := freshRecord(domainStateHealthy)
	oldAt := time.Now().Add(-time.Hour)
	old.LastProbeAt = oldAt
	a.domainQuality["old.example.com"] = old
	samples := map[string]domainProbeSample{
		"a.example.com": failSample("dns"),
		"b.example.com": failSample("dns"),
		"c.example.com": failSample("dns"),
		"d.example.com": failSample("dns"),
		"e.example.com": failSample("dns"),
		"f.example.com": failSample("dns"),
		"g.example.com": failSample("dns"),
		"h.example.com": failSample("dns"),
		// 模板降级：这两个 TCP 通了，只报可达性。
		"i.example.com": {At: time.Now(), OK: true, Stage: "tcp-only", TCPMs: 30},
		"j.example.com": {At: time.Now(), OK: true, Stage: "tcp-only", TCPMs: 35},
		// 既有记录本轮同样 tcp-only 成功：它的 LastProbeAt 也不应被刷新。
		"old.example.com": {At: time.Now(), OK: true, Stage: "tcp-only", TCPMs: 40},
	}
	if broken := a.recordDomainProbeResults(samples); !broken {
		t.Fatal("8/10 解析失败应判定为通路故障")
	}
	if got := len(a.domainQuality); got != 1 {
		t.Fatalf("broken round ledger = %d records, want 1 (only pre-existing)", got)
	}
	if _, ok := a.domainQuality["i.example.com"]; ok {
		t.Fatal("通路故障轮给 tcp-only 成功的域名建了记录")
	}
	if rec := a.domainQuality["old.example.com"]; rec == nil {
		t.Fatal("pre-existing record pruned by broken round")
	} else if !rec.LastProbeAt.Equal(oldAt) {
		t.Fatalf("LastProbeAt = %v, want untouched %v", rec.LastProbeAt, oldAt)
	}
}
