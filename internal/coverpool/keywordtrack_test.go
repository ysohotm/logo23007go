package coverpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type countingSink struct {
	mu    sync.Mutex
	calls int
	texts []string
	err   error
	block chan struct{} // if non-nil, UpsertMany waits until closed
}

func (s *countingSink) UpsertMany(_ context.Context, texts []string) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.texts = append(s.texts, texts...)
	return s.err
}

func (s *countingSink) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func waitCalls(t *testing.T, s *countingSink, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.callCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("UpsertMany calls = %d, want >= %d", s.callCount(), want)
}

func TestKeywordTrackerNilSafe(t *testing.T) {
	var nilTracker *KeywordTracker
	nilTracker.Remember("x")

	noop := NewKeywordTracker(nil, nil)
	noop.Remember("x")
	noop.Remember("")
}

func TestKeywordTrackerRememberOnce(t *testing.T) {
	sink := &countingSink{}
	tr := NewKeywordTracker(sink, nil)

	tr.Remember("  hello  ")
	tr.Remember("hello")
	tr.Remember("hello")
	waitCalls(t, sink, 1)

	// Give a moment for any duplicate goroutine (should not fire).
	time.Sleep(50 * time.Millisecond)
	if got := sink.callCount(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.texts) != 1 || sink.texts[0] != "hello" {
		t.Fatalf("texts = %#v, want [hello]", sink.texts)
	}
}

func TestKeywordTrackerEmptyIgnored(t *testing.T) {
	sink := &countingSink{}
	tr := NewKeywordTracker(sink, nil)
	tr.Remember("")
	tr.Remember("   ")
	tr.Remember("\t\n")
	time.Sleep(50 * time.Millisecond)
	if got := sink.callCount(); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

func TestKeywordTrackerFailureRetries(t *testing.T) {
	sink := &countingSink{err: errors.New("mongo down")}
	tr := NewKeywordTracker(sink, nil)

	tr.Remember("retry-me")
	waitCalls(t, sink, 1)

	// Wait until seen is cleared after failure.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		_, ok := tr.seen["retry-me"]
		tr.mu.Unlock()
		if !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	sink.mu.Lock()
	sink.err = nil
	sink.mu.Unlock()

	tr.Remember("retry-me")
	waitCalls(t, sink, 2)
}

func TestKeywordTrackerCap(t *testing.T) {
	sink := &countingSink{block: make(chan struct{})}
	tr := NewKeywordTrackerWithCap(sink, nil, 2)

	tr.Remember("a")
	tr.Remember("b")
	tr.Remember("c") // over cap — skip

	// Unblock upserts so goroutines finish.
	close(sink.block)
	waitCalls(t, sink, 2)

	time.Sleep(30 * time.Millisecond)
	if got := sink.callCount(); got != 2 {
		t.Fatalf("calls = %d, want 2 (c should be skipped)", got)
	}

	// Existing keys still short-circuit (no extra call).
	tr.Remember("a")
	time.Sleep(30 * time.Millisecond)
	if got := sink.callCount(); got != 2 {
		t.Fatalf("after re-Remember(a) calls = %d, want 2", got)
	}
}

func TestKeywordTrackerConcurrentDedupe(t *testing.T) {
	sink := &countingSink{}
	tr := NewKeywordTracker(sink, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Remember("same")
		}()
	}
	wg.Wait()
	waitCalls(t, sink, 1)
	time.Sleep(30 * time.Millisecond)
	if got := sink.callCount(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}
