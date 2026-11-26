package cache

import (
	"sync"
	"time"
)

type entry struct {
	files     []string
	expiresAt time.Time
}

// FileCache caches directory file listings with absolute expiration.
type FileCache struct {
	mu      sync.RWMutex
	entries map[string]entry
	ttl     time.Duration
}

func NewFileCache(ttl time.Duration) *FileCache {
	return &FileCache{
		entries: make(map[string]entry),
		ttl:     ttl,
	}
}

func (c *FileCache) GetOrLoad(key string, load func() []string) []string {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if ok && time.Now().Before(e.expiresAt) {
		return e.files
	}

	files := load()
	c.mu.Lock()
	c.entries[key] = entry{files: files, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return files
}
