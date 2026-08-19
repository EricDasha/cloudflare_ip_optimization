package main

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
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

func TestIPManagerPriorityTargetsRemainFair(t *testing.T) {
	m := NewIPManager()
	m.SetIPAddresses([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"})
	m.SetPriorityIPs([]string{"192.0.2.1"})

	var got []string
	for range 5 {
		got = append(got, m.nextTargets(443, 1)...)
	}
	want := []string{
		"192.0.2.1:443", "192.0.2.1:443", "192.0.2.1:443",
		"192.0.2.2:443", "192.0.2.3:443",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weighted targets = %v, want %v", got, want)
	}
}

func TestDialFirstAvailableDoesNotWaitForSlowCandidate(t *testing.T) {
	started := make(chan string, 2)
	releaseFast := make(chan struct{})
	slowCanceled := make(chan struct{})
	winnerClosed := make(chan struct{})
	loserClosed := make(chan struct{})

	dial := func(ctx context.Context, _, addr string) (net.Conn, error) {
		started <- addr
		switch addr {
		case "fast":
			<-releaseFast
			client, peer := net.Pipe()
			go func() {
				buf := make([]byte, 1)
				_, _ = peer.Read(buf)
				_ = peer.Close()
				close(winnerClosed)
			}()
			return client, nil
		case "slow":
			<-ctx.Done()
			close(slowCanceled)
			client, peer := net.Pipe()
			go func() {
				buf := make([]byte, 1)
				_, _ = peer.Read(buf)
				_ = peer.Close()
				close(loserClosed)
			}()
			return client, nil
		default:
			return nil, errors.New("unexpected target")
		}
	}

	type result struct {
		conn net.Conn
		addr string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		conn, addr, _, err := dialFirstAvailable(context.Background(), []string{"fast", "slow"}, dial)
		resultCh <- result{conn: conn, addr: addr, err: err}
	}()

	for range 2 {
		<-started
	}
	close(releaseFast)

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("dialFirstAvailable() error = %v", got.err)
		}
		if got.addr != "fast" {
			t.Fatalf("dialFirstAvailable() addr = %q, want fast", got.addr)
		}
		_ = got.conn.Close()
	case <-time.After(time.Second):
		t.Fatal("dialFirstAvailable() waited for the slow candidate")
	}

	select {
	case <-slowCanceled:
	case <-time.After(time.Second):
		t.Fatal("slow candidate was not canceled")
	}
	select {
	case <-winnerClosed:
	case <-time.After(time.Second):
		t.Fatal("winning test connection was not closed")
	}
	select {
	case <-loserClosed:
	case <-time.After(time.Second):
		t.Fatal("losing connection was not cleaned up")
	}
}

func TestDialFirstAvailableReturnsErrorWhenAllCandidatesFail(t *testing.T) {
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		return nil, errors.New(addr + " failed")
	}

	conn, addr, _, err := dialFirstAvailable(context.Background(), []string{"one", "two"}, dial)
	if conn != nil {
		t.Fatal("dialFirstAvailable() returned a connection when all candidates failed")
	}
	if addr != "" {
		t.Fatalf("dialFirstAvailable() addr = %q, want empty", addr)
	}
	if err == nil || !strings.Contains(err.Error(), "所有转发地址均连接失败") {
		t.Fatalf("dialFirstAvailable() error = %v", err)
	}
}

func TestDialFirstAvailableRejectsEmptyTargets(t *testing.T) {
	conn, addr, _, err := dialFirstAvailable(context.Background(), nil, nil)
	if conn != nil || addr != "" || err == nil {
		t.Fatalf("dialFirstAvailable() = (%v, %q, %v), want nil, empty, error", conn, addr, err)
	}
}
