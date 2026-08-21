package main

import (
	"strings"
	"testing"
)

func TestCFnatListenPort(t *testing.T) {
	if got := cfnatListenPort("0.0.0.0:4321"); got != 4321 {
		t.Fatalf("port = %d", got)
	}
	if got := cfnatListenPort("invalid"); got != 1234 {
		t.Fatalf("fallback port = %d", got)
	}
}

func TestParseProcEndpoint(t *testing.T) {
	ip, port, ok := parseProcEndpoint("0101A8C0:04D2", false)
	if !ok || ip != "192.168.1.1" || port != 1234 {
		t.Fatalf("got %q:%d, %v", ip, port, ok)
	}
}

func TestProcTCPState(t *testing.T) {
	if got := procTCPState("01"); got != "ESTABLISHED" {
		t.Fatalf("state = %q", got)
	}
	if got := procTCPState("FF"); got != "UNKNOWN" {
		t.Fatalf("unknown state = %q", got)
	}
}

func TestParseProcTCPAggregatesPrivateSources(t *testing.T) {
	input := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode
   0: 00000000:04D2 6401A8C0:CF08 01 00000000:00000000 00:00000000 00000000 0 0 0
   1: 00000000:04D2 6401A8C0:CF09 06 00000000:00000000 00:00000000 00000000 0 0 0
   2: 00000000:04D2 08080808:CF10 01 00000000:00000000 00:00000000 00000000 0 0 0
   3: 00000000:04D2 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 0`
	byIP := make(map[string]*cfnatConnection)
	total := 0
	if err := parseProcTCP(strings.NewReader(input), false, 1234, byIP, &total); err != nil {
		t.Fatal(err)
	}
	item := byIP["192.168.1.100"]
	if total != 2 || item == nil || item.Connections != 2 || item.States["ESTABLISHED"] != 1 || item.States["TIME_WAIT"] != 1 {
		t.Fatalf("total=%d, item=%#v", total, item)
	}
	if _, exists := byIP["8.8.8.8"]; exists {
		t.Fatal("public source must be filtered")
	}
}
