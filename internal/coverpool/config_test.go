package coverpool

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeConfigStore struct {
	mu       sync.Mutex
	settings Settings
	getErr   error
	setErr   error
	gets     int
	sets     int
	getCh    chan struct{}
}

func (s *fakeConfigStore) Get(context.Context) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getCh != nil {
		select {
		case s.getCh <- struct{}{}:
		default:
		}
	}
	out := Settings{Count: s.settings.Count, Sizes: append([][2]int(nil), s.settings.Sizes...)}
	return out, s.getErr
}

func (s *fakeConfigStore) Set(_ context.Context, settings Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	if s.setErr != nil {
		return s.setErr
	}
	s.settings = Settings{Count: settings.Count, Sizes: append([][2]int(nil), settings.Sizes...)}
	return nil
}

func (s *fakeConfigStore) counts() (gets, sets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.sets
}

func (s *fakeConfigStore) setSettings(settings Settings) {
	s.mu.Lock()
	s.settings = settings
	s.mu.Unlock()
}

func TestConfigDefaults(t *testing.T) {
	var nilConfig *Config
	if got := nilConfig.Count(); got != 10 {
		t.Fatalf("nil Config.Count() = %d, want 10", got)
	}
	if !reflect.DeepEqual(nilConfig.Sizes(), DefaultSizes()) {
		t.Fatalf("nil Config.Sizes() = %v", nilConfig.Sizes())
	}
	if !nilConfig.Allows(600, 800) || nilConfig.Allows(240, 300) {
		t.Fatal("nil config should only allow default 600x800")
	}

	cfg := NewConfig(nil, nil)
	if got := cfg.Count(); got != 10 {
		t.Fatalf("Count() = %d before refresh, want default 10", got)
	}
	if !reflect.DeepEqual(cfg.Sizes(), DefaultSizes()) {
		t.Fatalf("Sizes() = %v, want default", cfg.Sizes())
	}
}

func TestConfigLoadsPersistedSettingsAtStartup(t *testing.T) {
	store := &fakeConfigStore{settings: Settings{Count: 20, Sizes: [][2]int{{300, 400}, {240, 300}}}}
	cfg := NewConfig(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.Load(ctx)
	if got := cfg.Count(); got != 20 {
		t.Fatalf("Count() = %d, want persisted 20", got)
	}
	if !cfg.Allows(300, 400) || !cfg.Allows(240, 300) || cfg.Allows(600, 800) {
		t.Fatalf("Allows mismatch: sizes=%v", cfg.Sizes())
	}
}

func TestConfigSet(t *testing.T) {
	ctx := context.Background()
	store := &fakeConfigStore{settings: DefaultSettings()}
	cfg := NewConfig(store, nil)

	want := Settings{Count: 42, Sizes: [][2]int{{600, 800}, {300, 400}}}
	if err := cfg.Set(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Snapshot(); !reflect.DeepEqual(got.Sizes, want.Sizes) || got.Count != 42 {
		t.Fatalf("Snapshot() = %+v", got)
	}
	if !reflect.DeepEqual(store.settings.Sizes, want.Sizes) || store.settings.Count != 42 {
		t.Fatalf("stored = %+v", store.settings)
	}

	if err := cfg.Set(ctx, Settings{Count: 0, Sizes: DefaultSizes()}); err == nil {
		t.Fatal("invalid count accepted")
	}
	if err := cfg.Set(ctx, Settings{Count: 10, Sizes: [][2]int{{0, 100}}}); err == nil {
		t.Fatal("invalid size accepted")
	}
	// Empty sizes fall back to default.
	if err := cfg.Set(ctx, Settings{Count: 8, Sizes: nil}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Sizes(), DefaultSizes()) || cfg.Count() != 8 {
		t.Fatalf("empty sizes should default: %+v", cfg.Snapshot())
	}

	store.setErr = errors.New("write failed")
	if err := cfg.Set(ctx, Settings{Count: 50, Sizes: DefaultSizes()}); err == nil {
		t.Fatal("Set returned nil after store failure")
	}
	if cfg.Count() != 8 {
		t.Fatalf("failed Set changed Count() to %d", cfg.Count())
	}

	withoutStore := NewConfig(nil, nil)
	if err := withoutStore.Set(ctx, Settings{Count: 7, Sizes: [][2]int{{240, 300}}}); err != nil {
		t.Fatal(err)
	}
	if !withoutStore.Allows(240, 300) || withoutStore.Count() != 7 {
		t.Fatalf("nil store Set failed: %+v", withoutStore.Snapshot())
	}
	var nilConfig *Config
	if err := nilConfig.Set(ctx, DefaultSettings()); err == nil {
		t.Fatal("nil Config.Set returned nil")
	}
}

func TestParseAndFormatSizes(t *testing.T) {
	got, err := ParseSizes(" 600x800, 300X400 ")
	if err != nil || !reflect.DeepEqual(got, [][2]int{{600, 800}, {300, 400}}) {
		t.Fatalf("ParseSizes = %v, %v", got, err)
	}
	if FormatSizes(got) != "600x800,300x400" {
		t.Fatalf("FormatSizes = %q", FormatSizes(got))
	}
	if _, err := ParseSizes("bad"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestConfigConcurrentReadWrite(t *testing.T) {
	cfg := NewConfig(&fakeConfigStore{settings: DefaultSettings()}, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				count := (worker+i)%100 + 1
				size := [2]int{100 + worker, 200 + i%10}
				if err := cfg.Set(ctx, Settings{Count: count, Sizes: [][2]int{{600, 800}, size}}); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
				_ = cfg.Allows(600, 800)
				_ = cfg.Count()
			}
		}(worker)
	}
	wg.Wait()
}

func TestConfigRunRefreshesAfterInterval(t *testing.T) {
	store := &fakeConfigStore{
		settings: Settings{Count: 25, Sizes: [][2]int{{300, 400}}},
		getCh:    make(chan struct{}, 16),
	}
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
	if !cfg.Allows(300, 400) {
		t.Fatal("expected 300x400 after refresh")
	}

	store.setSettings(Settings{Count: 60, Sizes: [][2]int{{240, 300}}})
	waitConfigCount(t, cfg, 60)
	if !cfg.Allows(240, 300) || cfg.Allows(300, 400) {
		t.Fatalf("sizes not refreshed: %v", cfg.Sizes())
	}

	store.mu.Lock()
	store.getErr = errors.New("read failed")
	store.settings = Settings{Count: 90, Sizes: DefaultSizes()}
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
