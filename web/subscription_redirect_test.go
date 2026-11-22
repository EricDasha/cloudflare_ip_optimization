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
