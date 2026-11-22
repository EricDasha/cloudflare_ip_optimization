package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

// 用户操作是最高顺位：后台扫描持锁时不能立刻 409，而是先取消后台扫描、
// 排队等锁；锁一释放就继续执行，不能把用户挡在门外。
func TestManualScanLockWaitsInsteadOfRejecting(t *testing.T) {
	a := &app{}
	a.proxyScanMu.Lock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		a.proxyScanMu.Unlock()
	}()

	rec := httptest.NewRecorder()
	started := time.Now()
	release, ok := a.lockScanForManual(rec)
	if !ok {
		t.Fatalf("用户操作被拒绝 = %d, want 排队后执行: %s", rec.Code, rec.Body.String())
	}
	defer release()
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("用户操作没有等待后台扫描退出，耗时 %v", elapsed)
	}
}

// 用户操作一开始，后台扫描的上下文必须被取消；后台据此在检查点让位。
func TestManualScanCancelsBackgroundContext(t *testing.T) {
	a := &app{}
	bgCtx, ok := a.backgroundScanContext()
	if !ok {
		t.Fatal("后台上下文应可创建")
	}
	if bgCtx.Err() != nil {
		t.Fatal("初始后台上下文不应已取消")
	}

	done := a.beginManualScan()
	select {
	case <-bgCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("用户操作开始后后台上下文未被取消")
	}
	if !a.manualScanPending() {
		t.Fatal("用户操作期间 manualScanPending 应为 true")
	}
	done()
	if a.manualScanPending() {
		t.Fatal("用户操作结束后 manualScanPending 应为 false")
	}
	if _, ok := a.backgroundScanContext(); !ok {
		t.Fatal("用户操作结束后后台上下文应可重建")
	}
}
