package main

import (
	"reflect"
	"testing"
)

// L3 降级：VLESS 终审没过，但 WS 101 已通过的候选可以顶上，
// 只要补到 MinPool 为止，不得越界扩池。
func TestDegradedFromWSResultsFillsToWant(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1"},
		{IP: "2.2.2.2", Error: "ws failed"},
		{IP: "3.3.3.3"},
		{IP: "4.4.4.4"},
	}
	got := degradedFromWSResults(results, nil, 2)
	want := []string{"1.1.1.1", "3.3.3.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("degradedFromWSResults() = %v, want %v", got, want)
	}
}

// 已经够了就不该再补——降级是为了不空池，不是把未验证候选塞满。
func TestDegradedFromWSResultsNoopWhenEnough(t *testing.T) {
	results := []proxyScanResult{{IP: "1.1.1.1"}, {IP: "2.2.2.2"}}
	if got := degradedFromWSResults(results, []string{"9.9.9.9", "8.8.8.8"}, 2); got != nil {
		t.Fatalf("已有 %d 个时不应补，got %v", 2, got)
	}
}

// 只补差额，不重复已入选的 IP。
func TestDegradedFromWSResultsSkipsExisting(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1"}, // 已入选
		{IP: "2.2.2.2"},
		{IP: "3.3.3.3"},
	}
	got := degradedFromWSResults(results, []string{"1.1.1.1"}, 3)
	want := []string{"2.2.2.2", "3.3.3.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("degradedFromWSResults() = %v, want %v", got, want)
	}
}

// 全部 WS 失败时不能凭空造出候选。
func TestDegradedFromWSResultsAllFailed(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Error: "timeout"},
		{IP: "2.2.2.2", Error: "reset"},
	}
	if got := degradedFromWSResults(results, nil, 3); len(got) != 0 {
		t.Fatalf("WS 全失败时不应产出候选，got %v", got)
	}
}

// 空结果集必须安全返回。
func TestDegradedFromWSResultsEmpty(t *testing.T) {
	if got := degradedFromWSResults(nil, nil, 3); len(got) != 0 {
		t.Fatalf("空结果集应返回空，got %v", got)
	}
}
