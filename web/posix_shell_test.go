package main

import (
	"runtime"
	"testing"
)

// requirePOSIXShell 跳过依赖 /bin/sh 或 /bin/true 的测试。
//
// 这些用例用「写一个 shell 脚本再 exec」的方式伪造子进程（cfnat / sing-box），
// Windows 无法直接 exec .sh，会报「%1 is not a valid Win32 application」。
// 断言本身与平台无关，只是夹具是 POSIX 的——在非 POSIX 宿主上跳过，
// 而不是让本地 build-dist.ps1 因平台差异整条构建失败。
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake child-process fixtures are POSIX shell scripts; skipped on Windows")
	}
}
