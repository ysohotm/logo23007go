package ipguard

import (
	"hash/fnv"
	"sync"
	"time"
)

const (
	limiterShards   = 64
	minSweepEvery   = 10 * time.Second
	defaultRLWindow = defaultRateWindowSec * time.Second
)

// rlEntry 记录对齐固定窗口的计数；winStart 为 UnixNano。
type rlEntry struct {
	winStart int64
	cur      int
	prev     int
}

type limiterShard struct {
	mu        sync.Mutex
	m         map[string]*rlEntry
	nextSweep int64
}

// Limiter 是分片的滑动窗口计数器（用上一窗口按剩余比例加权近似）。
type Limiter struct {
	shards [limiterShards]limiterShard
}

func NewLimiter() *Limiter {
	l := &Limiter{}
	for i := range l.shards {
		l.shards[i].m = make(map[string]*rlEntry)
	}
	return l
}

func (l *Limiter) shard(key string) *limiterShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &l.shards[h.Sum32()%limiterShards]
}

// Allow 判断 key 在 now 时是否未超过 window 内 max 次；只有放行的请求计数。
func (l *Limiter) Allow(key string, now time.Time, window time.Duration, max int) bool {
	if max <= 0 {
		return true
	}
	if window <= 0 {
		window = defaultRLWindow
	}
	w := int64(window)
	ts := now.UnixNano()
	curWS := ts - mod(ts, w)
	elapsed := ts - curWS

	sh := l.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if ts >= sh.nextSweep {
		for k, e := range sh.m {
			if e.winStart < curWS-w {
				delete(sh.m, k)
			}
		}
		sh.nextSweep = ts + int64(maxDuration(window, minSweepEvery))
	}

	e := sh.m[key]
	if e == nil {
		e = &rlEntry{winStart: curWS}
		sh.m[key] = e
	}
	switch e.winStart {
	case curWS:
	case curWS - w:
		e.prev, e.cur = e.cur, 0
	default:
		// 窗口长度变化导致未对齐时也走这里，直接清零。
		e.prev, e.cur = 0, 0
	}
	e.winStart = curWS

	est := float64(e.prev)*float64(w-elapsed)/float64(w) + float64(e.cur)
	if est >= float64(max) {
		return false
	}
	e.cur++
	return true
}

// Len 返回当前跟踪的 key 数。
func (l *Limiter) Len() int {
	n := 0
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
