package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// optimizeFixture 把两个外部动作换成假的：真刷新要连外网、真漏斗要重启
// cfnat，单测既不稳定也验证不了分支。和 revalidate_test 的做法保持一致。
type optimizeFixture struct {
	app     *app
	mu      sync.Mutex
	applied int
}

func newOptimizeFixture(t *testing.T, pool proxyActivePool, candidates proxyCandidateSnapshot) *optimizeFixture {
	t.Helper()
	t.Setenv("PROXY_AUTO_APPLY", "true")
	f := &optimizeFixture{app: &app{}}
	f.app.activePool = pool
	f.app.candidates = candidates

	orig := optimizeDeps
	t.Cleanup(func() { optimizeDeps = orig })

	optimizeDeps = optimizeDependencies{
		refresh: func(_ *app, _ context.Context) (bool, error) { return true, nil },
		apply: func(a *app, _ context.Context) bool {
			f.mu.Lock()
			f.applied++
			f.mu.Unlock()
			// 默认成功路径：换成一个新 IP，更新时间戳。
			a.activeMu.Lock()
			a.activePool = proxyActivePool{IPs: []string{"5.5.5.5"}, UpdatedAt: time.Now()}
			a.activeMu.Unlock()
			return true
		},
		snapshot: func(a *app) proxyActivePool { return a.proxyActivePoolSnapshot() },
		candidates: func(a *app) proxyCandidateSnapshot {
			return a.proxyCandidateSnapshot()
		},
	}
	return f
}

func (f *optimizeFixture) run() optimizeReport {
	f.mu.Lock()
	f.applied = 0
	f.mu.Unlock()
	if !f.app.beginOptimize() {
		panic("optimize seat already taken")
	}
	return f.app.runOptimize(context.Background())
}

// 一轮成功的全量重挑：报告要能说清「之前几个、之后几个、换没换」。
func TestOptimizeReportsPoolChange(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{IPs: []string{"1.1.1.1", "2.2.2.2"}, Domains: []string{"edge.example.com"}},
		proxyCandidateSnapshot{IPs: []string{"3.3.3.3", "4.4.4.4"}},
	)
	rep := f.run()

	if !rep.Refreshed || rep.Candidates != 2 {
		t.Fatalf("刷新/候选 = %v/%d, want true/2", rep.Refreshed, rep.Candidates)
	}
	if !rep.Applied || !rep.Changed {
		t.Fatalf("应记录已应用且已变化，实际 applied=%v changed=%v", rep.Applied, rep.Changed)
	}
	if rep.BeforeIPs != 2 || rep.AfterIPs != 1 {
		t.Fatalf("池前后 = %d→%d, want 2→1", rep.BeforeIPs, rep.AfterIPs)
	}
	if rep.Error != "" || rep.Blocked != "" {
		t.Fatalf("成功路径不该带错误：error=%q blocked=%q", rep.Error, rep.Blocked)
	}
	// 席位必须还回去，否则页面永远显示「优选中」。
	if f.app.optimizeIsRunning() {
		t.Fatal("结束后仍标记 running")
	}
	if last := f.app.optimizeDetail()["last"]; last == nil {
		t.Fatal("结束后没有留下上一轮报告")
	}
}

// 候选刷新被占用不是失败：手里那份快照照样能重考，报告要说明用了旧候选。
func TestOptimizeContinuesWhenRefreshBusy(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{IPs: []string{"1.1.1.1"}},
		proxyCandidateSnapshot{IPs: []string{"3.3.3.3"}},
	)
	optimizeDeps.refresh = func(_ *app, _ context.Context) (bool, error) {
		return false, errProxyCandidatesBusy
	}
	rep := f.run()
	if rep.Error != "" {
		t.Fatalf("刷新被占用不该终止：%q", rep.Error)
	}
	if !strings.Contains(rep.Note, "改用现有候选") {
		t.Fatalf("没有说明退回到现有候选：%q", rep.Note)
	}
	if !rep.Applied {
		t.Fatal("有候选就应该继续考试换池")
	}
}

// 池锁是用户亲手按下的：优选必须在开跑前停手并说明原因。否则漏斗跑完会
// 在提交前被锁检查静默丢弃，界面只会显示「没有可应用的变更」。
func TestOptimizeRefusesLockedPool(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{IPs: []string{"1.1.1.1"}, Lock: true},
		proxyCandidateSnapshot{IPs: []string{"3.3.3.3"}},
	)
	rep := f.run()
	if !strings.Contains(rep.Error, "锁定") {
		t.Fatalf("锁定池应报错说明原因，实际 %q", rep.Error)
	}
	f.mu.Lock()
	applied := f.applied
	f.mu.Unlock()
	if applied != 0 {
		t.Fatalf("锁定池不该进入漏斗，apply 调用了 %d 次", applied)
	}
}

// 候选为空才是真正的「没得跑」：必须明确报错，而不是假装完成。
func TestOptimizeFailsWithoutCandidates(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{IPs: []string{"1.1.1.1"}},
		proxyCandidateSnapshot{},
	)
	rep := f.run()
	if rep.Error == "" || !strings.Contains(rep.Error, "候选为空") {
		t.Fatalf("空候选应报错，实际 %q", rep.Error)
	}
	if f.applied != 0 {
		t.Fatal("空候选不该进入漏斗")
	}
}

// 手动优选不走换池闸，任何 poolBlock 都是别人记的账。旧实现按「记录时间
// 晚于开跑」归属，会把刷新阶段质量调度器刚记下的一笔冷却算到本轮头上，
// 让一次已经落地的优选返回 503、前端弹「换池被拦」。
func TestOptimizeIgnoresForeignBlockDuringRun(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{IPs: []string{"1.1.1.1"}},
		proxyCandidateSnapshot{IPs: []string{"3.3.3.3"}},
	)
	optimizeDeps.refresh = func(a *app, _ context.Context) (bool, error) {
		// 刷新阶段不持 proxyScanMu，调度器可以在这时记下自己的拦截。
		a.poolGuardMu.Lock()
		a.poolBlock = poolSwitchVerdict{Reason: "cooldown", NextAllowedAt: time.Now().Add(10 * time.Minute)}
		a.poolBlockAt = time.Now()
		a.poolBlockBy = "scheduler"
		a.poolGuardMu.Unlock()
		return true, nil
	}
	rep := f.run()
	if rep.Blocked != "" {
		t.Fatalf("别的执行器记的拦截被算到本轮：%q", rep.Blocked)
	}
	if !rep.Applied || rep.Error != "" {
		t.Fatalf("本轮应正常落地：applied=%v error=%q", rep.Applied, rep.Error)
	}
}

// 池错误是粘滞字段：上一轮留下的错误不能被当成本轮失败原因。这里只对比
// 时间，不看「有没有错误」，否则旧账会把一次正常复核说成故障。
func TestOptimizeDoesNotAdoptStalePoolError(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{
			IPs:   []string{"1.1.1.1"},
			Error: "上一轮的旧错误", ErrorAt: time.Now().Add(-time.Hour),
		},
		proxyCandidateSnapshot{IPs: []string{"3.3.3.3"}},
	)
	optimizeDeps.apply = func(_ *app, _ context.Context) bool { return false }
	rep := f.run()
	if rep.Error != "" {
		t.Fatalf("旧错误被当成本轮失败：%q", rep.Error)
	}
}

// 两个动作必须互斥：一个在跑时，另一个入口要么 409、要么 429，
// 绝不能回 202 让前端以为开跑了。
func TestOptimizeHTTPRejectsOverlap(t *testing.T) {
	a := &app{}
	a.revalidate = revalidateState{Running: true}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cfnat/optimize", strings.NewReader("{}"))
	a.handleOptimize(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("测试在跑时立即优选 = %d, want 409", rec.Code)
	}

	a = &app{}
	a.optimize = optimizeState{Running: true}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/cfnat/optimize", strings.NewReader("{}"))
	a.handleOptimize(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("优选已在跑时再点 = %d, want 429", rec.Code)
	}

	// 反过来：立即优选在跑时，「当前测试」入口回 409。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/cfnat/revalidate", strings.NewReader("{}"))
	a.handleRevalidate(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("优选在跑时当前测试 = %d, want 409", rec.Code)
	}
}

// GET 是给前端轮询的，必须回 200 且带 running 字段。
func TestOptimizeGETReturnsProgress(t *testing.T) {
	a := &app{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cfnat/optimize", nil)
	a.handleOptimize(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if _, ok := body["running"]; !ok {
		t.Fatalf("响应缺 running 字段: %v", body)
	}
}

// 未启用 PROXY_AUTO_APPLY 时，立即优选给出明确提示，而不是空跑一轮。
func TestOptimizeRequiresAutoApply(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{IPs: []string{"1.1.1.1"}},
		proxyCandidateSnapshot{IPs: []string{"3.3.3.3"}},
	)
	t.Setenv("PROXY_AUTO_APPLY", "false")
	rep := f.run()
	if rep.Error == "" || !strings.Contains(rep.Error, "PROXY_AUTO_APPLY") {
		t.Fatalf("未启用时应明确报错，实际 %q", rep.Error)
	}
	if f.applied != 0 {
		t.Fatal("未启用时不该进入漏斗")
	}
}

// 上一轮调度器留下的拦截记录不该算到本轮头上。poolBlock 是粘滞字段，
// 只有下次换池成功才清；直接读会让「候选全没过」看起来像「刚才被闸拦了」。
func TestOptimizeIgnoresStaleBlock(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{IPs: []string{"1.1.1.1"}},
		proxyCandidateSnapshot{IPs: []string{"3.3.3.3"}},
	)
	f.app.poolGuardMu.Lock()
	f.app.poolBlock = poolSwitchVerdict{Reason: "cooldown", NextAllowedAt: time.Now().Add(10 * time.Minute)}
	f.app.poolBlockAt = time.Now().Add(-time.Hour)
	f.app.poolBlockBy = "scheduler"
	f.app.poolGuardMu.Unlock()

	rep := f.run()
	if rep.Blocked != "" {
		t.Fatalf("旧拦截记录被算进本轮：%q", rep.Blocked)
	}
}

// 漏斗没提交、但在位探针在跑优选的几分钟里剔了人：池的 UpdatedAt 会变，
// 但这不是优选的战果。旧实现用时间戳判断，会把探针的动作算成「优选已应用」，
// 甚至把失败报成成功。返回值口径必须只认漏斗自己的提交。
func TestOptimizeDoesNotCreditForeignPoolChange(t *testing.T) {
	f := newOptimizeFixture(t,
		proxyActivePool{IPs: []string{"1.1.1.1"}},
		proxyCandidateSnapshot{IPs: []string{"3.3.3.3"}},
	)
	optimizeDeps.apply = func(a *app, _ context.Context) bool {
		a.activeMu.Lock()
		a.activePool = proxyActivePool{IPs: []string{"9.9.9.9"}, UpdatedAt: time.Now()}
		a.activeMu.Unlock()
		return false
	}

	rep := f.run()
	if rep.Applied {
		t.Fatal("探针剔人被算成了优选已应用")
	}
	if !rep.Changed {
		t.Fatal("池确实被别处改了，changed 应为 true")
	}
}
