package ipguard

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"logo23007go/internal/clientip"
)

// Store 持久化规则；ok=false 表示尚未保存过（使用默认值）。
type Store interface {
	Load(ctx context.Context) (cfg Config, ok bool, err error)
	Save(ctx context.Context, cfg Config) error
}

// 放行 / 拦截原因。
const (
	ReasonDisabled  = "disabled"
	ReasonIP        = "ip"
	ReasonUA        = "ua"
	ReasonCountry   = "country"
	ReasonNoCountry = "no_country"
	ReasonDenied    = "denied"
	ReasonBlacklist = "blacklist"
	ReasonRateLimit = "rate_limit"
)

type Decision struct {
	Allow   bool   `json:"allow"`
	Reason  string `json:"reason"`
	IP      string `json:"ip"`
	Country string `json:"country"`
	Trusted bool   `json:"trusted_peer"`
	UA      string `json:"ua"`
}

type Guard struct {
	Logger  *slog.Logger
	Store   Store
	Stats   *Stats
	Traffic *Traffic

	cur     atomic.Pointer[compiled]
	limiter *Limiter
}

func New(store Store, logger *slog.Logger) *Guard {
	g := &Guard{
		Logger: logger, Store: store, Stats: NewStats(500),
		Traffic: NewTraffic(DefaultRecentCapacity), limiter: NewLimiter(),
	}
	g.cur.Store(compile(DefaultConfig()))
	return g
}

func (g *Guard) Config() Config { return g.cur.Load().cfg }

// Apply 立即替换运行时规则（不落库）。
func (g *Guard) Apply(c Config) { g.cur.Store(compile(c)) }

// Save 校验、落库并立即生效。
func (g *Guard) Save(ctx context.Context, c Config) (Config, error) {
	c, err := c.Normalize()
	if err != nil {
		return c, err
	}
	c.UpdatedAt = time.Now()
	if g.Store != nil {
		if err := g.Store.Save(ctx, c); err != nil {
			return c, err
		}
	}
	g.Apply(c)
	return c, nil
}

// AddBlockIPs 把条目追加到黑名单并保存生效（重复项由 Normalize 去重）。
func (g *Guard) AddBlockIPs(ctx context.Context, items []string) (Config, error) {
	var add []string
	for _, s := range items {
		if s = strings.TrimSpace(s); s != "" {
			add = append(add, s)
		}
	}
	if len(add) == 0 {
		return g.Config(), errors.New("黑名单条目为空")
	}
	c := g.Config()
	c.BlockIPs = append(append(make([]string, 0, len(c.BlockIPs)+len(add)), c.BlockIPs...), add...)
	return g.Save(ctx, c)
}

// RecentSubnets 返回最近访问按网段聚合，并标注已被黑名单覆盖的网段。
func (g *Guard) RecentSubnets(limit int) RecentAccess {
	ra := g.Traffic.Recent(limit)
	cp := g.cur.Load()
	for i := range ra.Groups {
		p, err := netip.ParsePrefix(ra.Groups[i].Subnet)
		if err != nil {
			continue
		}
		ra.Groups[i].Blacklisted = cp.ipBlocked(p.Addr().String())
	}
	return ra
}

// Reload 从 Store 读取；失败时保留当前规则。
func (g *Guard) Reload(ctx context.Context) error {
	if g.Store == nil {
		return nil
	}
	c, ok, err := g.Store.Load(ctx)
	if err != nil {
		return err
	}
	if !ok {
		c = DefaultConfig()
	}
	g.Apply(c)
	return nil
}

// Run 周期性 Reload，直到 ctx 结束。
func (g *Guard) Run(ctx context.Context, every time.Duration) {
	load := func() {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := g.Reload(lctx); err != nil && g.Logger != nil {
			g.Logger.Warn("ipguard reload failed, keep current rules", "err", err)
		}
	}
	load()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			load()
		}
	}
}

func (g *Guard) Decide(r *http.Request) Decision { return g.decide(g.cur.Load(), r) }

func (g *Guard) decide(cp *compiled, r *http.Request) Decision {
	d := Decision{
		IP:      clientip.ClientIP(r),
		Trusted: clientip.FromTrustedPeer(r),
		UA:      r.UserAgent(),
	}
	if d.Trusted {
		d.Country = strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
	}
	switch {
	case !cp.cfg.Enabled:
		d.Allow, d.Reason = true, ReasonDisabled
	case cp.ipAllowed(d.IP):
		d.Allow, d.Reason = true, ReasonIP
	case cp.ipBlocked(d.IP):
		// 黑名单优先于 UA 白名单，防伪造蜘蛛 UA。
		d.Allow, d.Reason = false, ReasonBlacklist
	case cp.uaAllowed(d.UA):
		d.Allow, d.Reason = true, ReasonUA
	case d.Country == "":
		d.Allow, d.Reason = cp.cfg.AllowNoCountry, ReasonNoCountry
	default:
		_, ok := cp.countries[d.Country]
		d.Allow, d.Reason = ok, ReasonCountry
	}
	if !d.Allow && d.Reason != ReasonBlacklist {
		d.Reason = ReasonDenied
	}
	return d
}

// Middleware 对非豁免路径执行规则，拒绝时返回 403；放行后再限速，超限返回 503。
func (g *Guard) Middleware(next http.Handler, exempt func(path string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt != nil && exempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		now := time.Now()
		cp := g.cur.Load()
		d := g.decide(cp, r)
		g.Stats.Record(d, r)
		if !d.Allow {
			g.Traffic.Record(now, d.IP, d.Country, d.UA, d.Reason)
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
		if !g.rateAllow(cp, d, now) {
			g.Traffic.Record(now, d.IP, d.Country, d.UA, ReasonRateLimit)
			g.Stats.RecordRateLimited(d, r)
			w.Header().Set("Retry-After", strconv.Itoa(cp.rateWindowSec))
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "503 Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		g.Traffic.Record(now, d.IP, d.Country, d.UA, OutcomeAllowed)
		next.ServeHTTP(w, r)
	})
}

// rateAllow 在 ipguard 关闭（Reason=disabled）时同样生效。
func (g *Guard) rateAllow(cp *compiled, d Decision, now time.Time) bool {
	rl := cp.cfg.RateLimit
	if !rl.Enabled || (rl.ExemptIPWhitelist && d.Reason == ReasonIP) {
		return true
	}
	key := d.IP
	if cp.rateGlobal {
		key = "*"
	}
	return g.limiter.Allow(key, now, time.Duration(cp.rateWindowSec)*time.Second, cp.rateMax)
}
