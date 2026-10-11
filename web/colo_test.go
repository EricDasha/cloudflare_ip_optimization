package main

import (
	"encoding/json"
	"testing"
)

// coloFromCFRay 按 CFST 实战法提取三字机房码：CF-RAY 射线 ID 段恒为小写
// 十六进制，全串唯一的三连大写字母即机房码。空头/无匹配必须返回空串——
// 空串即「无机房情报」，软偏好闸把它当非偏好处理，永不阻塞入池。
func TestColoFromCFRay(t *testing.T) {
	for _, tc := range []struct {
		ray  string
		want string
	}{
		{"8f3b2c1d4e5f6a7b-HKG", "HKG"},
		{"7bd32409eda7b020-SJC", "SJC"},
		{"7bd32409eda7b020-NRT", "NRT"},
		{"  8f3b2c1d4e5f6a7b-HKG  ", "HKG"}, // 首尾空白须容忍
		{"", ""},                            // 空头
		{"8f3b2c1d4e5f6a7b", ""},            // 无机房段
		{"8f3b2c1d4e5f6a7b-", ""},           // 尾缀空
	} {
		if got := coloFromCFRay(tc.ray); got != tc.want {
			t.Fatalf("coloFromCFRay(%q) = %q, want %q", tc.ray, got, tc.want)
		}
	}
}

// proxyScanResult.Colo 必须以 omitempty 序列化：旧持久化文件（proxy-active.json、
// proxy-candidates.json）没有此字段，空值不得在 JSON 里留垃圾键。
func TestProxyScanResultColoJSONOmitEmpty(t *testing.T) {
	data, err := json.Marshal(proxyScanResult{IP: "1.2.3.4", Latency: 59})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"ip":"1.2.3.4","latency":59}`; string(data) != want {
		t.Fatalf("empty colo must be omitted, got %s", data)
	}
	data, err = json.Marshal(proxyScanResult{IP: "1.2.3.4", Latency: 59, Colo: "HKG"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"ip":"1.2.3.4","latency":59,"colo":"HKG"}`; string(data) != want {
		t.Fatalf("colo must serialize after latency, got %s", data)
	}
}

// orderPassedByColoPreference 三条铁律：未配置=恒等；充裕=偏好在前；匮乏=补位。
// 只重排不淘汰——长度永不缩，偏好为空串/未知机房者归非偏好区。
func TestOrderPassedByColoPreference(t *testing.T) {
	results := []proxyScanResult{
		{IP: "1.1.1.1", Colo: "HKG"},
		{IP: "2.2.2.2", Colo: "SJC"},
		{IP: "3.3.3.3", Colo: "HKG"},
		{IP: "4.4.4.4"}, // 无机房情报
		{IP: "5.5.5.5", Colo: "NRT"},
	}
	hkg := map[string]struct{}{"HKG": {}}

	// 未配置（空集）→ 恒等，序一字不动
	identity := orderPassedByColoPreference([]string{"2.2.2.2", "1.1.1.1"}, results, nil)
	if identity[0] != "2.2.2.2" || identity[1] != "1.1.1.1" {
		t.Fatalf("disabled preference must be identity, got %v", identity)
	}

	// 偏好充裕：HKG 两枚在前且区内保序，其余殿后
	got := orderPassedByColoPreference([]string{"2.2.2.2", "1.1.1.1", "5.5.5.5", "3.3.3.3", "4.4.4.4"}, results, hkg)
	want := []string{"1.1.1.1", "3.3.3.3", "2.2.2.2", "5.5.5.5", "4.4.4.4"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("abundant prefer: got %v, want %v", got, want)
		}
	}
	// 长度守恒——只重排不淘汰
	if len(got) != 5 {
		t.Fatalf("reorder must never shrink the list, got %d", len(got))
	}

	// 偏好匮乏：仅一枚 HKG 在前，其余机房照常补位
	scarce := orderPassedByColoPreference([]string{"2.2.2.2", "5.5.5.5", "1.1.1.1"}, results, hkg)
	if scarce[0] != "1.1.1.1" {
		t.Fatalf("scarce prefer: preferred must lead, got %v", scarce)
	}
}

// poolColoPreferenceSet 解析：分隔容忍、小写转大写、非三字码剔除、空即禁用。
func TestPoolColoPreferenceSetParsing(t *testing.T) {
	t.Setenv("PROXY_POOL_COLO_PREFER", "")
	if got := poolColoPreferenceSet(); got != nil {
		t.Fatalf("unset must disable, got %v", got)
	}
	t.Setenv("PROXY_POOL_COLO_PREFER", "hkg, SJC；nrt  xx")
	got := poolColoPreferenceSet()
	want := map[string]struct{}{"HKG": {}, "SJC": {}, "NRT": {}}
	if len(got) != len(want) {
		t.Fatalf("parse got %v, want %v", got, want)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing %s in %v", k, got)
		}
	}
	t.Setenv("PROXY_POOL_COLO_PREFER", "abcd,e,")
	if got := poolColoPreferenceSet(); got != nil {
		t.Fatalf("all-invalid codes must disable, got %v", got)
	}
}
