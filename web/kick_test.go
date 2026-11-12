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

// 顶替/勾选/扶持与踢人同一口径：漏斗考试期间放行它们，刚做的手动操作
// 会在漏斗落池时被开考前的旧快照整批覆盖（顶替的 IP 直接丢、锁位被清）。
func TestManualPoolOpsRefusedWhilePoolScanRunning(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    string
		handler func(a *app, w http.ResponseWriter, r *http.Request)
	}{
		{"顶替", "/api/cfnat/proxy-promote", `{"ip":"1.2.3.4"}`,
			func(a *app, w http.ResponseWriter, r *http.Request) { a.handleProxyPromote(w, r) }},
		{"勾选", "/api/cfnat/active/select", `{"enabled":["1.2.3.4"],"priority":[],"lock":false}`,
			func(a *app, w http.ResponseWriter, r *http.Request) { a.handleActiveSelect(w, r) }},
		{"扶持", "/api/cfnat/active/endorse", `{"members":"1.2.3.4"}`,
			func(a *app, w http.ResponseWriter, r *http.Request) { a.handleActiveEndorse(w, r) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &app{}
			a.proxyScanMu.Lock()
			defer a.proxyScanMu.Unlock()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			tc.handler(a, rec, req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("扫描在跑时%s = %d, want 409", tc.name, rec.Code)
			}
		})
	}
}
