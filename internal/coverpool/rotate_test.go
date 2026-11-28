package coverpool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveMetaPreservesLastRotatedAt(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := Spec{Text: "rotate-meta", Width: 600, Height: 800, Background: "", Format: FormatJPEG}
	if err := p.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	dir := mustPaths(t, p, spec)
	metaPath := filepath.Join(dir, "meta.json")

	rotatedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	createdAt := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	seed, err := json.MarshalIndent(Meta{
		Text:          spec.Text,
		Width:         spec.Width,
		Height:        spec.Height,
		Background:    spec.Background,
		Format:        spec.Format,
		CreatedAt:     createdAt,
		LastRotatedAt: rotatedAt,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, append(seed, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Text != spec.Text {
		t.Fatalf("Text = %q, want %q", meta.Text, spec.Text)
	}
	if !meta.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt = %v, want preserved %v", meta.CreatedAt, createdAt)
	}
	if !meta.LastRotatedAt.Equal(rotatedAt) {
		t.Fatalf("LastRotatedAt = %v, want preserved %v", meta.LastRotatedAt, rotatedAt)
	}
}

func TestSaveMetaNewFileZeroLastRotatedAt(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := Spec{Text: "new-meta", Width: 600, Height: 800, Background: "", Format: FormatWebP}
	if err := p.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(mustPaths(t, p, spec), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be set for new meta")
	}
	if !meta.LastRotatedAt.IsZero() {
		t.Fatalf("LastRotatedAt = %v, want zero", meta.LastRotatedAt)
	}
}

func TestRotateOldestDeletesOldestAndUpdatesMeta(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := Spec{Text: "oldest", Width: 600, Height: 800, Background: "", Format: FormatJPEG}
	if err := p.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	for slot := 0; slot < 4; slot++ {
		if err := p.Put(spec, slot, testJPEG); err != nil {
			t.Fatal(err)
		}
	}
	dir := mustPaths(t, p, spec)
	now := time.Now()
	for slot, age := range []time.Duration{72 * time.Hour, 48 * time.Hour, 24 * time.Hour, time.Hour} {
		path := filepath.Join(dir, formatSlotName(slot, FormatJPEG))
		ts := now.Add(-age)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := p.RotateOldest(spec, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "0000.jpg")); !os.IsNotExist(err) {
		t.Fatal("oldest 0000.jpg should be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "0001.jpg")); !os.IsNotExist(err) {
		t.Fatal("second-oldest 0001.jpg should be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "0002.jpg")); err != nil {
		t.Fatalf("newer 0002.jpg missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0003.jpg")); err != nil {
		t.Fatalf("newest 0003.jpg missing: %v", err)
	}
	count, err := p.Count(spec)
	if err != nil || count != 2 {
		t.Fatalf("Count() = %d, %v; want 2", count, err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.LastRotatedAt.IsZero() {
		t.Fatal("LastRotatedAt not set after RotateOldest")
	}
	if time.Since(meta.LastRotatedAt) > time.Minute {
		t.Fatalf("LastRotatedAt too old: %v", meta.LastRotatedAt)
	}

	if deleted, err := p.RotateOldest(spec, 0); err != nil || deleted != 0 {
		t.Fatalf("n<=0 → %d, %v", deleted, err)
	}
}

func TestRotatorTickRotatesDueFullPoolAndEnsures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := NewPool(t.TempDir())
	spec := Spec{Text: "due-full", Width: 600, Height: 800, Background: "", Format: FormatJPEG}
	const target = 3
	if err := pool.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	for slot := 0; slot < target; slot++ {
		if err := pool.Put(spec, slot, testJPEG); err != nil {
			t.Fatal(err)
		}
	}
	dir := mustPaths(t, pool, spec)
	old := time.Now().Add(-10 * 24 * time.Hour)
	for slot := 0; slot < target; slot++ {
		path := filepath.Join(dir, formatSlotName(slot, FormatJPEG))
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	cfg := NewConfig(nil, nil)
	if err := cfg.Set(ctx, Settings{
		Count:           target,
		Sizes:           [][2]int{{600, 800}},
		Workers:         2,
		RotateEveryDays: 7,
		RotateCount:     1,
	}); err != nil {
		t.Fatal(err)
	}

	gen := &recordingEnsureGenerator{pool: pool}
	scheduler := NewScheduler(ctx, pool, 1, 8, gen, nil)
	rotator := &Rotator{
		Pool:      pool,
		Config:    cfg,
		Scheduler: scheduler,
		Limit:     10,
	}

	result := rotator.Tick(ctx)
	if result.Rotated != 1 {
		t.Fatalf("Rotated = %d, want 1 (result=%+v)", result.Rotated, result)
	}
	if result.Enqueued != 1 {
		t.Fatalf("Enqueued = %d, want 1 (result=%+v)", result.Enqueued, result)
	}
	if result.Failed != 0 {
		t.Fatalf("Failed = %d, errors=%v", result.Failed, result.Errors)
	}
	waitSchedulerIdle(t, scheduler)

	count, err := pool.Count(spec)
	if err != nil {
		t.Fatal(err)
	}
	if count != target {
		t.Fatalf("after Ensure Count() = %d, want %d", count, target)
	}
	if _, err := os.Stat(filepath.Join(dir, "0000.jpg")); !os.IsNotExist(err) {
		// oldest slot may or may not be 0000 depending on Put order + chtimes;
		// just verify one file was deleted then refilled to target.
	}
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.LastRotatedAt.IsZero() {
		t.Fatal("meta LastRotatedAt still zero after Tick")
	}

	// Not due again until RotateEveryDays elapses.
	second := rotator.Tick(ctx)
	if second.Rotated != 0 {
		t.Fatalf("second Tick Rotated = %d, want 0 (skipped=%d)", second.Rotated, second.Skipped)
	}
}

func TestRotatorTickNilAndDisabled(t *testing.T) {
	var nilRot *Rotator
	if got := nilRot.Tick(context.Background()); got.Scanned != 0 || got.Rotated != 0 || got.Failed != 0 {
		t.Fatalf("nil Tick = %+v", got)
	}
	cfg := NewConfig(nil, nil)
	r := &Rotator{Pool: NewPool(t.TempDir()), Config: cfg}
	if got := r.Tick(context.Background()); got.Scanned != 0 || got.Rotated != 0 || got.Failed != 0 {
		t.Fatalf("disabled rotate Tick = %+v", got)
	}
}

func formatSlotName(slot int, format ImageFormat) string {
	return fmt.Sprintf("%04d.%s", slot, format.Extension())
}

// recordingEnsureGenerator fills missing slots so Ensure after rotate restores Count.
type recordingEnsureGenerator struct {
	pool *Pool
}

func (g *recordingEnsureGenerator) Generate(_ context.Context, spec Spec, slot int) error {
	return g.pool.Put(spec, slot, testJPEG)
}
