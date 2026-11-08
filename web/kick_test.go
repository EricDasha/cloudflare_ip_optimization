package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 漏斗考试期间不能放行踢人：漏斗用的是开考前复制的候选快照，被踢的 IP
// 会照样通过考试、在落池时被写回来，用户的操作被静默撤销。
func TestKickRefusedWhilePoolScanRunning(t *testing.T) {
	a := &app{}
	a.proxyScanMu.Lock()
	defer a.proxyScanMu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cfnat/active/kick",
		strings.NewReader(`{"ip":"1.2.3.4"}`))
	a.handleActiveKick(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("扫描在跑时踢人 = %d, want 409", rec.Code)
	}
}
