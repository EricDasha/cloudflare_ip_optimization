//go:build ignore

// cfnat.go 独立 main 的转发管道测试，不进 go test ./...；文件列表显式跑：
//
//	go test -run TestCopyWithIdleTimeoutWriteDeadline cfnat.go pipe_timeout_test.go
package main

import (
	"net"
	"testing"
	"time"
)

// 双向堵写必须靠写超时解开：对端只发不收、上游同时停读时，两个拷贝
// goroutine 都会卡在 Write 里，读超时永远到不了，closeBoth 永不执行。
func TestCopyWithIdleTimeoutWriteDeadline(t *testing.T) {
	srcLocal, srcRemote := net.Pipe()
	dstLocal, dstRemote := net.Pipe()
	defer srcLocal.Close()
	defer srcRemote.Close()
	defer dstLocal.Close()
	defer dstRemote.Close()
	// srcRemote 持续供数；dstRemote 永不读，写方向必然堵住。
	go func() {
		buf := make([]byte, 32*1024)
		for {
			if _, err := srcRemote.Write(buf); err != nil {
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := copyWithIdleTimeout(dstLocal, srcLocal, 200*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write must return an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("copyWithIdleTimeout did not return: write direction has no timeout")
	}
}
