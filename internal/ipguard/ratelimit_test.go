package ipguard

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// t0 对齐到 10s 窗口起点。
var t0 = time.Unix(1_000_000_000, 0)

const win = 10 * time.Second

func allowN(l *Limiter, key string, now time.Time, n, max int) int {
	ok := 0
	for range n {
		if l.Allow(key, now, win, max) {
			ok++
		}
	}
	return ok
}

func TestLimiterBasic(t *testing.T) {
	l := NewLimiter()
	if got := allowN(l, "a", t0, 100, 100); got != 100 {
		t.Fatalf("want 100 allowed, got %d", got)
	}
	if l.Allow("a", t0.Add(time.Second), win, 100) {
		t.Fatal("101st should be rejected")
	}
	if !l.Allow("a", t0.Add(2*win), win, 100) {
		t.Fatal("should be allowed again after two windows")
	}
}

func TestLimiterSliding(t *testing.T) {
	l := NewLimiter()
	if got := allowN(l, "a", t0, 100, 100); got != 100 {
		t.Fatalf("want 100, got %d", got)
	}
	if l.Allow("a", t0.Add(win), win, 100) {
		t.Fatal("start of next window with full prev should block")
	}
	if got := allowN(l, "a", t0.Add(win+win/2), 100, 100); got != 50 {
		t.Fatalf("halfway want 50 allowed, got %d", got)
	}
}

func TestLimiterKeysIndependent(t *testing.T) {
	l := NewLimiter()
	if got := allowN(l, "a", t0, 10, 5); got != 5 {
		t.Fatalf("a want 5, got %d", got)
	}
	if got := allowN(l, "b", t0, 10, 5); got != 5 {
		t.Fatalf("b want 5, got %d", got)
	}
}

func TestLimiterSweep(t *testing.T) {
	l := NewLimiter()
	for i := range 500 {
		l.Allow(fmt.Sprintf("old%d", i), t0, win, 10)
	}
	if l.Len() != 500 {
		t.Fatalf("want 500 keys, got %d", l.Len())
	}
	later := t0.Add(time.Minute)
	for i := range 2000 {
		l.Allow(fmt.Sprintf("new%d", i), later, win, 10)
	}
	if l.Len() != 2000 {
		t.Fatalf("stale keys not swept, len=%d", l.Len())
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l := NewLimiter()
	var ok atomic.Int64
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				if l.Allow("shared", t0, win, 1000) {
					ok.Add(1)
				}
				l.Allow(fmt.Sprintf("k%d-%d", g, i), t0, win, 1000)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1000 {
		t.Fatalf("want exactly 1000 allowed, got %d", ok.Load())
	}
}

func TestLimiterNonPositiveArgs(t *testing.T) {
	l := NewLimiter()
	if !l.Allow("a", t0, 0, 1) || l.Allow("a", t0, 0, 1) {
		t.Fatal("zero window should fall back to default window")
	}
	if !l.Allow("b", t0, win, 0) {
		t.Fatal("max<=0 means unlimited")
	}
}
