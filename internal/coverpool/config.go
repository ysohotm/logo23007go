package coverpool

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store persists the shared cover-pool settings.
type Store interface {
	Get(context.Context) (Settings, error)
	Set(context.Context, Settings) error
}

// Settings is the admin-editable cover-pool configuration.
type Settings struct {
	Count int      `json:"count"`
	Sizes [][2]int `json:"sizes"`
}

const (
	defaultCount = 10
	maxSizes     = 32
)

// DefaultSizes are pooled when Mongo has no sizes yet. Matches the app
// default cover size (no WxH in the URL).
func DefaultSizes() [][2]int {
	return [][2]int{{600, 800}}
}

// DefaultSettings returns count=10 and DefaultSizes.
func DefaultSettings() Settings {
	return Settings{Count: defaultCount, Sizes: DefaultSizes()}
}

// Config provides the current settings without reading the store on requests.
type Config struct {
	store  Store
	logger *slog.Logger
	count  atomic.Int64

	mu    sync.RWMutex
	sizes [][2]int
	setMu sync.Mutex
}

// NewConfig returns a configuration with the safe defaults. The first refresh
// happens only after Run's first interval, or immediately via Load.
func NewConfig(store Store, logger *slog.Logger) *Config {
	cfg := &Config{store: store, logger: logger, sizes: DefaultSizes()}
	cfg.count.Store(defaultCount)
	return cfg
}

// Count returns the current target, defaulting to 10 for nil or invalid values.
func (c *Config) Count() int {
	if c == nil {
		return defaultCount
	}
	count := c.count.Load()
	if count < 1 || count > 100 {
		return defaultCount
	}
	return int(count)
}

// Sizes returns a copy of the allowlisted pool sizes.
func (c *Config) Sizes() [][2]int {
	if c == nil {
		return DefaultSizes()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.sizes) == 0 {
		return DefaultSizes()
	}
	out := make([][2]int, len(c.sizes))
	copy(out, c.sizes)
	return out
}

// Allows reports whether width×height should use the finished-image pool.
func (c *Config) Allows(width, height int) bool {
	for _, size := range c.Sizes() {
		if size[0] == width && size[1] == height {
			return true
		}
	}
	return false
}

// Snapshot returns the current count and sizes for admin APIs.
func (c *Config) Snapshot() Settings {
	return Settings{Count: c.Count(), Sizes: c.Sizes()}
}

// Set validates and persists settings before making them visible locally.
func (c *Config) Set(ctx context.Context, settings Settings) error {
	if c == nil {
		return fmt.Errorf("coverpool: nil config")
	}
	normalized, err := NormalizeSettings(settings)
	if err != nil {
		return err
	}

	c.setMu.Lock()
	defer c.setMu.Unlock()
	if c.store != nil {
		if err := c.store.Set(ctx, normalized); err != nil {
			return err
		}
	}
	c.apply(normalized)
	return nil
}

// Load restores persisted settings at startup without blocking each request.
func (c *Config) Load(ctx context.Context) {
	c.refresh(ctx)
}

// Run refreshes settings every 30 seconds until ctx is canceled.
func (c *Config) Run(ctx context.Context) {
	c.run(ctx, 30*time.Second)
}

func (c *Config) run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

func (c *Config) refresh(ctx context.Context) {
	if c == nil || c.store == nil {
		return
	}
	settings, err := c.store.Get(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("coverpool config refresh failed; keeping current settings", "err", err)
		}
		return
	}
	normalized, err := NormalizeSettings(settings)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("coverpool config refresh failed; keeping current settings", "err", err)
		}
		return
	}
	c.apply(normalized)
}

func (c *Config) apply(settings Settings) {
	c.count.Store(int64(settings.Count))
	c.mu.Lock()
	c.sizes = append([][2]int(nil), settings.Sizes...)
	c.mu.Unlock()
}

// NormalizeSettings validates count/sizes and fills defaults for empty sizes.
func NormalizeSettings(settings Settings) (Settings, error) {
	if settings.Count < 1 || settings.Count > 100 {
		return Settings{}, fmt.Errorf("coverpool: count %d outside 1..100", settings.Count)
	}
	sizes, err := normalizeSizes(settings.Sizes)
	if err != nil {
		return Settings{}, err
	}
	if len(sizes) == 0 {
		sizes = DefaultSizes()
	}
	return Settings{Count: settings.Count, Sizes: sizes}, nil
}

func normalizeSizes(in [][2]int) ([][2]int, error) {
	if len(in) > maxSizes {
		return nil, fmt.Errorf("coverpool: too many sizes (max %d)", maxSizes)
	}
	seen := make(map[[2]int]struct{}, len(in))
	out := make([][2]int, 0, len(in))
	for _, size := range in {
		if size[0] <= 0 || size[1] <= 0 {
			return nil, fmt.Errorf("coverpool: invalid size %dx%d", size[0], size[1])
		}
		if size[0] > 4096 || size[1] > 4096 {
			return nil, fmt.Errorf("coverpool: size %dx%d too large", size[0], size[1])
		}
		if _, ok := seen[size]; ok {
			continue
		}
		seen[size] = struct{}{}
		out = append(out, size)
	}
	return out, nil
}

// ParseSizes parses "300x400, 600x800" into [][2]int.
func ParseSizes(s string) ([][2]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out [][2]int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		ws, hs, ok := strings.Cut(strings.ToLower(part), "x")
		if !ok {
			return nil, fmt.Errorf("invalid size %q: want WIDTHxHEIGHT", part)
		}
		w, err1 := strconv.Atoi(strings.TrimSpace(ws))
		h, err2 := strconv.Atoi(strings.TrimSpace(hs))
		if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
			return nil, fmt.Errorf("invalid size %q: want positive WIDTHxHEIGHT", part)
		}
		out = append(out, [2]int{w, h})
	}
	return out, nil
}

// FormatSizes renders sizes as "600x800,300x400".
func FormatSizes(sizes [][2]int) string {
	parts := make([]string, 0, len(sizes))
	for _, size := range sizes {
		parts = append(parts, fmt.Sprintf("%dx%d", size[0], size[1]))
	}
	return strings.Join(parts, ",")
}
