package main

import "testing"

func TestParseCFTrace(t *testing.T) {
	body := "fl=123f45\nh=www.example.com\nip=203.0.113.7\nts=1700000000.000\nvisit_scheme=https\nuag=curl/8.0\ncolo=NRT\nsliver=none\nhttp=http/2\nloc=JP\ntls=TLSv1.3\nsni=plaintext\nwarp=off\ngateway=off\nrbi=off\nkex=X25519\n"
	trace := parseCFTrace(body)
	if trace.IP != "203.0.113.7" {
		t.Fatalf("IP = %q", trace.IP)
	}
	if trace.Colo != "NRT" {
		t.Fatalf("Colo = %q", trace.Colo)
	}
	if trace.VisitScheme != "https" || trace.TLS != "TLSv1.3" || trace.Kex != "X25519" {
		t.Fatalf("unexpected trace: %+v", trace)
	}
}

func goodTrace() cfTrace {
	return cfTrace{
		IP:          "198.51.100.9",
		Colo:        "NRT",
		VisitScheme: "https",
		TLS:         "TLSv1.3",
		Kex:         "X25519",
		UAG:         traceProbeUserAgent,
		CFRay:       "8f3b2c1d4e5f6a7b-NRT",
		Server:      "cloudflare",
	}
}

func TestTraceL7RejectReason(t *testing.T) {
	good := goodTrace()
	if reason := traceL7RejectReason(good); reason != "" {
		t.Fatalf("good trace rejected: %s", reason)
	}
	cases := []struct {
		name  string
		trace cfTrace
		want  string
	}{
		{"plain http", func() cfTrace { tr := goodTrace(); tr.VisitScheme = "http"; return tr }(), "trace visit_scheme=http"},
		{"tls off", func() cfTrace { tr := goodTrace(); tr.TLS = "off"; return tr }(), "trace tls=off"},
		{"kex none", func() cfTrace { tr := goodTrace(); tr.Kex = "none"; return tr }(), "trace kex=none"},
		{"no colo", func() cfTrace { tr := goodTrace(); tr.Colo = ""; return tr }(), "trace 未返回 colo"},
		{"no ip", func() cfTrace { tr := goodTrace(); tr.IP = ""; return tr }(), "trace 未返回 ip"},
		{"uag mismatch", func() cfTrace { tr := goodTrace(); tr.UAG = "curl/8.0"; return tr }(), "trace uag 不匹配"},
		{"no cf-ray", func() cfTrace { tr := goodTrace(); tr.CFRay = ""; return tr }(), "trace 缺少 CF-RAY"},
		{"wrong server", func() cfTrace { tr := goodTrace(); tr.Server = "nginx"; return tr }(), "trace server 不是 cloudflare"},
	}
	for _, tc := range cases {
		if got := traceL7RejectReason(tc.trace); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTraceSummaryKeepsClientIPOutOfAdmission(t *testing.T) {
	trace := goodTrace()
	if reason := traceL7RejectReason(trace); reason != "" {
		t.Fatalf("client IP must not affect admission, got %q", reason)
	}
	if summary := trace.summary(); summary == "" {
		t.Fatal("summary should not be empty")
	}
}
