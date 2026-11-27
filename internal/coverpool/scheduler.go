package coverpool

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// schedulerProcs is the fixed size of the long-lived worker goroutine pool.
// Throughput is controlled by maxConcurrent (Settings.Workers / SetMaxConcurrent).
const schedulerProcs = 32

// Generator produces one image in a pool slot.
type Generator interface {
	Generate(context.Context, Spec, int) error
}

type scheduleTask struct {
	spec   Spec
	target int
}

// Scheduler fills pools with a fixed goroutine pool; Generate concurrency is
// bounded by maxConcurrent (adjustable via SetMaxConcurrent).
type Scheduler struct {
	ctx      context.Context
	pool     *Pool
	generate Generator
	logger   *slog.Logger
	workers  int // fixed goroutine count (= schedulerProcs)
	queue    chan scheduleTask

	maxConcurrent atomic.Int64
	admitted      atomic.Int64 // workers holding a concurrency slot (fill or waiting for work)
	inflightGen   atomic.Int64 // concurrent Generate calls (Progress.Active)
	completed     atomic.Int64
	failed        atomic.Int64

	mu       sync.Mutex
	inflight map[Spec]struct{}
	wg       sync.WaitGroup
}

// Progress is a snapshot of background fill activity.
type Progress struct {
	QueueLen    int   `json:"queue_len"`
	QueueCap    int   `json:"queue_cap"`
	Inflight    int   `json:"inflight"`     // specs claimed (queued or filling)
	Active      int   `json:"active"`       // concurrent Generate calls
	Workers     int   `json:"workers"`      // max concurrent fills / Generate
	WorkerProcs int   `json:"worker_procs"` // fixed long-lived worker goroutines
	Completed   int64 `json:"completed"`    // images generated OK this process
	Failed      int64 `json:"failed"`       // generate failures this process
	Busy        bool  `json:"busy"`         // queue_len>0 || inflight>0 || active>0
}

// NewScheduler starts schedulerProcs worker goroutines that stop when ctx is canceled.
// The workers parameter only sets the initial maxConcurrent (≤0 → 2, >32 → 32).
func NewScheduler(ctx context.Context, pool *Pool, workers, queue int, generate Generator, logger *slog.Logger) *Scheduler {
	if ctx == nil {
		ctx = context.Background()
	}
	if workers <= 0 {
		workers = 2
	}
	if workers > schedulerProcs {
		workers = schedulerProcs
	}
	if queue <= 0 {
		queue = 256
	}
	s := &Scheduler{
		ctx:      ctx,
		pool:     pool,
		generate: generate,
		logger:   logger,
		workers:  schedulerProcs,
		queue:    make(chan scheduleTask, queue),
		inflight: make(map[Spec]struct{}),
	}
	s.maxConcurrent.Store(int64(workers))
	s.wg.Add(schedulerProcs)
	for range schedulerProcs {
		go s.work()
	}
	return s
}

// SetMaxConcurrent updates the fill/Generate concurrency limit (1..32).
func (s *Scheduler) SetMaxConcurrent(n int) {
	if s == nil {
		return
	}
	if n < 1 {
		n = 1
	}
	if n > 32 {
		n = 32
	}
	s.maxConcurrent.Store(int64(n))
}

// MaxConcurrent returns the current concurrency limit.
func (s *Scheduler) MaxConcurrent() int {
	if s == nil {
		return 2
	}
	n := int(s.maxConcurrent.Load())
	if n < 1 {
		return 2
	}
	return n
}

// Progress returns a nil-safe snapshot. Nil receiver → zero Progress with Workers=2.
func (s *Scheduler) Progress() Progress {
	if s == nil {
		return Progress{Workers: 2}
	}
	s.mu.Lock()
	inflight := len(s.inflight)
	s.mu.Unlock()
	queueLen := len(s.queue)
	active := int(s.inflightGen.Load())
	return Progress{
		QueueLen:    queueLen,
		QueueCap:    cap(s.queue),
		Inflight:    inflight,
		Active:      active,
		Workers:     s.MaxConcurrent(),
		WorkerProcs: s.workers,
		Completed:   s.completed.Load(),
		Failed:      s.failed.Load(),
		Busy:        queueLen > 0 || inflight > 0 || active > 0,
	}
}

// Ensure schedules one fill round per spec. A full queue rejects the task and
// clears its marker so a later request can retry it.
func (s *Scheduler) Ensure(spec Spec, target int) bool {
	if s == nil || target <= 1 || s.ctx.Err() != nil {
		return false
	}

	s.mu.Lock()
	if _, exists := s.inflight[spec]; exists {
		s.mu.Unlock()
		return false
	}
	s.inflight[spec] = struct{}{}
	select {
	case s.queue <- scheduleTask{spec: spec, target: target}:
		s.mu.Unlock()
		return true
	default:
		delete(s.inflight, spec)
		s.mu.Unlock()
		return false
	}
}

// Wait blocks until all workers have stopped after service cancellation.
func (s *Scheduler) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}

func (s *Scheduler) work() {
	defer s.wg.Done()
	for {
		if !s.acquireAdmission() {
			return
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			s.releaseAdmission()
			return
		case task := <-s.queue:
			timer.Stop()
			s.fill(task)
			s.releaseAdmission()
		case <-timer.C:
			// Idle: drop admission so maxConcurrent slots stay available for
			// real work and so Progress.Active can settle to 0.
			s.releaseAdmission()
		}
	}
}

func (s *Scheduler) fill(task scheduleTask) {
	defer func() {
		s.mu.Lock()
		delete(s.inflight, task.spec)
		s.mu.Unlock()
	}()

	if s.pool == nil || s.generate == nil {
		return
	}
	missing, err := s.pool.Missing(task.spec, task.target)
	if err != nil {
		s.logFailure(task.spec, 0, "inspect missing slots", err)
		return
	}
	// Admission already caps parallel fills; generate slots sequentially.
	for _, slot := range missing {
		if s.ctx.Err() != nil {
			return
		}
		s.inflightGen.Add(1)
		err := s.generate.Generate(s.ctx, task.spec, slot)
		s.inflightGen.Add(-1)
		if err != nil {
			s.failed.Add(1)
			s.logFailure(task.spec, slot, "generate cover", err)
			return
		}
		s.completed.Add(1)
	}
}

func (s *Scheduler) acquireAdmission() bool {
	for {
		if s.ctx.Err() != nil {
			return false
		}
		max := int64(s.MaxConcurrent())
		cur := s.admitted.Load()
		if cur < max && s.admitted.CompareAndSwap(cur, cur+1) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *Scheduler) releaseAdmission() {
	s.admitted.Add(-1)
}

func (s *Scheduler) logFailure(spec Spec, slot int, operation string, err error) {
	if s.logger != nil {
		s.logger.Warn("coverpool background task stopped", "operation", operation, "text", spec.Text, "slot", slot, "err", err)
	}
}
