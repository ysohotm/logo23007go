package coverpool

import "context"

// PoolEntry is one on-disk pool directory inventory record.
type PoolEntry struct {
	ID         string // sha256 hashKey(spec) = dir basename
	Text       string
	Width      int
	Height     int
	Background string
	Format     ImageFormat
	Count      int
	Dir        string // absolute pool dir
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
}
