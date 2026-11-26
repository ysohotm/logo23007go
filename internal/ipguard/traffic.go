package ipguard

import (
	"net/netip"
	"sort"
	"sync"
	"time"
)

// 访问结果：放行，或拒绝原因 ReasonDenied / ReasonBlacklist / ReasonRateLimit。
const OutcomeAllowed = "allowed"

const DefaultRecentCapacity = 10000

const minuteBuckets = 60

type MinuteCount struct {
	Minute      time.Time `json:"minute"` // 该分钟起点（本地时区，Truncate(time.Minute)）
	Total       int64     `json:"total"`
	Allowed     int64     `json:"allowed"`
	Denied      int64     `json:"denied"`       // 403，含黑名单
	Blacklisted int64     `json:"blacklisted"`  // 403 中因黑名单
	RateLimited int64     `json:"rate_limited"` // 503
}

type SubnetGroup struct {
	Subnet      string    `json:"subnet"` // IPv4 为 a.b.c.0/24，IPv6 为 /48
	Count       int       `json:"count"`
	Blocked     int       `json:"blocked"` // outcome != allowed 的条数
	IPs         int       `json:"ips"`     // 不同 IP 数
	UAs         int       `json:"uas"`     // 不同 UA 字符串数（空 UA 也算一种）
	Country     string    `json:"country"` // 出现最多的非空国家码，没有则 ""
	TopUA       string    `json:"top_ua"`  // 出现最多的 UA
	LastSeen    time.Time `json:"last_seen"`
	Blacklisted bool      `json:"blacklisted"` // 由 Guard 填充，Traffic 不设置
}

type RecentAccess struct {
	Total    int           `json:"total"` // 缓冲中的记录数
	Capacity int           `json:"capacity"`
	Oldest   time.Time     `json:"oldest"`  // 最早一条的时间，无记录为零值
	Groups   []SubnetGroup `json:"groups"`  // 非 nil
	Subnets  int           `json:"subnets"` // 截断前的网段总数
}

type minuteBucket struct {
	minute      int64 // unix 秒 / 60
	total       int64
	allowed     int64
	denied      int64
	blacklisted int64
	rateLimited int64
}

type accessEntry struct {
	time    time.Time
	ip      string
	country string
	ua      string
	outcome string
}

// Traffic 进程内按分钟计数 + 最近访问环形缓冲；Record 只写字段，聚合在查询时做。
type Traffic struct {
	mu      sync.Mutex
	buckets [minuteBuckets]minuteBucket
	ring    []accessEntry
	next    int
	full    bool
}

func NewTraffic(capacity int) *Traffic {
	if capacity <= 0 {
		capacity = DefaultRecentCapacity
	}
	return &Traffic{ring: make([]accessEntry, capacity)}
}

func (t *Traffic) Record(now time.Time, ip, country, ua, outcome string) {
	if len(ua) > 300 {
		ua = ua[:300]
	}
	m := now.Unix() / 60
	t.mu.Lock()
	defer t.mu.Unlock()
	b := &t.buckets[m%minuteBuckets]
	if b.minute != m {
		*b = minuteBucket{minute: m}
	}
	b.total++
	switch outcome {
	case OutcomeAllowed:
		b.allowed++
	case ReasonDenied:
		b.denied++
	case ReasonBlacklist:
		b.denied++
		b.blacklisted++
	case ReasonRateLimit:
		b.rateLimited++
	}
	t.ring[t.next] = accessEntry{time: now, ip: ip, country: country, ua: ua, outcome: outcome}
	t.next = (t.next + 1) % len(t.ring)
	if t.next == 0 {
		t.full = true
	}
}

// Minutes 返回最近 n 分钟（旧→新），最后一个为 now 所在的未结束分钟。
func (t *Traffic) Minutes(now time.Time, n int) []MinuteCount {
	n = max(1, min(n, minuteBuckets))
	cur := now.Unix() / 60
	out := make([]MinuteCount, n)
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range out {
		m := cur - int64(n-1-i)
		c := MinuteCount{Minute: time.Unix(m*60, 0)}
		if b := &t.buckets[m%minuteBuckets]; b.minute == m {
			c.Total, c.Allowed, c.Denied = b.total, b.allowed, b.denied
			c.Blacklisted, c.RateLimited = b.blacklisted, b.rateLimited
		}
		out[i] = c
	}
	return out
}

type subnetAgg struct {
	g         SubnetGroup
	ips       map[string]struct{}
	uas       map[string]int
	countries map[string]int
}

// Recent 按网段聚合缓冲中的访问，Count 降序、Subnet 升序；limit<=0 返回全部。
func (t *Traffic) Recent(limit int) RecentAccess {
	t.mu.Lock()
	n := t.next
	if t.full {
		n = len(t.ring)
	}
	entries := make([]accessEntry, n)
	if t.full {
		copy(entries, t.ring[t.next:])
		copy(entries[len(t.ring)-t.next:], t.ring[:t.next])
	} else {
		copy(entries, t.ring[:n])
	}
	capacity := len(t.ring)
	t.mu.Unlock()

	ra := RecentAccess{Total: n, Capacity: capacity, Groups: []SubnetGroup{}}
	if n > 0 {
		ra.Oldest = entries[0].time
	}
	aggs := map[string]*subnetAgg{}
	for i := range entries {
		e := &entries[i]
		sn := SubnetOf(e.ip)
		if sn == "" {
			continue
		}
		a := aggs[sn]
		if a == nil {
			a = &subnetAgg{
				g:         SubnetGroup{Subnet: sn},
				ips:       map[string]struct{}{},
				uas:       map[string]int{},
				countries: map[string]int{},
			}
			aggs[sn] = a
		}
		a.g.Count++
		if e.outcome != OutcomeAllowed {
			a.g.Blocked++
		}
		a.ips[e.ip] = struct{}{}
		a.uas[e.ua]++
		if e.country != "" {
			a.countries[e.country]++
		}
		if e.time.After(a.g.LastSeen) {
			a.g.LastSeen = e.time
		}
	}

	groups := make([]SubnetGroup, 0, len(aggs))
	for _, a := range aggs {
		a.g.IPs = len(a.ips)
		a.g.UAs = len(a.uas)
		a.g.TopUA = topKey(a.uas)
		a.g.Country = topKey(a.countries)
		groups = append(groups, a.g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].Subnet < groups[j].Subnet
	})
	ra.Subnets = len(groups)
	if limit > 0 && limit < len(groups) {
		groups = groups[:limit]
	}
	ra.Groups = groups
	return ra
}

// topKey 返回计数最多的 key，并列取字典序最小；空 map 返回 ""。
func topKey(m map[string]int) string {
	best, bestN := "", 0
	for k, v := range m {
		if v > bestN || (v == bestN && k < best) {
			best, bestN = k, v
		}
	}
	return best
}

func (t *Traffic) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buckets = [minuteBuckets]minuteBucket{}
	clear(t.ring)
	t.next, t.full = 0, false
}

// SubnetOf 返回 IP 所在网段（IPv4 /24，IPv6 /48）；解析失败返回 ""。
func SubnetOf(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return p.String()
}
