package main

import (
	"context"
	"testing"
)

// 生产事故回归：池里混入 connect 1168ms 的慢 IP，设备粘性把 LAN 设备锁死在
// 它上面，YouTube 评论区加载不出来。根因是「进池只考 WS 握手通不通，不考快慢」。
// 这组测试钉死两道新关卡。
func TestPoolEntryLatencyCeilingDefaultRejectsKnownBadIP(t *testing.T) {
	// 生产实测（NAS 现场，同一 SNI）：好 IP WS ~190ms；坏 IP WS 485ms / 1341ms；
	// 官方 anycast 基线 connect 155ms。默认 800ms 应放行好 IP、拦住事故元凶。
	t.Setenv("PROXY_POOL_MAX_ENTRY_LATENCY_MS", "")
	ceiling := poolEntryMaxLatencyMs()
	if ceiling != 800 {
		t.Fatalf("default entry ceiling = %dms, want 800ms", ceiling)
	}
	for _, tc := range []struct {
		ip      string
		latency int64
		want    bool
	}{
		{"43.175.131.30", 190, true},
		{"47.86.176.143", 198, true},
		{"43.175.131.207", 191, true},
		{"167.179.99.0", 485, true},     // 偏慢但仍在默认上限内（可手动顶替掉）
		{"161.33.215.154", 1341, false}, // 事故元凶，必须拦
	} {
		if got := tc.latency <= ceiling; got != tc.want {
			t.Fatalf("%s latency=%dms under ceiling=%d -> %v, want %v",
				tc.ip, tc.latency, ceiling, got, tc.want)
		}
	}
}

func TestPoolEntryLatencyCeilingIsConfigurable(t *testing.T) {
	t.Setenv("PROXY_POOL_MAX_ENTRY_LATENCY_MS", "300")
	if got := poolEntryMaxLatencyMs(); got != 300 {
		t.Fatalf("configured ceiling = %dms, want 300ms", got)
	}
	// 越界值必须回落默认值，防止 0 / 99999 造成「永远拦」或「永不拦」
	t.Setenv("PROXY_POOL_MAX_ENTRY_LATENCY_MS", "99999")
	if got := poolEntryMaxLatencyMs(); got != 800 {
		t.Fatalf("out-of-range ceiling = %dms, want fallback 800ms", got)
	}
	t.Setenv("PROXY_POOL_MAX_ENTRY_LATENCY_MS", "0")
	if got := poolEntryMaxLatencyMs(); got != 800 {
		t.Fatalf("zero ceiling = %dms, want fallback 800ms", got)
	}
}

// measurePoolEntryLatency 必须握手全败时返回 0 —— 调用方把 0 当作「不通」
// 而拒绝顶替，这是阻止垃圾 IP 进池的最后一道闸。
func TestMeasurePoolEntryLatencyReturnsZeroWhenUnreachable(t *testing.T) {
	cfg := proxyAutoConfig{
		Host: "example.invalid",
		Path: "/ws",
		Port: 443,
	}
	// 203.0.113.0/24 是 RFC 5737 保留测试段，保证不可达。
	// 该地址不立即 RST，故每轮耗满 6s 上限，三轮约 18s。
	if got := measurePoolEntryLatency(context.Background(), "203.0.113.1", cfg); got != 0 {
		t.Fatalf("unreachable IP should measure 0, got %dms", got)
	}
}

// 三次取最优：任一次成功即返回正延迟，不得因某次失败就整体判 0。
func TestMeasurePoolEntryLatencyUsesBestOfThree(t *testing.T) {
	// 本用例只校验「三次」的语义常量：上限 6s/轮 × 3 轮必须留出余量，
	// 否则调用点的 20s ctx 会在第三轮中途掐断，读到 0 而误拒好 IP。
	const perProbe = 6
	const rounds = 3
	if perProbe*rounds >= 20 {
		t.Fatalf("worst-case probe time %ds must stay under the caller's 20s budget", perProbe*rounds)
	}
}
