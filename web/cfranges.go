package main

import "net"

// cloudflareIPv4CIDRs 是 Cloudflare 官方公布的 IPv4 边缘段（15 段）。
//
// 用途：候选池准入闸——入口 IP 必须是 Cloudflare 的 anycast 边缘，否则
// cfnat -fixed 拨号时连上的是别人的 VPS。历史上 bestcf 生态里大量「优选 IP」
// 实为第三方反代/ProxyIP（阿里云、DigitalOcean、OVH、M247 之类），混进入口
// 候选后 TCP:443 必然能通、VLESS 终审必然全灭，白白烧掉探针预算与时间窗。
// 此闸在源头掐断这类污染，让每一分探针预算都花在真正的边缘地址上。
//
// 注意：ProxyIP 有它自己的用途（Worker 出站中转），但那是另一条链路，
// 不属于此处；本闸只判断「能不能作为 -fixed 入口」。
var cloudflareIPv4CIDRs = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
}

var cloudflareIPv4Nets = mustParseCloudflareCIDRs(cloudflareIPv4CIDRs)

func mustParseCloudflareCIDRs(values []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic("invalid Cloudflare IPv4 CIDR: " + value)
		}
		nets = append(nets, network)
	}
	return nets
}

// isCloudflareIPv4 报告 ip 是否落在 Cloudflare 官方 IPv4 边缘段内。
// 非 IPv4（含 nil、IPv6）一律 false：入口池只收 IPv4。
func isCloudflareIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	for _, network := range cloudflareIPv4Nets {
		if network.Contains(v4) {
			return true
		}
	}
	return false
}
