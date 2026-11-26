package coverpool

import (
	"context"
	"log/slog"
	"sync"
)

// Generator produces one image in a pool slot.
type Generator interface {
	Generate(context.Context, Spec, int) error
}

type scheduleTask struct {
	spec   Spec
	target int
}

// Scheduler fills pools with a bounded number of background workers.
type Scheduler struct {
	ctx      context.Context
	pool     *Pool
	generate Generator
	logger   *slog.Logger
	workers  int
	queue    chan scheduleTask

	mu       sync.Mutex
	inflight map[Spec]struct{}
	wg       sync.WaitGroup
}

// NewScheduler starts background workers that stop when ctx is canceled.
func NewScheduler(ctx context.Context, pool *Pool, workers, queue int, generate Generator, logger *slog.Logger) *Scheduler {
	if ctx == nil {
		ctx = context.Background()
	}
	if workers <= 0 {
		workers = 2
	}
	if queue <= 0 {
		queue = 256
	}
	s := &Scheduler{
		ctx:      ctx,
		pool:     pool,
		generate: generate,
		logger:   logger,
		workers:  workers,
		queue:    make(chan scheduleTask, queue),
		inflight: make(map[Spec]struct{}),
	}
	s.wg.Add(workers)
	for range workers {
		go s.work()
	}
	return s
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
		select {
		case <-s.ctx.Done():
			return
		case task := <-s.queue:
			s.fill(task)
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
	for _, slot := range missing {
		if err := s.generate.Generate(s.ctx, task.spec, slot); err != nil {
			s.logFailure(task.spec, slot, "generate cover", err)
			return
		}
	}
}

func (s *Scheduler) logFailure(spec Spec, slot int, operation string, err error) {
	if s.logger != nil {
		s.logger.Warn("coverpool background task stopped", "operation", operation, "text", spec.Text, "slot", slot, "err", err)
	}
}
