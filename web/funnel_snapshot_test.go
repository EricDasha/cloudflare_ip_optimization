package main

import "testing"

// 刷新发生了但候选集合没变：考试结果依然指向同一代候选，必须放行。
// 线上形态：12:18 / 12:21 两轮各有 20 个候选通过 VLESS 终审，只因订阅慢扫
// 触发了一次刷新就被整轮丢掉，池子直到 12:24 恰好没有刷新插入才建起来。
func TestCandidateResultsStillValidWhenSameGeneration(t *testing.T) {
	latest := proxyCandidateSnapshot{IPs: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}}
	if !candidateResultsStillValid(latest, []string{"2.2.2.2", "3.3.3.3"}) {
		t.Fatal("same-generation refresh must not void a good round")
	}
}

// 通关的 IP 已从候选池消失：那代候选不存在了，结果必须作废。
func TestCandidateResultsInvalidWhenPassedIPPurged(t *testing.T) {
	latest := proxyCandidateSnapshot{IPs: []string{"1.1.1.1"}}
	if candidateResultsStillValid(latest, []string{"1.1.1.1", "9.9.9.9"}) {
		t.Fatal("round must be voided when a passed IP was purged from candidates")
	}
}

// 空结果没有可作废的对象；调用方另有 MinPool 判据兜底。
func TestCandidateResultsValidWhenNothingPassed(t *testing.T) {
	if !candidateResultsStillValid(proxyCandidateSnapshot{}, nil) {
		t.Fatal("empty pass list should not be reported as stale snapshot")
	}
}

// 候选池整体换血（刷新后全新一批）：旧结果一个都对不上，必须作废。
func TestCandidateResultsInvalidWhenPoolReplacedWholesale(t *testing.T) {
	latest := proxyCandidateSnapshot{IPs: []string{"10.0.0.1", "10.0.0.2"}}
	if candidateResultsStillValid(latest, []string{"1.1.1.1", "2.2.2.2"}) {
		t.Fatal("round must be voided when the whole candidate generation was replaced")
	}
}
