package main

import "testing"

// A4：VLESS 测速端点必须可覆盖——speed.cloudflare.com 经 sing-box 隧道时
// 常在 20s 预算内跑不完，硬编码会让「测」按钮的测速半边恒为失败。
func TestVLESSSpeedTestURLDefaultUsesCloudflare(t *testing.T) {
	t.Setenv("PROXY_VLESS_SPEED_URL", "")
	got := vlessSpeedTestURL(2097152)
	want := "https://speed.cloudflare.com/__down?bytes=2097152"
	if got != want {
		t.Fatalf("vlessSpeedTestURL() = %q, want %q", got, want)
	}
}

func TestVLESSSpeedTestURLHonorsOverride(t *testing.T) {
	t.Setenv("PROXY_VLESS_SPEED_URL", "https://proof.ovh.net/files/10Mb.dat")
	got := vlessSpeedTestURL(2097152)
	want := "https://proof.ovh.net/files/10Mb.dat"
	if got != want {
		t.Fatalf("vlessSpeedTestURL() override = %q, want %q", got, want)
	}
}

func TestVLESSSpeedTestURLSubstitutesBytesPlaceholder(t *testing.T) {
	t.Setenv("PROXY_VLESS_SPEED_URL", "https://mirror.example/down?bytes={bytes}")
	got := vlessSpeedTestURL(1048576)
	want := "https://mirror.example/down?bytes=1048576"
	if got != want {
		t.Fatalf("vlessSpeedTestURL() placeholder = %q, want %q", got, want)
	}
}

func TestVLESSSpeedTestURLTrimsWhitespace(t *testing.T) {
	t.Setenv("PROXY_VLESS_SPEED_URL", "  https://mirror.example/f.bin  ")
	got := vlessSpeedTestURL(1048576)
	want := "https://mirror.example/f.bin"
	if got != want {
		t.Fatalf("vlessSpeedTestURL() trim = %q, want %q", got, want)
	}
}

// A1：根目录是多个单文件 main 的集合，不可作为 ./... 包编译。
// 这些文件必须带 //go:build ignore，否则 go build ./... 必然失败。
func TestStandaloneMainsCarryBuildIgnore(t *testing.T) {
	// 该测试跑在 ./web 包内，只需保证 vlessSpeedTestURL 的宿主常量存在且唯一。
	if defaultVLESSSpeedHost != "speed.cloudflare.com" {
		t.Fatalf("defaultVLESSSpeedHost = %q", defaultVLESSSpeedHost)
	}
}
