package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestAllowHTTPSRedirect(t *testing.T) {
	base, _ := url.Parse("https://sub.example.com/convert")
	cases := []struct {
		name string
		to   string
		want bool
	}{
		{"same host https", "https://sub.example.com/other", true},
		{"same host https case-insensitive", "https://SUB.EXAMPLE.COM/other", true},
		{"same host downgrade to http", "http://sub.example.com/other", false},
		{"different host https", "https://evil.example.com/other", false},
		{"different host http", "http://evil.example.com/other", false},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodGet, c.to, nil)
		if got := allowHTTPSRedirect(req, base); got != c.want {
			t.Errorf("%s: allowHTTPSRedirect=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestSubscriptionRedirectPolicyCapsAtTen(t *testing.T) {
	base, _ := url.Parse("https://sub.example.com/convert")
	policy := subscriptionRedirectPolicy(base)
	req, _ := http.NewRequest(http.MethodGet, "https://sub.example.com/other", nil)
	via := make([]*http.Request, 9)
	for i := range via {
		via[i] = req
	}
	if err := policy(req, via); err != nil {
		t.Errorf("9 redirects should be allowed, got %v", err)
	}
	via = append(via, req)
	if err := policy(req, via); err == nil || err.Error() != "stopped after 10 redirects" {
		t.Errorf("10 redirects should stop, got %v", err)
	}
	// 次数未超限时原有 HTTPS/同 host 规则仍在。
	bad, _ := http.NewRequest(http.MethodGet, "http://sub.example.com/other", nil)
	if err := policy(bad, nil); err != http.ErrUseLastResponse {
		t.Errorf("http downgrade should return ErrUseLastResponse, got %v", err)
	}
	other, _ := http.NewRequest(http.MethodGet, "https://evil.example.com/other", nil)
	if err := policy(other, nil); err != http.ErrUseLastResponse {
		t.Errorf("different host should return ErrUseLastResponse, got %v", err)
	}
}
