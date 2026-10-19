package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// handleActiveSelect POST /api/cfnat/active/select
//
// 用户亲手决定谁承载流量：提交勾选集合（enabled）、置顶集合（priority）与
// 锁位开关（lock），立即换池并重启 cfnat。
//
// 语义边界：
//   - enabled 之外的在位成员写入 Disabled：仍在位、仍可体检，但不进 -fixed；
//   - priority 成员排在 -fixed 最前（扶持登基自动入列）；
//   - lock 为真时只转发 priority 成员，其余一律不转——这是「劣质成员分走流量」
//     的根治阀门，由用户自己拍板。
func (a *app) handleActiveSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	// 锁序全局规范 refresh → proxyScan → cfnatCtl；只拿换池锁，等待而非拒绝。
	if !lockTimeout(&a.cfnatCtlMu, 15*time.Second) {
		http.Error(w, "换池操作正在运行，请稍后再试", http.StatusTooManyRequests)
		return
	}
	defer a.cfnatCtlMu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var body struct {
		Enabled  []string `json:"enabled"`
		Priority []string `json:"priority"`
		Lock     bool     `json:"lock"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "无效的请求体", http.StatusBadRequest)
		return
	}

	current := a.proxyActivePoolSnapshot()
	all := append(append([]string(nil), current.IPs...), current.Domains...)
	if len(all) == 0 {
		http.Error(w, "生效池为空，先扶持登基或点「立即优选」", http.StatusBadRequest)
		return
	}
	known := make(map[string]bool, len(all))
	for _, member := range all {
		known[member] = true
	}
	enabled := make(map[string]bool, len(body.Enabled))
	for _, member := range body.Enabled {
		if known[member] {
			enabled[member] = true
		}
	}
	disabled := make([]string, 0, len(all))
	for _, member := range all {
		if !enabled[member] {
			disabled = append(disabled, member)
		}
	}
	priority := make([]string, 0, len(body.Priority))
	for _, member := range body.Priority {
		if known[member] && !containsString(priority, member) {
			priority = append(priority, member)
		}
	}
	if len(disabled) == len(all) {
		http.Error(w, "至少要保留一个转发成员", http.StatusBadRequest)
		return
	}

	pool := current
	pool.UpdatedAt = time.Now()
	pool.Disabled = disabled
	pool.Priority = priority
	pool.Lock = body.Lock
	// 先用同一套规则试算，绝不用空 -fixed 重启 cfnat。
	if _, _, err := planFixedMembers(pool); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.applyPoolLocked(pool, current); err != nil {
		http.Error(w, "换池失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.markPoolSwitched(pool.UpdatedAt)
	log.Printf("active pool select: enabled=%d disabled=%d priority=%d lock=%t",
		len(enabled), len(disabled), len(priority), body.Lock)
	writeJSON(w, map[string]any{
		"ok": true, "enabled": len(enabled), "disabled": len(disabled),
		"priority": len(priority), "lock": body.Lock,
	})
}
