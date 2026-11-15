package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 只有整段都被关掉才算「已关闭」。段里还剩一个成员在转发就必须算开着，
// 否则界面会挂上「整段已停」的标签，而实际还有流量在走。
func TestPoolDisabledSegmentsOnlyListsFullyOffSegments(t *testing.T) {
	pool := proxyActivePool{
		IPs: []string{
			"43.155.82.88",   // 43.155.0.0/16 两个成员，只关了一个
			"43.155.90.10",   //
			"35.78.243.103",  // 35.78.0.0/16 两个成员，全关
			"35.78.1.2",      //
			"13.231.221.179", // 13.231.0.0/16 没关
		},
		Disabled: []string{"43.155.82.88", "35.78.243.103", "35.78.1.2"},
	}
	got := poolDisabledSegments(pool)
	if len(got) != 1 || got[0] != "35.78.0.0/16" {
		t.Fatalf("got %v, want [35.78.0.0/16]", got)
	}
	// 空池不能 panic，也不能凭空返回段。
	if got := poolDisabledSegments(proxyActivePool{}); len(got) != 0 {
		t.Fatalf("empty pool got %v, want none", got)
	}
}

// 撞锁提示必须说清楚是谁在占锁。原来那句「优选或补位正在运行」把
// 当前测试、后台优化器、关闭席体检全都含糊成了「优选」。
func TestScanBusyMessageNamesTheHolder(t *testing.T) {
	a := &app{}
	release := a.noteScanActivity("revalidate", "验证 IP")
	a.setScanActivityPhase("revalidate", "验证 IP", 3, 10)
	msg := scanBusyMessage(a.scanActivitySnapshot())
	release()
	if !strings.Contains(msg, "当前测试") {
		t.Fatalf("msg = %q, want 当前测试", msg)
	}
	if !strings.Contains(msg, "3/10") {
		t.Fatalf("msg = %q, want 进度 3/10", msg)
	}
	// 没有持有者时不编造提示，调用方自己决定回什么。
	if msg := scanBusyMessage(a.scanActivitySnapshot()); msg != "" {
		t.Fatalf("idle msg = %q, want empty", msg)
	}
}

// 没有登记过的持有者不能被 setScanActivityPhase 凭空造出一条「正在运行」。
func TestScanActivityPhaseIgnoredWithoutHolder(t *testing.T) {
	a := &app{}
	a.setScanActivityPhase("optimize", "刷新候选", 0, 0)
	if snap := a.scanActivitySnapshot(); snap["busy"] != false {
		t.Fatalf("busy = %v, want false", snap["busy"])
	}
}

// 扫描在跑时关段不再被 409 挡回：用户操作先取消后台扫描，再排队等锁，
// 锁一释放就继续。这里用空池验证它确实等到了锁，而不是立刻拒绝。
func TestSegmentToggleWaitsForPoolScan(t *testing.T) {
	a := &app{}
	a.proxyScanMu.Lock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		a.proxyScanMu.Unlock()
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cfnat/active/segment",
		strings.NewReader(`{"segment":"35.78.0.0/16"}`))
	started := time.Now()
	a.handleSegmentToggle(rec, req)
	if rec.Code == http.StatusConflict || rec.Code == http.StatusTooManyRequests {
		t.Fatalf("扫描在跑时关段被拒绝 = %d, want 排队后执行", rec.Code)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("关段没有等待后台扫描退出，耗时 %v", elapsed)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("空池关段 = %d, want 404", rec.Code)
	}
}

func TestSegmentToggleValidatesInput(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"缺段标识", `{"disabled":true}`, http.StatusBadRequest},
		{"请求体损坏", `{`, http.StatusBadRequest},
		{"段内无在位成员", `{"segment":"203.0.113.0/24"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &app{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/cfnat/active/segment",
				strings.NewReader(tc.body))
			a.handleSegmentToggle(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d (%s), want %d", rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
			}
		})
	}
	// GET 一律拒绝：这个端点会改转发面，不能靠预取/刷新触发。
	a := &app{}
	rec := httptest.NewRecorder()
	a.handleSegmentToggle(rec, httptest.NewRequest(http.MethodGet, "/api/cfnat/active/segment", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
}
