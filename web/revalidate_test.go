package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// revalidateFixture 造一个只够跑「暂停—验—剔」的 app，并把四个外部依赖
// 换成可编排的假货。真的 fork cfnat、真的拨号外网，单测既不稳也验不出东西。
type revalidateFixture struct {
	app         *app
	started     []proxyActivePool // 每次恢复转发时传进去的池
	applied     []proxyActivePool // 每次落地新池时传进去的池
	startCalls  int
	refillCalls int
	applyErr    error
	startErr    error
}

func newRevalidateFixture(t *testing.T, pool proxyActivePool) *revalidateFixture {
	t.Helper()
	f := &revalidateFixture{}
	f.app = &app{dataDir: t.TempDir()}
	f.app.activePool = pool

	orig := revalidateDeps
	t.Cleanup(func() { revalidateDeps = orig })

	revalidateDeps = revalidateDependencies{
		checkIPs: func(a *app, ctx context.Context, ips []string, cfg proxyAutoConfig) []revalidateMemberResult {
			out := make([]revalidateMemberResult, 0, len(ips))
			for _, ip := range ips {
				out = append(out, revalidateMemberResult{Member: ip, Kind: "ip", OK: ip != "10.0.0.9"})
			}
			return out
		},
		checkDomains: func(a *app, ctx context.Context, domains []string, base int) []revalidateMemberResult {
			out := make([]revalidateMemberResult, 0, len(domains))
			for _, d := range domains {
				out = append(out, revalidateMemberResult{Member: d, Kind: "domain", OK: d != "bad.example"})
			}
			return out
		},
		stopForwarding: func(a *app) error { return nil },
		startForwarding: func(a *app, pool proxyActivePool) error {
			f.started = append(f.started, pool)
			f.startCalls++
			return f.startErr
		},
		applyPool: func(a *app, pool, current proxyActivePool) error {
			f.applied = append(f.applied, pool)
			return f.applyErr
		},
		refill: func(a *app) { f.refillCalls++ },
	}
	return f
}

// 绝不踢空：全部成员都验不过时，保留原池、标记 Aborted，并且转发必须拉回来。
//
// 这是整条流程里最要紧的一条。全灭说明验证通路本身坏了（模板失效、出口不通）
// 的概率远大于「所有成员同时死」，此时把池清空，用户既没网也没得看。
func TestRevalidateKeepsPoolWhenEveryMemberFails(t *testing.T) {
	pool := proxyActivePool{
		Host: "h", Path: "/p",
		IPs:      []string{"10.0.0.9", "10.0.0.9"},
		Domains:  []string{"bad.example"},
		Priority: []string{"10.0.0.9"},
		Disabled: []string{"10.0.0.9"},
	}
	f := newRevalidateFixture(t, pool)
	f.app.beginRevalidate(3)

	report, err := f.app.revalidateActivePool(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !report.Aborted {
		t.Fatal("all-fail round was not marked Aborted")
	}
	if len(f.applied) != 0 {
		t.Fatalf("all-fail round applied a new pool: %+v", f.applied[0])
	}
	if f.startCalls != 1 {
		t.Fatalf("forwarding restarted %d times, want exactly 1", f.startCalls)
	}
	if got := f.started[0]; len(got.IPs) != 2 || len(got.Domains) != 1 {
		t.Fatalf("restarted with a mutated pool: %+v", got)
	}
	// 报告仍要列出每个成员的死因——「为什么不踢」必须看得见。
	if len(report.Members) != 3 {
		t.Fatalf("report members = %d, want 3", len(report.Members))
	}
	if f.refillCalls != 0 {
		t.Fatal("all-fail round triggered a refill although the pool was left untouched")
	}
}

// 部分剔除：只把不合格的成员摘掉，Priority / Disabled 里的同名残留一并清掉，
// 否则它们会以「已关闭成员」的身份留在池定义里，下次重启又被读回来。
func TestRevalidatePrunesOnlyFailedMembers(t *testing.T) {
	pool := proxyActivePool{
		Host: "h", Path: "/p",
		IPs:      []string{"10.0.0.1", "10.0.0.9", "10.0.0.2"},
		Domains:  []string{"good.example", "bad.example"},
		Priority: []string{"10.0.0.9", "10.0.0.1"},
		Disabled: []string{"bad.example", "10.0.0.2"},
		Results: []proxyScanResult{
			{IP: "10.0.0.1"}, {IP: "10.0.0.9"}, {IP: "10.0.0.2"},
		},
	}
	f := newRevalidateFixture(t, pool)
	f.app.beginRevalidate(5)

	report, err := f.app.revalidateActivePool(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Aborted {
		t.Fatalf("partial round was aborted: %s", report.Note)
	}
	if len(f.applied) != 1 {
		t.Fatalf("applied %d times, want 1", len(f.applied))
	}
	got := f.applied[0]
	if len(got.IPs) != 2 || got.IPs[0] != "10.0.0.1" || got.IPs[1] != "10.0.0.2" {
		t.Fatalf("IPs = %v, want [10.0.0.1 10.0.0.2]", got.IPs)
	}
	if len(got.Domains) != 1 || got.Domains[0] != "good.example" {
		t.Fatalf("Domains = %v, want [good.example]", got.Domains)
	}
	if len(got.Priority) != 1 || got.Priority[0] != "10.0.0.1" {
		t.Fatalf("Priority = %v, want [10.0.0.1]", got.Priority)
	}
	if len(got.Disabled) != 1 || got.Disabled[0] != "10.0.0.2" {
		t.Fatalf("Disabled = %v, want [10.0.0.2]", got.Disabled)
	}
	// 被剔成员的成绩记录也要清掉，否则页面上还挂着一个不在池里的 IP。
	if len(got.Results) != 2 {
		t.Fatalf("Results = %v, want 2 entries", got.Results)
	}
	for _, r := range got.Results {
		if r.IP == "10.0.0.9" {
			t.Fatal("removed member's scan record survived the prune")
		}
	}
	// applyPool 成功后不再重复重启：它自己已经把 cfnat 用新池拉起来了。
	if f.startCalls != 0 {
		t.Fatalf("forwarding restarted %d times after a successful apply, want 0", f.startCalls)
	}
	// 剔了人就要补：只清不补，池会一轮比一轮薄，最后两三个成员时一次
	// 抖动就是全灭。
	if f.refillCalls != 1 {
		t.Fatalf("refill called %d times after removing members, want 1", f.refillCalls)
	}
	if report.Passed != 3 || len(report.Removed) != 2 {
		t.Fatalf("passed=%d removed=%v", report.Passed, report.Removed)
	}
}

// 全部通过：不动池，但转发必须恢复。这里最容易漏——「没什么要改」不等于
// 「不需要把停掉的 cfnat 拉回来」。
func TestRevalidateRestartsForwardingWhenNothingRemoved(t *testing.T) {
	pool := proxyActivePool{
		Host: "h", Path: "/p",
		IPs:     []string{"10.0.0.1", "10.0.0.2"},
		Domains: []string{"good.example"},
	}
	f := newRevalidateFixture(t, pool)
	f.app.beginRevalidate(3)

	report, err := f.app.revalidateActivePool(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(report.Removed) != 0 {
		t.Fatalf("removed = %v, want none", report.Removed)
	}
	if len(f.applied) != 0 {
		t.Fatal("all-pass round applied a new pool")
	}
	if f.startCalls != 1 {
		t.Fatalf("forwarding restarted %d times, want 1", f.startCalls)
	}
	if f.refillCalls != 0 {
		t.Fatalf("refill called %d times although nothing was removed", f.refillCalls)
	}
}

// 落地失败：池保持原样，标记 Aborted，转发照样要拉回来。
func TestRevalidateRollsBackWhenApplyFails(t *testing.T) {
	pool := proxyActivePool{
		Host: "h", Path: "/p",
		IPs: []string{"10.0.0.1", "10.0.0.9"},
	}
	f := newRevalidateFixture(t, pool)
	f.applyErr = errors.New("save failed")
	f.app.beginRevalidate(2)

	report, err := f.app.revalidateActivePool(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !report.Aborted {
		t.Fatal("failed apply was not marked Aborted")
	}
	if f.startCalls != 1 {
		t.Fatalf("forwarding restarted %d times after rollback, want 1", f.startCalls)
	}
	if got := f.started[0]; len(got.IPs) != 2 {
		t.Fatalf("rollback restarted with %v, want the original 2 IPs", got.IPs)
	}
}

// 暂停失败：连池都没碰，但报告要说明白，前端不能显示「验证完成」。
func TestRevalidateReportsStopFailure(t *testing.T) {
	pool := proxyActivePool{Host: "h", Path: "/p", IPs: []string{"10.0.0.1"}}
	f := newRevalidateFixture(t, pool)
	revalidateDeps.stopForwarding = func(a *app) error { return errors.New("kill failed") }
	f.app.beginRevalidate(1)

	report, err := f.app.revalidateActivePool(context.Background())
	if err == nil {
		t.Fatal("stop failure was swallowed")
	}
	if report.Paused {
		t.Fatal("report claims forwarding was paused when the stop failed")
	}
	if f.startCalls != 0 {
		t.Fatal("forwarding was restarted although it never stopped")
	}
}

// 席位互斥：一轮没跑完，第二轮必须被挡在门外，否则两个流程会同时暂停转发、
// 互相把对方的探针拖成超时。
func TestBeginRevalidateRejectsSecondRound(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	if !a.beginRevalidate(3) {
		t.Fatal("first round was rejected")
	}
	if a.beginRevalidate(3) {
		t.Fatal("second round was admitted while the first was still running")
	}
	a.endRevalidate(revalidateReport{Note: "done"})
	if !a.beginRevalidate(3) {
		t.Fatal("round was not admitted after the previous one finished")
	}
}

// 进度必须能读到：前端靠 revalidateStatus 显示「验到第几个」，
// 靠 revalidateDetail 拿上一轮结论。两处漏一个，页面就会卡在「进行中」。
func TestRevalidateStatusExposesProgressAndLastReport(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	a.beginRevalidate(4)
	a.setRevalidatePhase("验证域名", 2)
	st := a.revalidateStatus()
	if st["running"] != true || st["phase"] != "验证域名" || st["tested"] != 2 || st["total"] != 4 {
		t.Fatalf("status = %+v", st)
	}
	a.endRevalidate(revalidateReport{Tested: 4, Passed: 3, Removed: []string{"x"}})
	st = a.revalidateStatus()
	if st["running"] != false {
		t.Fatal("round still reported as running after endRevalidate")
	}
	detail := a.revalidateDetail()
	if _, ok := detail["last"]; !ok {
		t.Fatal("detail has no last report to show")
	}
}

// 复验判据：偶发类失败值得再试一次，确定类失败不值得。
// 这条防的是「一次超时就踢掉好成员」——实测一轮 26 个域名掉 3 个，
// 其中一个上一轮还是 successRate=0.8 的健康成员。
func TestRevalidateWorthRetry(t *testing.T) {
	for _, class := range []string{failureTimeout, failureProtocol, failureOther, failureNone} {
		if !revalidateWorthRetry(class) {
			t.Fatalf("%q should be worth a retry", class)
		}
	}
	for _, class := range []string{failureRefused, failureUnreachable, failureHandshake} {
		if revalidateWorthRetry(class) {
			t.Fatalf("%q is a definite answer, retrying only wastes budget", class)
		}
	}
}

// keepOnly 保持原顺序，且空输入返回 nil（落盘时是 null 而不是 []）。
func TestKeepOnlyPreservesOrder(t *testing.T) {
	got := keepOnly([]string{"a", "b", "c", "d"}, []string{"d", "b"})
	if len(got) != 2 || got[0] != "b" || got[1] != "d" {
		t.Fatalf("keepOnly = %v, want [b d]", got)
	}
	if keepOnly(nil, []string{"a"}) != nil {
		t.Fatal("keepOnly(nil) should be nil")
	}
}

// 并发下状态读写不能有数据竞争：用 -race 跑时这条会真正起作用。
func TestRevalidateStateConcurrentAccess(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	a.beginRevalidate(10)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.setRevalidatePhase("验证 IP", i)
			_ = a.revalidateStatus()
			_ = a.revalidateDetail()
		}(i)
	}
	wg.Wait()
	a.endRevalidate(revalidateReport{})
}

// 接口契约：GET 读状态，POST 开跑，重复 POST 回 429。
//
// 429 这一条是给前端用的判据——只有它能区分「开跑了」和「已经有一轮在跑」。
// 都回 200 的话，用户连点两次会以为点了两次，实际第二次什么也没发生。
func TestHandleRevalidateHTTPContract(t *testing.T) {
	f := newRevalidateFixture(t, proxyActivePool{
		Host: "h", Path: "/p", IPs: []string{"10.0.0.1"},
	})

	// GET：尚未跑过，last 不存在但 running=false 必须给出来。
	rec := httptest.NewRecorder()
	f.app.handleRevalidate(rec, httptest.NewRequest(http.MethodGet, "/api/cfnat/revalidate", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET body is not JSON: %v", err)
	}
	if body["running"] != false {
		t.Fatalf("GET running = %v, want false", body["running"])
	}

	// POST ?wait=1：同步跑完，直接拿到报告。
	rec = httptest.NewRecorder()
	f.app.handleRevalidate(rec, httptest.NewRequest(http.MethodPost, "/api/cfnat/revalidate?wait=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("POST body is not JSON: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("POST ok = %v", body["ok"])
	}

	// 席位被占时 POST 必须回 429，而不是假装开跑。
	f.app.beginRevalidate(1)
	rec = httptest.NewRecorder()
	f.app.handleRevalidate(rec, httptest.NewRequest(http.MethodPost, "/api/cfnat/revalidate", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("busy POST status = %d, want 429", rec.Code)
	}

	// 其它方法一律 405。
	rec = httptest.NewRecorder()
	f.app.handleRevalidate(rec, httptest.NewRequest(http.MethodDelete, "/api/cfnat/revalidate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d, want 405", rec.Code)
	}
}

// 异步入口必须把「跑不起来」的失败也写进报告。前端只轮询 report，
// 看不到 error 返回值——失败只留在 HTTP 日志里，页面上就成了「验证完成」。
func TestRevalidateFailureLandsInReport(t *testing.T) {
	f := newRevalidateFixture(t, proxyActivePool{Host: "h", Path: "/p"}) // 空池
	f.app.beginRevalidate(0)

	report, err := f.app.revalidateActivePool(context.Background())
	if err == nil {
		t.Fatal("empty pool did not error")
	}
	if !report.Aborted || report.Note == "" {
		t.Fatalf("error did not land in the report: %+v", report)
	}
	detail := f.app.revalidateDetail()
	last, ok := detail["last"].(*revalidateReport)
	if !ok || last == nil || !last.Aborted {
		t.Fatalf("detail.last = %#v, want the aborted report", detail["last"])
	}
}

// 预算耗尽没轮上的域名不得按失败剔除：它根本没被测过。
func TestRevalidateKeepsSkippedDomains(t *testing.T) {
	pool := proxyActivePool{
		Host:    "h",
		Path:    "/p",
		IPs:     []string{"10.0.0.1"},
		Domains: []string{"good.example", "skipped.example", "bad.example"},
	}
	f := newRevalidateFixture(t, pool)
	revalidateDeps.checkDomains = func(a *app, ctx context.Context, domains []string, base int) []revalidateMemberResult {
		out := make([]revalidateMemberResult, 0, len(domains))
		for _, d := range domains {
			switch d {
			case "bad.example":
				out = append(out, revalidateMemberResult{Member: d, Kind: "domain", OK: false, Stage: "speed", Error: "boom"})
			case "skipped.example":
				out = append(out, revalidateMemberResult{Member: d, Kind: "domain", OK: false, Stage: "skipped", Error: "验证预算用尽", ErrorClass: failureTimeout})
			default:
				out = append(out, revalidateMemberResult{Member: d, Kind: "domain", OK: true})
			}
		}
		return out
	}
	f.app.beginRevalidate(4)

	report, err := f.app.revalidateActivePool(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Aborted {
		t.Fatalf("round was aborted: %s", report.Note)
	}
	if len(f.applied) != 1 {
		t.Fatalf("applied %d times, want 1", len(f.applied))
	}
	got := f.applied[0]
	if len(got.Domains) != 2 || got.Domains[0] != "good.example" || got.Domains[1] != "skipped.example" {
		t.Fatalf("Domains = %v, want [good.example skipped.example]", got.Domains)
	}
	if len(report.Removed) != 1 || report.Removed[0] != "bad.example" {
		t.Fatalf("Removed = %v, want [bad.example]", report.Removed)
	}
}

// 总预算到期时正在跑的域名探针被截断，必须记 skipped 保留，不得按真失败剔除。
func TestRevalidateOneDomainBudgetExpiryIsSkipped(t *testing.T) {
	a := &app{dataDir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := a.revalidateOneDomain(ctx, "example.com", vlessProbeConfig{}, nil, false)
	if res.Stage != "skipped" {
		t.Fatalf("cancelled ctx must yield skipped, got stage=%q err=%q", res.Stage, res.Error)
	}
	// 对照：预算充足时 TCP 被拒是真失败，记 tcp 段。
	res = a.revalidateOneDomain(context.Background(), "127.0.0.1", vlessProbeConfig{}, nil, false)
	if res.Stage != "tcp" || res.OK {
		t.Fatalf("live ctx refused dial must be tcp failure, got stage=%q ok=%v err=%q", res.Stage, res.OK, res.Error)
	}
}
