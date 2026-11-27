package coverpool

import (
	"testing"
	"time"
)

func TestSizeStatsNilSafe(t *testing.T) {
	var s *SizeStats
	s.Record(100, 200)
	s.RecordPoolHit()
	s.RecordLive()
	total, items := s.Snapshot()
	if total != 0 {
		t.Fatalf("nil Snapshot total = %d, want 0", total)
	}
	if items == nil {
		t.Fatal("nil Snapshot items must be non-nil empty slice")
	}
	if len(items) != 0 {
		t.Fatalf("nil Snapshot len = %d, want 0", len(items))
	}
	poolHits, liveOk := s.ServeCounts()
	if poolHits != 0 || liveOk != 0 {
		t.Fatalf("nil ServeCounts = %d,%d, want 0,0", poolHits, liveOk)
	}
	s.Reset()
	if !s.StartedAt().IsZero() {
		t.Fatal("nil StartedAt must be zero")
	}
}

func TestSizeStatsRecordAndSnapshot(t *testing.T) {
	s := NewSizeStats()
	if s.StartedAt().IsZero() {
		t.Fatal("StartedAt should be set after NewSizeStats")
	}

	s.Record(100, 200)
	s.Record(100, 200)
	s.Record(50, 50)
	s.Record(300, 100)
	s.Record(300, 100)
	s.Record(300, 100)

	total, items := s.Snapshot()
	if total != 6 {
		t.Fatalf("total = %d, want 6", total)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}
	// Sort: Count desc, then Width, Height asc.
	want := []SizeStat{
		{Width: 300, Height: 100, Count: 3},
		{Width: 100, Height: 200, Count: 2},
		{Width: 50, Height: 50, Count: 1},
	}
	for i, w := range want {
		if items[i] != w {
			t.Fatalf("items[%d] = %+v, want %+v", i, items[i], w)
		}
	}
}

func TestSizeStatsSnapshotTieBreak(t *testing.T) {
	s := NewSizeStats()
	s.Record(200, 100)
	s.Record(100, 200)
	s.Record(100, 100)
	// All count=1 → Width asc, then Height asc.
	_, items := s.Snapshot()
	want := []SizeStat{
		{Width: 100, Height: 100, Count: 1},
		{Width: 100, Height: 200, Count: 1},
		{Width: 200, Height: 100, Count: 1},
	}
	if len(items) != len(want) {
		t.Fatalf("len = %d, want %d", len(items), len(want))
	}
	for i, w := range want {
		if items[i] != w {
			t.Fatalf("items[%d] = %+v, want %+v", i, items[i], w)
		}
	}
}

func TestSizeStatsIgnoreNonPositive(t *testing.T) {
	s := NewSizeStats()
	s.Record(0, 100)
	s.Record(100, 0)
	s.Record(-1, 50)
	s.Record(50, -1)
	s.Record(0, 0)
	total, items := s.Snapshot()
	if total != 0 || len(items) != 0 {
		t.Fatalf("expected empty, got total=%d items=%v", total, items)
	}
}

func TestSizeStatsReset(t *testing.T) {
	s := NewSizeStats()
	started := s.StartedAt()
	s.Record(10, 20)
	s.RecordPoolHit()
	s.RecordLive()
	time.Sleep(2 * time.Millisecond)
	s.Reset()
	if !s.StartedAt().After(started) {
		t.Fatal("Reset should refresh StartedAt")
	}
	total, items := s.Snapshot()
	if total != 0 || len(items) != 0 {
		t.Fatalf("after Reset expected empty, got total=%d items=%v", total, items)
	}
	poolHits, liveOk := s.ServeCounts()
	if poolHits != 0 || liveOk != 0 {
		t.Fatalf("after Reset ServeCounts = %d,%d, want 0,0", poolHits, liveOk)
	}
	s.Record(10, 20)
	total, items = s.Snapshot()
	if total != 1 || len(items) != 1 || items[0].Count != 1 {
		t.Fatalf("after Reset Record failed: total=%d items=%v", total, items)
	}
}

func TestSizeStatsServeCounts(t *testing.T) {
	s := NewSizeStats()
	poolHits, liveOk := s.ServeCounts()
	if poolHits != 0 || liveOk != 0 {
		t.Fatalf("initial ServeCounts = %d,%d, want 0,0", poolHits, liveOk)
	}
	s.RecordPoolHit()
	s.RecordPoolHit()
	s.RecordLive()
	s.RecordLive()
	s.RecordLive()
	// Record must not affect serve counts.
	s.Record(100, 200)
	poolHits, liveOk = s.ServeCounts()
	if poolHits != 2 || liveOk != 3 {
		t.Fatalf("ServeCounts = %d,%d, want 2,3", poolHits, liveOk)
	}
	total, _ := s.Snapshot()
	if total != 1 {
		t.Fatalf("Snapshot total = %d, want 1 (Record independent)", total)
	}
}

func TestSizeStatsCap512(t *testing.T) {
	s := NewSizeStats()
	for i := 1; i <= sizeStatsCap; i++ {
		s.Record(i, 1)
	}
	// Existing key still increments.
	s.Record(1, 1)
	s.Record(1, 1)
	// New key beyond cap ignored.
	s.Record(sizeStatsCap+1, 1)
	s.Record(sizeStatsCap+2, 99)

	total, items := s.Snapshot()
	if len(items) != sizeStatsCap {
		t.Fatalf("len(items) = %d, want %d", len(items), sizeStatsCap)
	}
	// 512 first records + 2 extra for (1,1)
	if total != int64(sizeStatsCap)+2 {
		t.Fatalf("total = %d, want %d", total, sizeStatsCap+2)
	}
	if items[0].Width != 1 || items[0].Height != 1 || items[0].Count != 3 {
		t.Fatalf("top item = %+v, want {1 1 3}", items[0])
	}
}
