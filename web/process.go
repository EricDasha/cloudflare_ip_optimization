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

// rotatingLogWriter 是带「单份上限 + 按日轮转」的追加日志写入器。
//
// 为什么需要它：managedProcess 原来直接把子进程 stdout 追加到 <name>.log，
// 既没有大小上限也不按天切分，cfnat.log 在线上长到了 639 MB。Docker 的
// json-file 轮转只管容器 stdout，管不到 /data 里的这个文件。
//
// 轮转用 rename 而不是截断，保留最近 keep 份；存量文件若已超过 capBytes，
// 只保留尾部（最新）那一段，避免「轮转一次仍然占着 600MB」。
type rotatingLogWriter struct {
	mu       sync.Mutex
	path     string
	capBytes int64
	keep     int
	file     *os.File
	size     int64
	year     int
	yearDay  int
}

func newRotatingLogWriter(path string, capBytes int64, keep int) (*rotatingLogWriter, error) {
	w := &rotatingLogWriter{path: path, capBytes: capBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingLogWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.size = st.Size()
	w.year = st.ModTime().Year()
	w.yearDay = st.ModTime().YearDay()
	return nil
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	dayChanged := now.Year() != w.year || now.YearDay() != w.yearDay
	overCap := w.size > 0 && w.size+int64(len(p)) > w.capBytes
	if dayChanged || overCap {
		// 轮转失败不阻断业务：继续写当前文件，下次再试。
		if err := w.rotate(); err != nil && w.file == nil {
			_ = w.open()
		}
	}
	if w.file == nil {
		// 磁盘不可写时丢弃文件日志，但不能让子进程输出卡死或让本进程崩溃。
		return len(p), nil
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingLogWriter) rotate() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	for i := w.keep; i >= 1; i-- {
		current := fmt.Sprintf("%s.%d", w.path, i)
		if i == w.keep {
			_ = os.Remove(current)
			continue
		}
		if _, err := os.Stat(current); err == nil {
			_ = os.Rename(current, fmt.Sprintf("%s.%d", w.path, i+1))
		}
	}
	if st, err := os.Stat(w.path); err == nil {
		if st.Size() > w.capBytes {
			// 老账过大：只保留尾部，随后清空当前文件重新开始。
			// 复制失败就不截断，宁可多占磁盘也不能丢日志。
			if err := keepFileTail(w.path, w.path+".1", w.capBytes); err == nil {
				_ = os.Truncate(w.path, 0)
			}
		} else {
			_ = os.Rename(w.path, w.path+".1")
		}
	}
	return w.open()
}

func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// keepFileTail 把 src 的尾部最多 capBytes 字节写入 dst。
func keepFileTail(src, dst string, capBytes int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	offset := st.Size() - capBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := in.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

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
	logPath := filepath.Join(dataDir, p.name+".log")
	logWriter, logErr := newRotatingLogWriter(logPath, maxLogFileBytes, maxLogFileBackups)
	if logErr == nil {
		mw := io.MultiWriter(p.logs, logWriter)
		cmd.Stdout = mw
		cmd.Stderr = mw
	} else {
		_, _ = fmt.Fprintf(p.logs, "open log file failed: %v\n", logErr)
		cmd.Stdout = p.logs
		cmd.Stderr = p.logs
	}

	var input io.WriteCloser
	var err error
	if stdin != "" {
		input, err = cmd.StdinPipe()
		if err != nil {
			cancel()
			if logWriter != nil {
				_ = logWriter.Close()
			}
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		cancel()
		if logWriter != nil {
			_ = logWriter.Close()
		}
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
		if logWriter != nil {
			_ = logWriter.Close()
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
	Sticky   bool   `json:"sticky"`
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
		Num:      envInt("CFNAT_NUM", defaultCFnatNum),
		Port:     envInt("CFNAT_PORT", 443),
		Random:   envBool("CFNAT_RANDOM", true),
		Sticky:   envBool("CFNAT_STICKY", true),
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
	// 回退深度必须在 [1, maxCFnatNum] 内：越界一律回落默认值，
	// 避免把单连接出站尝试数放大到不可控。
	if c.Num < 1 || c.Num > maxCFnatNum {
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

// args 生成 cfnat 命令行参数。布尔旗标一律用 "-flag=值" 单参形式：
// Go 的 flag 包对 bool 旗标不消费后随参数，"-flag true" 会把 true 当位置参数并
// 就此截断后续解析（历史遗留：-task/-tls 曾长期因此未被读入）。
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
		"-sticky=" + strconv.FormatBool(c.Sticky),
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
