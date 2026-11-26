package logo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BgCacheConfig configures the on-disk cache of resized background images.
type BgCacheConfig struct {
	Dir     string   // cache root dir; "" => cache disabled
	Sizes   [][2]int // allowlisted {width,height}; only these sizes are cached
	Quality int      // JPEG quality of cached files; <=0 => 92
}

// WarmStats reports WarmBgCache progress.
type WarmStats struct{ Total, Done, Skipped, Failed int64 }

const defaultBgCacheQuality = 92

type bgCache struct {
	dir     string
	sizes   map[[2]int]struct{}
	quality int

	mu       sync.Mutex
	inflight map[string]*bgFill
}

type bgFill struct {
	done chan struct{}
	ok   bool // cache file was written successfully
	err  error
}

// EnableBgCache creates cfg.Dir and enables the background cache. Dir=="" disables it.
func (c *Creater) EnableBgCache(cfg BgCacheConfig) error {
	if cfg.Dir == "" {
		c.bg.Store(nil)
		return nil
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return fmt.Errorf("create bg cache dir: %w", err)
	}
	q := cfg.Quality
	if q <= 0 {
		q = defaultBgCacheQuality
	}
	sizes := make(map[[2]int]struct{}, len(cfg.Sizes))
	for _, s := range cfg.Sizes {
		sizes[s] = struct{}{}
	}
	c.bg.Store(&bgCache{
		dir:      cfg.Dir,
		sizes:    sizes,
		quality:  q,
		inflight: make(map[string]*bgFill),
	})
	return nil
}

// ParseSizes parses "300x400, 600x800" into [][2]int{{300,400},{600,800}}.
func ParseSizes(s string) ([][2]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out [][2]int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		ws, hs, ok := strings.Cut(strings.ToLower(part), "x")
		if !ok {
			return nil, fmt.Errorf("invalid size %q: want WIDTHxHEIGHT", part)
		}
		w, err1 := strconv.Atoi(strings.TrimSpace(ws))
		h, err2 := strconv.Atoi(strings.TrimSpace(hs))
		if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
			return nil, fmt.Errorf("invalid size %q: want positive WIDTHxHEIGHT", part)
		}
		out = append(out, [2]int{w, h})
	}
	return out, nil
}

func (b *bgCache) path(src string, w, h int) string {
	base := filepath.Base(src)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return filepath.Join(b.dir, strconv.Itoa(w)+"x"+strconv.Itoa(h), base+".jpg")
}

// loadBackground is LoadImage backed by the disk cache for allowlisted sizes.
// Every call returns a freshly allocated image that the caller may draw on.
func (c *Creater) loadBackground(src string, w, h int) (image.Image, error) {
	bc := c.bg.Load()
	if bc == nil || w <= 0 || h <= 0 {
		return c.LoadImage(src, w, h)
	}
	if _, ok := bc.sizes[[2]int{w, h}]; !ok {
		return c.LoadImage(src, w, h)
	}
	img, _, err := c.fillBg(bc, src, w, h, true)
	return img, err
}

// fillBg makes sure the cache file for (src,w,h) is fresh. With want=true it
// returns an image owned by the caller and ignores cache write failures; with
// want=false it returns no image and reports write failures as errors.
// generated is true when this call produced a new cache file.
func (c *Creater) fillBg(bc *bgCache, src string, w, h int, want bool) (img image.Image, generated bool, err error) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return nil, false, fmt.Errorf("图像文件未找到: %s", src)
	}
	srcMod := srcInfo.ModTime()
	path := bc.path(src, w, h)

	if fi, err := os.Stat(path); err == nil && !fi.ModTime().Before(srcMod) {
		if !want {
			return nil, false, nil
		}
		if img, err := decodeCachedJPEG(path); err == nil {
			return img, false, nil
		}
	}

	bc.mu.Lock()
	if f, ok := bc.inflight[path]; ok {
		bc.mu.Unlock()
		<-f.done
		if !want {
			if f.ok {
				return nil, false, nil
			}
			if f.err == nil {
				return nil, false, errors.New("bg cache fill failed")
			}
			return nil, false, f.err
		}
		if f.ok {
			if img, err := decodeCachedJPEG(path); err == nil {
				return img, false, nil
			}
		}
		img, err := c.LoadImage(src, w, h)
		return img, false, err
	}
	f := &bgFill{done: make(chan struct{})}
	bc.inflight[path] = f
	bc.mu.Unlock()
	defer func() {
		bc.mu.Lock()
		delete(bc.inflight, path)
		bc.mu.Unlock()
		close(f.done)
	}()

	img, err = c.LoadImage(src, w, h)
	if err != nil {
		f.err = err
		return nil, false, err
	}
	if werr := writeCachedJPEG(path, img, bc.quality, srcMod); werr != nil {
		f.err = werr
		if !want {
			return nil, false, werr
		}
		return img, false, nil
	}
	f.ok = true
	if !want {
		return nil, true, nil
	}
	return img, true, nil
}

func decodeCachedJPEG(path string) (image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return jpeg.Decode(bytes.NewReader(data))
}

// writeCachedJPEG atomically writes img to path. The file mtime is never older
// than srcMod, otherwise a source with a future mtime would never be fresh.
func writeCachedJPEG(path string, img image.Image, quality int, srcMod time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.jpg")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	// image/jpeg only fast-paths *image.RGBA; imaging returns opaque NRGBA with the same layout.
	enc := img
	if n, isN := img.(*image.NRGBA); isN && n.Opaque() {
		enc = &image.RGBA{Pix: n.Pix, Stride: n.Stride, Rect: n.Rect}
	}
	bw := bufio.NewWriterSize(tmp, 64<<10)
	if err := jpeg.Encode(bw, enc, &jpeg.Options{Quality: quality}); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if srcMod.After(time.Now()) {
		if err := os.Chtimes(tmpName, srcMod, srcMod); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// WarmBgCache pre-generates cache files for every background image x size.
func (c *Creater) WarmBgCache(ctx context.Context, sizes [][2]int, workers int, progress func(WarmStats)) (WarmStats, error) {
	bc := c.bg.Load()
	if bc == nil {
		return WarmStats{}, errors.New("bg cache not enabled")
	}
	if workers <= 0 {
		workers = 1
	}
	files := c.GetImageFilesWithCache()
	total := int64(len(files) * len(sizes))

	var done, skipped, failed, processed atomic.Int64
	snapshot := func() WarmStats {
		return WarmStats{Total: total, Done: done.Load(), Skipped: skipped.Load(), Failed: failed.Load()}
	}
	var progressMu sync.Mutex
	report := func() {
		if progress == nil {
			return
		}
		progressMu.Lock()
		progress(snapshot())
		progressMu.Unlock()
	}

	type job struct {
		src  string
		w, h int
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if j.w <= 0 || j.h <= 0 {
					failed.Add(1)
				} else if _, generated, err := c.fillBg(bc, j.src, j.w, j.h, false); err != nil {
					failed.Add(1)
				} else if generated {
					done.Add(1)
				} else {
					skipped.Add(1)
				}
				if processed.Add(1)%500 == 0 {
					report()
				}
			}
		}()
	}

produce:
	for _, f := range files {
		for _, s := range sizes {
			select {
			case jobs <- job{src: f, w: s[0], h: s[1]}:
			case <-ctx.Done():
				break produce
			}
		}
	}
	close(jobs)
	wg.Wait()

	report()
	st := snapshot()
	if err := ctx.Err(); err != nil {
		return st, err
	}
	return st, nil
}
