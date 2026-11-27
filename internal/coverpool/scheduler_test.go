package coverpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"
)

type generateCall struct {
	spec Spec
	slot int
}

type blockingGenerator struct {
	started chan generateCall
	release chan error
	pool    *Pool
}

func (g *blockingGenerator) Generate(ctx context.Context, spec Spec, slot int) error {
	g.started <- generateCall{spec: spec, slot: slot}
	select {
	case err := <-g.release:
		if err == nil && g.pool != nil {
			return g.pool.Put(spec, slot, testJPEG)
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSchedulerDeduplicatesInflightSpec(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generator := &blockingGenerator{
		started: make(chan generateCall, 4),
		release: make(chan error, 4),
	}
	pool := NewPool(t.TempDir())
	generator.pool = pool
	scheduler := NewScheduler(ctx, pool, 1, 1, generator, nil)
	spec := testSpec("deduplicate")

	if !scheduler.Ensure(spec, 4) {
		t.Fatal("first Ensure returned false")
	}
	first := receiveGenerateCall(t, generator.started)
	for i := 0; i < 20; i++ {
		if scheduler.Ensure(spec, 10) {
			t.Fatal("duplicate Ensure returned true while the spec was inflight")
		}
	}

	generator.release <- nil
	for _, wantSlot := range []int{1, 2, 3} {
		call := receiveGenerateCall(t, generator.started)
		if call.spec != spec || call.slot != wantSlot {
			t.Fatalf("Generate() = %+v, want spec %+v slot %d", call, spec, wantSlot)
		}
		generator.release <- nil
	}
	waitSchedulerIdle(t, scheduler)

	if !scheduler.Ensure(spec, 6) {
		t.Fatal("Ensure with a larger target returned false after the first round")
	}
	for _, wantSlot := range []int{4, 5} {
		call := receiveGenerateCall(t, generator.started)
		if call.slot != wantSlot {
			t.Fatalf("larger target generated slot %d, want %d", call.slot, wantSlot)
		}
		generator.release <- nil
	}
	waitSchedulerIdle(t, scheduler)
	if first.spec != spec || first.slot != 0 {
		t.Fatalf("first Generate() = %+v, want slot 0", first)
	}
}

func TestSchedulerGeneratesOnlyMissingSlots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewPool(t.TempDir())
	spec := testSpec("partial")
	for _, slot := range []int{0, 2, 5} {
		if err := pool.Put(spec, slot, testJPEG); err != nil {
			t.Fatal(err)
		}
	}
	generator := &recordingGenerator{}
	scheduler := NewScheduler(ctx, pool, 1, 1, generator, nil)

	if !scheduler.Ensure(spec, 6) {
		t.Fatal("Ensure returned false")
	}
	waitSchedulerIdle(t, scheduler)
	if got, want := generator.calls(), []int{1, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("generated slots = %v, want %v", got, want)
	}
	if got := scheduler.Ensure(spec, 1); got {
		t.Fatal("Ensure accepted target 1")
	}
	if got := scheduler.Ensure(spec, 0); got {
		t.Fatal("Ensure accepted target 0")
	}
}

func TestSchedulerStopsRoundAfterFailureAndRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generator := &blockingGenerator{
		started: make(chan generateCall, 8),
		release: make(chan error, 8),
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	pool := NewPool(t.TempDir())
	generator.pool = pool
	scheduler := NewScheduler(ctx, pool, 1, 1, generator, logger)
	spec := testSpec("retry")

	if !scheduler.Ensure(spec, 4) {
		t.Fatal("first Ensure returned false")
	}
	for range 2 {
		receiveGenerateCall(t, generator.started)
		generator.release <- nil
	}
	receiveGenerateCall(t, generator.started)
	generator.release <- errors.New("generate failed")
	waitSchedulerIdle(t, scheduler)
	if len(generator.started) != 0 {
		t.Fatal("generation continued after a slot failed")
	}
	if !bytes.Contains(logs.Bytes(), []byte("generate failed")) {
		t.Fatalf("failure was not logged: %s", logs.String())
	}

	if !scheduler.Ensure(spec, 4) {
		t.Fatal("Ensure returned false after failure")
	}
	for _, wantSlot := range []int{2, 3} {
		call := receiveGenerateCall(t, generator.started)
		if call.slot != wantSlot {
			t.Fatalf("retry generated slot %d, want %d", call.slot, wantSlot)
		}
		generator.release <- nil
	}
	waitSchedulerIdle(t, scheduler)
}

func TestSchedulerQueueIsBoundedAndRetryable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstGenerator := &blockingGenerator{
		started: make(chan generateCall, 4),
		release: make(chan error, 4),
	}
	// maxConcurrent=1 → only one fill runs; extra tasks stay in the queue.
	scheduler := NewScheduler(ctx, NewPool(t.TempDir()), 1, 1, firstGenerator, nil)
	first := testSpec("first")
	second := testSpec("second")
	third := testSpec("third")

	if !scheduler.Ensure(first, 2) {
		t.Fatal("first Ensure returned false")
	}
	receiveGenerateCall(t, firstGenerator.started)
	if !scheduler.Ensure(second, 2) {
		t.Fatal("second Ensure returned false with one queue slot available")
	}
	if scheduler.Ensure(third, 2) {
		t.Fatal("third Ensure returned true with a full queue")
	}
	if scheduler.Ensure(third, 2) {
		t.Fatal("rejected Ensure left the spec marked inflight")
	}

	// Finish first fill (2 slots) so the queued second starts and frees a queue slot.
	firstGenerator.release <- nil
	receiveGenerateCall(t, firstGenerator.started)
	firstGenerator.release <- nil
	deadline := time.Now().Add(2 * time.Second)
	for !scheduler.Ensure(third, 2) {
		if time.Now().After(deadline) {
			t.Fatal("Ensure did not retry a spec after queue saturation")
		}
		time.Sleep(time.Millisecond)
	}
	secondCall := receiveGenerateCall(t, firstGenerator.started)
	if secondCall.spec != second || secondCall.slot != 0 {
		t.Fatalf("queued task = %+v, want second slot 0", secondCall)
	}
	firstGenerator.release <- nil
	receiveGenerateCall(t, firstGenerator.started)
	firstGenerator.release <- nil
	thirdCall := receiveGenerateCall(t, firstGenerator.started)
	if thirdCall.spec != third || thirdCall.slot != 0 {
		t.Fatalf("retry after queue saturation = %+v, want third slot 0", thirdCall)
	}

	cancel()
	waitScheduler(t, scheduler)
}

func TestSchedulerCancelReleasesGenerationAndWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	generator := &blockingGenerator{
		started: make(chan generateCall, 1),
		release: make(chan error),
	}
	scheduler := NewScheduler(ctx, NewPool(t.TempDir()), 1, 1, generator, nil)
	if !scheduler.Ensure(testSpec("cancel"), 2) {
		t.Fatal("Ensure returned false")
	}
	receiveGenerateCall(t, generator.started)

	cancel()
	waitScheduler(t, scheduler)
	if scheduler.Ensure(testSpec("after-cancel"), 2) {
		t.Fatal("Ensure accepted work after cancellation")
	}
}

func TestSchedulerProgressNilSafe(t *testing.T) {
	var scheduler *Scheduler
	got := scheduler.Progress()
	want := Progress{Workers: 2}
	if got != want {
		t.Fatalf("nil Progress() = %+v, want %+v", got, want)
	}
}

func TestSchedulerProgressCounts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewPool(t.TempDir())
	generator := &blockingGenerator{
		started: make(chan generateCall, 8),
		release: make(chan error, 8),
		pool:    pool,
	}
	scheduler := NewScheduler(ctx, pool, 2, 8, generator, nil)
	spec := testSpec("progress")
	const target = 4

	if !scheduler.Ensure(spec, target) {
		t.Fatal("Ensure returned false")
	}
	receiveGenerateCall(t, generator.started)
	p := scheduler.Progress()
	if !p.Busy {
		t.Fatalf("Progress while working: Busy=false, got %+v", p)
	}
	if p.Workers != scheduler.MaxConcurrent() {
		t.Fatalf("Workers = %d, want %d", p.Workers, scheduler.MaxConcurrent())
	}
	if p.WorkerProcs != schedulerProcs {
		t.Fatalf("WorkerProcs = %d, want %d", p.WorkerProcs, schedulerProcs)
	}
	if p.QueueCap != 8 {
		t.Fatalf("QueueCap = %d, want 8", p.QueueCap)
	}

	generator.release <- nil
	deadline := time.Now().Add(2 * time.Second)
	for scheduler.Progress().Completed < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Completed >= 1")
		}
		time.Sleep(time.Millisecond)
	}
	if p := scheduler.Progress(); p.Completed < 1 {
		t.Fatalf("Completed = %d, want >= 1", p.Completed)
	}

	for i := 1; i < target; i++ {
		receiveGenerateCall(t, generator.started)
		generator.release <- nil
	}
	waitSchedulerIdle(t, scheduler)

	p = scheduler.Progress()
	if p.Completed != int64(target) {
		t.Fatalf("final Completed = %d, want %d", p.Completed, target)
	}
	if p.Failed != 0 {
		t.Fatalf("Failed = %d, want 0", p.Failed)
	}
	if p.Busy {
		t.Fatalf("final Progress still Busy: %+v", p)
	}
	if p.Inflight != 0 || p.Active != 0 || p.QueueLen != 0 {
		t.Fatalf("final Progress not idle: %+v", p)
	}
}

func TestSchedulerSetMaxConcurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler := NewScheduler(ctx, NewPool(t.TempDir()), 4, 8, &recordingGenerator{}, nil)
	if got := scheduler.MaxConcurrent(); got != 4 {
		t.Fatalf("MaxConcurrent() = %d, want 4", got)
	}
	scheduler.SetMaxConcurrent(1)
	if got := scheduler.MaxConcurrent(); got != 1 {
		t.Fatalf("after SetMaxConcurrent(1) = %d", got)
	}
	scheduler.SetMaxConcurrent(100)
	if got := scheduler.MaxConcurrent(); got != 32 {
		t.Fatalf("SetMaxConcurrent clamps to 32, got %d", got)
	}
	scheduler.SetMaxConcurrent(0)
	if got := scheduler.MaxConcurrent(); got != 1 {
		t.Fatalf("SetMaxConcurrent clamps low to 1, got %d", got)
	}
	var nilSched *Scheduler
	nilSched.SetMaxConcurrent(8)
	if got := nilSched.MaxConcurrent(); got != 2 {
		t.Fatalf("nil MaxConcurrent = %d", got)
	}
}

func TestSchedulerSetMaxConcurrentRaisesParallelism(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewPool(t.TempDir())
	generator := &blockingGenerator{
		started: make(chan generateCall, 16),
		release: make(chan error, 16),
		pool:    pool,
	}
	scheduler := NewScheduler(ctx, pool, 2, 16, generator, nil)
	if got := scheduler.Progress().WorkerProcs; got != schedulerProcs {
		t.Fatalf("WorkerProcs = %d, want %d", got, schedulerProcs)
	}
	if got := scheduler.MaxConcurrent(); got != 2 {
		t.Fatalf("MaxConcurrent = %d, want 2", got)
	}

	// Multiple specs so distinct worker goroutines can Generate in parallel
	// (fill is serial within one task).
	for i := 0; i < 4; i++ {
		spec := testSpec(fmt.Sprintf("par-%d", i))
		if !scheduler.Ensure(spec, 2) {
			t.Fatalf("Ensure(%s) returned false", spec.Text)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for scheduler.Progress().Active < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Active=2, got %+v", scheduler.Progress())
		}
		time.Sleep(time.Millisecond)
	}
	receiveGenerateCall(t, generator.started)
	receiveGenerateCall(t, generator.started)

	if p := scheduler.Progress(); p.Active != 2 || p.Workers != 2 {
		t.Fatalf("before raise: Active=%d Workers=%d, want 2/2 (got %+v)", p.Active, p.Workers, p)
	}
	time.Sleep(50 * time.Millisecond)
	if p := scheduler.Progress(); p.Active > 2 {
		t.Fatalf("Active rose above maxConcurrent=2: %d", p.Active)
	}

	scheduler.SetMaxConcurrent(4)
	if got := scheduler.Progress().Workers; got != 4 {
		t.Fatalf("Workers after SetMaxConcurrent(4) = %d, want 4", got)
	}

	deadline = time.Now().Add(2 * time.Second)
	for scheduler.Progress().Active <= 2 {
		if time.Now().After(deadline) {
			t.Fatalf("Active did not exceed 2 after SetMaxConcurrent(4): %+v", scheduler.Progress())
		}
		time.Sleep(time.Millisecond)
	}

	const totalImages = 8
	// Unblock the two Generates still waiting from the maxConcurrent=2 phase.
	generator.release <- nil
	generator.release <- nil
	// Consume+release the rest (including any that started after the raise).
	deadline = time.Now().Add(3 * time.Second)
	for {
		p := scheduler.Progress()
		if !p.Busy && p.Completed == int64(totalImages) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out draining: %+v", p)
		}
		select {
		case <-generator.started:
			generator.release <- nil
		case <-time.After(10 * time.Millisecond):
		}
	}
	waitSchedulerIdle(t, scheduler)
	if got := scheduler.MaxConcurrent(); got != 4 {
		t.Fatalf("final MaxConcurrent = %d, want 4", got)
	}
}

func TestSchedulerNilSafetyAndDefaults(t *testing.T) {
	var scheduler *Scheduler
	if scheduler.Ensure(testSpec("nil"), 10) {
		t.Fatal("nil Scheduler.Ensure returned true")
	}
	scheduler.Wait()

	ctx, cancel := context.WithCancel(context.Background())
	nilPool := NewScheduler(ctx, nil, 0, 0, &recordingGenerator{}, nil)
	if got := nilPool.workers; got != schedulerProcs {
		t.Fatalf("worker procs = %d, want %d", got, schedulerProcs)
	}
	if got := nilPool.MaxConcurrent(); got != 2 {
		t.Fatalf("default MaxConcurrent = %d, want 2", got)
	}
	if got := cap(nilPool.queue); got != 256 {
		t.Fatalf("default queue capacity = %d, want 256", got)
	}
	if !nilPool.Ensure(testSpec("nil-pool"), 2) {
		t.Fatal("nil-pool Ensure returned false")
	}
	waitSchedulerIdle(t, nilPool)

	nilGenerator := NewScheduler(ctx, NewPool(t.TempDir()), 1, 1, nil, nil)
	if !nilGenerator.Ensure(testSpec("nil-generator"), 2) {
		t.Fatal("nil-generator Ensure returned false")
	}
	waitSchedulerIdle(t, nilGenerator)

	cancel()
	waitScheduler(t, nilPool)
	waitScheduler(t, nilGenerator)
}

type recordingGenerator struct {
	mu   sync.Mutex
	used []int
}

func (g *recordingGenerator) Generate(_ context.Context, _ Spec, slot int) error {
	g.record(slot)
	return nil
}

func (g *recordingGenerator) calls() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int(nil), g.used...)
}

func (g *recordingGenerator) record(slot int) {
	g.mu.Lock()
	g.used = append(g.used, slot)
	g.mu.Unlock()
}

func receiveGenerateCall(t *testing.T, calls <-chan generateCall) generateCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Generate")
		return generateCall{}
	}
}

func waitSchedulerIdle(t *testing.T, scheduler *Scheduler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p := scheduler.Progress()
		if p.Inflight == 0 && p.Active == 0 && p.QueueLen == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for scheduler to become idle: %+v", p)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitScheduler(t *testing.T, scheduler *Scheduler) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		scheduler.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(fmt.Sprintf("scheduler.Wait did not return after cancellation"))
	}
}
