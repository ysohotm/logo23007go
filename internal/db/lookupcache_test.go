package db

import (
	"strconv"
	"testing"
	"time"
)

func TestLookupCacheHit(t *testing.T) {
	c := newLookupCache(time.Minute, 10)
	c.set("a", "fileA")
	v, ok := c.get("a")
	if !ok || v.(string) != "fileA" {
		t.Fatalf("get a = %v, %v", v, ok)
	}
	if _, ok := c.get("missing"); ok {
		t.Fatal("unexpected hit for missing key")
	}
	c.delete("a")
	if _, ok := c.get("a"); ok {
		t.Fatal("hit after delete")
	}
}

func TestLookupCacheTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newLookupCache(30*time.Minute, 10)
	c.now = func() time.Time { return now }
	c.set("a", "fileA")
	now = now.Add(29 * time.Minute)
	if _, ok := c.get("a"); !ok {
		t.Fatal("expected hit before TTL")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.get("a"); ok {
		t.Fatal("expected miss after TTL")
	}
	if c.len() != 0 {
		t.Fatalf("expired entry not removed, len=%d", c.len())
	}
}

func TestLookupCacheCapacity(t *testing.T) {
	const max = 100
	c := newLookupCache(time.Hour, max)
	for i := 0; i < 1000; i++ {
		c.set(strconv.Itoa(i), i)
		if c.len() > max {
			t.Fatalf("len %d exceeds max %d at i=%d", c.len(), max, i)
		}
	}
	if _, ok := c.get("999"); !ok {
		t.Fatal("most recent entry should be present")
	}
}

func TestLookupCacheEvictsExpiredFirst(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newLookupCache(time.Minute, 4)
	c.now = func() time.Time { return now }
	c.set("old1", 1)
	c.set("old2", 2)
	now = now.Add(30 * time.Second)
	c.set("new1", 3)
	c.set("new2", 4)
	now = now.Add(45 * time.Second)
	c.set("new3", 5)
	for _, k := range []string{"new1", "new2", "new3"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("%s should survive eviction", k)
		}
	}
	if c.len() != 3 {
		t.Fatalf("len=%d, want 3", c.len())
	}
}
