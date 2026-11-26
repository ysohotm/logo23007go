package coverpool

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Store persists the shared cover-pool target count.
type Store interface {
	Get(context.Context) (int, error)
	Set(context.Context, int) error
}

// Config provides the current target count without reading the store on requests.
type Config struct {
	store  Store
	logger *slog.Logger
	count  atomic.Int64
	setMu  sync.Mutex
}

// NewConfig returns a configuration with the safe default. The first refresh
// happens only after Run's first interval.
func NewConfig(store Store, logger *slog.Logger) *Config {
	cfg := &Config{store: store, logger: logger}
	cfg.count.Store(10)
	return cfg
}

// Count returns the current target, defaulting to 10 for nil or zero values.
func (c *Config) Count() int {
	if c == nil {
		return 10
	}
	count := c.count.Load()
	if count < 1 || count > 100 {
		return 10
	}
	return int(count)
}

// Set validates and persists a new target before making it visible locally.
func (c *Config) Set(ctx context.Context, count int) error {
	if c == nil {
		return fmt.Errorf("coverpool: nil config")
	}
	if count < 1 || count > 100 {
		return fmt.Errorf("coverpool: count %d outside 1..100", count)
	}

	c.setMu.Lock()
	defer c.setMu.Unlock()
	if c.store != nil {
		if err := c.store.Set(ctx, count); err != nil {
			return err
		}
	}
	c.count.Store(int64(count))
	return nil
}

// Load restores the persisted count at startup without blocking on each request.
func (c *Config) Load(ctx context.Context) {
	c.refresh(ctx)
}

// Run refreshes the count every 30 seconds until ctx is canceled.
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
	count, err := c.store.Get(ctx)
	if err == nil && (count < 1 || count > 100) {
		err = fmt.Errorf("coverpool: stored count %d outside 1..100", count)
	}
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("coverpool config refresh failed; keeping current count", "err", err)
		}
		return
	}
	c.count.Store(int64(count))
}
