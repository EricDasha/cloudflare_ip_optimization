package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// manualScanPriority 让用户操作优先于后台探测。
//
// 背景：proxyScanMu 是一把普通互斥锁，后台质量调度器可能抱着它跑几分钟
// （WS 45 秒 + VLESS 最多 10 分钟）。用户此时点「关段」「踢人」「立即优选」
// 只能吃 409，体感就是「卡住了还不让我操作」。
//
// 这里用一个可取消的后台上下文解决：用户操作开始时取消当前后台扫描，
// 后台循环在下一次检查点退出；用户操作结束后，下一轮后台再创建新上下文。
// 后台任何落池动作都必须先检查 ctx.Err()，避免被取消后仍把旧快照写回去。
type manualScanPriority struct {
	mu               sync.Mutex
	manualOps        int
	backgroundCtx    context.Context
	backgroundCancel context.CancelFunc
}

// beginManualScan 标记一个用户操作开始，并取消正在跑的后台探测。
// 返回的函数必须在用户操作结束时调用，恢复后台探测资格。
func (a *app) beginManualScan() func() {
	a.scanPriority.mu.Lock()
	a.scanPriority.manualOps++
	if a.scanPriority.backgroundCancel != nil {
		a.scanPriority.backgroundCancel()
		a.scanPriority.backgroundCancel = nil
		a.scanPriority.backgroundCtx = nil
	}
	a.scanPriority.mu.Unlock()
	return func() {
		a.scanPriority.mu.Lock()
		if a.scanPriority.manualOps > 0 {
			a.scanPriority.manualOps--
		}
		a.scanPriority.mu.Unlock()
	}
}

// backgroundScanContext 返回当前后台扫描共用的上下文。
// 第二个返回值为 false 表示用户操作正在进行，后台应直接让位。
func (a *app) backgroundScanContext() (context.Context, bool) {
	a.scanPriority.mu.Lock()
	defer a.scanPriority.mu.Unlock()
	if a.scanPriority.manualOps > 0 {
		return nil, false
	}
	if a.scanPriority.backgroundCtx == nil {
		a.scanPriority.backgroundCtx, a.scanPriority.backgroundCancel = context.WithCancel(context.Background())
	}
	return a.scanPriority.backgroundCtx, true
}

func (a *app) manualScanPending() bool {
	a.scanPriority.mu.Lock()
	defer a.scanPriority.mu.Unlock()
	return a.scanPriority.manualOps > 0
}

// lockScanForManual 是用户操作抢扫描锁的统一入口。
//
// 与后台的 TryLock 不同：用户操作先登记优先级（取消后台扫描），再排队等锁。
// 后台在检查点看到取消后会尽快退出，正常情况下几十秒内就能拿到；真等不到
// 才返回 429，而不是一上来就 409。返回的释放函数同时归还扫描锁和优先级。
func (a *app) lockScanForManual(w http.ResponseWriter) (func(), bool) {
	done := a.beginManualScan()
	if !lockTimeout(&a.proxyScanMu, 15*time.Second) {
		done()
		http.Error(w, scanBusyMessageOrDefault(a.scanActivitySnapshot(), "后台扫描正在退出，请稍后再试"), http.StatusTooManyRequests)
		return nil, false
	}
	return func() {
		a.proxyScanMu.Unlock()
		done()
	}, true
}
