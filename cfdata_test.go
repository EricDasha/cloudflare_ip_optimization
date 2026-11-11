//go:build ignore

// cfdata.go 的单元测试，用 `go test cfdata.go cfdata_test.go` 显式运行。
package main

import "testing"

func TestSafeDCFilename(t *testing.T) {
	cases := []struct {
		dc   string
		want string
	}{
		{"HKG", "HKG.csv"},
		{"../../x", "______x.csv"},
		{"result", "result.csv"},
		{"a/b", "a_b.csv"},
		{"", "result.csv"},
		{"SJC-1_x", "SJC-1_x.csv"},
	}
	for _, c := range cases {
		if got := safeDCFilename(c.dc); got != c.want {
			t.Errorf("safeDCFilename(%q) = %q, want %q", c.dc, got, c.want)
		}
	}
}
