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

func TestTraceL7RejectReason(t *testing.T) {
	good := cfTrace{Colo: "NRT", VisitScheme: "https", TLS: "TLSv1.3", Kex: "X25519"}
	if reason := traceL7RejectReason(good); reason != "" {
		t.Fatalf("good trace rejected: %s", reason)
	}
	cases := []struct {
		name  string
		trace cfTrace
		want  string
	}{
		{"plain http", cfTrace{Colo: "NRT", VisitScheme: "http", TLS: "TLSv1.3", Kex: "X25519"}, "trace visit_scheme=http"},
		{"tls off", cfTrace{Colo: "NRT", VisitScheme: "https", TLS: "off", Kex: "X25519"}, "trace tls=off"},
		{"kex none", cfTrace{Colo: "NRT", VisitScheme: "https", TLS: "TLSv1.3", Kex: "none"}, "trace kex=none"},
		{"no colo", cfTrace{VisitScheme: "https", TLS: "TLSv1.3", Kex: "X25519"}, "trace 未返回 colo"},
	}
	for _, tc := range cases {
		if got := traceL7RejectReason(tc.trace); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTraceSummaryKeepsClientIPOutOfAdmission(t *testing.T) {
	trace := cfTrace{IP: "198.51.100.9", Colo: "HKG", VisitScheme: "https", TLS: "TLSv1.3", Kex: "X25519"}
	if reason := traceL7RejectReason(trace); reason != "" {
		t.Fatalf("client IP must not affect admission, got %q", reason)
	}
	if summary := trace.summary(); summary == "" {
		t.Fatal("summary should not be empty")
	}
}
