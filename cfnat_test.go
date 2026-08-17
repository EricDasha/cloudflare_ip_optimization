package main

import (
	"reflect"
	"testing"
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
