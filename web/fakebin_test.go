package main

// 假二进制夹具：测试需要一个「别的进程」来冒充 cfnat / sing-box——
// 它们只要按约定挂住、报错，或起一个假代理就够。此前这些夹具写成
// `#!/bin/sh` 和 `python3` 脚本，在 Windows 上 exec 直接失败
// （`%1 is not a valid Win32 application`），22 项测试因此长期红着；
// 即使在 Linux 上，它们也隐含依赖 python3 与可执行位。
//
// 这里改成「测试二进制复执行自身」：子进程就是我们自己这个可执行文件，
// 靠 GO_FAKE_BIN 认领假身份。任何平台都合法可执行，不依赖 shell、
// python 或 chmod。被测代码完全不知道这层存在——它只看到一次普通的
// exec，以及一个会挂住/报错/听端口的子进程。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// fakeBinEnv 选择假二进制行为；空值表示当前是正常测试进程。
	fakeBinEnv = "GO_FAKE_BIN"
	// fakeBinArgFile 让假 cfnat 把自己的启动参数追加到指定文件。
	// 走文件而不是 stdout：被测代码把子进程输出接进了日志缓冲。
	fakeBinArgFile = "GO_FAKE_BIN_ARGFILE"

	fakeModeCFnatSleep   = "cfnat-sleep"
	fakeModeSingBoxBind  = "singbox-bind-conflict"
	fakeModeSingBoxDead  = "singbox-dead"
	fakeModeSingBoxProxy = "singbox-proxy"
	fakeModeSingBoxSlow  = "singbox-slow-body"
	// fakeModeExitZero 立刻成功退出，等价 Unix 的 /bin/true。
	// 用它替代写死 "/bin/true"：Windows 上那个路径不存在。
	fakeModeExitZero = "exit-zero"

	fakeBindConflictNoise = "FATAL: listen tcp 127.0.0.1: bind: address already in use"
)

// installFakeBinary 把当前进程切到指定假身份，返回可传给被测代码的
// 可执行文件路径。argFile 非空时，假 cfnat 会把每次启动的 argv 追加进去。
//
// t.Setenv 只作用于当前测试进程，但子进程继承环境，所以 exec 出去的那
// 一份自然带上同样的 GO_FAKE_BIN。
func installFakeBinary(t *testing.T, mode, argFile string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		// 极少数平台 os.Executable 不可用；go test 拉起的正是 os.Args[0]。
		if args0 := os.Args[0]; args0 != "" {
			exe = args0
		} else {
			t.Fatalf("resolve test binary path: %v", err)
		}
	}
	t.Setenv(fakeBinEnv, mode)
	if argFile != "" {
		t.Setenv(fakeBinArgFile, argFile)
	}
	return exe
}

// fakeCFnatBin 返回冒充常驻 cfnat 的可执行路径。
func fakeCFnatBin(t *testing.T, argFile string) string {
	t.Helper()
	return installFakeBinary(t, fakeModeCFnatSleep, argFile)
}

// fakeSingBoxBin 返回指定行为的假 sing-box 路径。
func fakeSingBoxBin(t *testing.T, mode string) string {
	t.Helper()
	return installFakeBinary(t, mode, "")
}

// fakeSingBoxBinWithArgFile 同 fakeSingBoxBin，但把每次启动的 argv
// 追加到 argFile——用于统计「被启动了几次」。
func fakeSingBoxBinWithArgFile(t *testing.T, mode, argFile string) string {
	t.Helper()
	return installFakeBinary(t, mode, argFile)
}

// runFakeBinaryIfChild 由 TestMain 调用：当前进程若被当作假二进制执行，
// 就在这里扮演完并退出（不返回）。
func runFakeBinaryIfChild() {
	mode := os.Getenv(fakeBinEnv)
	if mode == "" {
		return
	}
	switch mode {
	case fakeModeCFnatSleep:
		fakeCFnatSleep()
	case fakeModeSingBoxBind:
		fakeSingBoxBindConflict()
	case fakeModeSingBoxDead:
		// 60ms 后退出：够被测代码前两次 100ms 就绪拨号先失败，
		// 第三次拨通时进程已死——正是这条测试要卡住的时间窗。
		time.Sleep(60 * time.Millisecond)
		fakeSingBoxBindConflict()
	case fakeModeSingBoxProxy:
		fakeSingBoxServe(false)
	case fakeModeSingBoxSlow:
		fakeSingBoxServe(true)
	case fakeModeExitZero:
		// 什么都不做，正常退出（exit 0）。
	default:
		fmt.Fprintf(os.Stderr, "unknown %s=%q\n", fakeBinEnv, mode)
		os.Exit(2)
	}
	os.Exit(0)
}

// fakeCFnatSleep 冒充常驻 cfnat：挂住直到被 stop() 杀掉。
func fakeCFnatSleep() {
	recordFakeStart()
	// 阻塞等外部 Kill：stop() 会先 cancel ctx 再 Kill，这里不必主动退出。
	select {}
}

// fakeSingBoxBindConflict 每次启动记一次尝试，然后照真实 sing-box
// 绑定失败的样子报错退出，用于验证探针的换端口重试。
func fakeSingBoxBindConflict() {
	recordFakeStart()
	fmt.Fprintln(os.Stderr, fakeBindConflictNoise)
	os.Exit(1)
}

// recordFakeStart 按需把本次 argv 追加到 argFile。
// 夹具里刻意不检查写失败：参数记录只是断言辅助，写不进去也不该
// 让子进程以「启动失败」的面貌反过来干扰被测逻辑。
func recordFakeStart() {
	argFile := os.Getenv(fakeBinArgFile)
	if argFile == "" {
		return
	}
	line := strings.Join(os.Args[1:], " ") + "\n"
	f, err := os.OpenFile(argFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

// fakeSingBoxServe 冒充 sing-box 在探针 inbound 端口上起 HTTP 服务。
//
// slowBody=true 复现「响应头给了但 body 拖着不给」：状态码与
// Content-Length 先发，隔 10s 才结束响应，用来确认探针在读 body 之前
// 就按状态码判定失败，而不是等到读超时。
func fakeSingBoxServe(slowBody bool) {
	cfgPath := ""
	for i := 1; i < len(os.Args)-1; i++ {
		if os.Args[i] == "-c" {
			cfgPath = os.Args[i+1]
			break
		}
	}
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "fake sing-box: missing -c <config>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake sing-box: read config: %v\n", err)
		os.Exit(2)
	}
	var cfg struct {
		Inbounds []struct {
			ListenPort int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || len(cfg.Inbounds) == 0 {
		fmt.Fprintf(os.Stderr, "fake sing-box: bad config %s: %v\n", cfgPath, err)
		os.Exit(2)
	}
	status := 403
	if v := os.Getenv("FAKE_PROXY_STATUS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			status = n
		}
	}
	payload := []byte(os.Getenv("FAKE_PROXY_BODY"))

	srv := &http.Server{
		Addr: fmt.Sprintf("127.0.0.1:%d", cfg.Inbounds[0].ListenPort),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slowBody {
				// 声明的长度远大于实际写入量，且先把头刷出去再睡。
				w.Header().Set("Content-Length", "5000000")
				w.WriteHeader(status)
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
				time.Sleep(10 * time.Second)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(status)
			if r.Method != http.MethodHead && len(payload) > 0 {
				_, _ = w.Write(payload)
			}
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "fake sing-box: serve: %v\n", err)
		os.Exit(1)
	}
}
