package main

import (
	"errors"
	"testing"
	"time"
)

func newSourceHealthApp() *app {
	return &app{sourceHealth: make(map[string]*sourceHealthStat)}
}

// 连续失败达到阈值后必须进入退避，否则每轮都要为必死的源白等超时。
func TestSourceHealthBacksOffAfterThreshold(t *testing.T) {
	a := newSourceHealthApp()
	key := "https://dead.example.com/list.txt"

	for i := 0; i < sourceBackoffThreshold-1; i++ {
		a.recordSourceHealth(key, 0, errors.New("boom"))
		if a.sourceInBackoff(key) {
			t.Fatalf("第 %d 次失败不应退避（阈值 %d）", i+1, sourceBackoffThreshold)
		}
	}
	a.recordSourceHealth(key, 0, errors.New("boom"))
	if !a.sourceInBackoff(key) {
		t.Fatalf("连续 %d 次失败后必须退避", sourceBackoffThreshold)
	}
}

// 退避时长随失败次数翻倍，但不得超过上限。
func TestSourceHealthBackoffGrowsAndCaps(t *testing.T) {
	a := newSourceHealthApp()
	key := "https://dead.example.com/list.txt"

	for i := 0; i < sourceBackoffThreshold; i++ {
		a.recordSourceHealth(key, 0, errors.New("boom"))
	}
	first := a.sourceHealth[key].BackoffUntil.Sub(time.Now())
	if first < sourceBackoffBase/2 || first > sourceBackoffBase+time.Minute {
		t.Fatalf("首次退避约应为 %v，实际 %v", sourceBackoffBase, first)
	}

	// 再多失败若干次，退避应增长但封顶。
	for i := 0; i < 10; i++ {
		a.recordSourceHealth(key, 0, errors.New("boom"))
	}
	capped := a.sourceHealth[key].BackoffUntil.Sub(time.Now())
	if capped > sourceBackoffMax+time.Minute {
		t.Fatalf("退避不得超过上限 %v，实际 %v", sourceBackoffMax, capped)
	}
}

// 成功一次即完全复位：退避清空、连败归零。
func TestSourceHealthSuccessResetsBackoff(t *testing.T) {
	a := newSourceHealthApp()
	key := "https://flaky.example.com/list.txt"

	for i := 0; i < sourceBackoffThreshold; i++ {
		a.recordSourceHealth(key, 0, errors.New("boom"))
	}
	if !a.sourceInBackoff(key) {
		t.Fatal("前置条件不成立：应已退避")
	}

	a.recordSourceHealth(key, 12, nil)
	if a.sourceInBackoff(key) {
		t.Fatal("成功一次后必须立即解除退避")
	}
	stat := a.sourceHealth[key]
	if stat.ConsecutiveFailures != 0 {
		t.Fatalf("连败计数应归零，实际 %d", stat.ConsecutiveFailures)
	}
	if !stat.BackoffUntil.IsZero() {
		t.Fatalf("退避截止应清空，实际 %v", stat.BackoffUntil)
	}
	if stat.LastError != "" {
		t.Fatalf("有产出时上次错误应清空，实际 %q", stat.LastError)
	}
}

// 从没失败过的源不应被判为退避。
func TestSourceHealthUnknownKeyNotInBackoff(t *testing.T) {
	a := newSourceHealthApp()
	if a.sourceInBackoff("https://never-tried.example.com/x.txt") {
		t.Fatal("未知源不应被判为退避")
	}
}

// 空 key 不记账，避免把无标识的调用写成脏数据。
func TestRecordSourceHealthIgnoresEmptyKey(t *testing.T) {
	a := newSourceHealthApp()
	a.recordSourceHealth("", 5, nil)
	if len(a.sourceHealth) != 0 {
		t.Fatalf("空 key 不应入账，实际 %v", a.sourceHealth)
	}
}
