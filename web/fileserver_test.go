package main

import (
	"path/filepath"
	"testing"
)

// A1 安全回归：VLESS outbound 模板（vless-probe-outbound.json）内含真实
// UUID/SNI/path，文件接口绝不能奉还——README/DESIGN 承诺「API 不返回模板
// 内容」由 allowedFile 兑现。旧实现仅凭 .json 白名单放行，模板恰是 .json，
// 曾可经 GET /api/file/vless-probe-outbound.json 直接泄露节点凭证。
func TestAllowedFileBlocksVLESSTemplate(t *testing.T) {
	t.Setenv("PROXY_VLESS_TEMPLATE", "")
	if allowedFile("vless-probe-outbound.json") {
		t.Fatal("default VLESS template must be denied by the file API")
	}
	t.Setenv("PROXY_VLESS_TEMPLATE", filepath.Join(t.TempDir(), "my-secret-outbound.json"))
	if allowedFile("my-secret-outbound.json") {
		t.Fatal("custom-named VLESS template must be denied by the file API")
	}
	// 恢复默认模板名后，再验大小写变体与 sing-box 探针临时配置残留
	t.Setenv("PROXY_VLESS_TEMPLATE", "")
	if allowedFile("Vless-Probe-Outbound.JSON") {
		t.Fatal("case-variant template name must be denied")
	}
	if allowedFile(".vless-probe-123456.json") {
		t.Fatal("leftover sing-box probe config must be denied")
	}
}

// 白名单本体不受模板闸误伤：普通 csv/txt/json/log 照常放行，其余照拒。
func TestAllowedFileWhitelistUnchanged(t *testing.T) {
	t.Setenv("PROXY_VLESS_TEMPLATE", "")
	for _, name := range []string{"ip.csv", "result.txt", "proxy-active.json", "cfnat.log"} {
		if !allowedFile(name) {
			t.Fatalf("whitelisted %q must stay allowed", name)
		}
	}
	for _, name := range []string{"evil.exe", "config.yaml", "vless-probe.bin"} {
		if allowedFile(name) {
			t.Fatalf("%q must stay denied", name)
		}
	}
	// 路径穿越仍被 Base 校验拦死
	if allowedFile(`..\ip.csv`) {
		t.Fatal("path traversal must be denied")
	}
}
