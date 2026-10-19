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

// /proc/net/tcp6 用四个 32 位字、每字按内存小端打印。::ffff:192.168.1.11
// 的正确写法是 00000000 00000000 FFFF0000 0B01A8C0。逐字反转后必须还原成同一
// 台设备——否则双栈监听下入站设备会整批消失。
func TestParseProcEndpointIPv6Mapped(t *testing.T) {
	ip, port, ok := parseProcEndpoint("0000000000000000FFFF00000B01A8C0:04D2", true)
	if !ok || ip != "192.168.1.11" || port != 1234 {
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

// 设备名册按「本地端口 == cfnat 监听端口」识别入站连接，只排掉回环/未指定。
// 公网来源同样要认——设备经端口映射从外网进来是常态。
func TestParseProcTCPAggregatesSources(t *testing.T) {
	input := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode
   0: 00000000:04D2 6401A8C0:CF08 01 00000000:00000000 00:00000000 00000000 0 0 0
   1: 00000000:04D2 6401A8C0:CF09 06 00000000:00000000 00:00000000 00000000 0 0 0
   2: 00000000:04D2 08080808:CF10 01 00000000:00000000 00:00000000 00000000 0 0 0
   3: 00000000:04D2 0100007F:CF11 01 00000000:00000000 00:00000000 00000000 0 0 0
   4: 00000000:04D2 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 0`
	byIP := make(map[string]*cfnatConnection)
	stats := procTCPStats{}
	if err := parseProcTCP(strings.NewReader(input), false, 1234, byIP, &stats); err != nil {
		t.Fatal(err)
	}
	item := byIP["192.168.1.100"]
	if stats.Total != 3 || stats.PortHits != 4 || stats.Scanned != 5 {
		t.Fatalf("stats=%+v", stats)
	}
	if item == nil || item.Connections != 2 || item.States["ESTABLISHED"] != 1 || item.States["TIME_WAIT"] != 1 {
		t.Fatalf("item=%#v", item)
	}
	if _, exists := byIP["8.8.8.8"]; !exists {
		t.Fatal("public source must be counted")
	}
	if _, exists := byIP["127.0.0.1"]; exists {
		t.Fatal("loopback source must be filtered")
	}
}
