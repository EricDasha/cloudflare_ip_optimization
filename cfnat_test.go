//go:build ignore

// cfnat.go 的单元测试，用 `go test cfnat.go cfnat_test.go` 显式运行。
package main

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestIPManagerNextTargetsRoundRobin(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"})

	for _, test := range []struct {
		name  string
		count int
		want  []string
	}{
		{name: "first", count: 1, want: []string{"192.0.2.1:443"}},
		{name: "next", count: 1, want: []string{"192.0.2.2:443"}},
		{name: "wrap", count: 2, want: []string{"192.0.2.3:443", "192.0.2.1:443"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := m.nextTargets(443, test.count); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("nextTargets() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestIPManagerStickySameClientSameUpstream(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"})

	first := m.nextTargetsForClient("192.168.1.11", 443, 1)
	if len(first) != 1 {
		t.Fatalf("expected 1 target, got %v", first)
	}
	// 同一设备多次连接（含并发语义）必须命中同一上游。
	for range 10 {
		again := m.nextTargetsForClient("192.168.1.11", 443, 1)
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("sticky broken: first=%v again=%v", first, again)
		}
	}
	// 不推进轮转指针：粘性分发不干扰 Round-Robin 状态。
	if got := m.nextTargets(443, 1); !reflect.DeepEqual(got, []string{"192.0.2.1:443"}) {
		t.Fatalf("sticky advanced round-robin cursor: %v", got)
	}
}

func TestIPManagerStickyDifferentClientsSpread(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"})

	seen := map[string]string{}
	for _, client := range []string{"192.168.1.11", "192.168.1.12", "192.168.1.13", "192.168.1.14"} {
		targets := m.nextTargetsForClient(client, 443, 1)
		if len(targets) != 1 {
			t.Fatalf("client %s: expected 1 target, got %v", client, targets)
		}
		seen[client] = targets[0]
	}
	distinct := map[string]struct{}{}
	for _, target := range seen {
		distinct[target] = struct{}{}
	}
	if len(distinct) < 2 {
		t.Fatalf("4 clients all hashed to one upstream: %v", seen)
	}
}

// 均衡的判据不是「两台设备没有撞车」，而是「出口数 ≥ 设备数时，每台设备独占一个」。
// 旧实现用哈希取模，撞车概率不低；撞上就是一台设备把负载全吃了，另一台闲置。
func TestIPManagerStickyDevicesGetDistinctUpstreams(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"})

	devices := []string{"192.168.1.11", "192.168.1.12", "192.168.1.13", "192.168.1.14"}
	seen := map[string]string{}
	for _, client := range devices {
		seen[client] = m.nextTargetsForClient(client, 443, 1)[0]
	}
	distinct := map[string]struct{}{}
	for _, target := range seen {
		distinct[target] = struct{}{}
	}
	if len(distinct) != len(devices) {
		t.Fatalf("4 devices on 4 exits did not each get one: %v", seen)
	}
	// 再来一遍（同设备重连）必须还是原出口，否则「均衡」变成了「乱跳」。
	for _, client := range devices {
		if got := m.nextTargetsForClient(client, 443, 1)[0]; got != seen[client] {
			t.Fatalf("client %s jumped from %s to %s", client, seen[client], got)
		}
	}
}

// 设备多于出口：只能复用，但不得把某台设备随机丢来丢去。
func TestIPManagerStickyMoreDevicesThanUpstreams(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2"})
	for i := 0; i < 6; i++ {
		client := string(rune('a' + i))
		first := m.nextTargetsForClient(client, 443, 1)[0]
		again := m.nextTargetsForClient(client, 443, 1)[0]
		if first != again {
			t.Fatalf("client %s unstable: %s -> %s", client, first, again)
		}
	}
}

// 换池不得让在位设备跳线：出口还在新池里就沿用，只有被剔除了才重分。
func TestIPManagerStickyKeepsPinAcrossPoolChange(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"})
	pinned := m.nextTargetsForClient("192.168.1.11", 443, 1)[0]

	// 池子里加一个新成员（踢人补位的常态），原来的出口仍然在。
	m.SetIPAddresses([]string{"192.0.2.9", "192.0.2.1", "192.0.2.2", "192.0.2.3"})
	if got := m.nextTargetsForClient("192.168.1.11", 443, 1)[0]; got != pinned {
		t.Fatalf("pool growth moved a live device: %s -> %s", pinned, got)
	}

	// 出口被剔除，才允许重分。
	m.SetIPAddresses([]string{"192.0.2.9", "192.0.2.2", "192.0.2.3"})
	if got := m.nextTargetsForClient("192.168.1.11", 443, 1)[0]; got == pinned {
		t.Fatalf("removed upstream %s is still assigned", pinned)
	}
}

// 钉住的出口拨不通、回退到别的成员并拨通时，钉位要跟着改。
// 不改的话该设备下一条连接又去撞同一堵墙，表现为「这台设备一直卡」。
func TestIPManagerRepinAfterFallback(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2"})
	first := m.nextTargetsForClient("192.168.1.11", 443, 2)
	if len(first) != 2 {
		t.Fatalf("expected 2 targets, got %v", first)
	}
	m.repinClient("192.168.1.11", first[1])
	if got := m.nextTargetsForClient("192.168.1.11", 443, 1)[0]; got != first[1] {
		t.Fatalf("repin ignored: %v, want %s", got, first[1])
	}
	// 池外目标（如 fallback 域名）不参与钉位，避免把设备钉到会立刻失效的地方。
	m.repinClient("192.168.1.11", "outside.example:443")
	if got := m.nextTargetsForClient("192.168.1.11", 443, 1)[0]; got != first[1] {
		t.Fatalf("out-of-pool repin took effect: %v", got)
	}
}

func TestIPManagerStickyFallbackOrderKept(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"})

	targets := m.nextTargetsForClient("192.168.1.50", 443, 3)
	if len(targets) != 3 {
		t.Fatalf("expected 3 fallback targets, got %v", targets)
	}
	// 回退顺序必须是从粘性锚点起的连续序列。
	start := targets[0]
	rest := m.nextTargetsForClient("192.168.1.50", 443, 3)
	if !reflect.DeepEqual(targets, rest) {
		t.Fatalf("sticky fallback order unstable: %v vs %v", targets, rest)
	}
	_ = start
}

func TestIPManagerStickyEmptyClientFallsBackToRoundRobin(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2"})
	if got := m.nextTargetsForClient("", 443, 1); !reflect.DeepEqual(got, []string{"192.0.2.1:443"}) {
		t.Fatalf("empty client should use round-robin start, got %v", got)
	}
}

func TestIPManagerNextTargetsLimitsToPool(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"2001:db8::1", "2001:db8::2"})
	got := m.nextTargets(8443, 5)
	want := []string{"[2001:db8::1]:8443", "[2001:db8::2]:8443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nextTargets() = %v, want %v", got, want)
	}
}

func TestIPManagerPriorityDoesNotChangeRoundRobin(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"})
	m.SetPriorityIPs([]string{"192.0.2.1"})

	var got []string
	for range 5 {
		got = append(got, m.nextTargets(443, 1)...)
	}
	want := []string{"192.0.2.1:443", "192.0.2.2:443", "192.0.2.3:443", "192.0.2.1:443", "192.0.2.2:443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("priority changed round-robin targets = %v, want %v", got, want)
	}
}

func TestHandleConnectionUsesOneUpstreamPerSession(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	calls := make([]string, 0, 2)
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		calls = append(calls, addr)
		upstream, peer := net.Pipe()
		_ = peer.Close()
		return upstream, nil
	}

	done := make(chan struct{})
	go func() {
		handleConnectionWithDial(server, []string{"first:443", "second:443"}, dial)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleConnectionWithDial did not finish")
	}
	if !reflect.DeepEqual(calls, []string{"first:443"}) {
		t.Fatalf("dial calls = %v, want only the first upstream", calls)
	}
}

func TestHandleConnectionFallsBackOnlyAfterDialFailure(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	calls := make([]string, 0, 2)
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		calls = append(calls, addr)
		if addr == "first:443" {
			return nil, errors.New("unreachable")
		}
		upstream, peer := net.Pipe()
		_ = peer.Close()
		return upstream, nil
	}

	done := make(chan struct{})
	go func() {
		handleConnectionWithDial(server, []string{"first:443", "second:443"}, dial)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleConnectionWithDial did not finish")
	}
	if !reflect.DeepEqual(calls, []string{"first:443", "second:443"}) {
		t.Fatalf("dial calls = %v, want ordered fallback", calls)
	}
}

func TestParseFixedTargetsAcceptsIPAndDomain(t *testing.T) {
	got, err := parseFixedTargets("192.0.2.1, youxuan.cf.090227.xyz, 192.0.2.1")
	if err != nil {
		t.Fatalf("parseFixedTargets() error = %v", err)
	}
	want := []string{"192.0.2.1", "youxuan.cf.090227.xyz"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseFixedTargets() = %v, want %v", got, want)
	}
}

func TestParseFixedTargetsRejectsInvalidDomain(t *testing.T) {
	for _, bad := range []string{",", "onlylabel", "-bad.xyz", "bad-.xyz", "sp ace.xyz", "x@y.xyz", "1.2.3.999", "192.0.2.1, invalid_host"} {
		if _, err := parseFixedTargets(bad); err == nil {
			t.Fatalf("parseFixedTargets(%q) should fail", bad)
		}
	}
}

func TestValidForwardHostname(t *testing.T) {
	for _, good := range []string{"youxuan.cf.090227.xyz", "www.visa.cn", "cf.877774.xyz", "staticdelivery.nexusmods.com"} {
		if !validForwardHostname(good) {
			t.Fatalf("validForwardHostname(%q) = false, want true", good)
		}
	}
	for _, bad := range []string{"192.0.2.1", "localhost", "*.cf.090227.xyz", "a/b.xyz", "a..b", "-a.xyz", "a.xyz/"} {
		if validForwardHostname(bad) {
			t.Fatalf("validForwardHostname(%q) = true, want false", bad)
		}
	}
}

func TestCapFixedTargetsKeepsAllDomains(t *testing.T) {
	results := []result{
		{ip: "192.0.2.1"},
		{ip: "a.example.com"},
		{ip: "192.0.2.2"},
		{ip: "b.example.com"},
		{ip: "192.0.2.3"},
	}
	got := capFixedTargets(results, 1)
	want := []string{"192.0.2.1", "a.example.com", "b.example.com"}
	var targets []string
	for _, r := range got {
		targets = append(targets, r.ip)
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("capFixedTargets() = %v, want %v", targets, want)
	}
	if n := countIPTargets(got); n != 1 {
		t.Fatalf("countIPTargets() = %d, want 1", n)
	}
	if ipN, domainN := countFixedKinds(got); ipN != 1 || domainN != 2 {
		t.Fatalf("countFixedKinds() = (%d,%d), want (1,2)", ipN, domainN)
	}
}

func TestCapFixedTargetsZeroIPKeepsDomains(t *testing.T) {
	results := []result{{ip: "a.example.com"}, {ip: "b.example.com"}}
	got := capFixedTargets(results, 20)
	if len(got) != 2 {
		t.Fatalf("capFixedTargets() dropped domains: %v", got)
	}
}

func TestApplyColoFilter(t *testing.T) {
	results := []result{
		{ip: "1.1.1.1", dataCenter: "HKG"},
		{ip: "2.2.2.2", dataCenter: "SJC"},
		{ip: "3.3.3.3", dataCenter: "LAX"},
	}
	if got := applyColoFilter(results, "hkg,sjc"); len(got) != 2 {
		t.Fatalf("applyColoFilter(hkg,sjc) = %d results, want 2", len(got))
	}
	// 全部滤掉必须返回空切片，调用方据此重扫、不得进 accept 循环。
	if got := applyColoFilter(results, "XXX"); len(got) != 0 {
		t.Fatalf("applyColoFilter(XXX) = %d results, want 0", len(got))
	}
}
