package main

import (
	"context"
	"errors"
	"time"
)

// ============================================================================
// 立即优选（全量重挑）：刷新候选 → 三层漏斗 → 换池
// ============================================================================
//
// 2026-11-05 之前，「立即优选」按钮走的就是这条全量流程；后来它被改成
// 「在位体检」，原因很实在——用户点按钮时多数是「现在很卡」，需要的是清理
// 坏成员，而不是去外面捞新的。问题在于改完只留下了体检，把「外面有没有更好
// 的」整条能力扔掉了，界面上连个入口都没有。
//
// 这里把两件事重新拆开：
//   - 当前测试（/api/cfnat/revalidate）：暂停转发，验在位成员，剔坏，恢复。
//   - 立即优选（本文件）：刷新候选，跑三层漏斗，按质量换池。
//
// 全量流程要跑几分钟（刷新 90s 上限 + 初筛/精筛/终审），所以和体检一样
// 走异步：立刻回 202，前端轮询 GET 同一路径拿进度与上一轮报告。

// optimizeRefreshBudget 是候选刷新的总预算：排队等 refreshMu 最多 60s，
// 抓取自身另有 90s 上限。只留 120s 会在排队 30s 以上时把抓取窗口截断，
// 慢源必然超时、优选退化成拿旧候选重考；这里按「60+90」再加 30s 余量。
const optimizeRefreshBudget = 180 * time.Second

// optimizeReport 是一轮立即优选的完整结论。
//
// 只报「做了什么」不够：被换池闸拦下时，池子没动，用户看到的必须是
// 「为什么没换」，而不是一句含糊的「优选完成」。所以 blocked 单独一列。
type optimizeReport struct {
	StartedAt     time.Time `json:"startedAt"`
	FinishedAt    time.Time `json:"finishedAt"`
	Refreshed     bool      `json:"refreshed"`
	Candidates    int       `json:"candidates"`
	BeforeIPs     int       `json:"beforeIps"`
	BeforeDomains int       `json:"beforeDomains"`
	AfterIPs      int       `json:"afterIps"`
	AfterDomains  int       `json:"afterDomains"`
	Applied       bool      `json:"applied"`
	Changed       bool      `json:"changed"`
	// Blocked 预留给「换池闸拦截」。手动优选不走闸门，当前不会赋值；
	// 字段保留是为了 /api 输出形状稳定，前端也仍兼容这个分支。
	Blocked string `json:"blocked,omitempty"`
	Note    string `json:"note,omitempty"`
	Error   string `json:"error,omitempty"`
}

// optimizeState 是正在进行的立即优选的实时进度，供 /api/status 观测。
type optimizeState struct {
	Running   bool
	StartedAt time.Time
	Phase     string
	Last      *optimizeReport
	LastAt    time.Time
}

// beginOptimize 标记一轮立即优选开始。同一时刻只允许一轮。
func (a *app) beginOptimize() bool {
	a.optimizeMu.Lock()
	defer a.optimizeMu.Unlock()
	if a.optimize.Running {
		return false
	}
	a.optimize = optimizeState{Running: true, StartedAt: time.Now(), Phase: "刷新候选"}
	return true
}

func (a *app) setOptimizePhase(phase string) {
	a.optimizeMu.Lock()
	a.optimize.Phase = phase
	a.optimizeMu.Unlock()
}

func (a *app) endOptimize(report optimizeReport) {
	a.optimizeMu.Lock()
	finished := report
	a.optimize = optimizeState{Last: &finished, LastAt: time.Now()}
	a.optimizeMu.Unlock()
}

// optimizeStatus 给 /api/status 用的只读快照。
func (a *app) optimizeStatus() map[string]any {
	a.optimizeMu.Lock()
	defer a.optimizeMu.Unlock()
	state := a.optimize
	out := map[string]any{
		"running": state.Running,
		"phase":   state.Phase,
	}
	if state.Running {
		out["startedAt"] = state.StartedAt
	}
	return out
}

// optimizeDetail 是给 /api/cfnat/optimize 的完整快照：进度 + 上一轮报告。
func (a *app) optimizeDetail() map[string]any {
	out := a.optimizeStatus()
	a.optimizeMu.Lock()
	last, lastAt := a.optimize.Last, a.optimize.LastAt
	a.optimizeMu.Unlock()
	if last != nil {
		out["last"] = last
		out["lastAt"] = lastAt
	}
	return out
}

// optimizeIsRunning 供「当前测试」入口做互斥判断。
func (a *app) optimizeIsRunning() bool {
	a.optimizeMu.Lock()
	defer a.optimizeMu.Unlock()
	return a.optimize.Running
}

// revalidateIsRunning 供「立即优选」入口做互斥判断。
func (a *app) revalidateIsRunning() bool {
	a.revalidateMu.Lock()
	defer a.revalidateMu.Unlock()
	return a.revalidate.Running
}

// claimOptimize 原子地完成「检查当前测试 → 抢优选席位」。
//
// 返回 conflict 说明被谁挡下："" 表示成功，否则是 "revalidate"（应回 409）
// 或 "optimize"（应回 429）。分两步做（先读后抢）会留下并发窗口，
// 两个请求可能同时看到对方空闲然后一起开跑——都动 cfnat，不是小事。
func (a *app) claimOptimize() (ok bool, conflict string) {
	a.actionMu.Lock()
	defer a.actionMu.Unlock()
	if a.revalidateIsRunning() {
		return false, "revalidate"
	}
	if !a.beginOptimize() {
		return false, "optimize"
	}
	return true, ""
}

// claimRevalidate 是 claimOptimize 的镜像。
func (a *app) claimRevalidate(target int) (ok bool, conflict string) {
	a.actionMu.Lock()
	defer a.actionMu.Unlock()
	if a.optimizeIsRunning() {
		return false, "optimize"
	}
	if !a.beginRevalidate(target) {
		return false, "revalidate"
	}
	return true, ""
}

// optimizeDependencies 把「刷新候选」和「漏斗换池」抽出来，只为单测能
// 伪造成功/失败/被拦三种结局——真跑一轮要连外网、要重启 cfnat，单测既
// 不稳定也验证不了分支。生产永远用下面的默认实现。
type optimizeDependencies struct {
	refresh    func(a *app, ctx context.Context) (bool, error)
	apply      func(a *app, ctx context.Context) bool
	snapshot   func(a *app) proxyActivePool
	candidates func(a *app) proxyCandidateSnapshot
}

var optimizeDeps = optimizeDependencies{
	refresh: func(a *app, ctx context.Context) (bool, error) {
		return a.refreshProxyCandidates(ctx)
	},
	apply: func(a *app, ctx context.Context) bool {
		// manual=true：用户显式点了按钮，按手动采用的口径放行，
		// 不与后台调度器的 30 分钟冷却互相顶牛。
		return a.autoApplyProxyPool(ctx, true)
	},
	snapshot: func(a *app) proxyActivePool {
		return a.proxyActivePoolSnapshot()
	},
	candidates: func(a *app) proxyCandidateSnapshot {
		return a.proxyCandidateSnapshot()
	},
}

// runOptimize 执行一轮全量重挑。调用方必须先 beginOptimize 抢到席位；
// 本函数只负责跑完并把席位与报告交回去。
//
// 刷新失败不直接终止：手里已有的候选快照依然可以重考，这比「因为一个源
// 拉不到就什么都不做」有用。真正没得跑的情况只有「候选为空」。
func (a *app) runOptimize(parent context.Context) (report optimizeReport) {
	report = optimizeReport{StartedAt: time.Now()}
	before := optimizeDeps.snapshot(a)
	report.BeforeIPs = len(before.IPs)
	report.BeforeDomains = len(before.Domains)
	defer func() {
		if report.FinishedAt.IsZero() {
			report.FinishedAt = time.Now()
		}
		a.endOptimize(report)
	}()

	// 池锁（只转固定的）下，漏斗跑完也会在提交前被静默丢弃，界面上只会显示
	// 「没有可应用的变更」。在开跑前就拦下：既省掉几分钟探测，也把真正原因
	// 说清楚。锁是用户亲手按下的，不该由自动流程替他决定。
	if before.Lock {
		report.Error = "生效池已锁定（只转固定的），先解锁再优选"
		return
	}

	// ---- 刷新候选 ----
	// 与 post-cfdata 同理：刷新会把父 ctx 的预算吃光，漏斗必须另开一份，
	// 否则精筛阶段的每个拨号都会瞬间 i/o timeout（看起来像全网断线）。
	a.setOptimizePhase("刷新候选")
	refreshCtx, cancelRefresh := context.WithTimeout(parent, optimizeRefreshBudget)
	ok, err := optimizeDeps.refresh(a, refreshCtx)
	cancelRefresh()
	switch {
	case err != nil && errors.Is(err, errProxyCandidatesBusy):
		report.Note = "候选刷新被占用，改用现有候选"
	case err != nil:
		report.Note = "候选刷新失败，改用现有候选: " + err.Error()
	default:
		report.Refreshed = ok
	}

	// 注意：这里要看的是「候选名单」，不是「在位池」。用 activePool 判空会
	// 得出「池里有 5 个所以能优选」的结论，可实际上漏斗无米下锅。
	candidates := optimizeDeps.candidates(a)
	report.Candidates = len(candidates.IPs)
	if len(candidates.IPs) == 0 {
		report.Error = "候选为空，没有可优选的 IP"
		return
	}
	if cfg := defaultProxyAutoConfig(); !cfg.Enabled {
		report.Error = "PROXY_AUTO_APPLY 未启用，立即优选不可用"
		return
	}

	// ---- 三层漏斗 ----
	a.setOptimizePhase("漏斗考试")
	applyCtx, cancelApply := newAutoApplyContext()
	applied := optimizeDeps.apply(a, applyCtx)
	cancelApply()

	after := optimizeDeps.snapshot(a)
	report.AfterIPs = len(after.IPs)
	report.AfterDomains = len(after.Domains)
	// 只认漏斗自己的提交结果。用 UpdatedAt 前后比较会把在位探针在这几分钟里
	// 剔人的动作算成优选战果，甚至让「考试失败」显示成「优选完成」。
	report.Applied = applied
	report.Changed = !sameIPSet(before.IPs, after.IPs) || !sameStringSet(before.Domains, after.Domains)
	// 手动优选按 manual 口径绕过换池闸，本轮不可能产生 poolBlock。刷新阶段
	// 最长三分钟、只持 refreshMu，每 5 分钟一轮的质量调度器完全可能在这期间
	// 记下一笔冷却拦截；按时间戳归属会把它算到本轮头上，让一次已经落地的
	// 优选返回 503。别人的账就让别人自己报。
	//
	// 池错误同理：pool.Error 是粘滞字段，只有等时间的比较还不够——旧账冒充
	// 本轮失败，与「假捷报」是同一类问题，只是一头一尾。这里只认本轮新写的。
	if !report.Applied && after.Error != "" && !after.ErrorAt.Before(report.StartedAt) {
		report.Error = after.Error
	}
	return report
}
