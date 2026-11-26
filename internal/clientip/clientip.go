package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ServerIPs 自有服务器公网 / tailscale 地址：既是可信代理，也是 ipguard 默认白名单。
var ServerIPs = []string{
	"80.251.212.10",   // bwh8
	"100.95.91.5",     // bwh8 tailscale
	"179.253.236.72",  // dmit01
	"179.255.115.142", // dmit02
	"207.56.22.48",    // hehk03
	"104.238.220.50",  // v99
}

// PrivateCIDRs 回环与内网段。
var PrivateCIDRs = []string{
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128",
}

var cloudflareCIDRs = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

var trusted = func() []netip.Prefix {
	all := append(append(append([]string{}, PrivateCIDRs...), cloudflareCIDRs...), ServerIPs...)
	out := make([]netip.Prefix, 0, len(all))
	for _, c := range all {
		p, err := ParsePrefix(c)
		if err != nil {
			panic(err)
		}
		out = append(out, p)
	}
	return out
}()

// ParsePrefix 解析 CIDR 或单个 IP（单 IP 视为 /32 或 /128）。
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func isTrusted(a netip.Addr) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func parse(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// FromTrustedPeer 连接对端是否为可信代理（只有此时转发头 / CF-IPCountry 才可信）。
func FromTrustedPeer(r *http.Request) bool {
	a, ok := parse(remoteHost(r))
	return ok && isTrusted(a)
}

// ClientIP 返回访客真实 IP。仅当连接对端是可信代理时才读取转发头。
func ClientIP(r *http.Request) string {
	host := remoteHost(r)
	remote, ok := parse(host)
	if !ok {
		return host
	}
	if !isTrusted(remote) {
		return remote.String()
	}

	if a, ok := parse(r.Header.Get("CF-Connecting-IP")); ok {
		return a.String()
	}

	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if a, ok := parse(parts[i]); ok && !isTrusted(a) {
			return a.String()
		}
	}

	if a, ok := parse(r.Header.Get("X-Real-IP")); ok {
		return a.String()
	}
	return remote.String()
}
