package logo

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// newBgCacheCreater builds a Creater over a temp root holding only cover.jpg,
// with the bg cache enabled for 300x400 and 150x210.
func newBgCacheCreater(t *testing.T) (c *Creater, root, cacheDir string) {
	t.Helper()
	repoRoot := findRoot(t)
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app_data", "fonts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "app_data", "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repoRoot, "app_data", "fonts", "msyhbd.ttf"), filepath.Join(root, "app_data", "fonts", "msyhbd.ttf")); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(repoRoot, "app_data", "images", "cover.jpg"), filepath.Join(root, "app_data", "images", "cover.jpg"))

	c, err := NewCreater(root)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir = filepath.Join(root, "bgcache")
	if err := c.EnableBgCache(BgCacheConfig{Dir: cacheDir, Sizes: [][2]int{{300, 400}, {150, 210}}}); err != nil {
		t.Fatal(err)
	}
	return c, root, cacheDir
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustModTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func assertWebP(t *testing.T, r *FileCreateResult) {
	t.Helper()
	if !r.Success || r.Format != FormatWebP || !bytes.HasPrefix(r.Data, []byte("RIFF")) || !bytes.Contains(r.Data[:min(16, len(r.Data))], []byte("WEBP")) {
		t.Fatalf("expected WebP, got success=%v format=%s magic=%x", r.Success, r.Format, r.Data[:min(16, len(r.Data))])
	}
}

func assertValidJPEG(t *testing.T, path string) image.Config {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatalf("cache file %s is not a valid JPEG: %v", path, err)
	}
	return cfg
}

func TestParseSizes(t *testing.T) {
	got, err := ParseSizes(" 300x400, 600X800 ,150 x 210")
	if err != nil {
		t.Fatal(err)
	}
	if want := [][2]int{{300, 400}, {600, 800}, {150, 210}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, blank := range []string{"", "   "} {
		if got, err := ParseSizes(blank); err != nil || got != nil {
			t.Fatalf("blank %q: got %v, %v", blank, got, err)
		}
	}
	for _, bad := range []string{"300", "300x", "x400", "0x400", "300x-1", "axb", "300x400,", "300x400x2"} {
		if _, err := ParseSizes(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestBgCacheHitDoesNotRewrite(t *testing.T) {
	c, _, cacheDir := newBgCacheCreater(t)
	cached := filepath.Join(cacheDir, "300x400", "cover.jpg")

	r, err := c.CreateBytesImage("标题", 300, 400, "cover.jpg", FormatWebP)
	if err != nil {
		t.Fatal(err)
	}
	assertWebP(t, r)
	if cfg := assertValidJPEG(t, cached); cfg.Width > 300 || cfg.Height > 400 {
		t.Fatalf("cached image %dx%d exceeds 300x400", cfg.Width, cfg.Height)
	}
	mt := mustModTime(t, cached)

	r, err = c.CreateBytesImage("标题", 300, 400, "cover.jpg", FormatWebP)
	if err != nil {
		t.Fatal(err)
	}
	assertWebP(t, r)
	if got := mustModTime(t, cached); !got.Equal(mt) {
		t.Fatalf("cache file rewritten on hit: %v -> %v", mt, got)
	}
}

func TestBgCacheSkipsNonAllowlistedSize(t *testing.T) {
	c, _, cacheDir := newBgCacheCreater(t)
	if _, err := c.CreateBytesImage("标题", 123, 456, "cover.jpg", FormatWebP); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "123x456")); !os.IsNotExist(err) {
		t.Fatalf("expected no cache dir for non-allowlisted size, stat err=%v", err)
	}
}

func TestBgCacheDisabled(t *testing.T) {
	c, _, cacheDir := newBgCacheCreater(t)
	if err := c.EnableBgCache(BgCacheConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateBytesImage("标题", 300, 400, "cover.jpg", FormatWebP); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "300x400")); !os.IsNotExist(err) {
		t.Fatalf("expected no cache files when disabled, stat err=%v", err)
	}
}

func TestBgCacheStaleRegenerates(t *testing.T) {
	c, root, cacheDir := newBgCacheCreater(t)
	src := filepath.Join(root, "app_data", "images", "cover.jpg")
	cached := filepath.Join(cacheDir, "300x400", "cover.jpg")

	if _, err := c.CreateBytesImage("标题", 300, 400, "cover.jpg", FormatWebP); err != nil {
		t.Fatal(err)
	}
	old := mustModTime(t, cached)
	newer := old.Add(2 * time.Second)
	if err := os.Chtimes(src, newer, newer); err != nil {
		t.Fatal(err)
	}

	r, err := c.CreateBytesImage("标题", 300, 400, "cover.jpg", FormatWebP)
	if err != nil {
		t.Fatal(err)
	}
	assertWebP(t, r)
	got := mustModTime(t, cached)
	if got.Equal(old) || got.Before(newer) {
		t.Fatalf("expected regenerated cache with mtime >= %v, got %v (old %v)", newer, got, old)
	}
	assertValidJPEG(t, cached)

	// Fresh again: no further rewrite.
	if _, err := c.CreateBytesImage("标题", 300, 400, "cover.jpg", FormatWebP); err != nil {
		t.Fatal(err)
	}
	if again := mustModTime(t, cached); !again.Equal(got) {
		t.Fatalf("cache rewritten after regeneration: %v -> %v", got, again)
	}
}

func TestBgCacheCorruptFileRegenerates(t *testing.T) {
	c, _, cacheDir := newBgCacheCreater(t)
	cached := filepath.Join(cacheDir, "300x400", "cover.jpg")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("not a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(cached, future, future); err != nil {
		t.Fatal(err)
	}
	r, err := c.CreateBytesImage("标题", 300, 400, "cover.jpg", FormatWebP)
	if err != nil {
		t.Fatal(err)
	}
	assertWebP(t, r)
	assertValidJPEG(t, cached)
}

func pixPtr(t *testing.T, img image.Image) *uint8 {
	t.Helper()
	switch m := img.(type) {
	case *image.RGBA:
		return &m.Pix[0]
	case *image.NRGBA:
		return &m.Pix[0]
	case *image.YCbCr:
		return &m.Y[0]
	}
	t.Fatalf("unexpected image type %T", img)
	return nil
}

func TestBgCacheConcurrentSameKey(t *testing.T) {
	c, root, cacheDir := newBgCacheCreater(t)
	src := filepath.Join(root, "app_data", "images", "cover.jpg")
	const n = 16

	imgs := make([]image.Image, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			imgs[i], errs[i] = c.loadBackground(src, 300, 400)
		}()
	}
	wg.Wait()
	seen := make(map[*uint8]bool, n)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		p := pixPtr(t, imgs[i])
		if seen[p] {
			t.Fatalf("goroutine %d got a shared image instance", i)
		}
		seen[p] = true
	}

	var wg2 sync.WaitGroup
	results := make([]*FileCreateResult, n)
	for i := range n {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			results[i], errs[i] = c.CreateBytesImage("标题", 150, 210, "cover.jpg", FormatWebP)
		}()
	}
	wg2.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("CreateBytesImage %d: %v", i, errs[i])
		}
		assertWebP(t, results[i])
	}

	for _, size := range []string{"300x400", "150x210"} {
		entries, err := os.ReadDir(filepath.Join(cacheDir, size))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "cover.jpg" {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("%s: expected exactly [cover.jpg], got %v", size, names)
		}
	}
}

func TestWarmBgCache(t *testing.T) {
	c, _, cacheDir := newBgCacheCreater(t)
	sizes := [][2]int{{300, 400}, {150, 210}}

	var calls int
	st, err := c.WarmBgCache(context.Background(), sizes, 2, func(WarmStats) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if want := (WarmStats{Total: 2, Done: 2}); st != want {
		t.Fatalf("first run: got %+v want %+v", st, want)
	}
	if calls < 1 {
		t.Fatalf("progress not called")
	}
	assertValidJPEG(t, filepath.Join(cacheDir, "300x400", "cover.jpg"))
	assertValidJPEG(t, filepath.Join(cacheDir, "150x210", "cover.jpg"))

	st, err = c.WarmBgCache(context.Background(), sizes, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (WarmStats{Total: 2, Skipped: 2}); st != want {
		t.Fatalf("second run: got %+v want %+v", st, want)
	}

	// Sizes outside cfg.Sizes are still warmed.
	st, err = c.WarmBgCache(context.Background(), [][2]int{{64, 64}}, 1, nil)
	if err != nil || st.Done != 1 {
		t.Fatalf("non-allowlisted warm: %+v, %v", st, err)
	}
	assertValidJPEG(t, filepath.Join(cacheDir, "64x64", "cover.jpg"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.WarmBgCache(ctx, [][2]int{{32, 32}}, 1, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled warm: got err %v", err)
	}

	plain, err := NewCreater(findRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.WarmBgCache(context.Background(), sizes, 1, nil); err == nil {
		t.Fatalf("expected error when bg cache not enabled")
	}
}
