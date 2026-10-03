package main

import (
	"strings"
	"testing"
)

func TestParseProcTCPUpstreamGroupsByRemoteIP(t *testing.T) {
	// 443 = 0x01BB；local 临时端口 EFACEFA0:C350 等。
	input := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode
   0: EFACEFA0:C350 08080808:01BB 01 00000000:00000000 00:00000000 00000000 0 0 100
   1: EFACEFA0:C351 08080808:01BB 01 00000000:00000000 00:00000000 00000000 0 0 101
   2: EFACEFA0:C352 08080808:01BB 08 00000000:00000000 00:00000000 00000000 0 0 102
   3: EFACEFA0:C353 08080808:01BB 06 00000000:00000000 00:00000000 00000000 0 0 103
   4: EFACEFA0:C354 6401A8C0:01BB 01 00000000:00000000 00:00000000 00000000 0 0 104
   5: EFACEFA0:C355 08080808:0050 01 00000000:00000000 00:00000000 00000000 0 0 105
   6: EFACEFA0:C356 08080808:01BB 0A 00000000:00000000 00:00000000 00000000 0 0 106`
	byIP := make(map[string]*upstreamConnection)
	total := 0
	if err := parseProcTCPUpstream(strings.NewReader(input), false, 443, byIP, &total); err != nil {
		t.Fatal(err)
	}
	item := byIP["8.8.8.8"]
	if item == nil {
		t.Fatalf("8.8.8.8 missing, got %#v", byIP)
	}
	// 行0/1 = ESTABLISHED ×2；行2 = CLOSE_WAIT 计入（在途）；行3 TIME_WAIT 排除；行6 LISTEN 排除。
	if item.Connections != 3 || item.States["ESTABLISHED"] != 2 || item.States["CLOSE_WAIT"] != 1 {
		t.Fatalf("connections=%d states=%#v", item.Connections, item.States)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3（私网/非443端口/TIME_WAIT/LISTEN 均不得计入）", total)
	}
	if _, exists := byIP["192.168.1.100"]; exists {
		t.Fatal("private upstream must be filtered")
	}
}

func TestWithoutPoolIP(t *testing.T) {
	ips := []string{"1.2.3.4", "5.6.7.8", "9.10.11.12"}
	remaining, found := withoutPoolIP(ips, "5.6.7.8")
	if !found || len(remaining) != 2 || remaining[0] != "1.2.3.4" || remaining[1] != "9.10.11.12" {
		t.Fatalf("remaining=%v found=%v", remaining, found)
	}
	remaining, found = withoutPoolIP(ips, "255.255.255.255")
	if found || len(remaining) != 3 {
		t.Fatalf("missing kick must keep pool intact, found=%v remaining=%v", found, remaining)
	}
	remaining, found = withoutPoolIP([]string{"1.2.3.4"}, "1.2.3.4")
	if !found || len(remaining) != 0 {
		t.Fatalf("last-ip kick: found=%v remaining=%v", found, remaining)
	}
}
