package main

import (
	"strconv"
	"time"
)

// scanActivity 记录「此刻谁占着 proxyScanMu」。
//
// proxyScanMu 有十几个持有者：立即优选、当前测试、踢人后的自动补位、后台
// 优化器、关闭席体检、换池前的在位验活……每一个都可能跑几分钟。界面上的
// 按钮撞上这把锁时只能回一句「优选或补位正在运行」，用户无从判断到底是哪
// 一个、跑到哪一步了、还要等多久——2026-11-14 用户点了段禁用之后就是被这
// 句话挡住的，页面上没有任何地方能看出进度。
//
// 这里不改变锁的语义，只是把持有者登记下来，让 /api/status 的 scanBusy 能
// 回答「现在在跑什么」。登记是可选的：没有登记的持有者仍然会占锁，只是
// 界面会显示成未知操作。
type scanActivity struct {
	Kind      string    `json:"kind"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"startedAt"`
	Done      int       `json:"done"`
	Total     int       `json:"total"`
}

// noteScanActivity 登记一个占用 proxyScanMu 的操作，返回的闭包必须 defer。
//
// 调用点必须在**拿到锁之后**：先登记后抢锁会让「等锁」被显示成「在跑」。
func (a *app) noteScanActivity(kind, phase string) func() {
	a.scanActMu.Lock()
	a.scanAct = scanActivity{Kind: kind, Phase: phase, StartedAt: time.Now()}
	a.scanActMu.Unlock()
	return func() {
		a.scanActMu.Lock()
		a.scanAct = scanActivity{}
		a.scanActMu.Unlock()
	}
}

// setScanActivityPhase 更新阶段与进度。Kind 为空说明没有登记过持有者
// （例如后台优化器直接抢锁），此时不创建记录——凭空造一条「正在运行」
// 比不显示更糟。
func (a *app) setScanActivityPhase(kind, phase string, done, total int) {
	a.scanActMu.Lock()
	defer a.scanActMu.Unlock()
	if a.scanAct.Kind == "" {
		return
	}
	a.scanAct.Phase = phase
	a.scanAct.Done = done
	a.scanAct.Total = total
}

// scanActivitySnapshot 给 /api/status 用。busy=false 时其余字段全部省略，
// 前端只需判断一个布尔值。
func (a *app) scanActivitySnapshot() map[string]any {
	a.scanActMu.Lock()
	act := a.scanAct
	a.scanActMu.Unlock()
	if act.Kind == "" {
		return map[string]any{"busy": false}
	}
	return map[string]any{
		"busy":       true,
		"kind":       act.Kind,
		"phase":      act.Phase,
		"startedAt":  act.StartedAt,
		"elapsedSec": int(time.Since(act.StartedAt).Seconds()),
		"done":       act.Done,
		"total":      act.Total,
	}
}

// scanBusyMessage 把快照翻成一句人话，给按钮的冲突提示复用。
// 拿不到具体操作时返回空串，调用方退回原来的笼统提示。
func scanBusyMessage(snapshot map[string]any) string {
	if busy, _ := snapshot["busy"].(bool); !busy {
		return ""
	}
	kind, _ := snapshot["kind"].(string)
	phase, _ := snapshot["phase"].(string)
	labels := map[string]string{
		"optimize":       "立即优选",
		"revalidate":     "当前测试",
		"refill":         "踢人后的自动补位",
		"background":     "后台优化器",
		"disabled-bench": "关闭席体检",
		"throne":         "在位验活",
	}
	label := labels[kind]
	if label == "" {
		label = "优选"
	}
	msg := label + "正在运行"
	if phase != "" {
		msg += "（" + phase + "）"
	}
	if done, _ := snapshot["done"].(int); done > 0 {
		if total, _ := snapshot["total"].(int); total > 0 {
			msg += "，已完成 " + strconv.Itoa(done) + "/" + strconv.Itoa(total)
		}
	}
	return msg + "，等它结束再操作"
}
