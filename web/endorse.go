package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// 用户扶持登基：与「考生供给」分明的一条路。
//
// 考生供给（proxy-scan/apply）把粘贴内容当考生，逐条走 WS 体检，择优入池；
// 扶持登基信用户所给必为可用——只校验公网性/域名合法性，随即并入生效池并重启
// 转发，不做任何探测。它是手动动作中的最高优先：与手动供给同级，不受冷却与
// 健康闸限制；已在位的成员幂等跳过，绝不重复重启 cfnat。

// maxPoolMembers 是生效池的硬顶（IP + 域名合计）：cfnat -fixed 是命令行参数，
// 无上限会让一次误粘贴把拨号目标撑到不可控。超限在解析阶段就拒绝。
const maxPoolMembers = 48

type endorsedMembers struct {
	IPs     []string
	Domains []string
}

// parseEndorsedMembers 把用户原文解析为登基成员：逐行拆解，# 之后为注释，
// 「host:port」只取主机部分，公网 IPv4 入 IP 轨、合法域名（小写化后）入域名轨。
// 任何一条不合法就整份拒绝并指名道姓——吞掉半个坏输入会让用户误以为全部生效。
func parseEndorsedMembers(text string) (endorsedMembers, error) {
	var out endorsedMembers
	seen := make(map[string]bool)
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	for _, line := range lines {
		entry := strings.TrimSpace(line)
		if entry == "" {
			continue
		}
		if at := strings.IndexAny(entry, "#＃"); at >= 0 {
			entry = strings.TrimSpace(entry[:at])
		}
		if entry == "" {
			continue
		}
		if host, _, err := net.SplitHostPort(entry); err == nil && host != "" {
			entry = strings.TrimSpace(host)
		}
		if ip := net.ParseIP(entry); ip != nil {
			if !isPublicIPv4(ip) {
				return out, fmt.Errorf("%q 不是公网 IPv4，拒绝登基", entry)
			}
			value := ip.To4().String()
			if seen[value] {
				continue
			}
			seen[value] = true
			out.IPs = append(out.IPs, value)
			continue
		}
		host := strings.ToLower(strings.TrimSuffix(entry, "."))
		if !validCandidateHostname(host) {
			return out, fmt.Errorf("%q 既不是公网 IPv4 也不是合法域名，拒绝登基", entry)
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		out.Domains = append(out.Domains, host)
	}
	if len(out.IPs)+len(out.Domains) == 0 {
		return out, errors.New("没有解析出任何可登基的 IP 或域名")
	}
	if len(out.IPs)+len(out.Domains) > maxPoolMembers {
		return out, fmt.Errorf("单次最多扶持 %d 个成员，当前解析出 %d 个", maxPoolMembers, len(out.IPs)+len(out.Domains))
	}
	return out, nil
}

// handleActiveEndorse POST /api/cfnat/active/endorse
// 免体检扶持登基：并入生效池 → 重启转发 → 立即生效。已在位者跳过；
// 池空时这就是唯一的起势入口（考生供给要体检通过才肯给座位）。
func (a *app) handleActiveEndorse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	// 锁序全局规范 refresh → proxyScan → cfnatCtl；只拿换池锁，等待而非拒绝。
	if !lockTimeout(&a.cfnatCtlMu, 15*time.Second) {
		http.Error(w, "换池操作正在运行，请稍后再试", http.StatusTooManyRequests)
		return
	}
	defer a.cfnatCtlMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Members string `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "无效的请求体", http.StatusBadRequest)
		return
	}
	parsed, err := parseEndorsedMembers(body.Members)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	current := a.proxyActivePoolSnapshot()
	addedIPs := make([]string, 0, len(parsed.IPs))
	addedDomains := make([]string, 0, len(parsed.Domains))
	skipped := 0
	for _, ip := range parsed.IPs {
		if containsString(current.IPs, ip) || containsString(addedIPs, ip) {
			skipped++
			continue
		}
		addedIPs = append(addedIPs, ip)
	}
	for _, domain := range parsed.Domains {
		if containsString(current.Domains, domain) || containsString(addedDomains, domain) {
			skipped++
			continue
		}
		addedDomains = append(addedDomains, domain)
	}
	if len(current.IPs)+len(current.Domains)+len(addedIPs)+len(addedDomains) > maxPoolMembers {
		http.Error(w, fmt.Sprintf("生效池上限 %d 席（现 %d 席），请先踢出旧成员再扶持", maxPoolMembers, len(current.IPs)+len(current.Domains)), http.StatusBadRequest)
		return
	}
	// 扶持即置顶：已在位但尚未置顶的成员，同样要提到 -fixed 最前。
	wanted := append(append([]string(nil), parsed.IPs...), parsed.Domains...)
	priority := append([]string(nil), current.Priority...)
	promoted := 0
	for _, member := range wanted {
		if !containsString(priority, member) {
			priority = append(priority, member)
			promoted++
		}
	}
	disabled := make([]string, 0, len(current.Disabled))
	for _, member := range current.Disabled {
		if !containsString(wanted, member) {
			disabled = append(disabled, member)
		}
	}
	if len(addedIPs) == 0 && len(addedDomains) == 0 && promoted == 0 && len(disabled) == len(current.Disabled) {
		// 幂等：全部已在位且已置顶，不重启转发。
		writeJSON(w, map[string]any{
			"ok": true, "ips": []string{}, "domains": []string{}, "skipped": skipped,
			"ips_total": len(current.IPs), "domains_total": len(current.Domains), "unchanged": true,
		})
		return
	}
	now := time.Now()
	pool := current
	pool.UpdatedAt = now
	pool.IPs = append(append([]string(nil), current.IPs...), addedIPs...)
	pool.Domains = append(append([]string(nil), current.Domains...), addedDomains...)
	pool.Priority = priority
	pool.Disabled = disabled
	// 扶持成员没有体检成绩，成绩单不造假：无记录即「在位，体检随时点」。
	if err := a.applyPoolLocked(pool, current); err != nil {
		http.Error(w, "扶持登基换池失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.markPoolSwitched(now)
	log.Printf("active pool endorsed (no exam): +%d IPs +%d domains, pool now %d IPs +%d domains",
		len(addedIPs), len(addedDomains), len(pool.IPs), len(pool.Domains))
	writeJSON(w, map[string]any{
		"ok": true, "ips": addedIPs, "domains": addedDomains, "skipped": skipped,
		"ips_total": len(pool.IPs), "domains_total": len(pool.Domains),
	})
}
