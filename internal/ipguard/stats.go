package ipguard

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

type BlockedEntry struct {
	Time    time.Time `json:"time"`
	IP      string    `json:"ip"`
	Country string    `json:"country"`
	UA      string    `json:"ua"`
	Method  string    `json:"method"`
	Host    string    `json:"host"`
	Path    string    `json:"path"`
	// Reason: denied（403）/ blacklist（403 黑名单）/ rate_limit（503）。
	Reason string `json:"reason"`
}

type CountryCount struct {
	Country string `json:"country"`
	Count   int64  `json:"count"`
}

type StatsSnapshot struct {
	Since            time.Time        `json:"since"`
	Allowed          map[string]int64 `json:"allowed"`
	Blocked          int64            `json:"blocked"`
	BlockedCountries []CountryCount   `json:"blocked_countries"`
	Blacklisted      int64            `json:"blacklisted"`
	RateLimited      int64            `json:"rate_limited"`
}

// Stats 进程内计数与最近拦截环形缓冲；重启清零。
type Stats struct {
	mu          sync.Mutex
	since       time.Time
	allowed     map[string]int64
	blocked     int64
	blacklisted int64
	rateLimited int64
	byCountry   map[string]int64
	ring        []BlockedEntry
	next        int
	full        bool
}

func NewStats(capacity int) *Stats {
	s := &Stats{ring: make([]BlockedEntry, capacity)}
	s.Reset()
	return s
}

func (s *Stats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.since = time.Now()
	s.allowed = map[string]int64{}
	s.blocked = 0
	s.blacklisted = 0
	s.rateLimited = 0
	s.byCountry = map[string]int64{}
	s.next, s.full = 0, false
	clear(s.ring)
}

func (s *Stats) Record(d Decision, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.Allow {
		s.allowed[d.Reason]++
		return
	}
	s.blocked++
	if d.Reason == ReasonBlacklist {
		s.blacklisted++
	}
	country := d.Country
	if country == "" {
		country = "-"
	}
	s.byCountry[country]++
	s.push(d, r, d.Reason)
}

// RecordRateLimited 记录被限速的请求；该请求已由 Record 计入 allowed，这里不计 blocked。
func (s *Stats) RecordRateLimited(d Decision, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited++
	s.push(d, r, ReasonRateLimit)
}

// push 需持有 s.mu。
func (s *Stats) push(d Decision, r *http.Request, reason string) {
	ua := d.UA
	if len(ua) > 300 {
		ua = ua[:300]
	}
	path := r.URL.RequestURI()
	if len(path) > 500 {
		path = path[:500]
	}
	s.ring[s.next] = BlockedEntry{
		Time: time.Now(), IP: d.IP, Country: d.Country, UA: ua,
		Method: r.Method, Host: r.Host, Path: path, Reason: reason,
	}
	s.next = (s.next + 1) % len(s.ring)
	if s.next == 0 {
		s.full = true
	}
}

func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed := make(map[string]int64, len(s.allowed))
	for k, v := range s.allowed {
		allowed[k] = v
	}
	countries := make([]CountryCount, 0, len(s.byCountry))
	for k, v := range s.byCountry {
		countries = append(countries, CountryCount{k, v})
	}
	sort.Slice(countries, func(i, j int) bool {
		if countries[i].Count != countries[j].Count {
			return countries[i].Count > countries[j].Count
		}
		return countries[i].Country < countries[j].Country
	})
	return StatsSnapshot{
		Since: s.since, Allowed: allowed, Blocked: s.blocked,
		BlockedCountries: countries, Blacklisted: s.blacklisted, RateLimited: s.rateLimited,
	}
}

// Recent 返回最近的拦截记录，新的在前。
func (s *Stats) Recent(limit int) []BlockedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.next
	if s.full {
		n = len(s.ring)
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]BlockedEntry, 0, limit)
	for i := 1; i <= limit; i++ {
		out = append(out, s.ring[(s.next-i+len(s.ring))%len(s.ring)])
	}
	return out
}
