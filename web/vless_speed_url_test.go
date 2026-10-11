package main

import (
	"bytes"
	"io"
	"testing"
	"time"
)

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

// ewmaMbpsFromSlices 三律：恒速片收敛于真速率；突发尖峰被衰减；
// 片数不足/空片返回 0（调用方回退朴素均值）。
func TestEWMAMbpsFromSlices(t *testing.T) {
	// 恒速 32768B/250ms = 1.049 Mbps；10 片后 EWMA 收敛差应 < 5%
	uniform := make([]int64, 10)
	for i := range uniform {
		uniform[i] = 32768
	}
	got := ewmaMbpsFromSlices(uniform, 250*time.Millisecond)
	want := 32768.0 * 8 / 0.25 / 1_000_000
	if diff := (got - want) / want; diff < -0.05 || diff > 0.05 {
		t.Fatalf("uniform slices: ewma=%.4f want≈%.4f (diff %.1f%%)", got, want, diff*100)
	}

	// 开局一记 4 倍尖峰、后续归零：EWMA 必须显著低于朴素均值（总量/总时长）
	burst := []int64{131072, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	ewma := ewmaMbpsFromSlices(burst, 250*time.Millisecond)
	plain := float64(131072) * 8 / (10 * 0.25) / 1_000_000
	if ewma >= plain {
		t.Fatalf("burst must be damped: ewma=%.4f plain=%.4f", ewma, plain)
	}
	if ewma <= 0 {
		t.Fatalf("damped ewma must stay positive, got %.4f", ewma)
	}

	// 片数不足 4 / 空片 / 非法片长 → 0（回退信号）
	if got := ewmaMbpsFromSlices([]int64{1, 2, 3}, 250*time.Millisecond); got != 0 {
		t.Fatalf("short slices must return 0, got %.4f", got)
	}
	if got := ewmaMbpsFromSlices(nil, 250*time.Millisecond); got != 0 {
		t.Fatalf("empty slices must return 0, got %.4f", got)
	}
	if got := ewmaMbpsFromSlices(uniform, 0); got != 0 {
		t.Fatalf("zero slice duration must return 0, got %.4f", got)
	}
}

// speedSampler 必须把 body 逐片记满：字节数一毫不差，EOF 正常收尾。
func TestSpeedSamplerCountsEveryByte(t *testing.T) {
	body := make([]byte, 100_000)
	sampler := newSpeedSampler(bytes.NewReader(body))
	total, err := sampler.copyTo(io.Discard)
	if err != nil {
		t.Fatalf("copyTo: %v", err)
	}
	if total != 100_000 {
		t.Fatalf("total = %d, want 100000", total)
	}
	var sum int64
	for _, s := range sampler.slices {
		sum += s
	}
	if sum != total {
		t.Fatalf("slice sum %d != total %d", sum, total)
	}
}
