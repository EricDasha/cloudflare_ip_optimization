package main

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
)

func TestExtractSubscriptionHostnames(t *testing.T) {
	vmessJSON, err := json.Marshal(map[string]string{"add": "vmess.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	vmess := base64.StdEncoding.EncodeToString(vmessJSON)
	ss := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret@ss.example.com:443"))
	body := "<a href=\"https://ordinary.example.com\">ignore</a>\n" +
		"vless://id@vless.example.com:443?security=tls\n" +
		"trojan://secret@trojan.example.com:443\n" +
		"vmess://" + vmess + "\n" +
		"ss://" + ss + "\n" +
		"server: clash.example.com\n" +
		"server: 192.168.1.1\n"

	want := []string{
		"vless.example.com", "trojan.example.com", "vmess.example.com", "ss.example.com", "clash.example.com",
	}
	if got := extractSubscriptionHostnames(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("extractSubscriptionHostnames() = %v, want %v", got, want)
	}
}

func TestExtractSubscriptionHostnamesFromBase64Document(t *testing.T) {
	document := base64.StdEncoding.EncodeToString([]byte("vless://id@encoded.example.com:443\n"))
	if got := extractSubscriptionHostnames(document); !reflect.DeepEqual(got, []string{"encoded.example.com"}) {
		t.Fatalf("extractSubscriptionHostnames() = %v", got)
	}
}

func TestValidCandidateHostname(t *testing.T) {
	for _, test := range []struct {
		host string
		want bool
	}{
		{host: "node.example.com", want: true},
		{host: "127.0.0.1", want: false},
		{host: "localhost", want: false},
		{host: "bad_label.example.com", want: false},
		{host: "-bad.example.com", want: false},
	} {
		if got := validCandidateHostname(test.host); got != test.want {
			t.Fatalf("validCandidateHostname(%q) = %v, want %v", test.host, got, test.want)
		}
	}
}
