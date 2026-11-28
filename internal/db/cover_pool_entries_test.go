package db

import (
	"context"
	"testing"
	"time"

	"logo23007go/internal/coverpool"
)

func TestCoverPoolEntryStoreImplementsInterface(t *testing.T) {
	var _ coverpool.EntryRegistry = (*CoverPoolEntryStore)(nil)
}

func TestCoverPoolEntryStoreNilSafe(t *testing.T) {
	ctx := context.Background()
	entry := coverpool.PoolEntry{ID: "abc", Text: "t", Width: 1, Height: 2, Count: 3}

	var nilStore *CoverPoolEntryStore
	if err := nilStore.Upsert(ctx, entry); err != nil {
		t.Fatalf("nil store Upsert: %v", err)
	}
	if err := nilStore.UpsertMany(ctx, []coverpool.PoolEntry{entry}); err != nil {
		t.Fatalf("nil store UpsertMany: %v", err)
	}
	if err := nilStore.UpsertMany(ctx, nil); err != nil {
		t.Fatalf("nil store UpsertMany empty: %v", err)
	}
	if err := nilStore.Delete(ctx, "abc"); err != nil {
		t.Fatalf("nil store Delete: %v", err)
	}
	if n, err := nilStore.SumCount(ctx, "t", 1, 2, true); err != nil || n != 0 {
		t.Fatalf("nil store SumCount = %d, %v", n, err)
	}
	if got, err := nilStore.ListBackground(ctx, true, 10); err != nil || got != nil {
		t.Fatalf("nil store ListBackground = %v, %v", got, err)
	}
	if got, err := nilStore.ListDueRotate(ctx, 1, time.Now(), 10); err != nil || got != nil {
		t.Fatalf("nil store ListDueRotate = %v, %v", got, err)
	}
	if err := nilStore.EnsureIndexes(ctx); err != nil {
		t.Fatalf("nil store EnsureIndexes: %v", err)
	}

	empty := &CoverPoolEntryStore{}
	if err := empty.Upsert(ctx, entry); err != nil {
		t.Fatalf("nil Coll Upsert: %v", err)
	}
	if err := empty.UpsertMany(ctx, []coverpool.PoolEntry{entry}); err != nil {
		t.Fatalf("nil Coll UpsertMany: %v", err)
	}
	if err := empty.UpsertMany(ctx, nil); err != nil {
		t.Fatalf("nil Coll UpsertMany empty: %v", err)
	}
	if err := empty.Delete(ctx, "abc"); err != nil {
		t.Fatalf("nil Coll Delete: %v", err)
	}
	if n, err := empty.SumCount(ctx, "t", 1, 2, false); err != nil || n != 0 {
		t.Fatalf("nil Coll SumCount = %d, %v", n, err)
	}
	if got, err := empty.ListBackground(ctx, false, 0); err != nil || got != nil {
		t.Fatalf("nil Coll ListBackground = %v, %v", got, err)
	}
	if got, err := empty.ListDueRotate(ctx, 0, time.Time{}, 0); err != nil || got != nil {
		t.Fatalf("nil Coll ListDueRotate = %v, %v", got, err)
	}
	if err := empty.EnsureIndexes(ctx); err != nil {
		t.Fatalf("nil Coll EnsureIndexes: %v", err)
	}
}

func TestClampCoverPoolEntryLimit(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{0, 1000},
		{-1, 1000},
		{1, 1},
		{1000, 1000},
		{50000, 50000},
		{50001, 50000},
	}
	for _, tt := range tests {
		if got := clampCoverPoolEntryLimit(tt.in); got != tt.want {
			t.Fatalf("clampCoverPoolEntryLimit(%d)=%d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestSpecID(t *testing.T) {
	spec := coverpool.Spec{Text: "hello", Width: 600, Height: 800, Background: "", Format: coverpool.FormatJPEG}
	id := coverpool.SpecID(spec)
	if len(id) != 64 {
		t.Fatalf("SpecID length = %d, want 64 hex chars", len(id))
	}
	if coverpool.SpecID(spec) != id {
		t.Fatal("SpecID not stable")
	}
	other := coverpool.Spec{Text: "hello", Width: 600, Height: 800, Background: "bg", Format: coverpool.FormatJPEG}
	if coverpool.SpecID(other) == id {
		t.Fatal("different Background must change SpecID")
	}
}
