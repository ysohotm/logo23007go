package coverpool

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeRegistry is a minimal EntryRegistry for StatusFor / RemoveEntry / rotate tests.
type fakeRegistry struct {
	sumCount     int
	sumCountErr  error
	sumCalls     int
	lastEmptyBG  bool
	deleted      []string
	upserted     []PoolEntry
	upsertErr    error
	deleteErr    error
	dueRotate    []PoolEntry
}

func (f *fakeRegistry) Upsert(_ context.Context, e PoolEntry) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserted = append(f.upserted, e)
	return nil
}

func (f *fakeRegistry) UpsertMany(ctx context.Context, entries []PoolEntry) error {
	for _, e := range entries {
		if err := f.Upsert(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeRegistry) Delete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeRegistry) SumCount(_ context.Context, text string, width, height int, emptyBackgroundOnly bool) (int, error) {
	f.sumCalls++
	f.lastEmptyBG = emptyBackgroundOnly
	if f.sumCountErr != nil {
		return 0, f.sumCountErr
	}
	return f.sumCount, nil
}

func (f *fakeRegistry) ListBackground(context.Context, bool, int) ([]PoolEntry, error) {
	return nil, nil
}

func (f *fakeRegistry) ListDueRotate(context.Context, int, time.Time, int) ([]PoolEntry, error) {
	if f == nil {
		return nil, nil
	}
	return append([]PoolEntry(nil), f.dueRotate...), nil
}

func TestStatusForProgress(t *testing.T) {
	ctx := context.Background()
	pool := NewPool(t.TempDir())
	cfg := NewConfig(nil, nil)
	if err := cfg.Set(ctx, Settings{Count: 5, Sizes: [][2]int{{600, 800}, {300, 400}}}); err != nil {
		t.Fatal(err)
	}

	// Non-empty backgrounds must still count (progress aggregates across backgrounds).
	bgA := Spec{Text: "alpha", Width: 600, Height: 800, Background: "a.webp", Format: FormatWebP}
	bgB := Spec{Text: "alpha", Width: 600, Height: 800, Background: "b.webp", Format: FormatWebP}
	if err := pool.Put(bgA, 0, testWebP); err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(bgB, 0, testWebP); err != nil {
		t.Fatal(err)
	}

	got := StatusFor(pool, cfg, []string{"alpha", "beta"})
	want := []KeywordStatus{
		{
			Text: "alpha",
			Sizes: []SizeProgress{
				{Width: 600, Height: 800, Count: 2, Target: 5},
				{Width: 300, Height: 400, Count: 0, Target: 5},
			},
		},
		{
			Text: "beta",
			Sizes: []SizeProgress{
				{Width: 600, Height: 800, Count: 0, Target: 5},
				{Width: 300, Height: 400, Count: 0, Target: 5},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusFor() = %#v, want %#v", got, want)
	}
}

func TestStatusForMissingPoolConfig(t *testing.T) {
	if got := StatusFor(nil, nil, []string{"x"}); !reflect.DeepEqual(got, []KeywordStatus{{Text: "x"}}) {
		t.Fatalf("nil pool+cfg = %#v", got)
	}

	cfg := NewConfig(nil, nil)
	if err := cfg.Set(context.Background(), Settings{Count: 5, Sizes: [][2]int{{600, 800}}}); err != nil {
		t.Fatal(err)
	}
	got := StatusFor(nil, cfg, []string{"x"})
	want := []KeywordStatus{{
		Text: "x",
		Sizes: []SizeProgress{
			{Width: 600, Height: 800, Count: 0, Target: 5},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nil pool = %#v, want %#v", got, want)
	}

	if got := StatusFor(NewPool(t.TempDir()), nil, nil); len(got) != 0 {
		t.Fatalf("empty texts = %#v", got)
	}
}

func TestStatusForUsesRegistrySumCount(t *testing.T) {
	ctx := context.Background()
	pool := NewPool(t.TempDir())
	reg := &fakeRegistry{sumCount: 42}
	pool.SetRegistry(reg)

	cfg := NewConfig(nil, nil)
	if err := cfg.Set(ctx, Settings{Count: 10, Sizes: [][2]int{{600, 800}}}); err != nil {
		t.Fatal(err)
	}

	// Disk has no images; registry must win.
	got := StatusFor(pool, cfg, []string{"alpha"})
	want := []KeywordStatus{{
		Text: "alpha",
		Sizes: []SizeProgress{
			{Width: 600, Height: 800, Count: 42, Target: 10},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusFor() = %#v, want %#v", got, want)
	}
	if reg.sumCalls != 1 || !reg.lastEmptyBG {
		t.Fatalf("SumCount calls=%d emptyBG=%v, want 1/true", reg.sumCalls, reg.lastEmptyBG)
	}
}

func TestStatusForRegistryErrorFallsBack(t *testing.T) {
	ctx := context.Background()
	pool := NewPool(t.TempDir())
	reg := &fakeRegistry{sumCountErr: errors.New("mongo down")}
	pool.SetRegistry(reg)

	cfg := NewConfig(nil, nil)
	if err := cfg.Set(ctx, Settings{Count: 5, Sizes: [][2]int{{600, 800}}}); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Text: "alpha", Width: 600, Height: 800, Background: "", Format: FormatWebP}
	if err := pool.Put(spec, 0, testWebP); err != nil {
		t.Fatal(err)
	}

	got := StatusFor(pool, cfg, []string{"alpha"})
	if got[0].Sizes[0].Count != 1 {
		t.Fatalf("fallback count = %d, want 1", got[0].Sizes[0].Count)
	}
}
