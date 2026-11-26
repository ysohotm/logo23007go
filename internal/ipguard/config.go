package ipguard

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"logo23007go/internal/clientip"
)

// Config 是后台可编辑、存于 Mongo 的规则。
type Config struct {
	Enabled        bool     `json:"enabled" bson:"enabled"`
	Countries      []string `json:"countries" bson:"countries"`
	AllowNoCountry bool     `json:"allow_no_country" bson:"allow_no_country"`
	IPs            []string `json:"ips" bson:"ips"`
	// BlockIPs 是 IP/CIDR 黑名单，命中返回 403；IP 白名单优先。旧文档无该字段解码为 nil，即无黑名单。
	BlockIPs []string `json:"block_ips" bson:"block_ips"`
	UAs      []string `json:"uas" bson:"uas"`
	// 旧文档无 rate_limit 字段时解码为零值，即关闭限速。
	RateLimit RateLimitConfig `json:"rate_limit" bson:"rate_limit"`
	UpdatedAt time.Time       `json:"updated_at" bson:"updated_at"`
}

// 限速范围。
const (
	ScopeIP     = "ip"
	ScopeGlobal = "global"
)

const (
	defaultRateWindowSec = 10
	defaultRateMax       = 100
	maxRateWindowSec     = 3600
	maxRateMax           = 1000000
)

// RateLimitConfig 是滑动窗口限速规则，超限返回 503。
type RateLimitConfig struct {
	Enabled bool `json:"enabled" bson:"enabled"`
	// Scope: ip 按真实客户端 IP 计数；global 全站共用一个计数。
	Scope     string `json:"scope" bson:"scope"`
	WindowSec int    `json:"window_sec" bson:"window_sec"`
	Max       int    `json:"max" bson:"max"`
	// ExemptIPWhitelist 为 true 时，因 IP 白名单放行（Reason=ip）的请求不限速。
	ExemptIPWhitelist bool `json:"exempt_ip_whitelist" bson:"exempt_ip_whitelist"`
}

func DefaultRateLimit() RateLimitConfig {
	return RateLimitConfig{
		Enabled:           false,
		Scope:             ScopeIP,
		WindowSec:         defaultRateWindowSec,
		Max:               defaultRateMax,
		ExemptIPWhitelist: true,
	}
}

// withDefaults 填充零值，使旧文档（无 rate_limit）也能通过校验。
func (rl RateLimitConfig) withDefaults() RateLimitConfig {
	rl.Scope = strings.ToLower(strings.TrimSpace(rl.Scope))
	if rl.Scope == "" {
		rl.Scope = ScopeIP
	}
	if rl.WindowSec == 0 {
		rl.WindowSec = defaultRateWindowSec
	}
	if rl.Max == 0 {
		rl.Max = defaultRateMax
	}
	return rl
}

func DefaultConfig() Config {
	ips := append(append([]string{}, clientip.ServerIPs...), clientip.PrivateCIDRs...)
	return Config{
		Enabled:        true,
		Countries:      []string{"HK", "CN", "SG"},
		AllowNoCountry: true,
		IPs:            ips,
		BlockIPs:       []string{},
		UAs: []string{
			"Googlebot", "Baiduspider", "bingbot", "Sogou", "360Spider", "YisouSpider", "Bytespider",
		},
		RateLimit: DefaultRateLimit(),
	}
}

// Normalize 去空白、去重、国家转大写，并校验国家码与 IP/CIDR。
func (c Config) Normalize() (Config, error) {
	var bad []string
	out := c
	out.Countries = dedupe(c.Countries, func(s string) (string, bool) {
		s = strings.ToUpper(s)
		if len(s) != 2 {
			bad = append(bad, "国家码:"+s)
			return "", false
		}
		return s, true
	})
	out.IPs = dedupe(c.IPs, func(s string) (string, bool) {
		if _, err := clientip.ParsePrefix(s); err != nil {
			bad = append(bad, "IP:"+s)
			return "", false
		}
		return s, true
	})
	out.BlockIPs = dedupe(c.BlockIPs, func(s string) (string, bool) {
		if _, err := clientip.ParsePrefix(s); err != nil {
			bad = append(bad, "黑名单:"+s)
			return "", false
		}
		return s, true
	})
	out.UAs = dedupe(c.UAs, func(s string) (string, bool) { return s, true })
	rl := c.RateLimit.withDefaults()
	if rl.Scope != ScopeIP && rl.Scope != ScopeGlobal {
		bad = append(bad, "限速范围:"+rl.Scope)
	}
	if rl.WindowSec < 1 || rl.WindowSec > maxRateWindowSec {
		bad = append(bad, fmt.Sprintf("限速窗口:%d", rl.WindowSec))
	}
	if rl.Max < 1 || rl.Max > maxRateMax {
		bad = append(bad, fmt.Sprintf("限速条数:%d", rl.Max))
	}
	out.RateLimit = rl
	if len(bad) > 0 {
		return c, fmt.Errorf("无效条目: %s", strings.Join(bad, ", "))
	}
	return out, nil
}

func dedupe(in []string, fn func(string) (string, bool)) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		v, ok := fn(s)
		if !ok {
			continue
		}
		key := strings.ToLower(v)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	return out
}

// compiled 是 Config 的运行时查询形式。
type compiled struct {
	cfg       Config
	countries map[string]struct{}
	allow     prefixSet
	block     prefixSet
	uas       []string

	rateWindowSec int
	rateMax       int
	rateGlobal    bool
}

func compile(c Config) *compiled {
	cp := &compiled{cfg: c, countries: make(map[string]struct{}, len(c.Countries))}
	// 未经 Normalize 的配置（如旧文档、Apply 直传）也要保证窗口/条数为正。
	rl := c.RateLimit.withDefaults()
	cp.rateWindowSec = min(rl.WindowSec, maxRateWindowSec)
	if cp.rateWindowSec < 1 {
		cp.rateWindowSec = defaultRateWindowSec
	}
	cp.rateMax = rl.Max
	if cp.rateMax < 1 {
		cp.rateMax = defaultRateMax
	}
	cp.rateGlobal = rl.Scope == ScopeGlobal
	for _, s := range c.Countries {
		cp.countries[strings.ToUpper(strings.TrimSpace(s))] = struct{}{}
	}
	cp.allow = newPrefixSet(c.IPs)
	cp.block = newPrefixSet(c.BlockIPs)
	for _, s := range c.UAs {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			cp.uas = append(cp.uas, s)
		}
	}
	return cp
}

// prefixSet 按前缀长度分组做哈希查找，查询为 O(不同前缀长度数)，与条目数无关。
type prefixSet struct {
	lens []int // 去重，降序
	set  map[netip.Prefix]struct{}
}

func newPrefixSet(ss []string) prefixSet {
	ps := prefixSet{set: make(map[netip.Prefix]struct{}, len(ss))}
	seen := make(map[int]struct{})
	for _, s := range ss {
		p, err := clientip.ParsePrefix(s)
		if err != nil {
			continue
		}
		ps.set[p.Masked()] = struct{}{}
		if _, ok := seen[p.Bits()]; !ok {
			seen[p.Bits()] = struct{}{}
			ps.lens = append(ps.lens, p.Bits())
		}
	}
	slices.SortFunc(ps.lens, func(a, b int) int { return b - a })
	return ps
}

// contains 的 key 由 a.Prefix 生成，天然与 a 同族，IPv4/IPv6 同长度前缀不会串。
func (ps prefixSet) contains(a netip.Addr) bool {
	a = a.Unmap()
	for _, n := range ps.lens {
		if n > a.BitLen() {
			continue
		}
		p, err := a.Prefix(n)
		if err != nil {
			continue
		}
		if _, ok := ps.set[p]; ok {
			return true
		}
	}
	return false
}

// parseIPAddr 拒绝带 zone 的地址，与 netip.Prefix.Contains 语义一致。
func parseIPAddr(ip string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a, true
}

func (cp *compiled) ipAllowed(ip string) bool {
	a, ok := parseIPAddr(ip)
	return ok && cp.allow.contains(a)
}

func (cp *compiled) ipBlocked(ip string) bool {
	a, ok := parseIPAddr(ip)
	return ok && cp.block.contains(a)
}

func (cp *compiled) uaAllowed(ua string) bool {
	if ua == "" {
		return false
	}
	ua = strings.ToLower(ua)
	for _, k := range cp.uas {
		if strings.Contains(ua, k) {
			return true
		}
	}
	return false
}
