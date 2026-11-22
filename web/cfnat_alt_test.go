package main

import (
	"strings"
	"testing"
)

func TestNormalizeCFnatAltListener(t *testing.T) {
	cfg := normalizeCFnat(cfnatConfig{AltAddr: "  0.0.0.0:1235  ", AltPort: 8443})
	if cfg.AltAddr != "0.0.0.0:1235" {
		t.Fatalf("AltAddr = %q", cfg.AltAddr)
	}
	if cfg.AltPort != 8443 {
		t.Fatalf("AltPort = %d", cfg.AltPort)
	}
	args := strings.Join(cfg.args(), " ")
	if !strings.Contains(args, "-addr2 0.0.0.0:1235") {
		t.Fatalf("args missing alt addr: %s", args)
	}
	if !strings.Contains(args, "-port2 8443") {
		t.Fatalf("args missing alt port: %s", args)
	}
}

func TestNormalizeCFnatAltPortOutOfRange(t *testing.T) {
	cfg := normalizeCFnat(cfnatConfig{AltAddr: "0.0.0.0:1235", AltPort: 70000})
	if cfg.AltPort != 0 {
		t.Fatalf("out-of-range AltPort = %d, want 0", cfg.AltPort)
	}
	cfg = normalizeCFnat(cfnatConfig{AltAddr: "", AltPort: 8443})
	if cfg.AltPort != 0 {
		t.Fatalf("AltPort without AltAddr = %d, want 0", cfg.AltPort)
	}
	args := strings.Join(cfg.args(), " ")
	if strings.Contains(args, "-addr2") {
		t.Fatalf("disabled alt listener should not add args: %s", args)
	}
}
