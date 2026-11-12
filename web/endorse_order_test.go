package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 扶持即置顶：新扶持的成员必须排在已有置顶成员之前。
func TestHandleActiveEndorsePinsToFront(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-cfnat.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	a := &app{dataDir: dir, cfnat: newManagedProcess("cfnat", script)}
	a.activePool = proxyActivePool{
		IPs:       []string{"1.1.1.1", "2.2.2.2"},
		Priority:  []string{"1.1.1.1"},
		UpdatedAt: time.Now(),
	}
	// 先以当前池参数启动 fake cfnat，扶持后走重启路径。
	fixed, priority, err := planFixedMembers(a.activePool)
	if err != nil {
		t.Fatalf("planFixedMembers: %v", err)
	}
	cfg := defaultCFnatConfig()
	cfg.Fixed = strings.Join(fixed, ",")
	cfg.Priority = strings.Join(priority, ",")
	cfg.Fallback = strings.TrimSpace(env("CFNAT_FALLBACK", ""))
	if err := a.cfnat.start(dir, cfg.args(), ""); err != nil {
		t.Fatalf("seed start: %v", err)
	}
	defer a.cfnat.stop()

	req := httptest.NewRequest(http.MethodPost, "/api/cfnat/active/endorse",
		strings.NewReader(`{"members":"3.3.3.3"}`))
	rec := httptest.NewRecorder()
	a.handleActiveEndorse(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("endorse status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	saved := a.proxyActivePoolSnapshot()
	if len(saved.Priority) != 2 || saved.Priority[0] != "3.3.3.3" || saved.Priority[1] != "1.1.1.1" {
		t.Fatalf("Priority = %v, want [3.3.3.3 1.1.1.1]", saved.Priority)
	}
}
