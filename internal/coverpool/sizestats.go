package coverpool

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const sizeStatsCap = 512

// SizeStat is one WxH bucket and its request count.
type SizeStat struct {
	Width  int   `json:"w"`
	Height int   `json:"h"`
	Count  int64 `json:"count"`
}

// SizeStats counts image requests by WxH in process memory.
type SizeStats struct {
	mu        sync.Mutex
	counts    map[string]int64
	poolHits  int64
	liveOk    int64
	startedAt time.Time
}

// NewSizeStats creates empty counters starting now.
func NewSizeStats() *SizeStats {
	return &SizeStats{
		counts:    make(map[string]int64),
		startedAt: time.Now(),
	}
}

func sizeKey(width, height int) string {
	return fmt.Sprintf("%dx%d", width, height)
}

// Record increments the counter for width×height. No-op if s==nil or width<=0 or height<=0.
func (s *SizeStats) Record(width, height int) {
	if s == nil || width <= 0 || height <= 0 {
		return
	}
	key := sizeKey(width, height)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.counts[key]; !ok && len(s.counts) >= sizeStatsCap {
		return
	}
	s.counts[key]++
}

// RecordPoolHit 预生成池命中并成功写出一次。nil-safe。
func (s *SizeStats) RecordPoolHit() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.poolHits++
}

// RecordLive 实时生成并成功写出一次（未走池命中）。nil-safe。
func (s *SizeStats) RecordLive() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveOk++
}

// ServeCounts 返回本进程累计 poolHits、liveOk。nil → 0,0。
func (s *SizeStats) ServeCounts() (poolHits, liveOk int64) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.poolHits, s.liveOk
}

// Snapshot returns total requests and sizes sorted by Count desc, then Width, Height asc.
// Never returns a nil slice (empty slice OK). Nil receiver → total 0, empty slice.
func (s *SizeStats) Snapshot() (total int64, items []SizeStat) {
	items = make([]SizeStat, 0)
	if s == nil {
		return 0, items
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items = make([]SizeStat, 0, len(s.counts))
	for key, count := range s.counts {
		var w, h int
		fmt.Sscanf(key, "%dx%d", &w, &h)
		total += count
		items = append(items, SizeStat{Width: w, Height: h, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		if items[i].Width != items[j].Width {
			return items[i].Width < items[j].Width
		}
		return items[i].Height < items[j].Height
	})
	return total, items
}

// Reset clears counters. Nil-safe.
func (s *SizeStats) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = make(map[string]int64)
	s.poolHits = 0
	s.liveOk = 0
	s.startedAt = time.Now()
}

// StartedAt returns when counters started (NewSizeStats or last Reset). Zero if nil.
func (s *SizeStats) StartedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startedAt
}
