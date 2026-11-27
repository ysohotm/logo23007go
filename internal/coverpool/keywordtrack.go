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

// KeywordTracker asynchronously upserts newly seen keywords with in-memory dedupe
// and a bounded worker queue (avoids goroutine storms under high traffic).
type KeywordTracker struct {
	sink   KeywordSink
	logger *slog.Logger
	cap    int

	mu   sync.Mutex
	seen map[string]struct{}

	queue chan string
	once  sync.Once
}

const keywordTrackWorkers = 2
const keywordTrackQueue = 4096

// NewKeywordTracker creates a tracker with a 100_000 distinct-key cap.
func NewKeywordTracker(sink KeywordSink, logger *slog.Logger) *KeywordTracker {
	return NewKeywordTrackerWithCap(sink, logger, 100000)
}

// NewKeywordTrackerWithCap creates a tracker with a custom distinct-key cap.
func NewKeywordTrackerWithCap(sink KeywordSink, logger *slog.Logger, cap int) *KeywordTracker {
	if cap < 0 {
		cap = 0
	}
	t := &KeywordTracker{
		sink:   sink,
		logger: logger,
		cap:    cap,
		seen:   make(map[string]struct{}),
		queue:  make(chan string, keywordTrackQueue),
	}
	return t
}

func (t *KeywordTracker) startWorkers() {
	t.once.Do(func() {
		for range keywordTrackWorkers {
			go t.worker()
		}
	})
}

func (t *KeywordTracker) worker() {
	for text := range t.queue {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := t.sink.UpsertMany(ctx, []string{text})
		cancel()
		if err != nil {
			t.mu.Lock()
			delete(t.seen, text)
			t.mu.Unlock()
			if t.logger != nil {
				t.logger.Warn("keyword upsert failed", "text", text, "err", err)
			}
		}
	}
}

// Remember schedules an upsert for trimmed non-empty text if not recently seen.
// Nil-safe, non-blocking. Dedupes in-memory; drops when queue is full.
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

	t.startWorkers()
	select {
	case t.queue <- text:
	default:
		// queue full: allow retry later
		t.mu.Lock()
		delete(t.seen, text)
		t.mu.Unlock()
	}
}
