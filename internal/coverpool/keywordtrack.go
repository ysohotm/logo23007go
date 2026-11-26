package coverpool

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// KeywordSink persists cover keywords (e.g. db.CoverKeywordStore).
type KeywordSink interface {
	UpsertMany(ctx context.Context, texts []string) error
}

// KeywordTracker asynchronously upserts newly seen keywords with in-memory dedupe.
type KeywordTracker struct {
	sink   KeywordSink
	logger *slog.Logger
	cap    int

	mu   sync.Mutex
	seen map[string]struct{}
}

// NewKeywordTracker creates a tracker with a 100_000 distinct-key cap.
func NewKeywordTracker(sink KeywordSink, logger *slog.Logger) *KeywordTracker {
	return NewKeywordTrackerWithCap(sink, logger, 100000)
}

// NewKeywordTrackerWithCap creates a tracker with a custom distinct-key cap.
func NewKeywordTrackerWithCap(sink KeywordSink, logger *slog.Logger, cap int) *KeywordTracker {
	if cap < 0 {
		cap = 0
	}
	return &KeywordTracker{
		sink:   sink,
		logger: logger,
		cap:    cap,
		seen:   make(map[string]struct{}),
	}
}

// Remember schedules an upsert for trimmed non-empty text if not recently seen.
// Nil-safe, non-blocking (goroutine). Dedupes in-memory so repeat traffic does not hit Mongo every time.
func (t *KeywordTracker) Remember(text string) {
	if t == nil || t.sink == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	t.mu.Lock()
	if _, ok := t.seen[text]; ok {
		t.mu.Unlock()
		return
	}
	if len(t.seen) >= t.cap {
		t.mu.Unlock()
		return
	}
	t.seen[text] = struct{}{}
	t.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := t.sink.UpsertMany(ctx, []string{text}); err != nil {
			t.mu.Lock()
			delete(t.seen, text)
			t.mu.Unlock()
			if t.logger != nil {
				t.logger.Warn("keyword upsert failed", "text", text, "err", err)
			}
		}
	}()
}
