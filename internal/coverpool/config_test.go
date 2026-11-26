package coverpool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeConfigStore struct {
	mu     sync.Mutex
	value  int
	getErr error
	setErr error
	gets   int
	sets   int
	getCh  chan struct{}
}

func (s *fakeConfigStore) Get(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getCh != nil {
		select {
		case s.getCh <- struct{}{}:
		default:
		}
	}
	return s.value, s.getErr
}

func (s *fakeConfigStore) Set(_ context.Context, count int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	if s.setErr != nil {
		return s.setErr
	}
	s.value = count
	return nil
}

func (s *fakeConfigStore) counts() (gets, sets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.sets
}

func (s *fakeConfigStore) setValue(value int) {
	s.mu.Lock()
	s.value = value
	s.mu.Unlock()
}

func TestConfigDefaults(t *testing.T) {
	var nilConfig *Config
	if got := nilConfig.Count(); got != 10 {
		t.Fatalf("nil Config.Count() = %d, want 10", got)
	}
	if got := (&Config{}).Count(); got != 10 {
		t.Fatalf("zero Config.Count() = %d, want 10", got)
	}

	store := &fakeConfigStore{value: 25}
	cfg := NewConfig(store, nil)
	if got := cfg.Count(); got != 10 {
		t.Fatalf("Count() = %d before refresh, want default 10", got)
	}
	if gets, _ := store.counts(); gets != 0 {
		t.Fatalf("NewConfig read store %d times, want 0", gets)
	}
}

func TestConfigLoadsPersistedCountAtStartup(t *testing.T) {
	store := &fakeConfigStore{value: 20}
	cfg := NewConfig(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.Load(ctx)
	if got := cfg.Count(); got != 20 {
		t.Fatalf("Count() = %d, want persisted 20", got)
	}
}

func TestConfigSet(t *testing.T) {
	ctx := context.Background()
	store := &fakeConfigStore{value: 10}
	cfg := NewConfig(store, nil)

	for _, count := range []int{1, 100, 42} {
		if err := cfg.Set(ctx, count); err != nil {
			t.Fatalf("Set(%d) returned error: %v", count, err)
		}
		if got := cfg.Count(); got != count {
			t.Fatalf("after Set(%d), Count() = %d", count, got)
		}
		if got := store.value; got != count {
			t.Fatalf("after Set(%d), stored value = %d", count, got)
		}
	}

	for _, count := range []int{-1, 0, 101} {
		if err := cfg.Set(ctx, count); err == nil {
			t.Fatalf("Set(%d) succeeded, want validation error", count)
		}
	}
	if _, sets := store.counts(); sets != 3 {
		t.Fatalf("invalid values reached store: total sets = %d, want 3", sets)
	}
	if got := cfg.Count(); got != 42 {
		t.Fatalf("invalid Set changed Count() to %d, want 42", got)
	}

	store.setErr = errors.New("write failed")
	if err := cfg.Set(ctx, 50); err == nil {
		t.Fatal("Set returned nil after store failure")
	}
	if got := cfg.Count(); got != 42 {
		t.Fatalf("failed Set changed Count() to %d, want 42", got)
	}

	withoutStore := NewConfig(nil, nil)
	if err := withoutStore.Set(ctx, 7); err != nil {
		t.Fatalf("Set with nil store returned error: %v", err)
	}
	if got := withoutStore.Count(); got != 7 {
		t.Fatalf("Set with nil store: Count() = %d, want 7", got)
	}
	var nilConfig *Config
	if err := nilConfig.Set(ctx, 7); err == nil {
		t.Fatal("nil Config.Set returned nil, want error")
	}
}

func TestConfigConcurrentReadWrite(t *testing.T) {
	cfg := NewConfig(&fakeConfigStore{value: 10}, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				count := (worker+i)%100 + 1
				if err := cfg.Set(ctx, count); err != nil {
					t.Errorf("Set(%d): %v", count, err)
					return
				}
				if got := cfg.Count(); got < 1 || got > 100 {
					t.Errorf("Count() = %d, want 1..100", got)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestConfigRunRefreshesAfterInterval(t *testing.T) {
	store := &fakeConfigStore{value: 25, getCh: make(chan struct{}, 16)}
	cfg := NewConfig(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cfg.run(ctx, time.Millisecond)
	}()

	select {
	case <-store.getCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first scheduled refresh")
	}
	waitConfigCount(t, cfg, 25)

	store.setValue(60)
	waitConfigCount(t, cfg, 60)

	store.mu.Lock()
	store.getErr = errors.New("read failed")
	store.value = 90
	store.mu.Unlock()
	getsBefore, _ := store.counts()
	deadline := time.Now().Add(2 * time.Second)
	for {
		gets, _ := store.counts()
		if gets > getsBefore {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for failed refresh")
		}
		time.Sleep(time.Millisecond)
	}
	if got := cfg.Count(); got != 60 {
		t.Fatalf("failed refresh changed Count() to %d, want 60", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Config.run did not stop after cancellation")
	}
}

func TestConfigRunNilSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewConfig(nil, nil).run(ctx, time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("nil-store run did not stop after cancellation")
	}

	var cfg *Config
	cfg.Run(ctx)
}

func waitConfigCount(t *testing.T, cfg *Config, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := cfg.Count(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("Count() = %d, want %d", cfg.Count(), want))
		}
		time.Sleep(time.Millisecond)
	}
}
