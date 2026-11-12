//go:build ignore

// 独立 main 的下载上限测试：cfnat.go 与 cfdata.go 各自带 //go:build ignore，
// 不进 go test ./...；用文件列表显式跑：
//
//	go test -run TestGetURLContentDownloadLimit cfnat.go download_limit_test.go
//	go test -run TestGetURLContentDownloadLimit cfdata.go download_limit_test.go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetURLContentDownloadLimit(t *testing.T) {
	oversize := strings.Repeat("x\n", (2<<20)/2+100) // > 2 MiB
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, oversize)
	}))
	defer big.Close()
	if _, err := getURLContent(big.URL); err == nil {
		t.Fatal("oversize download must error, got nil")
	}
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "1.1.1.1\n2.2.2.2\n")
	}))
	defer small.Close()
	got, err := getURLContent(small.URL)
	if err != nil {
		t.Fatalf("small download must succeed: %v", err)
	}
	if !strings.Contains(got, "1.1.1.1") {
		t.Fatalf("unexpected body %q", got)
	}
}
