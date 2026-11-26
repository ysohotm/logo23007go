package ipguard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memStore struct {
	cfg   Config
	saved bool
}

func (m *memStore) Load(context.Context) (Config, bool, error) { return m.cfg, m.saved, nil }
func (m *memStore) Save(_ context.Context, c Config) error {
	m.cfg, m.saved = c, true
	return nil
}

func req(remote, ua string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/vod/test", nil)
	r.RemoteAddr = remote
	r.Header.Set("User-Agent", ua)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

const cfPeer = "172.68.1.1:443"

func TestDecide(t *testing.T) {
	g := New(nil, nil)
	cases := []struct {
		name   string
		r      *http.Request
		allow  bool
		reason string
	}{
		{"CN via cloudflare", req(cfPeer, "Mozilla", map[string]string{"CF-Connecting-IP": "1.2.3.4", "CF-IPCountry": "CN"}), true, ReasonCountry},
		{"hk lowercase", req(cfPeer, "Mozilla", map[string]string{"CF-Connecting-IP": "1.2.3.4", "CF-IPCountry": "hk"}), true, ReasonCountry},
		{"US via cloudflare denied", req(cfPeer, "Mozilla", map[string]string{"CF-Connecting-IP": "1.2.3.4", "CF-IPCountry": "US"}), false, ReasonDenied},
		{"spoofed country from untrusted ignored -> no country allowed", req("8.8.8.8:1", "Mozilla", map[string]string{"CF-IPCountry": "US"}), true, ReasonNoCountry},
		{"US but Googlebot UA", req(cfPeer, "Mozilla/5.0 (compatible; Googlebot/2.1)", map[string]string{"CF-Connecting-IP": "1.2.3.4", "CF-IPCountry": "US"}), true, ReasonUA},
		{"v99 direct", req("104.238.220.50:1", "curl", nil), true, ReasonIP},
		{"US client forwarded by v99", req("104.238.220.50:1", "curl", map[string]string{"X-Forwarded-For": "1.2.3.4", "CF-IPCountry": "US"}), false, ReasonDenied},
		{"loopback healthcheck", req("127.0.0.1:1", "curl", nil), true, ReasonIP},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := g.Decide(c.r)
			if d.Allow != c.allow || d.Reason != c.reason {
				t.Fatalf("got allow=%v reason=%s (ip=%s country=%s), want allow=%v reason=%s", d.Allow, d.Reason, d.IP, d.Country, c.allow, c.reason)
			}
		})
	}
}

func TestDecideNoCountryDenyAndDisabled(t *testing.T) {
	g := New(nil, nil)
	c := DefaultConfig()
	c.AllowNoCountry = false
	g.Apply(c)
	if d := g.Decide(req("8.8.8.8:1", "Mozilla", nil)); d.Allow {
		t.Fatalf("expected deny without country, got %+v", d)
	}
	c.Enabled = false
	g.Apply(c)
	if d := g.Decide(req("8.8.8.8:1", "Mozilla", nil)); !d.Allow || d.Reason != ReasonDisabled {
		t.Fatalf("expected disabled allow, got %+v", d)
	}
}

func TestDecideCIDRWhitelist(t *testing.T) {
	g := New(nil, nil)
	c := DefaultConfig()
	c.IPs = []string{"5.6.0.0/16"}
	g.Apply(c)
	r := req(cfPeer, "Mozilla", map[string]string{"CF-Connecting-IP": "5.6.7.8", "CF-IPCountry": "US"})
	if d := g.Decide(r); !d.Allow || d.Reason != ReasonIP {
		t.Fatalf("expected CIDR allow, got %+v", d)
	}
}

func TestMiddleware(t *testing.T) {
	g := New(nil, nil)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := g.Middleware(ok, func(p string) bool { return strings.HasPrefix(p, "/ytmadmin") })

	us := map[string]string{"CF-Connecting-IP": "1.2.3.4", "CF-IPCountry": "US"}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(cfPeer, "Mozilla", us))
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}

	r := req(cfPeer, "Mozilla", us)
	r.URL.Path = "/ytmadmin/"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("exempt path want 200, got %d", w.Code)
	}

	snap := g.Stats.Snapshot()
	if snap.Blocked != 1 || len(snap.BlockedCountries) != 1 || snap.BlockedCountries[0].Country != "US" {
		t.Fatalf("unexpected stats %+v", snap)
	}
	if rec := g.Stats.Recent(10); len(rec) != 1 || rec[0].IP != "1.2.3.4" {
		t.Fatalf("unexpected recent %+v", rec)
	}
}

func TestNormalize(t *testing.T) {
	c, err := Config{
		Countries: []string{" cn ", "CN", "hk", ""},
		IPs:       []string{"1.2.3.4", "1.2.3.4", "10.0.0.0/8"},
		UAs:       []string{"Googlebot", "googlebot", " "},
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Countries, ",") != "CN,HK" || len(c.IPs) != 2 || len(c.UAs) != 1 {
		t.Fatalf("unexpected %+v", c)
	}
	if _, err := (Config{Countries: []string{"CHN"}, IPs: []string{"x.y"}}).Normalize(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestSaveAndReload(t *testing.T) {
	st := &memStore{}
	g := New(st, nil)
	if err := g.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.Config().Enabled {
		t.Fatal("default should be enabled")
	}
	c := g.Config()
	c.Countries = []string{"us"}
	if _, err := g.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	g2 := New(st, nil)
	_ = g2.Reload(context.Background())
	if got := g2.Config().Countries; len(got) != 1 || got[0] != "US" {
		t.Fatalf("reload got %v", got)
	}
}

func rateGuard(rl RateLimitConfig) (*Guard, http.Handler) {
	g := New(nil, nil)
	c := DefaultConfig()
	c.RateLimit = rl
	g.Apply(c)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	return g, g.Middleware(ok, nil)
}

func cfReq(ip, country string) *http.Request {
	return req(cfPeer, "Mozilla", map[string]string{"CF-Connecting-IP": ip, "CF-IPCountry": country})
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// 窗口取 3600s，避免测试跨窗口边界导致抖动。
func TestMiddlewareRateLimitPerIP(t *testing.T) {
	g, h := rateGuard(RateLimitConfig{Enabled: true, Scope: ScopeIP, WindowSec: 3600, Max: 3})
	for i := range 3 {
		if w := serve(h, cfReq("1.2.3.4", "CN")); w.Code != 200 {
			t.Fatalf("req %d want 200, got %d", i, w.Code)
		}
	}
	w := serve(h, cfReq("1.2.3.4", "CN"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "3600" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected headers %v", w.Header())
	}
	if w := serve(h, cfReq("1.2.3.5", "CN")); w.Code != 200 {
		t.Fatalf("other IP want 200, got %d", w.Code)
	}
	if w := serve(h, cfReq("9.9.9.9", "US")); w.Code != http.StatusForbidden {
		t.Fatalf("denied want 403, got %d", w.Code)
	}

	snap := g.Stats.Snapshot()
	if snap.RateLimited != 1 || snap.Blocked != 1 || snap.Allowed[ReasonCountry] != 5 {
		t.Fatalf("unexpected stats %+v", snap)
	}
	rec := g.Stats.Recent(10)
	if len(rec) != 2 || rec[0].Reason != ReasonDenied || rec[1].Reason != ReasonRateLimit || rec[1].IP != "1.2.3.4" {
		t.Fatalf("unexpected recent %+v", rec)
	}
	g.Stats.Reset()
	if g.Stats.Snapshot().RateLimited != 0 {
		t.Fatal("reset should clear rate_limited")
	}
}

func TestMiddlewareRateLimitExemptWhitelist(t *testing.T) {
	_, h := rateGuard(RateLimitConfig{Enabled: true, Scope: ScopeIP, WindowSec: 3600, Max: 1, ExemptIPWhitelist: true})
	for i := range 5 {
		if w := serve(h, req("127.0.0.1:1", "curl", nil)); w.Code != 200 {
			t.Fatalf("whitelisted req %d want 200, got %d", i, w.Code)
		}
	}
	serve(h, cfReq("1.2.3.4", "CN"))
	if w := serve(h, cfReq("1.2.3.4", "CN")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-whitelisted want 503, got %d", w.Code)
	}

	_, h = rateGuard(RateLimitConfig{Enabled: true, Scope: ScopeIP, WindowSec: 3600, Max: 1})
	serve(h, req("127.0.0.1:1", "curl", nil))
	if w := serve(h, req("127.0.0.1:1", "curl", nil)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("without exemption want 503, got %d", w.Code)
	}
}

func TestMiddlewareRateLimitGlobal(t *testing.T) {
	_, h := rateGuard(RateLimitConfig{Enabled: true, Scope: ScopeGlobal, WindowSec: 3600, Max: 2})
	serve(h, cfReq("1.1.1.1", "CN"))
	serve(h, cfReq("2.2.2.2", "HK"))
	if w := serve(h, cfReq("3.3.3.3", "SG")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("global want 503, got %d", w.Code)
	}
}

func TestMiddlewareRateLimitDisabled(t *testing.T) {
	_, h := rateGuard(RateLimitConfig{Enabled: false, Scope: ScopeIP, WindowSec: 3600, Max: 1})
	for i := range 10 {
		if w := serve(h, cfReq("1.2.3.4", "CN")); w.Code != 200 {
			t.Fatalf("req %d want 200, got %d", i, w.Code)
		}
	}
}

func TestMiddlewareRateLimitWhenGuardDisabled(t *testing.T) {
	g := New(nil, nil)
	c := DefaultConfig()
	c.Enabled = false
	c.RateLimit = RateLimitConfig{Enabled: true, Scope: ScopeIP, WindowSec: 3600, Max: 1, ExemptIPWhitelist: true}
	g.Apply(c)
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil)
	serve(h, cfReq("1.2.3.4", "US"))
	if w := serve(h, cfReq("1.2.3.4", "US")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 with guard disabled, got %d", w.Code)
	}
}

func TestMiddlewareRateLimitZeroConfig(t *testing.T) {
	g := New(nil, nil)
	g.Apply(Config{RateLimit: RateLimitConfig{Enabled: true}})
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil)
	for range 100 {
		serve(h, req("8.8.8.8:1", "curl", nil))
	}
	w := serve(h, req("8.8.8.8:1", "curl", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "10" {
		t.Fatalf("zero config should use defaults, got %d %v", w.Code, w.Header())
	}
}

func TestNormalizeRateLimit(t *testing.T) {
	c, err := Config{}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if want := (RateLimitConfig{Scope: ScopeIP, WindowSec: 10, Max: 100}); c.RateLimit != want {
		t.Fatalf("defaults got %+v", c.RateLimit)
	}
	c, err = Config{RateLimit: RateLimitConfig{Scope: " GLOBAL ", WindowSec: 3600, Max: 1000000}}.Normalize()
	if err != nil || c.RateLimit.Scope != ScopeGlobal {
		t.Fatalf("got %+v err=%v", c.RateLimit, err)
	}
	if d := DefaultConfig().RateLimit; d.Enabled || d.Scope != ScopeIP || d.WindowSec != 10 || d.Max != 100 || !d.ExemptIPWhitelist {
		t.Fatalf("unexpected default %+v", d)
	}
	_, err = Config{RateLimit: RateLimitConfig{Scope: "xx", WindowSec: 5000, Max: -1}}.Normalize()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, s := range []string{"无效条目: ", "限速范围:xx", "限速窗口:5000", "限速条数:-1"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("error %q missing %q", err, s)
		}
	}
	if _, err := (Config{RateLimit: RateLimitConfig{Max: 1000001}}).Normalize(); err == nil {
		t.Fatal("expected max upper bound error")
	}
}

func blockGuard(block ...string) *Guard {
	g := New(nil, nil)
	c := DefaultConfig()
	c.BlockIPs = block
	g.Apply(c)
	return g
}

func TestDecideBlacklist(t *testing.T) {
	g := blockGuard("5.6.0.0/16", "2001:db8::/32")
	cases := []struct {
		name   string
		r      *http.Request
		allow  bool
		reason string
	}{
		{"CIDR hit with allowed country", cfReq("5.6.7.8", "CN"), false, ReasonBlacklist},
		{"blacklist beats spider UA", req(cfPeer, "Mozilla/5.0 (compatible; Googlebot/2.1)", map[string]string{"CF-Connecting-IP": "5.6.7.9", "CF-IPCountry": "US"}), false, ReasonBlacklist},
		{"IPv6 hit", cfReq("2001:db8::1", "CN"), false, ReasonBlacklist},
		{"IPv6 outside", cfReq("2001:db9::1", "CN"), true, ReasonCountry},
		{"outside blacklist", cfReq("5.7.0.1", "CN"), true, ReasonCountry},
		{"outside blacklist still denied by country", cfReq("5.7.0.1", "US"), false, ReasonDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := g.Decide(c.r)
			if d.Allow != c.allow || d.Reason != c.reason {
				t.Fatalf("got allow=%v reason=%s (ip=%s), want allow=%v reason=%s", d.Allow, d.Reason, d.IP, c.allow, c.reason)
			}
		})
	}
}

func TestDecideBlacklistWhitelistWins(t *testing.T) {
	g := New(nil, nil)
	c := DefaultConfig()
	c.IPs = append(c.IPs, "5.6.7.8")
	c.BlockIPs = []string{"5.6.0.0/16"}
	g.Apply(c)
	if d := g.Decide(cfReq("5.6.7.8", "US")); !d.Allow || d.Reason != ReasonIP {
		t.Fatalf("whitelist should win over blacklist, got %+v", d)
	}
	if d := g.Decide(cfReq("5.6.7.9", "CN")); d.Allow || d.Reason != ReasonBlacklist {
		t.Fatalf("neighbour should be blacklisted, got %+v", d)
	}
}

func TestDecideBlacklistIgnoredWhenDisabled(t *testing.T) {
	g := New(nil, nil)
	c := DefaultConfig()
	c.Enabled = false
	c.BlockIPs = []string{"5.6.0.0/16"}
	g.Apply(c)
	if d := g.Decide(cfReq("5.6.7.8", "CN")); !d.Allow || d.Reason != ReasonDisabled {
		t.Fatalf("blacklist must not apply when disabled, got %+v", d)
	}
}

// sumMinutes 合计最近 2 分钟，避免请求与查询跨分钟边界导致抖动。
func sumMinutes(g *Guard) MinuteCount {
	var s MinuteCount
	for _, m := range g.Traffic.Minutes(time.Now(), 2) {
		s.Total += m.Total
		s.Allowed += m.Allowed
		s.Denied += m.Denied
		s.Blacklisted += m.Blacklisted
		s.RateLimited += m.RateLimited
	}
	return s
}

func TestMiddlewareBlacklist(t *testing.T) {
	g := blockGuard("5.6.0.0/16")
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), nil)
	if w := serve(h, cfReq("5.6.7.8", "CN")); w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	if w := serve(h, cfReq("1.2.3.4", "US")); w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	snap := g.Stats.Snapshot()
	if snap.Blocked != 2 || snap.Blacklisted != 1 {
		t.Fatalf("unexpected stats %+v", snap)
	}
	rec := g.Stats.Recent(10)
	if len(rec) != 2 || rec[0].Reason != ReasonDenied || rec[1].Reason != ReasonBlacklist || rec[1].IP != "5.6.7.8" {
		t.Fatalf("unexpected recent %+v", rec)
	}
	if m := sumMinutes(g); m.Total != 2 || m.Denied != 2 || m.Blacklisted != 1 || m.Allowed != 0 {
		t.Fatalf("unexpected traffic %+v", m)
	}
	g.Stats.Reset()
	if g.Stats.Snapshot().Blacklisted != 0 {
		t.Fatal("reset should clear blacklisted")
	}
}

func TestMiddlewareTrafficCounts(t *testing.T) {
	g := New(nil, nil)
	c := DefaultConfig()
	c.RateLimit = RateLimitConfig{Enabled: true, Scope: ScopeIP, WindowSec: 3600, Max: 2}
	g.Apply(c)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := g.Middleware(ok, func(p string) bool { return strings.HasPrefix(p, "/ytmadmin") })

	for range 3 {
		serve(h, cfReq("1.2.3.4", "CN"))
	}
	if w := serve(h, cfReq("9.9.9.9", "US")); w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	r := cfReq("1.2.3.4", "CN")
	r.URL.Path = "/ytmadmin/"
	serve(h, r)

	if m := sumMinutes(g); m.Total != 4 || m.Allowed != 2 || m.Denied != 1 || m.RateLimited != 1 || m.Blacklisted != 0 {
		t.Fatalf("unexpected traffic %+v", m)
	}
	ra := g.Traffic.Recent(0)
	if ra.Total != 4 || ra.Subnets != 2 {
		t.Fatalf("unexpected recent access %+v", ra)
	}
}

func TestAddBlockIPs(t *testing.T) {
	ctx := context.Background()
	st := &memStore{}
	g := New(st, nil)

	c, err := g.AddBlockIPs(ctx, []string{" 5.6.0.0/16 ", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.BlockIPs) != 1 || c.BlockIPs[0] != "5.6.0.0/16" || !st.saved || len(st.cfg.BlockIPs) != 1 {
		t.Fatalf("unexpected config %v store %v", c.BlockIPs, st.cfg.BlockIPs)
	}
	if d := g.Decide(cfReq("5.6.7.8", "CN")); d.Reason != ReasonBlacklist {
		t.Fatalf("added entry should take effect, got %+v", d)
	}

	c, err = g.AddBlockIPs(ctx, []string{"5.6.0.0/16", "7.7.7.7"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.BlockIPs, ",") != "5.6.0.0/16,7.7.7.7" {
		t.Fatalf("dedupe failed: %v", c.BlockIPs)
	}

	if _, err := g.AddBlockIPs(ctx, []string{"1.2.3.0/99"}); err == nil || !strings.Contains(err.Error(), "黑名单") {
		t.Fatalf("expected invalid CIDR error, got %v", err)
	}
	if got := g.Config().BlockIPs; len(got) != 2 || len(st.cfg.BlockIPs) != 2 {
		t.Fatalf("invalid add must not change config, got %v / %v", got, st.cfg.BlockIPs)
	}

	if _, err := g.AddBlockIPs(ctx, []string{" ", ""}); err == nil {
		t.Fatal("expected empty input error")
	}
	if _, err := g.AddBlockIPs(ctx, nil); err == nil {
		t.Fatal("expected nil input error")
	}
}

func TestAddBlockIPsNoAliasing(t *testing.T) {
	g := New(&memStore{}, nil)
	base := make([]string, 1, 10)
	base[0] = "1.1.1.1"
	c := DefaultConfig()
	c.BlockIPs = base
	g.Apply(c)
	if _, err := g.AddBlockIPs(context.Background(), []string{"2.2.2.2"}); err != nil {
		t.Fatal(err)
	}
	if spill := base[:2]; spill[1] != "" {
		t.Fatalf("AddBlockIPs wrote into shared backing array: %v", spill)
	}
}

func TestRecentSubnets(t *testing.T) {
	g := blockGuard("5.6.0.0/16", "2001:db8::/32")
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil)
	serve(h, cfReq("5.6.7.8", "CN"))
	serve(h, cfReq("5.6.7.9", "CN"))
	serve(h, cfReq("1.2.3.4", "CN"))
	serve(h, cfReq("2001:db8:1::1", "CN"))

	ra := g.RecentSubnets(0)
	want := map[string]bool{"5.6.7.0/24": true, "1.2.3.0/24": false, "2001:db8:1::/48": true}
	if len(ra.Groups) != len(want) {
		t.Fatalf("unexpected groups %+v", ra.Groups)
	}
	for _, grp := range ra.Groups {
		bl, ok := want[grp.Subnet]
		if !ok || grp.Blacklisted != bl {
			t.Fatalf("group %+v: want blacklisted=%v (known=%v)", grp, bl, ok)
		}
	}
	if ra.Groups[0].Subnet != "5.6.7.0/24" || ra.Groups[0].Count != 2 || ra.Groups[0].Blocked != 2 {
		t.Fatalf("unexpected top group %+v", ra.Groups[0])
	}
}

func TestNormalizeBlockIPs(t *testing.T) {
	c, err := Config{BlockIPs: []string{" 5.6.0.0/16 ", "5.6.0.0/16", "", "2001:db8::/32"}}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.BlockIPs, ",") != "5.6.0.0/16,2001:db8::/32" {
		t.Fatalf("unexpected %v", c.BlockIPs)
	}
	_, err = Config{BlockIPs: []string{"nope"}}.Normalize()
	if err == nil || !strings.Contains(err.Error(), "黑名单") {
		t.Fatalf("expected blacklist validation error, got %v", err)
	}
}

func TestRingWraps(t *testing.T) {
	s := NewStats(3)
	r := httptest.NewRequest("GET", "/", nil)
	for _, ip := range []string{"a", "b", "c", "d"} {
		s.Record(Decision{IP: ip}, r)
	}
	rec := s.Recent(0)
	if len(rec) != 3 || rec[0].IP != "d" || rec[2].IP != "b" {
		t.Fatalf("unexpected ring %+v", rec)
	}
}
