package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type managedProcess struct {
	mu        sync.Mutex
	name      string
	bin       string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	done      chan struct{}
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
	p.done = make(chan struct{})
	p.startedAt = time.Now()
	done := p.done

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
		close(done)
	}()
	return nil
}

func (p *managedProcess) stop() error {
	p.mu.Lock()
	if p.cmd == nil || p.cmd.Process == nil || p.exitCode != nil {
		p.mu.Unlock()
		return nil
	}
	cancel := p.cancel
	proc := p.cmd.Process
	done := p.done
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("停止 %s 失败: %w", p.name, err)
		}
		<-done
		return nil
	}
}

type cfnatConfig struct {
	Addr     string `json:"addr"`
	Code     int    `json:"code"`
	Colo     string `json:"colo"`
	Delay    int    `json:"delay"`
	Domain   string `json:"domain"`
	Fixed    string `json:"fixed"`
	Priority string `json:"priority"`
	IPNum    int    `json:"ipnum"`
	IPs      string `json:"ips"`
	Num      int    `json:"num"`
	Port     int    `json:"port"`
	Random   bool   `json:"random"`
	TLS      bool   `json:"tls"`
	Task     int    `json:"task"`
	Fallback string `json:"fallback"`
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
		Addr:     env("CFNAT_ADDR", "0.0.0.0:1234"),
		Code:     envInt("CFNAT_CODE", 200),
		Colo:     env("CFNAT_COLO", ""),
		Delay:    envInt("CFNAT_DELAY", 300),
		Domain:   env("CFNAT_DOMAIN", "cloudflaremirrors.com/debian"),
		Fixed:    env("CFNAT_FIXED_IPS", ""),
		Priority: env("CFNAT_PRIORITY_IPS", ""),
		IPNum:    envInt("CFNAT_IPNUM", 20),
		IPs:      env("CFNAT_IPS", "4"),
		Num:      envInt("CFNAT_NUM", 1),
		Port:     envInt("CFNAT_PORT", 443),
		Random:   envBool("CFNAT_RANDOM", true),
		Task:     envInt("CFNAT_TASK", 100),
		TLS:      envBool("CFNAT_TLS", true),
		Fallback: env("CFNAT_FALLBACK", ""),
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
	if c.Priority == "" {
		c.Priority = d.Priority
	}
	if c.Fallback == "" {
		c.Fallback = d.Fallback
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
		"-fallback", c.Fallback,
		"-priority", c.Priority,
		"-ipnum", strconv.Itoa(c.IPNum),
		"-ips", c.IPs,
		"-num", strconv.Itoa(c.Num),
		"-port", strconv.Itoa(c.Port),
		"-random", strconv.FormatBool(c.Random),
		"-task", strconv.Itoa(c.Task),
		"-tls", strconv.FormatBool(c.TLS),
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
