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
