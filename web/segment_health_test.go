package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"syscall"
	"testing"
)

// --- 失败归类 ---

type fakeNetErr struct{ timeout bool }

func (e fakeNetErr) Error() string   { return "fake net error" }
func (e fakeNetErr) Timeout() bool   { return e.timeout }
func (e fakeNetErr) Temporary() bool { return false }

func TestClassifyProbeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, failureNone},
		{"net timeout", fakeNetErr{timeout: true}, failureTimeout},
		{"deadline", context.DeadlineExceeded, failureTimeout},
		{"refused syscall", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), failureRefused},
		{"unreachable syscall", fmt.Errorf("dial: %w", syscall.EHOSTUNREACH), failureUnreachable},
		{"reset syscall", fmt.Errorf("read: %w", syscall.ECONNRESET), failureProtocol},
		{"refused text", errors.New("dial tcp 1.2.3.4:443: connect: connection refused"), failureRefused},
		{"tls text", errors.New("tls: first record does not look like a TLS handshake"), failureHandshake},
		{"ws code", errors.New("WS 状态码 403"), failureProtocol},
		{"chinese timeout", errors.New("VLESS 探针启动超时"), failureTimeout},
		{"unknown", errors.New("something odd"), failureOther},
	}
	for _, tc := range cases {
		if got := classifyProbeError(tc.err); got != tc.want {
			t.Errorf("%s: classifyProbeError() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// net.Error 的 Timeout 判定优先于文本匹配：带 timeout 字样的非超时错误不能误判。
func TestClassifyProbeErrorPrefersTypeOverText(t *testing.T) {
	err := fakeNetErr{timeout: false}
	if got := classifyProbeError(err); got != failureOther {
		t.Fatalf("non-timeout net.Error classified as %q, want other", got)
	}
}

// --- 片区降权 ---

func TestDemotedSegmentsRequiresMinSamples(t *testing.T) {
	// 同段 4 个全失败：样本不足 5，不降权——一两个候选失败说明不了问题。
	results := make([]proxyScanResult, 0, 4)
	for i := 0; i < 4; i++ {
		results = append(results, proxyScanResult{
			IP: fmt.Sprintf("104.16.0.%d", i+1), Error: "timeout", ErrorClass: failureTimeout,
		})
	}
	if got := demotedSegments(results); len(got) != 0 {
		t.Fatalf("segment with %d samples was demoted, want none", 4)
	}
}

func TestDemotedSegmentsFlagsHighFailureSegment(t *testing.T) {
	results := make([]proxyScanResult, 0, 10)
	for i := 0; i < 5; i++ {
		results = append(results, proxyScanResult{IP: fmt.Sprintf("104.16.0.%d", i+1), Error: "timeout", ErrorClass: failureTimeout})
	}
	for i := 0; i < 5; i++ {
		results = append(results, proxyScanResult{IP: fmt.Sprintf("172.64.0.%d", i+1)})
	}
	got := demotedSegments(results)
	if _, ok := got["104.16.0.0/13"]; !ok {
		t.Fatalf("failed segment not demoted: %v", got)
	}
	if _, ok := got["172.64.0.0/13"]; ok {
		t.Fatal("healthy segment was demoted")
	}
}

func TestOrderPassedBySegmentHealthMovesTail(t *testing.T) {
	results := []proxyScanResult{
		{IP: "104.16.0.1", Error: "timeout", ErrorClass: failureTimeout},
		{IP: "104.16.0.2", Error: "timeout", ErrorClass: failureTimeout},
		{IP: "104.16.0.3", Error: "timeout", ErrorClass: failureTimeout},
		{IP: "104.16.0.4", Error: "timeout", ErrorClass: failureTimeout},
		{IP: "104.16.0.5", Error: "timeout", ErrorClass: failureTimeout},
		{IP: "172.64.0.1"},
		{IP: "172.64.0.2"},
	}
	passed := []string{"104.16.0.1", "172.64.0.1", "104.16.0.2", "172.64.0.2"}
	got := orderPassedBySegmentHealth(passed, results)
	want := []string{"172.64.0.1", "172.64.0.2", "104.16.0.1", "104.16.0.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderPassedBySegmentHealth() = %v, want %v", got, want)
	}
	if len(got) != len(passed) {
		t.Fatal("ordering must not change list length")
	}
}

func TestOrderPassedBySegmentHealthNoopWhenNothingDemoted(t *testing.T) {
	passed := []string{"104.16.0.1", "172.64.0.1"}
	got := orderPassedBySegmentHealth(passed, nil)
	if !reflect.DeepEqual(got, passed) {
		t.Fatalf("got %v, want identity", got)
	}
}

// --- 调度器换人的片区检查 ---

func TestDegradedSegmentDiversity(t *testing.T) {
	// 两段 → 一段：变脆，必须拒。
	if !degradedSegmentDiversity(
		[]string{"104.16.0.1", "172.64.0.1"},
		[]string{"104.16.0.1", "104.16.0.2"},
	) {
		t.Fatal("two segments collapsing to one must be flagged")
	}
	// 两段 → 两段：段数不变，放行。
	if degradedSegmentDiversity(
		[]string{"104.16.0.1", "172.64.0.1"},
		[]string{"104.16.0.2", "172.64.0.2"},
	) {
		t.Fatal("same segment count must not be flagged")
	}
	// 一段 → 两段：变强，放行。
	if degradedSegmentDiversity([]string{"104.16.0.1"}, []string{"104.16.0.1", "172.64.0.1"}) {
		t.Fatal("increasing segment count must not be flagged")
	}
}

// 端到端：段数会减少时，调度器换池路径必须拒绝。
// 这里只验证判定函数被正确接在替换前后——替换本身是纯函数。
func TestReplacePoolMemberThenDiversityCheck(t *testing.T) {
	current := []string{"104.16.0.1", "172.64.0.1", "131.0.72.1"}
	next := replacePoolMember(current, "172.64.0.1", "104.16.0.9")
	if !degradedSegmentDiversity(current, next) {
		t.Fatalf("replacing the only member of a segment was not flagged: %v -> %v", current, next)
	}
	// 同段内换人：段数不变。
	next2 := replacePoolMember(current, "104.16.0.1", "104.16.0.9")
	if degradedSegmentDiversity(current, next2) {
		t.Fatalf("same-segment replacement was wrongly flagged: %v -> %v", current, next2)
	}
}

// 扫描结果必须带上归类：没有归类的失败无法参与片区统计。
func TestScanResultsCarryErrorClass(t *testing.T) {
	cfg := proxyAutoConfig{Concurrency: 1, MaxLatency: 200}
	// 127.0.0.1 的 1 号端口在本机必然被拒——用来验证归类字段被填上。
	results := scanProxyWebSockets(context.Background(), []string{"127.0.0.1"}, proxyAutoConfig{
		Host: "example.com", Path: "/", Port: 1, Concurrency: 1, MaxLatency: 200,
	})
	_ = cfg
	if len(results) != 1 {
		t.Fatalf("got %d results", len(results))
	}
	if results[0].Error == "" {
		t.Skip("port 1 unexpectedly accepted a connection; environment-dependent")
	}
	if results[0].ErrorClass == "" {
		t.Fatal("failed scan result carries no error class")
	}
}

// 测试二进制同时充当假 cfnat / sing-box 的宿主：被 exec 出来时先认领
// 假身份、扮演完直接退出，否则照常跑测试。详见 fakebin_test.go。
func TestMain(m *testing.M) {
	runFakeBinaryIfChild()
	os.Exit(m.Run())
}

var _ = net.ParseIP
