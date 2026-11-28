package coverpool

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Rotator periodically deletes the oldest images from full pools and re-enqueues fills.
// Discovery uses Mongo EntryRegistry (not meta.json / disk walk).
type Rotator struct {
	Pool      *Pool
	Config    *Config
	Scheduler *Scheduler
	Logger    *slog.Logger
	Every     time.Duration // 0 → 1h
	Limit     int           // 0 → 50 max pools rotated per tick
}

// RotateResult summarizes one Tick pass.
type RotateResult struct {
	Scanned  int      `json:"scanned"`
	Rotated  int      `json:"rotated"`
	Skipped  int      `json:"skipped"`
	Enqueued int      `json:"enqueued"`
	Failed   int      `json:"failed"`
	Errors   []string `json:"errors,omitempty"` // capped at 20
}

// Run ticks on an interval until ctx is canceled. Every<=0 defaults to 1h.
func (r *Rotator) Run(ctx context.Context) {
	if r == nil {
		return
	}
	every := r.Every
	if every <= 0 {
		every = time.Hour
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = r.Tick(ctx)
		}
	}
}

// Tick performs one rotate pass using Mongo inventory. Also used by admin.
// When the scheduler queue is more than half full, Tick returns early with zeros
// to avoid piling more Ensure work onto a busy queue.
func (r *Rotator) Tick(ctx context.Context) RotateResult {
	var result RotateResult
	if r == nil || r.Config == nil || r.Pool == nil {
		return result
	}
	days := r.Config.RotateEveryDays()
	m := r.Config.RotateCount()
	if days <= 0 || m <= 0 {
		return result
	}
	reg := r.Pool.Registry()
	if reg == nil {
		if r.Logger != nil {
			r.Logger.Warn("coverpool rotate skipped: no registry")
		}
		return result
	}
	if r.Scheduler != nil {
		prog := r.Scheduler.Progress()
		if prog.QueueCap > 0 && prog.QueueLen > prog.QueueCap/2 {
			return result
		}
	}
	limit := r.Limit
	if limit <= 0 {
		limit = 50
	}
	target := r.Config.Count()
	olderThan := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := reg.ListDueRotate(ctx, target, olderThan, limit)
	if err != nil {
		result.Failed++
		result.Errors = append(result.Errors, err.Error())
		if r.Logger != nil {
			r.Logger.Warn("coverpool rotate list failed", "err", err)
		}
		return result
	}
	result.Scanned = len(entries)

	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		spec := Spec{
			Text:       e.Text,
			Width:      e.Width,
			Height:     e.Height,
			Background: e.Background,
			Format:     e.Format,
		}
		if spec.Format == "" {
			spec.Format = FormatWebP
		}
		if _, err := r.Pool.RotateOldest(spec, m); err != nil {
			result.Failed++
			if len(result.Errors) < 20 {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", e.ID, err))
			}
			if r.Logger != nil {
				r.Logger.Warn("coverpool rotate failed", "text", spec.Text, "err", err)
			}
			continue
		}
		result.Rotated++
		if r.Scheduler != nil && r.Scheduler.Ensure(spec, target) {
			result.Enqueued++
		}
	}
	return result
}
