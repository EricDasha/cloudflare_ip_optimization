package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 订阅区从主页迁出成独立页之后，路由必须真的把它服务出去。
// 这条守的是「页面文件存在但没人能打开」这类静默故障：embed 漏了、
// switch 分支漏了，症状都是 404，而只看 JS 是看不见的。
func TestStaticHandlerServesSubscriptionPage(t *testing.T) {
	h := staticHandler()
	for _, path := range []string{"/subscription", "/subscription/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
		body, _ := io.ReadAll(rec.Body)
		html := string(body)
		for _, want := range []string{"订阅本子", `id="subEntryBody"`, `id="subMaintainerBody"`, "/app.js"} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s missing %q", path, want)
			}
		}
		// 订阅页不该带主页的在位表——带了说明 serve 错了文件。
		if strings.Contains(html, `id="throneRows"`) {
			t.Fatalf("%s served the home page", path)
		}
	}
}

// 主页不再渲染订阅区，但必须留一个点得进去的入口，否则订阅页等于没做。
func TestStaticHandlerHomeLinksToSubscription(t *testing.T) {
	h := staticHandler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	html := string(body)
	if !strings.Contains(html, `href="/subscription"`) {
		t.Fatal("home page has no link to /subscription")
	}
	if strings.Contains(html, `id="subEntryBody"`) {
		t.Fatal("home page still embeds the subscription book")
	}
}
