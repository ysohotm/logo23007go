package coverpool

import (
	"testing"
)

func TestCountByTextSizeAggregatesBackgrounds(t *testing.T) {
	pool := NewPool(t.TempDir())
	base := Spec{Text: "alpha", Width: 600, Height: 800, Format: FormatWebP}
	bgA := base
	bgA.Background = "a.jpg"
	bgB := base
	bgB.Background = "b.jpg"

	if err := pool.Put(bgA, 0, testWebP); err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(bgA, 1, testWebP); err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(bgB, 0, testWebP); err != nil {
		t.Fatal(err)
	}

	if got := pool.CountByTextSize("alpha", 600, 800); got != 3 {
		t.Fatalf("CountByTextSize = %d, want 3", got)
	}
	if got := pool.CountByTextSize("alpha", 300, 400); got != 0 {
		t.Fatalf("other size = %d, want 0", got)
	}
	if got := pool.CountByTextSize("beta", 600, 800); got != 0 {
		t.Fatalf("other text = %d, want 0", got)
	}
}

func TestCountByTextSizeNilSafe(t *testing.T) {
	var pool *Pool
	if got := pool.CountByTextSize("x", 1, 1); got != 0 {
		t.Fatalf("nil pool = %d", got)
	}
	empty := NewPool("")
	if got := empty.CountByTextSize("x", 1, 1); got != 0 {
		t.Fatalf("empty root = %d", got)
	}
}

func TestRebuildIndexFromMeta(t *testing.T) {
	pool := NewPool(t.TempDir())
	spec := Spec{Text: "rebuild", Width: 240, Height: 300, Background: "navy", Format: FormatJPEG}
	if err := pool.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(spec, 0, testJPEG); err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(spec, 2, testJPEG); err != nil {
		t.Fatal(err)
	}

	// Force a clean rebuild from disk (ignores live reindex state).
	pool.indexMu.Lock()
	pool.byTH = nil
	pool.dirCount = nil
	pool.indexBuilt = false
	pool.indexMu.Unlock()

	if err := pool.RebuildIndex(); err != nil {
		t.Fatal(err)
	}
	if got := pool.CountByTextSize("rebuild", 240, 300); got != 2 {
		t.Fatalf("after RebuildIndex = %d, want 2", got)
	}
}
