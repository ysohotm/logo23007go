package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestImageBusyWhenSemFull(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	h := &Home{ImageSem: sem, ImageWait: 50 * time.Millisecond}

	cases := []struct {
		name    string
		target  string
		handler http.HandlerFunc
	}{
		{"Image", "/home/image?text=abc", h.Image},
		{"ImageKey", "/home/imagekey?id=x", h.ImageKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.target, nil)
			rec := httptest.NewRecorder()
			c.handler(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != "2" {
				t.Fatalf("Retry-After = %q, want 2", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if len(sem) != 1 {
				t.Fatalf("sem len = %d, want 1", len(sem))
			}
		})
	}
}

func TestAcquireImageSlotFree(t *testing.T) {
	h := &Home{ImageSem: make(chan struct{}, 1)}
	release, ok := h.acquireImageSlot(context.Background())
	if !ok {
		t.Fatal("expected slot")
	}
	if len(h.ImageSem) != 1 {
		t.Fatalf("sem len = %d, want 1", len(h.ImageSem))
	}
	release()
	release()
	if len(h.ImageSem) != 0 {
		t.Fatalf("sem len after release = %d, want 0", len(h.ImageSem))
	}
}

func TestAcquireImageSlotNilSem(t *testing.T) {
	h := &Home{}
	release, ok := h.acquireImageSlot(context.Background())
	if !ok || release == nil {
		t.Fatal("nil sem should be unlimited")
	}
	release()
}

func TestAcquireImageSlotCancelled(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	h := &Home{ImageSem: sem, ImageWait: 10 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	release, ok := h.acquireImageSlot(ctx)
	if ok || release != nil {
		t.Fatal("expected !ok with cancelled context")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v, want quick return", d)
	}
}
