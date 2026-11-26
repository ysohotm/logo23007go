package coverpool

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"
)

type stubGen struct {
	pool    *Pool
	err     error
	started chan generateCall
	release chan error
}

func (g *stubGen) Generate(ctx context.Context, spec Spec, slot int) error {
	if g.started != nil {
		g.started <- generateCall{spec: spec, slot: slot}
		select {
		case err := <-g.release:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if g.err != nil {
		return g.err
	}
	if g.pool != nil {
		return g.pool.Put(spec, slot, testWebP)
	}
	return nil
}

func TestNormalizeKeywords(t *testing.T) {
	tests := []struct {
		name string
		raw  []string
		want []string
	}{
		{
			name: "trim empty and dedupe",
			raw:  []string{"  alpha ", "", "beta", "alpha", "  beta  ", "gamma"},
			want: []string{"alpha", "beta", "gamma"},
		},
		{
			name: "cap at MaxEnqueueKeywords",
			raw: func() []string {
				raw := make([]string, MaxEnqueueKeywords+50)
				for i := range raw {
					raw[i] = "kw-" + strconv.Itoa(i)
				}
				return raw
			}(),
			want: nil, // checked by length below
		},
		{
			name: "all empty",
			raw:  []string{"", "  ", "\t"},
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeKeywords(tt.raw)
			if tt.name == "cap at MaxEnqueueKeywords" {
				if len(got) != MaxEnqueueKeywords {
					t.Fatalf("len = %d, want %d", len(got), MaxEnqueueKeywords)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NormalizeKeywords() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEnqueueSchedulesAllSizes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := NewPool(t.TempDir())
	cfg := NewConfig(nil, nil)
	if err := cfg.Set(ctx, Settings{Count: 3, Sizes: [][2]int{{600, 800}, {300, 400}}}); err != nil {
		t.Fatal(err)
	}
	gen := &stubGen{
		pool:    pool,
		started: make(chan generateCall, 8),
		release: make(chan error, 8),
	}
	scheduler := NewScheduler(ctx, pool, 1, 8, gen, nil)
	pre := &Prewarmer{Config: cfg, Pool: pool, Scheduler: scheduler, Generate: gen}

	first := pre.Enqueue(ctx, []string{"keyword-a"})
	if first.Keywords != 1 || first.Specs != 2 || first.Accepted != 2 || first.Skipped != 0 || first.Failed != 0 {
		t.Fatalf("first Enqueue = %+v, want Keywords=1 Specs=2 Accepted=2", first)
	}

	// Second call while still generating (inflight) should skip.
	second := pre.Enqueue(ctx, []string{"keyword-a"})
	if second.Accepted != 0 || second.Skipped != 2 || second.Failed != 0 {
		t.Fatalf("inflight Enqueue = %+v, want Skipped=2", second)
	}

	// Drain generation so pools fill.
	for range 6 { // 2 specs × 3 slots
		receiveGenerateCall(t, gen.started)
		gen.release <- nil
	}
	waitSchedulerIdle(t, scheduler)

	// After full, Count >= target → Skipped.
	third := pre.Enqueue(ctx, []string{"keyword-a"})
	if third.Accepted != 0 || third.Skipped != 2 || third.Failed != 0 {
		t.Fatalf("full Enqueue = %+v, want Skipped=2", third)
	}

	cancel()
	waitScheduler(t, scheduler)
}

func TestEnqueueCountOneSyncGeneratesSlotZero(t *testing.T) {
	ctx := context.Background()
	pool := NewPool(t.TempDir())
	cfg := NewConfig(nil, nil)
	if err := cfg.Set(ctx, Settings{Count: 1, Sizes: [][2]int{{240, 300}}}); err != nil {
		t.Fatal(err)
	}
	gen := &stubGen{pool: pool}
	pre := &Prewarmer{Config: cfg, Pool: pool, Generate: gen}

	got := pre.Enqueue(ctx, []string{"sync-kw"})
	if got.Keywords != 1 || got.Specs != 1 || got.Accepted != 1 || got.Failed != 0 {
		t.Fatalf("Enqueue = %+v, want Accepted=1", got)
	}
	spec := Spec{Text: "sync-kw", Width: 240, Height: 300, Background: "", Format: FormatWebP}
	count, err := pool.Count(spec)
	if err != nil || count != 1 {
		t.Fatalf("Count() = %d, %v, want 1", count, err)
	}
	missing, err := pool.Missing(spec, 1)
	if err != nil || len(missing) != 0 {
		t.Fatalf("Missing() = %v, %v", missing, err)
	}

	failing := &Prewarmer{
		Config:   cfg,
		Pool:     pool,
		Generate: &stubGen{err: errors.New("codec boom")},
	}
	// Already full → Skipped, not Failed.
	again := failing.Enqueue(ctx, []string{"sync-kw"})
	if again.Skipped != 1 || again.Failed != 0 {
		t.Fatalf("full sync path = %+v, want Skipped=1", again)
	}

	// Empty pool + generate error → Failed.
	cfg2 := NewConfig(nil, nil)
	if err := cfg2.Set(ctx, Settings{Count: 1, Sizes: [][2]int{{100, 100}}}); err != nil {
		t.Fatal(err)
	}
	failEmpty := &Prewarmer{Config: cfg2, Pool: pool, Generate: &stubGen{err: errors.New("codec boom")}}
	failed := failEmpty.Enqueue(ctx, []string{"fail-kw"})
	if failed.Failed != 1 || failed.Accepted != 0 || len(failed.Errors) == 0 {
		t.Fatalf("failed sync = %+v", failed)
	}
}

func TestEnqueueNilFieldsDoNotPanic(t *testing.T) {
	ctx := context.Background()
	cases := []*Prewarmer{
		nil,
		{},
		{Config: NewConfig(nil, nil)},
		{Config: NewConfig(nil, nil), Pool: NewPool(t.TempDir())},
		{Config: NewConfig(nil, nil), Generate: &stubGen{}},
		{Pool: NewPool(t.TempDir()), Generate: &stubGen{}},
	}
	for i, pre := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked: %v", i, r)
				}
			}()
			got := pre.Enqueue(ctx, []string{"x"})
			if got.Failed != 1 || len(got.Errors) == 0 {
				t.Fatalf("case %d = %+v, want Failed=1 with error", i, got)
			}
		}()
	}
}

func TestEnqueueWaitsNotNeededForAccepted(t *testing.T) {
	// Accepted is counted when Ensure returns true, before workers finish.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewPool(t.TempDir())
	cfg := NewConfig(nil, nil)
	if err := cfg.Set(ctx, Settings{Count: 2, Sizes: [][2]int{{600, 800}}}); err != nil {
		t.Fatal(err)
	}
	gen := &stubGen{
		pool:    pool,
		started: make(chan generateCall, 4),
		release: make(chan error),
	}
	scheduler := NewScheduler(ctx, pool, 1, 4, gen, nil)
	pre := &Prewarmer{Config: cfg, Pool: pool, Scheduler: scheduler, Generate: gen}

	done := make(chan EnqueueResult, 1)
	go func() { done <- pre.Enqueue(ctx, []string{"fast"}) }()
	select {
	case got := <-done:
		if got.Accepted != 1 {
			t.Fatalf("Enqueue = %+v, want Accepted=1 without waiting", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Enqueue blocked waiting for generation")
	}
	receiveGenerateCall(t, gen.started)
	gen.release <- nil
	receiveGenerateCall(t, gen.started)
	gen.release <- nil
	waitSchedulerIdle(t, scheduler)
	cancel()
	waitScheduler(t, scheduler)
}
