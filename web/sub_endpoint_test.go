package main

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeSub(t *testing.T, body []byte) []string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("订阅内容不是 base64: %v", err)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// 订阅只发官方段 IP：域名型成员和段外地址都要被挡在外面。
func TestSubscriptionOnlyShipsCloudflareEdgeIPs(t *testing.T) {
	t.Setenv("CF_SUB_UUID", "11111111-2222-3333-4444-555555555555")
	t.Setenv("CF_SUB_TOKEN", "")
	a := &app{dataDir: t.TempDir()}
	a.activePool = proxyActivePool{
		Host:    "vlesdy.example.com",
		Path:    "/thread?ed=2560",
		IPs:     []string{"172.64.145.33", "175.41.232.194", "104.18.39.52"},
		Domains: []string{"www.example.org"},
	}

	rec := httptest.NewRecorder()
	a.handleSubscription(rec, httptest.NewRequest("GET", "/sub", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	links := decodeSub(t, rec.Body.Bytes())
	if len(links) != 2 {
		t.Fatalf("应只发两个官方段 IP，实际 %d 条: %v", len(links), links)
	}
	joined := strings.Join(links, "\n")
	if strings.Contains(joined, "175.41.232.194") {
		t.Fatalf("段外地址不应下发：%s", joined)
	}
	if strings.Contains(joined, "example.org") {
		t.Fatalf("域名不应下发：%s", joined)
	}
	if !strings.Contains(joined, "sni=vlesdy.example.com") {
		t.Fatalf("sni 应取池子的 host：%s", joined)
	}
	if !strings.Contains(joined, "path=%2Fthread%3Fed%3D2560") {
		t.Fatalf("path 应完整转义：%s", joined)
	}
}

// 关闭的成员不能出现在订阅里：用户关掉它就是因为不想再走它。
func TestSubscriptionSkipsDisabledMembers(t *testing.T) {
	t.Setenv("CF_SUB_UUID", "11111111-2222-3333-4444-555555555555")
	a := &app{dataDir: t.TempDir()}
	a.activePool = proxyActivePool{
		Host:     "vlesdy.example.com",
		Path:     "/x",
		IPs:      []string{"172.64.145.33", "104.18.39.52"},
		Disabled: []string{"172.64.145.33"},
	}
	rec := httptest.NewRecorder()
	a.handleSubscription(rec, httptest.NewRequest("GET", "/sub", nil))
	links := decodeSub(t, rec.Body.Bytes())
	if len(links) != 1 || !strings.Contains(links[0], "104.18.39.52") {
		t.Fatalf("关闭成员未被跳过: %v", links)
	}
}

// 没有 UUID 就明确报错，而不是吐一份客户端连不上的订阅。
func TestSubscriptionRequiresUUID(t *testing.T) {
	t.Setenv("CF_SUB_UUID", "")
	a := &app{dataDir: t.TempDir()}
	a.activePool = proxyActivePool{Host: "h.example.com", IPs: []string{"172.64.145.33"}}
	rec := httptest.NewRecorder()
	a.handleSubscription(rec, httptest.NewRequest("GET", "/sub", nil))
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// token 配了就必须带对。
func TestSubscriptionTokenGuard(t *testing.T) {
	t.Setenv("CF_SUB_UUID", "11111111-2222-3333-4444-555555555555")
	t.Setenv("CF_SUB_TOKEN", "s3cret")
	a := &app{dataDir: t.TempDir()}
	a.activePool = proxyActivePool{Host: "h.example.com", Path: "/x", IPs: []string{"172.64.145.33"}}

	bad := httptest.NewRecorder()
	a.handleSubscription(bad, httptest.NewRequest("GET", "/sub", nil))
	if bad.Code != 403 {
		t.Fatalf("缺 token 应 403，实际 %d", bad.Code)
	}
	good := httptest.NewRecorder()
	a.handleSubscription(good, httptest.NewRequest("GET", "/sub?token=s3cret", nil))
	if good.Code != 200 {
		t.Fatalf("带对 token 应 200，实际 %d", good.Code)
	}
}

// 缺 proxyip 的路径要补上，已经有的不许重复补——重复补会让 Worker 认不出路径。
func TestWithProxyIP(t *testing.T) {
	cases := []struct{ in, proxyip, want string }{
		{"/thread?ed=2560", "proxyip.cmliussss.net", "/thread/proxyip=proxyip.cmliussss.net?ed=2560"},
		{"/thread/proxyip=1.2.3.4?ed=2560", "proxyip.cmliussss.net", "/thread/proxyip=1.2.3.4?ed=2560"},
		{"/", "p.example.com", "/proxyip=p.example.com"},
		{"/thread", "", "/thread"},
	}
	for _, c := range cases {
		if got := withProxyIP(c.in, c.proxyip); got != c.want {
			t.Fatalf("withProxyIP(%q, %q) = %q, want %q", c.in, c.proxyip, got, c.want)
		}
	}
}
