package main

import (
	"reflect"
	"testing"
)

// 第二轮审计收紧后的降级准入：只有「VLESS 终审失败、但有明确数据面
// 成功证据（DataProven）」的候选才能顶上。快速失败（refused/reset）
// 并不比超时更可信——refused 恰恰证明对端 443 已死，reset 在没收到
// 响应前发生时什么都没证明，other 里还混着探针基础设施失败。
// 本测试原按第一轮「非超时即放行」口径写，认为 refused/protocol
// 可采用；第二轮复审推翻了这一口径，见 degradedFromWSResults 的注释。
func TestDegradedFromWSResultsFillsToWant(t *testing.T) {
	results := []proxyScanResult{
		// 1.1.1.1：refused，无数据面证据——对端已死，拒绝。
		{IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 500, ErrorClass: "refused", Error: "refused"},
		{IP: "2.2.2.2", Stage: "WS_PASS"},
		// 3.3.3.3：reset，但隧道里收到过源站响应（状态码不对）——数据面
		// 真的通过，失败在测速端点，采用。
		{IP: "3.3.3.3", Stage: "VLESS_FAIL", DataLatency: 800, ErrorClass: "protocol", Error: "reset", DataProven: true},
		{IP: "4.4.4.4", Stage: "WS_PASS"},
	}
	got, rejected := degradedFromWSResults(results, nil, 2)
	want := []string{"3.3.3.3"}
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

// 只补差额，不重复已入选的 IP（有数据面证据的才补）。
func TestDegradedFromWSResultsSkipsExisting(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 500, ErrorClass: "refused", Error: "refused", DataProven: true}, // 已入选
		{IP: "2.2.2.2", Stage: "VLESS_FAIL", DataLatency: 600, ErrorClass: "protocol", Error: "reset", DataProven: true},
		{IP: "3.3.3.3", Stage: "VLESS_FAIL", DataLatency: 700, ErrorClass: "refused", Error: "refused", DataProven: true},
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

// 第二轮审计核心：VLESS 超时（errorClass=timeout，即使 DataLatency 约 15s）
// 不得进降级池；非超时快速失败（refused/reset）在没有数据面证据时
// 同样不得进——第一轮测试曾认为 reset 可采用，第二轮推翻：
// 没收到响应前的 reset 什么都没证明。
func TestDegradedFromWSResultsRejectsVLESSTimeout(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 15005, ErrorClass: "timeout", Error: "context deadline exceeded"},
		{IP: "2.2.2.2", Stage: "VLESS_FAIL", DataLatency: 15004, ErrorClass: "timeout", Error: "context deadline exceeded"},
		{IP: "3.3.3.3", Stage: "VLESS_FAIL", DataLatency: 5062, ErrorClass: "protocol", Error: "connection reset by peer"},
	}
	got, rejected := degradedFromWSResults(results, nil, 2)
	if len(got) != 0 {
		t.Fatalf("degradedFromWSResults() = %v, want 空（超时与无证据 reset 一律拒绝）", got)
	}
	if len(rejected) != 3 {
		t.Fatalf("rejected = %v, want 3 条拒绝原因", rejected)
	}
}

// 第二轮审计：每种 VLESS 失败归类逐项钉死——没有数据面证据时，
// timeout / refused / unreachable / handshake / protocol / other
// 一律拒绝；只有 DataProven 为真才采用，与归类无关。
func TestDegradedFromWSResultsPerErrorClass(t *testing.T) {
	cases := []struct {
		name       string
		errorClass string
		dataProven bool
		wantAdopt  bool
	}{
		{"timeout 无证据", "timeout", false, false},
		{"timeout 有证据", "timeout", true, true}, // 有证据即数据面通过，归类不否决
		{"refused 无证据", "refused", false, false},
		{"refused 有证据", "refused", true, true},
		{"unreachable 无证据", "unreachable", false, false},
		{"unreachable 有证据", "unreachable", true, true},
		{"handshake 无证据", "handshake", false, false},
		{"handshake 有证据", "handshake", true, true},
		{"protocol 无证据", "protocol", false, false},
		{"protocol 有证据", "protocol", true, true},
		{"other 无证据", "other", false, false},
		{"other 有证据", "other", true, true},
		{"未归类无证据", "", false, false},
		{"未归类有证据", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := []proxyScanResult{{
				IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 500,
				ErrorClass: tc.errorClass, Error: "boom", DataProven: tc.dataProven,
			}}
			got, rejected := degradedFromWSResults(results, nil, 1)
			if tc.wantAdopt {
				if len(got) != 1 || got[0] != "1.1.1.1" {
					t.Fatalf("got %v, want 采用 1.1.1.1", got)
				}
				if len(rejected) != 0 {
					t.Fatalf("rejected = %v, want 空", rejected)
				}
			} else {
				if len(got) != 0 {
					t.Fatalf("got %v, want 拒绝", got)
				}
				if len(rejected) != 1 {
					t.Fatalf("rejected = %v, want 1 条拒绝原因", rejected)
				}
			}
		})
	}
}

// 探针基础设施失败（sing-box 起不来）归 other、DataProven 为假，
// 绝不能因为「非超时」就进降级池：探针坏了会让全员失败，旧口径下
// 整批候选会被照单全收。
func TestDegradedFromWSResultsRejectsProbeInfraFailure(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 12, ErrorClass: "other", Error: "启动 sing-box 失败: no such file or directory"},
		{IP: "2.2.2.2", Stage: "VLESS_FAIL", DataLatency: 8, ErrorClass: "other", Error: "sing-box 提前退出: bad config"},
	}
	got, rejected := degradedFromWSResults(results, nil, 2)
	if len(got) != 0 {
		t.Fatalf("got %v, want 空（探针基础设施失败不是候选的证据）", got)
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected = %v, want 2 条拒绝原因", rejected)
	}
}

// DataLatency 再大也不是证据：15 秒超时（DataLatency 约 15005ms）
// 且 DataProven 为假时必须拒绝；反过来 DataProven 为真、
// DataLatency 很小（如状态码立即返回）时应当采用。
func TestDegradedFromWSResultsLatencyIsNotProof(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Stage: "VLESS_FAIL", DataLatency: 15005, ErrorClass: "timeout", Error: "context deadline exceeded"},
		{IP: "2.2.2.2", Stage: "VLESS_FAIL", DataLatency: 37, ErrorClass: "protocol", Error: "VLESS 数据面状态码 403，预期 204", DataProven: true},
	}
	got, _ := degradedFromWSResults(results, nil, 2)
	want := []string{"2.2.2.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("degradedFromWSResults() = %v, want %v", got, want)
	}
}
