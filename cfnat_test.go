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
