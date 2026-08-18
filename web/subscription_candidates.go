package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxSubscriptionHostnames = 64

var (
	nodeURIPattern = regexp.MustCompile(`(?i)\b(?:vless|trojan|hysteria2|hy2)://[^\s"'<>\\]+`)
	vmessPattern   = regexp.MustCompile(`(?i)\bvmess://([A-Za-z0-9+/_=-]+)`)
	ssPattern      = regexp.MustCompile(`(?i)\bss://([^\s"'<>\\]+)`)
	serverPattern  = regexp.MustCompile(`(?im)^\s*(?:server\s*:|"server"\s*:)\s*["']?([A-Za-z0-9.-]+)`)
)

func extractSubscriptionHostnames(text string) []string {
	seen := make(map[string]struct{})
	hosts := make([]string, 0)
	add := func(raw string) {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if !validCandidateHostname(host) || len(hosts) >= maxSubscriptionHostnames {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}

	parse := func(payload string) {
		payload = html.UnescapeString(payload)
		for _, raw := range nodeURIPattern.FindAllString(payload, -1) {
			if parsed, err := url.Parse(raw); err == nil {
				add(parsed.Hostname())
			}
		}
		for _, match := range vmessPattern.FindAllStringSubmatch(payload, -1) {
			if decoded, ok := decodeSubscriptionBase64(match[1]); ok {
				var node struct {
					Address string `json:"add"`
				}
				if json.Unmarshal(decoded, &node) == nil {
					add(node.Address)
				}
			}
		}
		for _, match := range ssPattern.FindAllStringSubmatch(payload, -1) {
			value := strings.SplitN(match[1], "#", 2)[0]
			value = strings.SplitN(value, "?", 2)[0]
			if decoded, ok := decodeSubscriptionBase64(value); ok {
				value = string(decoded)
			}
			if at := strings.LastIndex(value, "@"); at >= 0 {
				value = value[at+1:]
			}
			if host, _, err := net.SplitHostPort(value); err == nil {
				add(host)
			}
		}
		for _, match := range serverPattern.FindAllStringSubmatch(payload, -1) {
			add(match[1])
		}
	}

	parse(text)
	if decoded, ok := decodeSubscriptionBase64(strings.Join(strings.Fields(text), "")); ok {
		parse(string(decoded))
	}
	return hosts
}

func decodeSubscriptionBase64(value string) ([]byte, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 8 {
		return nil, false
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, true
		}
	}
	return nil, false
}

func validCandidateHostname(host string) bool {
	if len(host) < 4 || len(host) > 253 || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}

func resolveSubscriptionHostnames(parent context.Context, hosts []string, remaining int) []string {
	if remaining <= 0 || len(hosts) == 0 {
		return nil
	}
	if len(hosts) > maxSubscriptionHostnames {
		hosts = hosts[:maxSubscriptionHostnames]
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()

	type answer struct {
		index int
		ips   []string
	}
	answers := make(chan answer, len(hosts))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for index, host := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return
			}
			ips := make([]string, 0, len(addresses))
			for _, address := range addresses {
				if isPublicIPv4(address.IP) {
					ips = append(ips, address.IP.To4().String())
				}
			}
			answers <- answer{index: index, ips: ips}
		}()
	}
	go func() {
		wg.Wait()
		close(answers)
	}()

	byIndex := make([][]string, len(hosts))
	for item := range answers {
		byIndex[item.index] = item.ips
	}
	seen := make(map[string]struct{})
	result := make([]string, 0, remaining)
	for _, ips := range byIndex {
		for _, ip := range ips {
			if _, ok := seen[ip]; ok {
				continue
			}
			seen[ip] = struct{}{}
			result = append(result, ip)
			if len(result) >= remaining {
				return result
			}
		}
	}
	return result
}
