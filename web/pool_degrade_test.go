package main

import (
	"reflect"
	"testing"
)

// L3 降级（改动 B 后）：只有「进过 VLESS 终审、且失败不是超时」的候选
// 才能顶上。纯 WS 通过（未验数据面）与 VLESS 超时一律拒绝并记入 rejected。
func TestDegradedFromWSResultsFillsToWant(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 500, ErrorClass: "refused", Error: "refused"},
		{IP: "2.2.2.2", Stage: "WS_PASS"},
		{IP: "3.3.3.3", Stage: "VLESS_FAIL", DataLatency: 800, ErrorClass: "protocol", Error: "reset"},
		{IP: "4.4.4.4", Stage: "WS_PASS"},
	}
	got, rejected := degradedFromWSResults(results, nil, 2)
	want := []string{"1.1.1.1", "3.3.3.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("degradedFromWSResults() = %v, want %v", got, want)
	}
	// 2.2.2.2 在补满之前被考察过，应出现在拒绝名单里并说明原因。
	if len(rejected) == 0 {
		t.Fatal("纯 WS_PASS 候选应被拒绝并记录原因，rejected 为空")
	}
}

// 已经够了就不该再补——降级是为了不空池，不是把未验证候选塞满。
func TestDegradedFromWSResultsNoopWhenEnough(t *testing.T) {
	results := []proxyScanResult{{IP: "1.1.1.1"}, {IP: "2.2.2.2"}}
	if got, _ := degradedFromWSResults(results, []string{"9.9.9.9", "8.8.8.8"}, 2); got != nil {
		t.Fatalf("已有 %d 个时不应补，got %v", 2, got)
	}
}

// 只补差额，不重复已入选的 IP。
func TestDegradedFromWSResultsSkipsExisting(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 500, ErrorClass: "refused", Error: "refused"}, // 已入选
		{IP: "2.2.2.2", Stage: "VLESS_FAIL", DataLatency: 600, ErrorClass: "protocol", Error: "reset"},
		{IP: "3.3.3.3", Stage: "VLESS_FAIL", DataLatency: 700, ErrorClass: "refused", Error: "refused"},
	}
	got, _ := degradedFromWSResults(results, []string{"1.1.1.1"}, 3)
	want := []string{"2.2.2.2", "3.3.3.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("degradedFromWSResults() = %v, want %v", got, want)
	}
}

// 全部 WS 失败时不能凭空造出候选。
func TestDegradedFromWSResultsAllFailed(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "WS_FAIL", Error: "timeout"},
		{IP: "2.2.2.2", Stage: "WS_FAIL", Error: "reset"},
	}
	if got, _ := degradedFromWSResults(results, nil, 3); len(got) != 0 {
		t.Fatalf("WS 全失败时不应产出候选，got %v", got)
	}
}

// 空结果集必须安全返回。
func TestDegradedFromWSResultsEmpty(t *testing.T) {
	if got, _ := degradedFromWSResults(nil, nil, 3); len(got) != 0 {
		t.Fatalf("空结果集应返回空，got %v", got)
	}
}

// 改动 B 核心：纯 WS_PASS（未进 VLESS 终审）不得进降级池——WS 握手
// 对数据面毫无预测力（实测 13 个 VLESS_FAIL 的 WS 仅 228–423ms，
// 数据面却卡满 15s 超时）。
func TestDegradedFromWSResultsRejectsUnverifiedWS(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "WS_PASS", Latency: 228},
		{IP: "2.2.2.2", Stage: "WS_PASS", Latency: 300},
	}
	got, rejected := degradedFromWSResults(results, nil, 2)
	if len(got) != 0 {
		t.Fatalf("纯 WS_PASS 不应进降级池，got %v", got)
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected = %v, want 2 条拒绝原因", rejected)
	}
}

// 改动 B 核心：VLESS 超时（errorClass=timeout，即使 DataLatency 约 15s）
// 不得进降级池；非超时快速失败（refused）按交接单口径可采用。
func TestDegradedFromWSResultsRejectsVLESSTimeout(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 15005, ErrorClass: "timeout", Error: "context deadline exceeded"},
		{IP: "2.2.2.2", Stage: "VLESS_FAIL", DataLatency: 15004, ErrorClass: "timeout", Error: "context deadline exceeded"},
		{IP: "3.3.3.3", Stage: "VLESS_FAIL", DataLatency: 5062, ErrorClass: "protocol", Error: "connection reset by peer"},
	}
	got, rejected := degradedFromWSResults(results, nil, 2)
	want := []string{"3.3.3.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("degradedFromWSResults() = %v, want %v", got, want)
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected = %v, want 2 条超时拒绝原因", rejected)
	}
}
