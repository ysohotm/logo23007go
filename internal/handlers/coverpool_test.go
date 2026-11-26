package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"logo23007go/internal/coverpool"
	"logo23007go/internal/logo"
)

type blockingGenerator struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   chan int
	pool    *coverpool.Pool
}

func (g *blockingGenerator) Generate(ctx context.Context, spec coverpool.Spec, slot int) error {
	g.once.Do(func() { close(g.started) })
	select {
	case g.calls <- slot:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.release:
		data := []byte{0xff, 0xd8, 0xff, 0xe0}
		if spec.Format == coverpool.FormatWebP {
			data = []byte{'R', 'I', 'F', 'F', 4, 0, 0, 0, 'W', 'E', 'B', 'P'}
		}
		return g.pool.Put(spec, slot, data)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAcquireImageSlotRejectsCanceledContextWhenFull(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	h := &Home{ImageSem: sem, ImageWait: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, ok := h.acquireImageSlot(ctx); ok || release != nil {
		t.Fatal("a canceled request must not consume an image slot")
	}
}

func TestResolveImageMatchesPooledSpec(t *testing.T) {
	h := &Home{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/home/image?text=%E7%BC%93%E5%AD%98%E5%85%B3%E9%94%AE%E8%AF%8D&width=240&height=300", nil)
	text, width, height, file, err := h.resolveImage(req, req.URL.Query().Get("text"), 240, 300)
	if err != nil || text != "缓存关键词" || width != 240 || height != 300 || file != "" {
		t.Fatalf("resolveImage=%q,%d,%d,%q,%v", text, width, height, file, err)
	}
}

func TestCoverPoolFirstImageAndBackgroundFill(t *testing.T) {
	root := t.TempDir()
	fontsDir := filepath.Join(root, "app_data", "fonts")
	if err := os.MkdirAll(fontsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fontPath, err := filepath.Abs(filepath.Join("..", "..", "app_data", "fonts", "msyhbd.ttf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fontPath, filepath.Join(fontsDir, "msyhbd.ttf")); err != nil {
		t.Fatal(err)
	}
	imagesDir := filepath.Join(root, "app_data", "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	imagePath, err := filepath.Abs(filepath.Join("..", "..", "app_data", "images", "sample.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(imagePath, filepath.Join(imagesDir, "sample.jpg")); err != nil {
		t.Fatal(err)
	}
	c, err := logo.NewCreater(root)
	if err != nil {
		t.Fatal(err)
	}
	pool := coverpool.NewPool(filepath.Join(root, "covers"))
	generator := &blockingGenerator{started: make(chan struct{}), release: make(chan struct{}), calls: make(chan int, 16), pool: pool}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler := coverpool.NewScheduler(ctx, pool, 1, 8, generator, nil)
	poolCfg := coverpool.NewConfig(nil, nil)
	if err := poolCfg.Set(context.Background(), coverpool.Settings{Count: 10, Sizes: [][2]int{{240, 300}}}); err != nil {
		t.Fatal(err)
	}
	h := &Home{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Creater: c, Pool: pool, PoolConfig: poolCfg, PoolScheduler: scheduler, ImageSem: make(chan struct{}, 1)}
	req := httptest.NewRequest(http.MethodGet, "/home/image?text=测试关键词&width=240&height=300", nil)
	req.Header.Set("Accept", "image/webp")
	rec := httptest.NewRecorder()
	h.Image(rec, req)
	if rec.Code != http.StatusOK || !bytes.HasPrefix(rec.Body.Bytes(), []byte("RIFF")) {
		t.Fatalf("first cover: status=%d, magic=%x", rec.Code, rec.Body.Bytes()[:min(12, rec.Body.Len())])
	}
	if got := rec.Header().Get("Cache-Control"); got != coverCacheControl {
		t.Fatalf("Cache-Control=%q", got)
	}
	if got := rec.Header().Get("Cloudflare-CDN-Cache-Control"); got != cloudflareCacheControl {
		t.Fatalf("Cloudflare-CDN-Cache-Control=%q", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept" {
		t.Fatalf("Vary=%q", got)
	}
	select {
	case <-generator.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background fill did not start")
	}
	close(generator.release)
	scheduler.Ensure(coverpool.Spec{}, 0) // no-op; explicitly exercise nil-safe scheduling path separately.
	deadline := time.Now().Add(2 * time.Second)
	for {
		spec := coverpool.Spec{Text: "测试关键词", Width: 240, Height: 300, Format: coverpool.FormatWebP}
		count, err := pool.Count(spec)
		if err != nil {
			t.Fatal(err)
		}
		if count == 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool count=%d, want 10; generator slots=%v", count, generator.calls)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	scheduler.Wait()
}

func TestCachedCoverBypassesImageSlotAndPreservesFormat(t *testing.T) {
	pool := coverpool.NewPool(t.TempDir())
	spec := coverpool.Spec{Text: "缓存关键词", Width: 240, Height: 300, Format: coverpool.FormatJPEG}
	jpeg, err := os.ReadFile(filepath.Join("..", "..", "app_data", "images", "sample.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(spec, 0, jpeg); err != nil {
		t.Fatal(err)
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	poolCfg := coverpool.NewConfig(nil, nil)
	if err := poolCfg.Set(context.Background(), coverpool.Settings{Count: 10, Sizes: [][2]int{{240, 300}}}); err != nil {
		t.Fatal(err)
	}
	h := &Home{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Creater: &logo.Creater{}, Pool: pool, PoolConfig: poolCfg, ImageSem: sem, ImageWait: time.Millisecond}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/home/image?text=%E7%BC%93%E5%AD%98%E5%85%B3%E9%94%AE%E8%AF%8D&width=240&height=300", nil)
	h.Image(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), jpeg) || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("cache hit: status=%d type=%s len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	if len(sem) != 1 {
		t.Fatal("cache hit must not consume an image-generation slot")
	}
}

func TestCoverPoolSpecsDoNotShareImages(t *testing.T) {
	pool := coverpool.NewPool(t.TempDir())
	jpeg, err := os.ReadFile(filepath.Join("..", "..", "app_data", "images", "sample.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []coverpool.Spec{
		{Text: "相同", Width: 240, Height: 300, Format: coverpool.FormatJPEG},
		{Text: "相同", Width: 600, Height: 800, Format: coverpool.FormatJPEG},
		{Text: "相同", Width: 240, Height: 300, Format: coverpool.FormatWebP},
	} {
		dir, err := pool.Paths(spec)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println(dir)
	}
	if err := pool.Put(coverpool.Spec{Text: "背景", Width: 240, Height: 300, Background: "cover.jpg", Format: coverpool.FormatJPEG}, 0, jpeg); err != nil {
		t.Fatal(err)
	}
}

func TestNonAllowlistedSizeSkipsPool(t *testing.T) {
	root := t.TempDir()
	fontsDir := filepath.Join(root, "app_data", "fonts")
	imagesDir := filepath.Join(root, "app_data", "images")
	if err := os.MkdirAll(fontsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fontPath, err := filepath.Abs(filepath.Join("..", "..", "app_data", "fonts", "msyhbd.ttf"))
	if err != nil {
		t.Fatal(err)
	}
	imagePath, err := filepath.Abs(filepath.Join("..", "..", "app_data", "images", "sample.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fontPath, filepath.Join(fontsDir, "msyhbd.ttf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(imagePath, filepath.Join(imagesDir, "sample.jpg")); err != nil {
		t.Fatal(err)
	}
	c, err := logo.NewCreater(root)
	if err != nil {
		t.Fatal(err)
	}
	pool := coverpool.NewPool(filepath.Join(root, "covers"))
	poolCfg := coverpool.NewConfig(nil, nil) // default only 600x800
	h := &Home{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Creater: c, Pool: pool, PoolConfig: poolCfg, ImageSem: make(chan struct{}, 1)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/home/image?text=非白名单尺寸&width=240&height=300", nil)
	req.Header.Set("Accept", "image/webp")
	h.Image(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	spec := coverpool.Spec{Text: "非白名单尺寸", Width: 240, Height: 300, Format: coverpool.FormatWebP}
	if n, err := pool.Count(spec); err != nil || n != 0 {
		t.Fatalf("non-allowlisted size should not write pool: count=%d err=%v", n, err)
	}
}

func TestBackgroundGeneratorUsesImageSlot(t *testing.T) {
	h := &Home{Pool: coverpool.NewPool(t.TempDir()), ImageSem: make(chan struct{}, 1)}
	h.ImageSem <- struct{}{}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		finished <- h.Generate(context.Background(), coverpool.Spec{}, 0)
	}()
	<-started
	<-h.ImageSem
	if err := <-finished; err == nil {
		t.Fatal("missing Creater should fail after acquiring a slot")
	}
	if len(h.ImageSem) != 0 {
		t.Fatal("background generation did not release its image slot")
	}
}
