package coverpool

import (
	"context"
	"time"
)

// PoolEntry is one on-disk pool directory inventory record.
type PoolEntry struct {
	ID            string // sha256 hashKey(spec) = dir basename
	Text          string
	Width         int
	Height        int
	Background    string
	Format        ImageFormat
	Count         int
	Dir           string    // absolute pool dir
	LastRotatedAt time.Time // zero if never rotated
}

// EntryRegistry persists pool directory metadata in Mongo (not image bytes).
type EntryRegistry interface {
	Upsert(ctx context.Context, e PoolEntry) error
	UpsertMany(ctx context.Context, entries []PoolEntry) error
	Delete(ctx context.Context, id string) error
	// SumCount sums Count for text+width+height. If emptyBackgroundOnly, only Background=="".
	SumCount(ctx context.Context, text string, width, height int, emptyBackgroundOnly bool) (int, error)
	// ListBackground: nonempty=true → background!=""; nonempty=false → background=="".
	ListBackground(ctx context.Context, nonempty bool, limit int) ([]PoolEntry, error)
	// ListDueRotate: empty background, Count>=minCount, due by last_rotated_at or updated_at.
	// olderThan: entries with last_rotated_at<=olderThan, OR missing/zero last_rotated_at with updated_at<=olderThan.
	// limit clamped 1..50000 (default 50 if <1).
	// Sort: prefer oldest last_rotated_at / updated_at first.
	ListDueRotate(ctx context.Context, minCount int, olderThan time.Time, limit int) ([]PoolEntry, error)
	// EachEntry streams every inventory row (for startup index load). Stop on fn error.
	EachEntry(ctx context.Context, fn func(PoolEntry) error) error
}
