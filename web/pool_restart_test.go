package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// 同参不重启的判据必须两头都硬：进程在跑且参数逐字相同，才允许跳过重启。
func TestShouldSkipCFnatRestart(t *testing.T) {
	cfg := defaultCFnatConfig()
	cfg.Fixed = "1.1.1.1,edge.example.com"
	cfg.Priority = "1.1.1.1"
	want := cfg.args()

	running := processStatus{Running: true, Args: append([]string(nil), want...)}
	if !shouldSkipCFnatRestart(running, cfg.args()) {
		t.Fatal("参数逐字相同且进程在跑，应跳过重启")
	}

	stopped := processStatus{Running: false, Args: append([]string(nil), want...)}
	if shouldSkipCFnatRestart(stopped, cfg.args()) {
		t.Fatal("进程未运行时跳过重启会让新池没人转发，必须重启")
	}

	// 顺序不同也算不同：args() 输出顺序固定，顺序变了说明构造逻辑有别。
	shuffled := append([]string(nil), want...)
	shuffled[0], shuffled[1] = shuffled[1], shuffled[0]
	if sameCFnatArgs(shuffled, want) {
		t.Fatal("顺序不同不应判为相同")
	}

	changedCfg := defaultCFnatConfig()
	changedCfg.Fixed = "1.1.1.1,other.example.com"
	changedCfg.Priority = "1.1.1.1"
	changed := processStatus{Running: true, Args: changedCfg.args()}
	if shouldSkipCFnatRestart(changed, cfg.args()) {
		t.Fatal("-fixed 变了必须重启")
	}

	if sameCFnatArgs([]string{"-fixed", "a"}, []string{"-fixed", "a", "-num", "1"}) {
		t.Fatal("长度不同不应判为相同")
	}
}

// 闸门不能把服务拦死：IP 全无、域名又全被判坏时，池级兜底必须放行原域名，
// 否则 planFixedMembers 拿到空集合直接报错，1234 上一个目标都没有。
func TestGatePoolDomainsFallsBackWhenNoIPAndAllDomainsBlocked(t *testing.T) {
	a := newProbeTestApp(t)
	a.domainQuality["dead.example.com"] = freshRecord(domainStateDead)

	pool := proxyActivePool{Domains: []string{"dead.example.com"}}
	got := a.gatePoolDomains(pool)
	if !reflect.DeepEqual(got, []string{"dead.example.com"}) {
		t.Fatalf("无 IP 且域名全坏时应兜底放行，got %v", got)
	}

	// 还有可用 IP 时不兜底：坏域名该拦就拦，别拿它凑数。
	withIP := proxyActivePool{IPs: []string{"1.1.1.1"}, Domains: []string{"dead.example.com"}}
	if got := a.gatePoolDomains(withIP); len(got) != 0 {
		t.Fatalf("有可用 IP 时不得兜底坏域名，got %v", got)
	}
}

// 二进制），而不是以「参数相同」为由静默落盘。静默落盘的后果是池子改了、
// 1234 上却没人转发，而接口一路报成功。
func TestApplyPoolLockedRestartsWhenProcessDown(t *testing.T) {
	a := &app{
		dataDir: t.TempDir(),
		cfnat:   newManagedProcess("cfnat", filepath.Join(t.TempDir(), "missing-cfnat")),
	}
	pool := proxyActivePool{
		IPs:       []string{"1.1.1.1"},
		Domains:   []string{"edge.example.com"},
		UpdatedAt: time.Now(),
	}
	// 把参数预先写成与目标一致：若判据只看参数不看运行状态，会误跳过重启。
	fixed, priority, err := planFixedMembers(pool)
	if err != nil {
		t.Fatalf("planFixedMembers: %v", err)
	}
	cfg := defaultCFnatConfig()
	cfg.Fixed = strings.Join(fixed, ",")
	cfg.Priority = strings.Join(priority, ",")
	a.cfnat.args = cfg.args()

	if err := a.applyPoolLocked(pool, pool); err == nil {
		t.Fatal("进程未运行时应尝试启动，期望启动失败并返回错误")
	}
}

// 成功换池必须清掉粘滞的 pool.Error：调用方传 pool:=current，旧报错
// 会一路带进落盘，GUI 在换池成功后仍挂着上一轮的拦截原因。
func TestApplyPoolLockedClearsStalePoolError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-cfnat.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	a := &app{dataDir: dir, cfnat: newManagedProcess("cfnat", script)}
	pool := proxyActivePool{
		IPs:       []string{"1.1.1.1"},
		Error:     "上一轮拦截",
		ErrorAt:   time.Now(),
		UpdatedAt: time.Now(),
	}
	fixed, priority, err := planFixedMembers(pool)
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
	if err := a.applyPoolLocked(pool, pool); err != nil {
		t.Fatalf("applyPoolLocked: %v", err)
	}
	saved := a.proxyActivePoolSnapshot()
	if saved.Error != "" || !saved.ErrorAt.IsZero() {
		t.Fatalf("saved error = %q %v, want empty", saved.Error, saved.ErrorAt)
	}
}

// 回滚必须恢复正在跑的那份定制参数：种子进程用 Addr 127.0.0.1:11234、
// Num 7 启动，换池落盘失败触发回滚后，最后一次启动必须是这份参数，
// 而不是默认配置重算（0.0.0.0:1234/Num 3）+ 旧池原样 IP。
func TestApplyPoolLockedRollbackRestoresRunningArgs(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	script := filepath.Join(dir, "fake-cfnat.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+logPath+"\nexec sleep 30\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	a := &app{dataDir: dir, cfnat: newManagedProcess("cfnat", script)}
	seedCfg := defaultCFnatConfig()
	seedCfg.Addr = "127.0.0.1:11234"
	seedCfg.Num = 7
	seedCfg.Fixed = "3.3.3.3"
	if err := a.cfnat.start(dir, seedCfg.args(), ""); err != nil {
		t.Fatalf("seed cfnat start: %v", err)
	}
	defer a.cfnat.stop()
	// 等 seed 的启动记录落盘再继续：start() 返回只表示已 spawn，
	// shell 落日志有毫秒级滞后；不等它，后面 stop() 可能抢在 echo 前。
	seedDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(seedDeadline) {
		if data, err := os.ReadFile(logPath); err == nil && len(data) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 堵死落盘：proxy-active.json.tmp 预先建成目录，WriteFile 必败。
	if err := os.Mkdir(filepath.Join(dir, proxyActivePoolFile+".tmp"), 0755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	current := proxyActivePool{IPs: []string{"3.3.3.3"}, Disabled: []string{"3.3.3.3"}, UpdatedAt: time.Now()}
	pool := proxyActivePool{IPs: []string{"2.2.2.2"}, UpdatedAt: time.Now()}
	err := a.applyPoolLocked(pool, current)
	if err == nil || !strings.Contains(err.Error(), "保存生效池") {
		t.Fatalf("applyPoolLocked err = %v, want 保存生效池", err)
	}
	lines := []string{}
	// start() 返回只表示进程已 spawn，脚本落日志有毫秒级滞后，轮询等齐。
	// 注意中间那次新池启动的 echo 可能永远落不下来：落盘失败后 stop()
	// 立刻跟上，经常抢在 shell 执行 echo 之前——不断言它的行数，只
	// 断言最后一行（回滚启动常驻，echo 一定能落下来）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(logPath); err == nil {
			lines = strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) >= 2 && strings.Contains(lines[len(lines)-1], "127.0.0.1:11234") {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 start attempts (seed, rollback), got %d: %v", len(lines), lines)
	}
	// 回滚用的必须是正在跑的那份定制参数，而不是默认配置重算。
	last := lines[len(lines)-1]
	if !strings.Contains(last, "127.0.0.1:11234") || !strings.Contains(last, "-num 7") {
		t.Fatalf("rollback did not restore running args: %q", last)
	}
	if !a.cfnat.status().Running {
		t.Fatalf("cfnat not running after rollback")
	}
}

// 无可回滚时如实报错：从未成功启动过、旧池又全员关闭，回滚无参数可用。
func TestApplyPoolLockedRollbackImpossibleIsReported(t *testing.T) {
	a := &app{
		dataDir: t.TempDir(),
		cfnat:   newManagedProcess("cfnat", filepath.Join(t.TempDir(), "missing-cfnat")),
	}
	current := proxyActivePool{IPs: []string{"3.3.3.3"}, Disabled: []string{"3.3.3.3"}, UpdatedAt: time.Now()}
	pool := proxyActivePool{IPs: []string{"2.2.2.2"}, UpdatedAt: time.Now()}
	err := a.applyPoolLocked(pool, current)
	if err == nil || !strings.Contains(err.Error(), "无可回滚") {
		t.Fatalf("expected '无可回滚' error, got %v", err)
	}
}

// 报错只写内存不落盘，重启后报错消失，与 Error/ErrorAt 的持久化结构
// 自相矛盾；且快照-改-写回会盖掉夹在中间落成的池。
func TestSetProxyPoolErrorPersists(t *testing.T) {
	dir := t.TempDir()
	a := &app{dataDir: dir, activePool: proxyActivePool{IPs: []string{"1.2.3.4"}}}
	a.setProxyPoolError("boom", nil)
	if a.activePool.Error != "boom" || a.activePool.ErrorAt.IsZero() {
		t.Fatalf("memory error = %q %v, want boom + time", a.activePool.Error, a.activePool.ErrorAt)
	}
	data, err := os.ReadFile(filepath.Join(dir, proxyActivePoolFile))
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	if !strings.Contains(string(data), "boom") || !strings.Contains(string(data), "1.2.3.4") {
		t.Fatalf("pool file = %s, want error + ips persisted", data)
	}
}

// refillGap 缺口按实际转发的成员算：5 个在位里 2 个已关闭，实际只转
// 3 个，缺口是 2 不是 0；锁位时不补。
func TestRefillGapCountsForwardingOnly(t *testing.T) {
	pool := proxyActivePool{
		IPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"},
		Disabled: []string{"4.4.4.4", "5.5.5.5"},
	}
	if got := refillGap(pool, 5); got != 2 {
		t.Fatalf("gap = %d, want 2", got)
	}
	pool.Lock = true
	if got := refillGap(pool, 5); got != 0 {
		t.Fatalf("locked gap = %d, want 0", got)
	}
	pool.Lock = false
	pool.Disabled = nil
	if got := refillGap(pool, 5); got != 0 {
		t.Fatalf("full pool gap = %d, want 0", got)
	}
}

// 换池成功后被动健康账只留当前转发成员：离池 IP 的旧记录被裁掉。
func TestApplyPoolLockedPrunesStaleActiveHealth(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-cfnat.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	a := &app{
		dataDir:      dir,
		cfnat:        newManagedProcess("cfnat", script),
		activeHealth: map[string]*activeHealthStat{"9.9.9.9": {Success: 5}, "1.1.1.1": {Success: 5}},
	}
	pool := proxyActivePool{IPs: []string{"1.1.1.1"}, UpdatedAt: time.Now()}
	fixed, priority, err := planFixedMembers(pool)
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
	if err := a.applyPoolLocked(pool, pool); err != nil {
		t.Fatalf("applyPoolLocked: %v", err)
	}
	a.activeHealthMu.Lock()
	_, stale := a.activeHealth["9.9.9.9"]
	_, kept := a.activeHealth["1.1.1.1"]
	a.activeHealthMu.Unlock()
	if stale {
		t.Fatalf("stale activeHealth entry for 9.9.9.9 survived pool switch")
	}
	if !kept {
		t.Fatalf("current member 1.1.1.1 pruned by mistake")
	}
}

// F4：换池提交与报错落盘并发时，磁盘与内存最终必须一致——
// 旧实现里 setProxyPoolError「解锁后再落盘」，commitPoolWithoutRestart
// 「先落盘再上锁写内存」，两边的窗口互相覆盖，重启后池会静默回退
// 到旧成员。现两边都在同一把 activeMu 下完成「落盘+内存写回」。
func TestPoolFileAndMemoryAgreeUnderConcurrentWrites(t *testing.T) {
	a := &app{
		dataDir:    t.TempDir(),
		activePool: proxyActivePool{IPs: []string{"1.1.1.1"}, UpdatedAt: time.Now()},
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			pool := proxyActivePool{
				IPs:       []string{"1.1.1.1", "2.2.2.2"},
				UpdatedAt: time.Now(),
			}
			if i%2 == 0 {
				pool.IPs = []string{"1.1.1.1", "3.3.3.3"}
			}
			_ = a.commitPoolWithoutRestart(pool)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.setProxyPoolError("并发报错", nil)
		}
	}()
	wg.Wait()
	saved := a.proxyActivePoolSnapshot()
	data, err := os.ReadFile(filepath.Join(a.dataDir, proxyActivePoolFile))
	if err != nil {
		t.Fatalf("读池文件: %v", err)
	}
	var onDisk proxyActivePool
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("解析池文件: %v", err)
	}
	if !reflect.DeepEqual(onDisk.IPs, saved.IPs) || onDisk.Error != saved.Error {
		t.Fatalf("磁盘与内存不一致：磁盘 IPs=%v Error=%q，内存 IPs=%v Error=%q",
			onDisk.IPs, onDisk.Error, saved.IPs, saved.Error)
	}
}
