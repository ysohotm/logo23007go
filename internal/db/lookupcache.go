package db

import (
	"sync"
	"time"
)

type cacheEntry struct {
	val       any
	expiresAt time.Time
}

// lookupCache 简单的 TTL + 容量上限内存缓存，满时先清过期项，否则随机淘汰约 10%。
type lookupCache struct {
	mu  sync.Mutex
	m   map[string]cacheEntry
	ttl time.Duration
	max int
	now func() time.Time
}

func newLookupCache(ttl time.Duration, max int) *lookupCache {
	if max < 1 {
		max = 1
	}
	return &lookupCache{
		m:   make(map[string]cacheEntry),
		ttl: ttl,
		max: max,
		now: time.Now,
	}
}

func (c *lookupCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expiresAt) {
		delete(c.m, key)
		return nil, false
	}
	return e.val, true
}

func (c *lookupCache) set(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[key]; !exists && len(c.m) >= c.max {
		c.evictLocked()
	}
	c.m[key] = cacheEntry{val: v, expiresAt: c.now().Add(c.ttl)}
}

func (c *lookupCache) delete(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

func (c *lookupCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (c *lookupCache) evictLocked() {
	now := c.now()
	for k, e := range c.m {
		if !now.Before(e.expiresAt) {
			delete(c.m, k)
		}
	}
	if len(c.m) < c.max {
		return
	}
	n := c.max / 10
	if n < 1 {
		n = 1
	}
	for k := range c.m {
		if n <= 0 {
			break
		}
		delete(c.m, k)
		n--
	}
}
