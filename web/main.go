package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFS embed.FS

const (
	defaultDataDir                = "/data"
	maxLogBytes                   = 512 * 1024
	proxyCandidateRefreshInterval = 6 * time.Hour
	proxyCandidateCacheFile       = "proxy-candidates.json"
	proxyActivePoolFile           = "proxy-active.json"
)

type app struct {
	dataDir     string
	cfnat       *managedProcess
	cfdata      *managedProcess
	proxyScanMu sync.Mutex
	cfnatCtlMu  sync.Mutex
	candidateMu sync.RWMutex
	refreshMu   sync.Mutex
	candidates  proxyCandidateSnapshot
	activeMu    sync.RWMutex
	activePool  proxyActivePool
}

type managedProcess struct {
	mu        sync.Mutex
	name      string
	bin       string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	startedAt time.Time
	exitedAt  time.Time
	exitCode  *int
	lastError string
	args      []string
	logs      *logBuffer
}

type processStatus struct {
	Name      string   `json:"name"`
	Running   bool     `json:"running"`
	PID       int      `json:"pid,omitempty"`
	StartedAt string   `json:"startedAt,omitempty"`
	ExitedAt  string   `json:"exitedAt,omitempty"`
	ExitCode  *int     `json:"exitCode,omitempty"`
	LastError string   `json:"lastError,omitempty"`
	Args      []string `json:"args,omitempty"`
}

type logBuffer struct {
	mu  sync.Mutex
	buf string
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf += string(p)
	if len(l.buf) > maxLogBytes {
		l.buf = l.buf[len(l.buf)-maxLogBytes:]
		if idx := strings.IndexByte(l.buf, '\n'); idx >= 0 && idx+1 < len(l.buf) {
			l.buf = l.buf[idx+1:]
		}
	}
	return len(p), nil
}

func (l *logBuffer) lines(limit int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	text := strings.TrimRight(l.buf, "\n")
	if text == "" {
		return []string{}
	}
	lines := strings.Split(text, "\n")
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
}

func (l *logBuffer) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = ""
}

func newManagedProcess(name, bin string) *managedProcess {
	return &managedProcess{name: name, bin: bin, logs: &logBuffer{}}
}

func (p *managedProcess) status() processStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := processStatus{Name: p.name, LastError: p.lastError, Args: append([]string(nil), p.args...)}
	if !p.startedAt.IsZero() {
		st.StartedAt = p.startedAt.Format(time.RFC3339)
	}
	if !p.exitedAt.IsZero() {
		st.ExitedAt = p.exitedAt.Format(time.RFC3339)
	}
	if p.exitCode != nil {
		v := *p.exitCode
		st.ExitCode = &v
	}
	if p.cmd != nil && p.cmd.Process != nil && p.exitCode == nil {
		st.Running = true
		st.PID = p.cmd.Process.Pid
	}
	return st
}

func (p *managedProcess) start(dataDir string, args []string, stdin string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil && p.exitCode == nil {
		return fmt.Errorf("%s 已在运行", p.name)
	}
	p.logs.clear()
	p.exitCode = nil
	p.lastError = ""
	p.exitedAt = time.Time{}
	p.args = append([]string(nil), args...)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, p.bin, args...)
	cmd.Dir = dataDir
	var logFile *os.File
	if f, err := os.OpenFile(filepath.Join(dataDir, p.name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		logFile = f
		mw := io.MultiWriter(p.logs, f)
		cmd.Stdout = mw
		cmd.Stderr = mw
	} else {
		_, _ = fmt.Fprintf(p.logs, "open log file failed: %v\n", err)
		cmd.Stdout = p.logs
		cmd.Stderr = p.logs
	}

	var input io.WriteCloser
	var err error
	if stdin != "" {
		input, err = cmd.StdinPipe()
		if err != nil {
			cancel()
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	if input != nil {
		go func() {
			_, _ = io.WriteString(input, stdin)
			_ = input.Close()
		}()
	}
	p.cmd = cmd
	p.cancel = cancel
	p.startedAt = time.Now()

	go func() {
		err := cmd.Wait()
		if logFile != nil {
			_ = logFile.Close()
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.exitedAt = time.Now()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		p.exitCode = &code
		if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
			p.lastError = err.Error()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			p.lastError = "stopped"
		}
	}()
	return nil
}

func (p *managedProcess) stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil || p.exitCode != nil {
		return nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	go func(proc *os.Process) {
		time.Sleep(1500 * time.Millisecond)
		_ = proc.Kill()
	}(p.cmd.Process)
	return nil
}

type cfnatConfig struct {
	Addr   string `json:"addr"`
	Code   int    `json:"code"`
	Colo   string `json:"colo"`
	Delay  int    `json:"delay"`
	Domain string `json:"domain"`
	Fixed  string `json:"fixed"`
	IPNum  int    `json:"ipnum"`
	IPs    string `json:"ips"`
	Num    int    `json:"num"`
	Port   int    `json:"port"`
	Random bool   `json:"random"`
	Task   int    `json:"task"`
	TLS    bool   `json:"tls"`
}

type cfdataConfig struct {
	ForceUpdate bool   `json:"forceUpdate"`
	IPType      int    `json:"ipType"`
	DataCenter  string `json:"dataCenter"`
	Scan        int    `json:"scan"`
	Test        int    `json:"test"`
	Port        int    `json:"port"`
	Delay       int    `json:"delay"`
}

func defaultCFnatConfig() cfnatConfig {
	return cfnatConfig{
		Addr:   env("CFNAT_ADDR", "0.0.0.0:1234"),
		Code:   envInt("CFNAT_CODE", 200),
		Colo:   env("CFNAT_COLO", ""),
		Delay:  envInt("CFNAT_DELAY", 300),
		Domain: env("CFNAT_DOMAIN", "cloudflaremirrors.com/debian"),
		Fixed:  env("CFNAT_FIXED_IPS", ""),
		IPNum:  envInt("CFNAT_IPNUM", 20),
		IPs:    env("CFNAT_IPS", "4"),
		Num:    envInt("CFNAT_NUM", 5),
		Port:   envInt("CFNAT_PORT", 443),
		Random: envBool("CFNAT_RANDOM", true),
		Task:   envInt("CFNAT_TASK", 100),
		TLS:    envBool("CFNAT_TLS", true),
	}
}

func normalizeCFnat(c cfnatConfig) cfnatConfig {
	d := defaultCFnatConfig()
	if c.Addr == "" {
		c.Addr = d.Addr
	}
	if c.Code == 0 {
		c.Code = d.Code
	}
	if c.Delay == 0 {
		c.Delay = d.Delay
	}
	if c.Domain == "" {
		c.Domain = d.Domain
	}
	if c.Fixed == "" {
		c.Fixed = d.Fixed
	}
	if c.IPNum == 0 {
		c.IPNum = d.IPNum
	}
	if c.IPs == "" {
		c.IPs = d.IPs
	}
	if c.Num == 0 {
		c.Num = d.Num
	}
	if c.Port == 0 {
		c.Port = d.Port
	}
	if c.Task == 0 {
		c.Task = d.Task
	}
	return c
}

func (c cfnatConfig) args() []string {
	return []string{
		"-addr", c.Addr,
		"-code", strconv.Itoa(c.Code),
		"-colo", c.Colo,
		"-delay", strconv.Itoa(c.Delay),
		"-domain", c.Domain,
		"-fixed", c.Fixed,
		"-ipnum", strconv.Itoa(c.IPNum),
		"-ips", c.IPs,
		"-num", strconv.Itoa(c.Num),
		"-port", strconv.Itoa(c.Port),
		"-random=" + strconv.FormatBool(c.Random),
		"-task", strconv.Itoa(c.Task),
		"-tls=" + strconv.FormatBool(c.TLS),
	}
}

func defaultCFdataConfig() cfdataConfig {
	return cfdataConfig{
		ForceUpdate: false,
		IPType:      4,
		DataCenter:  "",
		Scan:        100,
		Test:        50,
		Port:        443,
		Delay:       500,
	}
}

func normalizeCFdata(c cfdataConfig) cfdataConfig {
	d := defaultCFdataConfig()
	if c.IPType != 4 && c.IPType != 6 {
		c.IPType = d.IPType
	}
	if c.Scan == 0 {
		c.Scan = d.Scan
	}
	if c.Test == 0 {
		c.Test = d.Test
	}
	if c.Port == 0 {
		c.Port = d.Port
	}
	if c.Delay == 0 {
		c.Delay = d.Delay
	}
	return c
}

func (c cfdataConfig) args() []string {
	return []string{
		"-auto",
		"-no-wait",
		"-update=" + strconv.FormatBool(c.ForceUpdate),
		"-iptype", strconv.Itoa(c.IPType),
		"-dc", strings.TrimSpace(c.DataCenter),
		"-scan", strconv.Itoa(c.Scan),
		"-test", strconv.Itoa(c.Test),
		"-port", strconv.Itoa(c.Port),
		"-delay", strconv.Itoa(c.Delay),
	}
}

func (a *app) cfdataInput(c cfdataConfig) string {
	ipCSV := filepath.Join(a.dataDir, "ip.csv")
	_, err := os.Stat(ipCSV)
	lines := []string{}
	if err == nil {
		if c.ForceUpdate {
			lines = append(lines, "y", strconv.Itoa(c.IPType))
		} else {
			lines = append(lines, "n")
		}
	} else {
		lines = append(lines, strconv.Itoa(c.IPType))
	}
	lines = append(lines, strings.TrimSpace(c.DataCenter))
	return strings.Join(lines, "\n") + "\n"
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runHealthcheck()
		return
	}

	dataDir := env("DATA_DIR", defaultDataDir)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	a := &app{
		dataDir: dataDir,
		cfnat:   newManagedProcess("cfnat", env("CFNAT_BIN", "/usr/local/bin/cfnat")),
		cfdata:  newManagedProcess("cfdata", env("CFDATA_BIN", "/usr/local/bin/cfdata")),
	}
	a.loadProxyCandidateCache()
	a.loadProxyActivePool()
	if envBool("CFNAT_AUTO_START", true) {
		cfg := a.cfnatStartupConfig()
		if err := a.cfnat.start(a.dataDir, cfg.args(), ""); err != nil {
			log.Printf("auto-start cfnat failed: %v", err)
		}
	}
	go a.runProxyCandidateRefreshLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", a.handleHealth)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/config", a.handleConfig)
	mux.HandleFunc("/api/cfnat/start", a.handleCFnatStart)
	mux.HandleFunc("/api/cfnat/stop", a.handleCFnatStop)
	mux.HandleFunc("/api/cfnat/proxy-scan", a.handleProxyScan)
	mux.HandleFunc("/api/cfnat/proxy-candidates", a.handleProxyCandidates)
	mux.HandleFunc("/api/cfdata/run", a.handleCFdataRun)
	mux.HandleFunc("/api/cfdata/stop", a.handleCFdataStop)
	mux.HandleFunc("/api/cfdata/results", a.handleCFdataResults)
	mux.HandleFunc("/api/cfdata/detail", a.handleCFdataDetail)
	mux.HandleFunc("/api/cfdata/speed", a.handleCFdataSpeed)
	mux.HandleFunc("/api/logs", a.handleLogs)
	mux.HandleFunc("/api/files", a.handleFiles)
	mux.HandleFunc("/api/file/", a.handleFileDownload)
	mux.Handle("/", staticHandler())

	addr := env("WEB_ADDR", "0.0.0.0:8080")
	log.Printf("cloudflare-tools web listening on %s, data dir=%s", addr, dataDir)
	log.Fatal(http.ListenAndServe(addr, logRequests(mux)))
}

type proxyScanConfig struct {
	IPs         string   `json:"ips"`
	Sources     []string `json:"sources"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	Concurrency int      `json:"concurrency"`
	MaxLatency  int      `json:"maxLatency"`
	Limit       int      `json:"limit"`
	TLS         bool     `json:"tls"`
}

type proxyCandidateSource struct {
	Name    string
	Domains []string
	URLs    []string
}

type proxyCandidateSnapshot struct {
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
	NextRefresh time.Time `json:"nextRefresh,omitempty"`
	Sources     []string  `json:"sources"`
	IPs         []string  `json:"ips"`
	Errors      []string  `json:"errors"`
}

type proxyActivePool struct {
	UpdatedAt time.Time         `json:"updatedAt,omitempty"`
	Host      string            `json:"host,omitempty"`
	Path      string            `json:"path,omitempty"`
	IPs       []string          `json:"ips"`
	Results   []proxyScanResult `json:"results,omitempty"`
	Error     string            `json:"error,omitempty"`
}

type proxyAutoConfig struct {
	Enabled     bool
	Host        string
	Path        string
	Port        int
	Concurrency int
	MaxLatency  int
	PoolSize    int
	MinPool     int
	VLESS       vlessProbeConfig
}

func defaultProxyAutoConfig() proxyAutoConfig {
	return proxyAutoConfig{
		Enabled:     envBool("PROXY_AUTO_APPLY", false),
		Host:        env("PROXY_AUTO_HOST", ""),
		Path:        env("PROXY_AUTO_PATH", "/"),
		Port:        envInt("PROXY_AUTO_PORT", 443),
		Concurrency: envInt("PROXY_AUTO_CONCURRENCY", 20),
		MaxLatency:  envInt("PROXY_AUTO_MAX_LATENCY", 5000),
		PoolSize:    envInt("PROXY_AUTO_POOL_SIZE", 5),
		MinPool:     envInt("PROXY_AUTO_MIN_POOL", 3),
		VLESS:       defaultVLESSProbeConfig(),
	}
}

var proxyCandidateSources = map[string]proxyCandidateSource{
	"zhaobo": {Name: "Zhaobo 聚合池", Domains: []string{"proxyip.zhaobo.org"}},
	"william": {
		Name:    "William 台湾/韩国",
		Domains: []string{"tw.william.us.ci", "kr.william.us.ci"},
	},
	"euorg": {Name: "EU.org 社区池", Domains: []string{"cdn.xn--b6gac.eu.org"}},
	"cmliussss-proxyip": {
		Name: "CMLiussss ProxyIP",
		Domains: []string{
			"ProxyIP.CMLiussss.net", "ProxyIP.HK.CMLiussss.net", "ProxyIP.SG.CMLiussss.net",
			"ProxyIP.JP.CMLiussss.net", "ProxyIP.KR.CMLiussss.net", "ProxyIP.IN.CMLiussss.net",
			"ProxyIP.GB.CMLiussss.net", "ProxyIP.FR.CMLiussss.net", "ProxyIP.DE.CMLiussss.net",
			"ProxyIP.NL.CMLiussss.net", "ProxyIP.SE.CMLiussss.net", "ProxyIP.FI.CMLiussss.net",
			"ProxyIP.PL.CMLiussss.net", "ProxyIP.RU.CMLiussss.net", "ProxyIP.CH.CMLiussss.net",
			"ProxyIP.LV.CMLiussss.net", "ProxyIP.US.CMLiussss.net", "ProxyIP.CA.CMLiussss.net",
		},
	},
	"third-party-subscriptions": {
		Name: "第三方订阅入口",
		URLs: []string{
			"https://sub.cmliussss.net",
			"https://owo.o00o.ooo",
			"https://cm.soso.edu.kg",
			"https://zrf.zrf.me",
		},
	},
	"090227": {
		Name: "090227 优选域名与 API",
		Domains: []string{
			"cf.090227.xyz", "youxuan.cf.090227.xyz", "123.cf.090227.xyz",
			"www.visa.cn", "mfa.gov.ua", "www.shopify.com", "store.ubi.com",
			"staticdelivery.nexusmods.com", "tencentapp.cn", "cloudflare-dl.byoip.top",
			"cf.877774.xyz", "saas.sin.fan", "bestcf.030101.xyz", "cloudflare.182682.xyz",
			"ipdb.api.030101.xyz", "wetest.vip", "ip.164746.xyz",
		},
		URLs: []string{
			"https://cf.090227.xyz/",
			"https://cf.090227.xyz/ct?ips=6",
			"https://cf.090227.xyz/cu",
			"https://cf.090227.xyz/cmcc?ips=8",
		},
	},
}

var defaultProxyCandidateSourceIDs = []string{
	"zhaobo", "william", "euorg", "cmliussss-proxyip", "third-party-subscriptions", "090227",
}

func (a *app) runProxyCandidateRefreshLoop() {
	a.refreshAndApplyProxyPool(context.Background())
	ticker := time.NewTicker(proxyCandidateRefreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.refreshAndApplyProxyPool(context.Background())
	}
}

func (a *app) refreshAndApplyProxyPool(ctx context.Context) {
	a.runScheduledCFdata(ctx)
	if a.refreshProxyCandidates(ctx) {
		a.autoApplyProxyPool(ctx)
	}
}

func (a *app) runScheduledCFdata(parent context.Context) {
	if !envBool("PROXY_AUTO_CFDATA", false) {
		return
	}
	timeoutSeconds := envInt("PROXY_AUTO_CFDATA_TIMEOUT", 600)
	if timeoutSeconds < 30 || timeoutSeconds > 3600 {
		timeoutSeconds = 600
	}
	status := a.cfdata.status()
	if !status.Running {
		cfg := defaultCFdataConfig()
		if err := a.cfdata.start(a.dataDir, cfg.args(), ""); err != nil {
			log.Printf("scheduled cfdata start failed: %v", err)
			return
		}
		log.Printf("scheduled cfdata scan started")
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = a.cfdata.stop()
			log.Printf("scheduled cfdata scan timed out after %ds", timeoutSeconds)
			return
		case <-ticker.C:
			status := a.cfdata.status()
			if !status.Running {
				if status.ExitCode != nil && *status.ExitCode == 0 {
					log.Printf("scheduled cfdata scan completed")
				} else {
					log.Printf("scheduled cfdata scan failed: %s", status.LastError)
				}
				return
			}
		}
	}
}

func (a *app) cfnatStartupConfig() cfnatConfig {
	cfg := defaultCFnatConfig()
	a.activeMu.RLock()
	defer a.activeMu.RUnlock()
	if len(a.activePool.IPs) > 0 {
		cfg.Fixed = strings.Join(a.activePool.IPs, ",")
	}
	return cfg
}

func (a *app) loadProxyActivePool() {
	data, err := os.ReadFile(filepath.Join(a.dataDir, proxyActivePoolFile))
	if err != nil {
		return
	}
	var pool proxyActivePool
	if json.Unmarshal(data, &pool) != nil {
		return
	}
	valid := make([]string, 0, len(pool.IPs))
	for _, raw := range pool.IPs {
		if ip := net.ParseIP(raw); isPublicIPv4(ip) {
			valid = append(valid, ip.To4().String())
		}
	}
	pool.IPs = valid
	a.activeMu.Lock()
	a.activePool = pool
	a.activeMu.Unlock()
}

func (a *app) saveProxyActivePool(pool proxyActivePool) error {
	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, proxyActivePoolFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *app) proxyActivePoolSnapshot() proxyActivePool {
	a.activeMu.RLock()
	defer a.activeMu.RUnlock()
	pool := a.activePool
	pool.IPs = append([]string(nil), pool.IPs...)
	pool.Results = append([]proxyScanResult(nil), pool.Results...)
	return pool
}

func (a *app) autoApplyProxyPool(parent context.Context) {
	cfg := defaultProxyAutoConfig()
	if !cfg.Enabled || cfg.Host == "" || cfg.Path == "" {
		return
	}
	if strings.ContainsAny(cfg.Host, "/\\") || !strings.HasPrefix(cfg.Path, "/") {
		log.Printf("proxy auto apply disabled: invalid host/path")
		return
	}
	if cfg.Port < 1 || cfg.Port > 65535 || cfg.Concurrency < 1 || cfg.Concurrency > 100 || cfg.MaxLatency < 1 || cfg.MaxLatency > 30000 || cfg.PoolSize < 1 || cfg.PoolSize > 50 || cfg.MinPool < 1 || cfg.MinPool > cfg.PoolSize {
		log.Printf("proxy auto apply disabled: invalid configuration")
		return
	}
	var vlessTemplate map[string]any
	if cfg.VLESS.Enabled {
		if err := cfg.VLESS.validate(); err != nil {
			a.setProxyPoolError("VLESS 终审配置无效；保留旧池: "+err.Error(), nil)
			return
		}
		var err error
		vlessTemplate, err = loadVLESSOutboundTemplate(cfg.VLESS.TemplatePath)
		if err != nil {
			a.setProxyPoolError("VLESS 终审不可用；保留旧池: "+err.Error(), nil)
			return
		}
	}
	candidates := a.proxyCandidateSnapshot().IPs
	if len(candidates) == 0 {
		return
	}
	results := scanProxyWebSockets(parent, candidates, cfg)
	passed := make([]string, 0, cfg.PoolSize)
	finalStage := "WS"
	if cfg.VLESS.Enabled {
		finalStage = "VLESS"
		passed = probeVLESSPool(parent, results, cfg.Port, cfg.PoolSize, cfg.VLESS, vlessTemplate)
	} else {
		for _, result := range results {
			if result.Error == "" {
				passed = append(passed, result.IP)
				if len(passed) >= cfg.PoolSize {
					break
				}
			}
		}
	}
	if len(passed) < cfg.MinPool {
		a.setProxyPoolError(fmt.Sprintf("%s 终审仅 %d 个通过，低于最小池 %d；保留旧池", finalStage, len(passed), cfg.MinPool), results)
		return
	}
	current := a.proxyActivePoolSnapshot()
	changed := strings.Join(current.IPs, ",") != strings.Join(passed, ",")
	pool := proxyActivePool{UpdatedAt: time.Now(), Host: cfg.Host, Path: cfg.Path, IPs: passed, Results: results}
	if !changed {
		if err := a.saveProxyActivePool(pool); err != nil {
			log.Printf("save reverified proxy active pool: %v", err)
			return
		}
		a.activeMu.Lock()
		a.activePool = pool
		a.activeMu.Unlock()
		log.Printf("proxy active pool reverified: %d IPs", len(passed))
		return
	}
	a.cfnatCtlMu.Lock()
	defer a.cfnatCtlMu.Unlock()
	cfnatCfg := defaultCFnatConfig()
	cfnatCfg.Fixed = strings.Join(passed, ",")
	rollbackCfg := defaultCFnatConfig()
	if len(current.IPs) > 0 {
		rollbackCfg.Fixed = strings.Join(current.IPs, ",")
	}
	if err := a.cfnat.stop(); err != nil {
		log.Printf("stop cfnat for auto pool: %v", err)
		return
	}
	time.Sleep(300 * time.Millisecond)
	if err := a.cfnat.start(a.dataDir, cfnatCfg.args(), ""); err != nil {
		log.Printf("start cfnat with auto pool: %v", err)
		time.Sleep(300 * time.Millisecond)
		if rollbackErr := a.cfnat.start(a.dataDir, rollbackCfg.args(), ""); rollbackErr != nil {
			log.Printf("rollback cfnat after auto pool failure: %v", rollbackErr)
		}
		return
	}
	if err := a.saveProxyActivePool(pool); err != nil {
		log.Printf("commit proxy active pool: %v; rolling back", err)
		_ = a.cfnat.stop()
		time.Sleep(300 * time.Millisecond)
		if rollbackErr := a.cfnat.start(a.dataDir, rollbackCfg.args(), ""); rollbackErr != nil {
			log.Printf("rollback cfnat after pool commit failure: %v", rollbackErr)
		}
		return
	}
	a.activeMu.Lock()
	a.activePool = pool
	a.activeMu.Unlock()
	log.Printf("proxy active pool applied: %s", strings.Join(passed, ","))
}

func (a *app) setProxyPoolError(message string, results []proxyScanResult) {
	pool := a.proxyActivePoolSnapshot()
	pool.Error = message
	if results != nil {
		pool.Results = results
	}
	a.activeMu.Lock()
	a.activePool = pool
	a.activeMu.Unlock()
	log.Printf("proxy auto apply skipped: %s", message)
}

func scanProxyWebSockets(parent context.Context, ips []string, cfg proxyAutoConfig) []proxyScanResult {
	results := make([]proxyScanResult, len(ips))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			started := time.Now()
			ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.MaxLatency)*time.Millisecond)
			defer cancel()
			err := probeProxyWebSocket(ctx, ip, cfg)
			latency := time.Since(started).Milliseconds()
			results[i] = proxyScanResult{IP: ip, Latency: latency, Stage: "WS_PASS"}
			if err != nil {
				results[i].Stage = "WS_FAIL"
				results[i].Error = err.Error()
			}
		}(i, ip)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if (results[i].Error == "") != (results[j].Error == "") {
			return results[i].Error == ""
		}
		return results[i].Latency < results[j].Latency
	})
	return results
}

func probeProxyWebSocket(ctx context.Context, ip string, cfg proxyAutoConfig) error {
	dialer := &net.Dialer{}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
	if err != nil {
		return err
	}
	defer raw.Close()
	tlsConn := tls.Client(raw, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return err
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+cfg.Host+cfg.Path, nil)
	if err != nil {
		return err
	}
	req.Host = cfg.Host
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("User-Agent", "cloudflare-tools-auto-probe")
	if err := req.Write(tlsConn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("WS 状态码 %d", resp.StatusCode)
	}
	return nil
}

func (a *app) refreshProxyCandidates(parent context.Context) bool {
	if !a.refreshMu.TryLock() {
		return false
	}
	defer a.refreshMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	resolved, sourceErrors, err := resolveProxySources(ctx, defaultProxyCandidateSourceIDs, 1000)
	if err != nil {
		sourceErrors = append(sourceErrors, err.Error())
	}
	officialLimit := envInt("PROXY_OFFICIAL_CANDIDATES", 150)
	if officialLimit < 0 || officialLimit > 1000 {
		officialLimit = 150
	}
	if officialLimit > 0 {
		official, officialErr := fetchOfficialCloudflareCandidates(ctx, officialLimit)
		if officialErr != nil {
			sourceErrors = append(sourceErrors, "Cloudflare 官方段: "+officialErr.Error())
		} else {
			resolved = append(resolved, official...)
		}
	}
	cfdataLimit := envInt("PROXY_CFDATA_CANDIDATES", 300)
	if cfdataLimit < 0 || cfdataLimit > 2000 {
		cfdataLimit = 300
	}
	if cfdataLimit > 0 {
		cfdataIPs, cfdataErr := a.cfdataCandidateIPs(cfdataLimit)
		if cfdataErr != nil {
			sourceErrors = append(sourceErrors, "CFdata: "+cfdataErr.Error())
		} else {
			resolved = append(resolved, cfdataIPs...)
		}
	}
	seen := make(map[string]struct{})
	ips := make([]string, 0, len(resolved))
	for _, raw := range resolved {
		if len(ips) >= 1000 {
			break
		}
		ip := net.ParseIP(raw)
		if !isPublicIPv4(ip) {
			continue
		}
		key := ip.To4().String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		ips = append(ips, key)
	}
	sort.Strings(ips)
	if len(ips) == 0 {
		if len(sourceErrors) == 0 {
			sourceErrors = append(sourceErrors, "候选源未返回公网 IPv4")
		}
		log.Printf("proxy candidate refresh failed: no public IPv4 candidates; errors=%v", sourceErrors)
		snapshot := a.proxyCandidateSnapshot()
		snapshot.NextRefresh = time.Now().Add(proxyCandidateRefreshInterval)
		snapshot.Errors = sourceErrors
		a.candidateMu.Lock()
		a.candidates = snapshot
		a.candidateMu.Unlock()
		return true
	}
	now := time.Now()
	snapshot := proxyCandidateSnapshot{
		UpdatedAt:   now,
		NextRefresh: now.Add(proxyCandidateRefreshInterval),
		Sources:     append(append([]string(nil), defaultProxyCandidateSourceIDs...), "cloudflare-official", "cfdata"),
		IPs:         ips,
		Errors:      sourceErrors,
	}
	a.candidateMu.Lock()
	a.candidates = snapshot
	a.candidateMu.Unlock()
	if err := a.saveProxyCandidateCache(snapshot); err != nil {
		log.Printf("save proxy candidate cache: %v", err)
	}
	log.Printf("proxy candidate cache refreshed: %d IPv4, next=%s", len(ips), snapshot.NextRefresh.Format(time.RFC3339))
	return true
}

func fetchOfficialCloudflareCandidates(ctx context.Context, limit int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cloudflare.com/ips-v4", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	return sampleIPv4CIDRs(string(data), limit), nil
}

func sampleIPv4CIDRs(content string, limit int) []string {
	if limit <= 0 {
		return []string{}
	}
	type ipv4Range struct {
		base uint32
		size uint64
	}
	ranges := make([]ipv4Range, 0)
	for _, field := range strings.Fields(content) {
		ip, network, err := net.ParseCIDR(field)
		if err != nil || ip.To4() == nil {
			continue
		}
		ones, bits := network.Mask.Size()
		if bits != 32 || ones < 0 || ones > 32 {
			continue
		}
		ranges = append(ranges, ipv4Range{base: binary.BigEndian.Uint32(ip.To4()), size: uint64(1) << uint(32-ones)})
	}
	result := make([]string, 0, min(limit, len(ranges)*10))
	seen := make(map[string]struct{})
	for round := 0; len(result) < limit && len(ranges) > 0 && round < limit*2; round++ {
		for index, network := range ranges {
			if len(result) >= limit {
				break
			}
			offset := (uint64(round+1)*2654435761 + uint64(index+1)*2246822519) % network.size
			if network.size > 2 {
				offset = 1 + offset%(network.size-2)
			}
			value := network.base + uint32(offset)
			buf := make(net.IP, net.IPv4len)
			binary.BigEndian.PutUint32(buf, value)
			candidate := buf.String()
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			result = append(result, candidate)
		}
	}
	return result
}

func (a *app) cfdataCandidateIPs(limit int) ([]string, error) {
	rows, _, err := a.readScanRows()
	if err != nil {
		return nil, err
	}
	ips := make([]string, 0, min(limit, len(rows)))
	for _, row := range rows {
		if ip := net.ParseIP(row.IP); isPublicIPv4(ip) {
			ips = append(ips, ip.To4().String())
			if len(ips) >= limit {
				break
			}
		}
	}
	return ips, nil
}

func (a *app) proxyCandidateSnapshot() proxyCandidateSnapshot {
	a.candidateMu.RLock()
	defer a.candidateMu.RUnlock()
	snapshot := a.candidates
	snapshot.Sources = append([]string(nil), snapshot.Sources...)
	snapshot.IPs = append([]string(nil), snapshot.IPs...)
	snapshot.Errors = append([]string(nil), snapshot.Errors...)
	return snapshot
}

func (a *app) loadProxyCandidateCache() {
	data, err := os.ReadFile(filepath.Join(a.dataDir, proxyCandidateCacheFile))
	if err != nil {
		return
	}
	var snapshot proxyCandidateSnapshot
	if json.Unmarshal(data, &snapshot) != nil || len(snapshot.IPs) == 0 {
		return
	}
	valid := snapshot.IPs[:0]
	for _, raw := range snapshot.IPs {
		if ip := net.ParseIP(raw); isPublicIPv4(ip) {
			valid = append(valid, ip.To4().String())
		}
	}
	snapshot.IPs = valid
	a.candidateMu.Lock()
	a.candidates = snapshot
	a.candidateMu.Unlock()
}

func (a *app) saveProxyCandidateCache(snapshot proxyCandidateSnapshot) error {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dataDir, proxyCandidateCacheFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *app) handleProxyCandidates(w http.ResponseWriter, r *http.Request) {
	writeSnapshot := func() {
		writeJSON(w, struct {
			proxyCandidateSnapshot
			Active proxyActivePool `json:"active"`
		}{proxyCandidateSnapshot: a.proxyCandidateSnapshot(), Active: a.proxyActivePoolSnapshot()})
	}
	switch r.Method {
	case http.MethodGet:
		writeSnapshot()
	case http.MethodPost:
		if !a.refreshProxyCandidates(r.Context()) {
			http.Error(w, "候选源刷新正在运行", http.StatusTooManyRequests)
			return
		}
		a.autoApplyProxyPool(r.Context())
		writeSnapshot()
	default:
		http.Error(w, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

type proxyScanResult struct {
	IP          string `json:"ip"`
	Latency     int64  `json:"latency"`
	DataLatency int64  `json:"dataLatency,omitempty"`
	Stage       string `json:"stage,omitempty"`
	Error       string `json:"error,omitempty"`
}

func normalizeProxyScan(c proxyScanConfig) (proxyScanConfig, error) {
	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" {
		return c, errors.New("SNI/Host 不能为空")
	}
	if c.Port == 0 {
		c.Port = 443
	}
	if c.Port < 1 || c.Port > 65535 {
		return c, errors.New("端口必须在 1-65535")
	}
	if c.Concurrency == 0 {
		c.Concurrency = 50
	}
	if c.Concurrency < 1 || c.Concurrency > 500 {
		return c, errors.New("并发必须在 1-500")
	}
	if c.MaxLatency == 0 {
		c.MaxLatency = 300
	}
	if c.MaxLatency < 1 || c.MaxLatency > 10000 {
		return c, errors.New("延迟上限必须在 1-10000 ms")
	}
	if c.Limit == 0 {
		c.Limit = 100
	}
	if c.Limit < 1 || c.Limit > 10000 {
		return c, errors.New("扫描数量必须在 1-10000")
	}
	return c, nil
}

func (a *app) handleProxyScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !a.proxyScanMu.TryLock() {
		http.Error(w, "已有反代 IP 扫描正在运行", http.StatusTooManyRequests)
		return
	}
	defer a.proxyScanMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var cfg proxyScanConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var err error
	if cfg, err = normalizeProxyScan(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	seen := make(map[string]struct{})
	ips := make([]string, 0, cfg.Limit)
	addIP := func(raw string) {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if !isPublicIPv4(ip) || len(ips) >= cfg.Limit {
			return
		}
		key := ip.To4().String()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		ips = append(ips, key)
	}
	for _, raw := range strings.FieldsFunc(cfg.IPs, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		addIP(raw)
	}
	resolved, sourceErrors, err := resolveProxySources(r.Context(), cfg.Sources, cfg.Limit-len(ips))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, ip := range resolved {
		addIP(ip)
	}
	if len(ips) == 0 {
		http.Error(w, "未提供有效 IPv4 地址", http.StatusBadRequest)
		return
	}
	results := scanProxyIPs(r.Context(), ips, cfg)
	writeJSON(w, map[string]any{"ok": true, "scanned": len(ips), "results": results, "sourceErrors": sourceErrors})
}

func isPublicIPv4(ip net.IP) bool {
	return ip != nil && ip.To4() != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast()
}

const maxCandidateSourceBody = 512 * 1024

func extractPublicIPv4(text string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	add := func(raw string) {
		if ip := net.ParseIP(raw); isPublicIPv4(ip) {
			key := ip.To4().String()
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				result = append(result, key)
			}
		}
	}
	parseText := func(value string) {
		for _, token := range strings.FieldsFunc(value, func(r rune) bool {
			return !(r == '.' || r >= '0' && r <= '9')
		}) {
			add(token)
		}
	}
	parseText(text)
	for _, line := range strings.Fields(text) {
		compact := strings.TrimSpace(line)
		if len(compact) < 16 || len(compact) > maxCandidateSourceBody {
			continue
		}
		for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			decoded, err := encoding.DecodeString(compact)
			if err == nil {
				parseText(string(decoded))
				break
			}
		}
	}
	return result
}

func fetchProxyCandidateURL(parent context.Context, rawURL string) ([]string, error) {
	target, err := url.Parse(rawURL)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" {
		return nil, errors.New("候选源 URL 必须是 HTTPS")
	}
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !strings.EqualFold(req.URL.Hostname(), target.Hostname()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(parent, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("HTTP 状态码 %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCandidateSourceBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxCandidateSourceBody {
		return nil, errors.New("候选源内容超过 512 KiB")
	}
	return extractPublicIPv4(string(body)), nil
}

func resolveProxySources(parent context.Context, sourceIDs []string, remaining int) ([]string, []string, error) {
	if remaining <= 0 || len(sourceIDs) == 0 {
		return []string{}, []string{}, nil
	}
	if len(sourceIDs) > len(proxyCandidateSources) {
		return nil, nil, errors.New("候选源数量无效")
	}
	seenSources := make(map[string]struct{})
	ips := make([]string, 0, remaining)
	errorsFound := make([]string, 0)
	for _, id := range sourceIDs {
		source, ok := proxyCandidateSources[id]
		if !ok {
			return nil, nil, fmt.Errorf("未知候选源: %s", id)
		}
		if _, ok := seenSources[id]; ok {
			continue
		}
		seenSources[id] = struct{}{}
		for _, domain := range source.Domains {
			ctx, cancel := context.WithTimeout(parent, 5*time.Second)
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, domain)
			cancel()
			if err != nil {
				errorsFound = append(errorsFound, fmt.Sprintf("%s: %v", source.Name, err))
				continue
			}
			for _, address := range addresses {
				if ip := address.IP.To4(); ip != nil {
					ips = append(ips, ip.String())
					if len(ips) >= remaining {
						return ips, errorsFound, nil
					}
				}
			}
		}
		for _, sourceURL := range source.URLs {
			if len(ips) >= remaining {
				return ips, errorsFound, nil
			}
			found, err := fetchProxyCandidateURL(parent, sourceURL)
			if err != nil {
				errorsFound = append(errorsFound, fmt.Sprintf("%s: %v", source.Name, err))
				continue
			}
			ips = append(ips, found...)
		}
	}
	return ips, errorsFound, nil
}

func scanProxyIPs(parent context.Context, ips []string, cfg proxyScanConfig) []proxyScanResult {
	results := make([]proxyScanResult, len(ips))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			started := time.Now()
			timeout := time.Duration(cfg.MaxLatency+1000) * time.Millisecond
			if timeout < 2*time.Second {
				timeout = 2 * time.Second
			}
			dialer := &net.Dialer{Timeout: timeout}
			ctx, cancel := context.WithTimeout(parent, timeout)
			defer cancel()
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
			if err == nil && cfg.TLS {
				tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
				err = tlsConn.HandshakeContext(ctx)
			}
			if conn != nil {
				_ = conn.Close()
			}
			latency := time.Since(started).Milliseconds()
			results[i] = proxyScanResult{IP: ip, Latency: latency, Stage: "TLS_PASS"}
			if err != nil {
				results[i].Stage = "TLS_FAIL"
				results[i].Error = err.Error()
				return
			}
			if latency > int64(cfg.MaxLatency) {
				results[i].Error = fmt.Sprintf("超过延迟上限 %d ms", cfg.MaxLatency)
			}
		}(i, ip)
	}
	wg.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if (results[i].Error == "") != (results[j].Error == "") {
			return results[i].Error == ""
		}
		return results[i].Latency < results[j].Latency
	})
	return results
}

func runHealthcheck() {
	url := env("HEALTHCHECK_URL", "http://127.0.0.1:8080/api/health")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("healthcheck failed: %v", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("healthcheck bad status: %s", resp.Status)
		os.Exit(1)
	}
}

func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/cfnat", "/cfdata", "/files":
			http.ServeFileFS(w, r, sub, "index.html")
			return
		default:
			fileServer.ServeHTTP(w, r)
		}
	})
}

func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "time": time.Now().Format(time.RFC3339)})
}

func (a *app) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"cfnat": a.cfnatStartupConfig(), "cfdata": defaultCFdataConfig()})
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"cfnat": a.cfnat.status(), "cfdata": a.cfdata.status(), "proxyAuto": a.proxyActivePoolSnapshot()})
}

func (a *app) handleCFnatStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	a.cfnatCtlMu.Lock()
	defer a.cfnatCtlMu.Unlock()
	var cfg cfnatConfig
	_ = json.NewDecoder(r.Body).Decode(&cfg)
	cfg = normalizeCFnat(cfg)
	if err := a.cfnat.stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	time.Sleep(300 * time.Millisecond)
	if err := a.cfnat.start(a.dataDir, cfg.args(), ""); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": a.cfnat.status()})
}

func (a *app) handleCFnatStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	a.cfnatCtlMu.Lock()
	defer a.cfnatCtlMu.Unlock()
	if err := a.cfnat.stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleCFdataRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var cfg cfdataConfig
	_ = json.NewDecoder(r.Body).Decode(&cfg)
	cfg = normalizeCFdata(cfg)
	if err := a.cfdata.start(a.dataDir, cfg.args(), ""); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": a.cfdata.status()})
}

func (a *app) handleCFdataStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := a.cfdata.stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *app) handleLogs(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	limit := queryInt(r, "lines", 300)
	var lines []string
	switch target {
	case "cfdata":
		lines = a.cfdata.logs.lines(limit)
	default:
		lines = a.cfnat.logs.lines(limit)
	}
	writeJSON(w, map[string]any{"target": target, "lines": lines})
}

type scanRow struct {
	IP             string `json:"ip"`
	DataCenter     string `json:"dataCenter"`
	DataCenterZh   string `json:"dataCenterZh"`
	DataCenterName string `json:"dataCenterName"`
	Region         string `json:"region"`
	RegionZh       string `json:"regionZh"`
	RegionEn       string `json:"regionEn"`
	City           string `json:"city"`
	CityZh         string `json:"cityZh"`
	CityEn         string `json:"cityEn"`
	Latency        string `json:"latency"`
	LatencyMS      int    `json:"latencyMs"`
	Source         string `json:"source"`
}

type dataCenterSummary struct {
	DataCenter     string `json:"dataCenter"`
	DataCenterZh   string `json:"dataCenterZh"`
	DataCenterName string `json:"dataCenterName"`
	Region         string `json:"region"`
	RegionZh       string `json:"regionZh"`
	RegionEn       string `json:"regionEn"`
	City           string `json:"city"`
	CityZh         string `json:"cityZh"`
	CityEn         string `json:"cityEn"`
	IPCount        int    `json:"ipCount"`
	MinLatencyMS   int    `json:"minLatencyMs"`
}

type detailRow struct {
	IP           string `json:"ip"`
	MinLatencyMS int    `json:"minLatencyMs"`
	MaxLatencyMS int    `json:"maxLatencyMs"`
	AvgLatencyMS int    `json:"avgLatencyMs"`
	LossRate     int    `json:"lossRate"`
}

type detailFile struct {
	Name    string `json:"name"`
	Rows    int    `json:"rows"`
	ModTime string `json:"modTime"`
}

func (a *app) handleCFdataResults(w http.ResponseWriter, r *http.Request) {
	scanRows, dcRows, err := a.readScanRows()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	liveRows := a.liveScanRows()
	detailFiles := a.detailFiles()
	writeJSON(w, map[string]any{
		"ipListSource": ipListSource(),
		"liveRows":     liveRows,
		"scanRows":     scanRows,
		"dataCenters":  dcRows,
		"detailFiles":  detailFiles,
	})
}

func (a *app) handleCFdataDetail(w http.ResponseWriter, r *http.Request) {
	file := strings.TrimSpace(r.URL.Query().Get("file"))
	if file != "" {
		file = filepath.Base(file)
	}
	if file == "" {
		files := a.detailFiles()
		if len(files) == 0 {
			writeJSON(w, map[string]any{"file": "", "rows": []detailRow{}})
			return
		}
		file = files[0].Name
	}
	if file == "ip.csv" || strings.ToLower(filepath.Ext(file)) != ".csv" || file != filepath.Base(file) {
		http.Error(w, "detail csv denied", http.StatusBadRequest)
		return
	}
	rows, err := readDetailCSV(filepath.Join(a.dataDir, file))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"file": file, "rows": rows})
}

func (a *app) handleCFdataSpeed(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	parsed := net.ParseIP(ip)
	if parsed == nil {
		http.Error(w, "invalid ip", http.StatusBadRequest)
		return
	}
	if !a.knownResultIP(ip) {
		http.Error(w, "ip not in cfdata results", http.StatusBadRequest)
		return
	}
	size := queryInt(r, "bytes", 2_000_000)
	if size > 20_000_000 {
		size = 20_000_000
	}
	result, err := downloadSpeedViaCloudflareIP(r.Context(), ip, size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, result)
}

func (a *app) knownResultIP(ip string) bool {
	candidates := []string{filepath.Join(a.dataDir, "ip.csv")}
	for _, file := range a.detailFiles() {
		candidates = append(candidates, filepath.Join(a.dataDir, file.Name))
	}
	for _, path := range candidates {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		reader := csv.NewReader(bufio.NewReader(f))
		reader.FieldsPerRecord = -1
		records, err := reader.ReadAll()
		_ = f.Close()
		if err != nil {
			continue
		}
		for i, record := range records {
			if i == 0 || len(record) == 0 {
				continue
			}
			if strings.TrimSpace(record[0]) == ip {
				return true
			}
		}
	}
	return false
}

func downloadSpeedViaCloudflareIP(ctx context.Context, ip string, size int) (map[string]any, error) {
	host := "speed.cloudflare.com"
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				port = "443"
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       15 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second}
	url := fmt.Sprintf("https://%s/__down?bytes=%d", host, size)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.Header.Set("User-Agent", "cloudflare-tools-web/1.0")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("speed endpoint status %d", resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, int64(size)))
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		elapsed = time.Millisecond
	}
	mbps := float64(n*8) / elapsed.Seconds() / 1_000_000
	return map[string]any{
		"ip":         ip,
		"bytes":      n,
		"durationMs": elapsed.Milliseconds(),
		"mbps":       mbps,
	}, nil
}

func (a *app) readScanRows() ([]scanRow, []dataCenterSummary, error) {
	path := filepath.Join(a.dataDir, "ip.csv")
	f, err := os.Open(path)
	if err != nil {
		return []scanRow{}, []dataCenterSummary{}, err
	}
	defer f.Close()

	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, nil, err
	}

	rows := make([]scanRow, 0, max(0, len(records)-1))
	summary := map[string]*dataCenterSummary{}
	for i, record := range records {
		if i == 0 || len(record) < 5 {
			continue
		}
		code := strings.TrimSpace(record[1])
		region := strings.TrimSpace(record[2])
		city := strings.TrimSpace(record[3])
		lat := strings.TrimSpace(record[4])
		latMS := parseIntPrefix(lat)
		names := decorateLocation(code, region, city)
		row := scanRow{
			IP:             strings.TrimSpace(record[0]),
			DataCenter:     code,
			DataCenterZh:   names.DataCenterZh,
			DataCenterName: names.DataCenterName,
			Region:         region,
			RegionZh:       names.RegionZh,
			RegionEn:       names.RegionEn,
			City:           city,
			CityZh:         names.CityZh,
			CityEn:         names.CityEn,
			Latency:        lat,
			LatencyMS:      latMS,
			Source:         "ip.csv",
		}
		rows = append(rows, row)
		if code == "" {
			continue
		}
		cur, ok := summary[code]
		if !ok {
			summary[code] = &dataCenterSummary{
				DataCenter:     code,
				DataCenterZh:   row.DataCenterZh,
				DataCenterName: row.DataCenterName,
				Region:         region,
				RegionZh:       row.RegionZh,
				RegionEn:       row.RegionEn,
				City:           city,
				CityZh:         row.CityZh,
				CityEn:         row.CityEn,
				IPCount:        1,
				MinLatencyMS:   latMS,
			}
			continue
		}
		cur.IPCount++
		if cur.MinLatencyMS == 0 || (latMS > 0 && latMS < cur.MinLatencyMS) {
			cur.MinLatencyMS = latMS
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LatencyMS < rows[j].LatencyMS })
	dcs := make([]dataCenterSummary, 0, len(summary))
	for _, v := range summary {
		dcs = append(dcs, *v)
	}
	sort.Slice(dcs, func(i, j int) bool {
		if dcs[i].MinLatencyMS == dcs[j].MinLatencyMS {
			return dcs[i].DataCenter < dcs[j].DataCenter
		}
		return dcs[i].MinLatencyMS < dcs[j].MinLatencyMS
	})
	return rows, dcs, nil
}

type decoratedLocation struct {
	DataCenterZh   string
	DataCenterName string
	RegionZh       string
	RegionEn       string
	CityZh         string
	CityEn         string
}

func decorateLocation(code, region, city string) decoratedLocation {
	code = strings.ToUpper(strings.TrimSpace(code))
	region = strings.TrimSpace(region)
	city = strings.TrimSpace(city)
	cityZh := cityZh(code, city)
	cityEn := cityEn(code, city)
	regionZh, regionEn := regionNames(region)
	name := code
	if cityZh != "" && cityEn != "" && cityZh != cityEn {
		name = code + " · " + cityZh + " / " + cityEn
	} else if cityZh != "" {
		name = code + " · " + cityZh
	}
	return decoratedLocation{
		DataCenterZh:   codeDisplay(code, city),
		DataCenterName: name,
		RegionZh:       regionZh,
		RegionEn:       regionEn,
		CityZh:         cityZh,
		CityEn:         cityEn,
	}
}

func (a *app) liveScanRows() []scanRow {
	lines := a.cfdata.logs.lines(2000)
	rows := make([]scanRow, 0, min(len(lines), 500))
	for _, line := range lines {
		row, ok := parseLiveScanLine(line)
		if ok {
			rows = append(rows, row)
		}
	}
	if len(rows) > 500 {
		rows = rows[len(rows)-500:]
	}
	return rows
}

func parseLiveScanLine(line string) (scanRow, bool) {
	if !strings.Contains(line, "有效IP:") || !strings.Contains(line, "延迟:") {
		return scanRow{}, false
	}
	parts := strings.Split(line, ",")
	if len(parts) < 2 {
		return scanRow{}, false
	}
	ipPart := strings.TrimSpace(parts[0])
	idx := strings.LastIndex(ipPart, "有效IP:")
	if idx < 0 {
		return scanRow{}, false
	}
	row := scanRow{IP: strings.TrimSpace(strings.TrimPrefix(ipPart[idx:], "有效IP:")), Source: "live"}
	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "数据中心:"):
			row.DataCenter = strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(part, "数据中心:")))
		case strings.HasPrefix(part, "地区:"):
			row.Region = strings.TrimSpace(strings.TrimPrefix(part, "地区:"))
		case strings.HasPrefix(part, "城市:"):
			row.City = strings.TrimSpace(strings.TrimPrefix(part, "城市:"))
		case strings.HasPrefix(part, "延迟:"):
			row.Latency = strings.TrimSpace(strings.TrimPrefix(part, "延迟:"))
			row.LatencyMS = parseIntPrefix(row.Latency)
		}
	}
	if row.DataCenter == "" || row.IP == "" {
		return scanRow{}, false
	}
	names := decorateLocation(row.DataCenter, row.Region, row.City)
	row.DataCenterZh = names.DataCenterZh
	row.DataCenterName = names.DataCenterName
	row.RegionZh = names.RegionZh
	row.RegionEn = names.RegionEn
	row.CityZh = names.CityZh
	row.CityEn = names.CityEn
	return row, true
}

func (a *app) detailFiles() []detailFile {
	entries, err := os.ReadDir(a.dataDir)
	if err != nil {
		return []detailFile{}
	}
	files := []detailFile{}
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".csv" || e.Name() == "ip.csv" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows, _ := countCSVRows(filepath.Join(a.dataDir, e.Name()))
		files = append(files, detailFile{Name: e.Name(), Rows: rows, ModTime: info.ModTime().Format(time.RFC3339)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime > files[j].ModTime })
	return files
}

func readDetailCSV(path string) ([]detailRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return []detailRow{}, err
	}
	defer f.Close()
	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	rows := make([]detailRow, 0, max(0, len(records)-1))
	for i, record := range records {
		if i == 0 || len(record) < 5 {
			continue
		}
		rows = append(rows, detailRow{
			IP:           strings.TrimSpace(record[0]),
			MinLatencyMS: atoi(strings.TrimSpace(record[1])),
			MaxLatencyMS: atoi(strings.TrimSpace(record[2])),
			AvgLatencyMS: atoi(strings.TrimSpace(record[3])),
			LossRate:     atoi(strings.TrimSpace(record[4])),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].LossRate == rows[j].LossRate {
			return rows[i].AvgLatencyMS < rows[j].AvgLatencyMS
		}
		return rows[i].LossRate < rows[j].LossRate
	})
	return rows, nil
}

func countCSVRows(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}
	return len(records) - 1, nil
}

func ipListSource() map[string]any {
	return map[string]any{
		"cacheFiles": []string{"ips-v4.txt", "ips-v6.txt", "locations.json"},
		"remote": map[string]string{
			"ipv4":      "https://www.baipiao.eu.org/cloudflare/ips-v4",
			"ipv6":      "https://www.baipiao.eu.org/cloudflare/ips-v6",
			"locations": "https://www.baipiao.eu.org/cloudflare/locations",
		},
		"behavior": "cfdata/cfnat 启动后优先读取工作目录本地缓存；缺少 ips-v4.txt/ips-v6.txt 或 locations.json 时才从远端下载并保存。",
	}
}

func parseIntPrefix(s string) int {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return 0
	}
	return atoi(fields[0])
}

func atoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func regionZh(region string) string {
	zh, _ := regionNames(region)
	return zh
}

func regionNames(region string) (string, string) {
	switch strings.TrimSpace(region) {
	case "Asia Pacific":
		return "亚太地区", "Asia Pacific"
	case "North America":
		return "北美洲", "North America"
	case "South America":
		return "南美洲", "South America"
	case "Europe":
		return "欧洲", "Europe"
	case "Middle East":
		return "中东", "Middle East"
	case "Africa":
		return "非洲", "Africa"
	case "Oceania":
		return "大洋洲", "Oceania"
	case "亚太", "亚太地区":
		return "亚太地区", "Asia Pacific"
	case "北美", "北美洲":
		return "北美洲", "North America"
	case "南美", "南美洲":
		return "南美洲", "South America"
	case "欧洲":
		return "欧洲", "Europe"
	case "中东":
		return "中东", "Middle East"
	case "非洲":
		return "非洲", "Africa"
	case "大洋洲":
		return "大洋洲", "Oceania"
	default:
		return region, region
	}
}

func codeDisplay(code, city string) string {
	zh := cityZh(code, city)
	if zh == "" {
		return code
	}
	return code + " · " + zh
}

func cityZh(code, city string) string {
	if v := cityZhByCode[strings.ToUpper(strings.TrimSpace(code))]; v != "" {
		return v
	}
	if v := cityZhByName[strings.ToLower(strings.TrimSpace(city))]; v != "" {
		return v
	}
	return city
}

func cityEn(code, city string) string {
	if v := cityEnByCode[strings.ToUpper(strings.TrimSpace(code))]; v != "" {
		return v
	}
	city = strings.TrimSpace(city)
	if v := cityEnByZh[city]; v != "" {
		return v
	}
	return city
}

var cityZhByCode = map[string]string{
	"HKG": "香港", "TPE": "台北", "KHH": "高雄", "NRT": "东京", "KIX": "大阪", "FUK": "福冈", "OKA": "那霸",
	"ICN": "首尔", "SIN": "新加坡", "BKK": "曼谷", "CNX": "清迈", "SGN": "胡志明市", "HAN": "河内", "DAD": "岘港",
	"KUL": "吉隆坡", "JHB": "新山", "KCH": "古晋", "MNL": "马尼拉", "CEB": "宿务", "CRK": "克拉克", "CGK": "雅加达",
	"DPS": "登巴萨", "DEL": "新德里", "BOM": "孟买", "BLR": "班加罗尔", "MAA": "金奈", "HYD": "海得拉巴",
	"LAX": "洛杉矶", "SJC": "圣何塞", "SFO": "旧金山", "SEA": "西雅图", "ORD": "芝加哥", "DFW": "达拉斯",
	"IAD": "阿什本", "EWR": "纽瓦克", "JFK": "纽约", "BOS": "波士顿", "MIA": "迈阿密", "DEN": "丹佛",
	"ATL": "亚特兰大", "PHX": "凤凰城", "LAS": "拉斯维加斯", "PDX": "波特兰", "SLC": "盐湖城", "HNL": "檀香山",
	"LHR": "伦敦", "MAN": "曼彻斯特", "CDG": "巴黎", "AMS": "阿姆斯特丹", "FRA": "法兰克福", "MUC": "慕尼黑",
	"DUS": "杜塞尔多夫", "HAM": "汉堡", "TXL": "柏林", "MAD": "马德里", "BCN": "巴塞罗那", "LIS": "里斯本",
	"FCO": "罗马", "MXP": "米兰", "ZRH": "苏黎世", "GVA": "日内瓦", "VIE": "维也纳", "PRG": "布拉格",
	"WAW": "华沙", "CPH": "哥本哈根", "ARN": "斯德哥尔摩", "HEL": "赫尔辛基", "OSL": "奥斯陆", "DUB": "都柏林",
	"SYD": "悉尼", "MEL": "墨尔本", "BNE": "布里斯班", "PER": "珀斯", "AKL": "奥克兰", "CHC": "基督城",
	"DXB": "迪拜", "DOH": "多哈", "TLV": "特拉维夫", "AMM": "安曼", "JED": "吉达", "RUH": "利雅得",
	"JNB": "约翰内斯堡", "CPT": "开普敦", "CAI": "开罗", "NBO": "内罗毕", "LOS": "拉各斯", "GRU": "圣保罗",
	"GIG": "里约热内卢", "SCL": "圣地亚哥", "BOG": "波哥大", "LIM": "利马", "MEX": "墨西哥城", "YYZ": "多伦多",
	"YVR": "温哥华", "YUL": "蒙特利尔",
}

var cityEnByCode = map[string]string{
	"HKG": "Hong Kong", "TPE": "Taipei", "KHH": "Kaohsiung City", "NRT": "Tokyo", "KIX": "Osaka", "FUK": "Fukuoka", "OKA": "Naha",
	"ICN": "Seoul", "SIN": "Singapore", "BKK": "Bangkok", "CNX": "Chiang Mai", "SGN": "Ho Chi Minh City", "HAN": "Hanoi", "DAD": "Da Nang",
	"KUL": "Kuala Lumpur", "JHB": "Johor Bahru", "KCH": "Kuching", "MNL": "Manila", "CEB": "Cebu", "CRK": "Tarlac City", "CGK": "Jakarta",
	"DPS": "Denpasar", "DEL": "New Delhi", "BOM": "Mumbai", "BLR": "Bangalore", "MAA": "Chennai", "HYD": "Hyderabad",
	"LAX": "Los Angeles", "SJC": "San Jose", "SFO": "San Francisco", "SEA": "Seattle", "ORD": "Chicago", "DFW": "Dallas",
	"IAD": "Ashburn", "EWR": "Newark", "JFK": "New York", "BOS": "Boston", "MIA": "Miami", "DEN": "Denver",
	"ATL": "Atlanta", "PHX": "Phoenix", "LAS": "Las Vegas", "PDX": "Portland", "SLC": "Salt Lake City", "HNL": "Honolulu",
	"LHR": "London", "MAN": "Manchester", "CDG": "Paris", "AMS": "Amsterdam", "FRA": "Frankfurt", "MUC": "Munich",
	"DUS": "Düsseldorf", "HAM": "Hamburg", "TXL": "Berlin", "MAD": "Madrid", "BCN": "Barcelona", "LIS": "Lisbon",
	"FCO": "Rome", "MXP": "Milan", "ZRH": "Zurich", "GVA": "Geneva", "VIE": "Vienna", "PRG": "Prague",
	"WAW": "Warsaw", "CPH": "Copenhagen", "ARN": "Stockholm", "HEL": "Helsinki", "OSL": "Oslo", "DUB": "Dublin",
	"SYD": "Sydney", "MEL": "Melbourne", "BNE": "Brisbane", "PER": "Perth", "AKL": "Auckland", "CHC": "Christchurch",
	"DXB": "Dubai", "DOH": "Doha", "TLV": "Tel Aviv", "AMM": "Amman", "JED": "Jeddah", "RUH": "Riyadh",
	"JNB": "Johannesburg", "CPT": "Cape Town", "CAI": "Cairo", "NBO": "Nairobi", "LOS": "Lagos", "GRU": "São Paulo",
	"GIG": "Rio de Janeiro", "SCL": "Santiago", "BOG": "Bogota", "LIM": "Lima", "MEX": "Mexico City", "YYZ": "Toronto",
	"YVR": "Vancouver", "YUL": "Montréal",
}

var cityZhByName = map[string]string{
	"hong kong": "香港", "tokyo": "东京", "osaka": "大阪", "seoul": "首尔", "singapore": "新加坡", "taipei": "台北",
	"los angeles": "洛杉矶", "san jose": "圣何塞", "san francisco": "旧金山", "seattle": "西雅图", "chicago": "芝加哥",
	"london": "伦敦", "paris": "巴黎", "frankfurt": "法兰克福", "amsterdam": "阿姆斯特丹", "sydney": "悉尼",
	"melbourne": "墨尔本", "dubai": "迪拜", "toronto": "多伦多", "vancouver": "温哥华", "mexico city": "墨西哥城",
}

var cityEnByZh = map[string]string{
	"香港": "Hong Kong", "台北": "Taipei", "高雄": "Kaohsiung City", "东京": "Tokyo", "大阪": "Osaka", "福冈": "Fukuoka",
	"首尔": "Seoul", "新加坡": "Singapore", "曼谷": "Bangkok", "胡志明市": "Ho Chi Minh City", "河内": "Hanoi",
	"洛杉矶": "Los Angeles", "圣何塞": "San Jose", "旧金山": "San Francisco", "西雅图": "Seattle", "芝加哥": "Chicago",
	"达拉斯": "Dallas", "丹佛": "Denver", "伦敦": "London", "巴黎": "Paris", "法兰克福": "Frankfurt",
	"阿姆斯特丹": "Amsterdam", "马德里": "Madrid", "悉尼": "Sydney", "多伦多": "Toronto",
}

type fileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
	Kind    string `json:"kind"`
}

func (a *app) handleFiles(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(a.dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	files := []fileInfo{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !allowedFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
			Kind:    strings.TrimPrefix(strings.ToLower(filepath.Ext(e.Name())), "."),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime > files[j].ModTime })
	writeJSON(w, map[string]any{"files": files, "dataCenters": a.dataCenters()})
}

func (a *app) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/file/")
	name = filepath.Base(name)
	if !allowedFile(name) {
		http.Error(w, "file denied", http.StatusBadRequest)
		return
	}
	path := filepath.Join(a.dataDir, name)
	http.ServeFile(w, r, path)
}

func (a *app) dataCenters() []string {
	path := filepath.Join(a.dataDir, "ip.csv")
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = -1
	rows, err := reader.ReadAll()
	if err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	for i, row := range rows {
		if i == 0 || len(row) < 2 {
			continue
		}
		dc := strings.TrimSpace(row[1])
		if dc != "" && !seen[dc] {
			seen[dc] = true
			out = append(out, dc)
		}
	}
	sort.Strings(out)
	return out
}

func allowedFile(name string) bool {
	if name != filepath.Base(name) {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".csv", ".txt", ".json", ".log":
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func queryInt(r *http.Request, key string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return def
	}
	return v
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Truncate(time.Millisecond))
		}
	})
}
