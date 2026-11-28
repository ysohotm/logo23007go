package coverpool

import (
	"context"
	"os"
	"path/filepath"
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

func TestRemoveEntryPathProtection(t *testing.T) {
	root := t.TempDir()
	pool := NewPool(root)
	reg := &fakeRegistry{}
	pool.SetRegistry(reg)

	outside := filepath.Join(t.TempDir(), "escape")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	err := pool.RemoveEntry(context.Background(), PoolEntry{
		ID: "x", Text: "t", Width: 1, Height: 1, Dir: outside,
	})
	if err == nil {
		t.Fatal("RemoveEntry accepted dir outside pool root")
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Fatalf("outside dir should remain: %v", statErr)
	}
	if len(reg.deleted) != 0 {
		t.Fatalf("registry Delete called on rejected path: %v", reg.deleted)
	}

	// Sibling prefix attack: root+"/evil" must not match root if root is a prefix of another path.
	sibling := root + "-evil"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	err = pool.RemoveEntry(context.Background(), PoolEntry{ID: "y", Dir: sibling})
	if err == nil {
		t.Fatal("RemoveEntry accepted sibling prefix path")
	}
}

func TestRemoveEntryDeletesDirAndRegistry(t *testing.T) {
	root := t.TempDir()
	pool := NewPool(root)
	reg := &fakeRegistry{}
	pool.SetRegistry(reg)

	spec := Spec{Text: "rm", Width: 100, Height: 200, Background: "", Format: FormatWebP}
	if err := pool.Put(spec, 0, testWebP); err != nil {
		t.Fatal(err)
	}
	dir, err := pool.Paths(spec)
	if err != nil {
		t.Fatal(err)
	}
	if pool.CountByTextSize("rm", 100, 200) != 1 {
		t.Fatal("expected indexed count 1")
	}

	entry := PoolEntry{
		ID: SpecID(spec), Text: spec.Text, Width: spec.Width, Height: spec.Height,
		Background: spec.Background, Format: spec.Format, Count: 1, Dir: dir,
	}
	if err := pool.RemoveEntry(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still exists: %v", err)
	}
	if len(reg.deleted) != 1 || reg.deleted[0] != SpecID(spec) {
		t.Fatalf("deleted = %v", reg.deleted)
	}
	if got := pool.CountByTextSize("rm", 100, 200); got != 0 {
		t.Fatalf("index after remove = %d, want 0", got)
	}
}

func TestRebuildIndexDoesNotUpsertRegistry(t *testing.T) {
	pool := NewPool(t.TempDir())
	reg := &fakeRegistry{}
	pool.SetRegistry(reg)

	spec := Spec{Text: "sync", Width: 240, Height: 300, Background: "", Format: FormatJPEG}
	if err := pool.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(spec, 0, testJPEG); err != nil {
		t.Fatal(err)
	}
	// Put already upserts; clear — RebuildIndex is memory-only and must not touch registry.
	reg.upserted = nil
	if err := pool.RebuildIndex(); err != nil {
		t.Fatal(err)
	}
	if len(reg.upserted) != 0 {
		t.Fatalf("upserted = %d, want 0 (RebuildIndex is memory-only)", len(reg.upserted))
	}
	if got := pool.CountByTextSize("sync", 240, 300); got != 1 {
		t.Fatalf("index after RebuildIndex = %d, want 1", got)
	}
}

func TestSyncRegistryFromDisk(t *testing.T) {
	pool := NewPool(t.TempDir())
	if n, err := pool.SyncRegistryFromDisk(context.Background()); err != nil || n != 0 {
		t.Fatalf("nil registry: n=%d err=%v", n, err)
	}

	reg := &fakeRegistry{}
	pool.SetRegistry(reg)
	spec := Spec{Text: "disk", Width: 10, Height: 20, Background: "bg", Format: FormatWebP}
	if err := pool.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(spec, 0, testWebP); err != nil {
		t.Fatal(err)
	}
	reg.upserted = nil
	n, err := pool.SyncRegistryFromDisk(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("SyncRegistryFromDisk = %d, %v", n, err)
	}
	if len(reg.upserted) != 1 || reg.upserted[0].Text != "disk" {
		t.Fatalf("upserted = %#v", reg.upserted)
	}
}

func TestLoadIndexFromRegistry(t *testing.T) {
	pool := NewPool(t.TempDir())
	reg := &fakeRegistry{upserted: []PoolEntry{
		{ID: "a", Text: "x", Width: 300, Height: 400, Count: 4, Dir: "/a"},
		{ID: "b", Text: "x", Width: 300, Height: 400, Count: 6, Dir: "/b"},
		{ID: "c", Text: "y", Width: 300, Height: 400, Count: 2, Dir: "/c"},
	}}
	pool.SetRegistry(reg)
	if err := pool.LoadIndexFromRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pool.CountByTextSize("x", 300, 400); got != 10 {
		t.Fatalf("CountByTextSize x = %d, want 10", got)
	}
	if got := pool.CountByTextSize("y", 300, 400); got != 2 {
		t.Fatalf("CountByTextSize y = %d, want 2", got)
	}
	if err := pool.RebuildIndexPreferRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pool.CountByTextSize("x", 300, 400); got != 10 {
		t.Fatalf("after PreferRegistry = %d, want 10", got)
	}
}
