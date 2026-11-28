package coverpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const registryTimeout = 2 * time.Second

// textSizeKey builds the aggregate index key for text × width × height.
func textSizeKey(text string, width, height int) string {
	return text + "\x00" + strconv.Itoa(width) + "x" + strconv.Itoa(height)
}

// CountByTextSize sums valid image files for text×width×height across ALL
// backgrounds and formats. If the index was never built, the first call runs
// RebuildIndex via sync.Once (main should also RebuildIndex at startup).
func (p *Pool) CountByTextSize(text string, width, height int) int {
	if p == nil || p.root == "" {
		return 0
	}
	p.ensureIndex()
	p.indexMu.RLock()
	defer p.indexMu.RUnlock()
	if p.byTH == nil {
		return 0
	}
	return p.byTH[textSizeKey(text, width, height)]
}

func (p *Pool) ensureIndex() {
	p.indexOnce.Do(func() {
		p.indexMu.RLock()
		built := p.indexBuilt
		p.indexMu.RUnlock()
		if built {
			return
		}
		_ = p.RebuildIndexPreferRegistry(context.Background())
	})
}

// RebuildIndexPreferRegistry loads the in-memory index from Mongo when a
// registry is attached; only falls back to walking disk meta.json if that fails
// or no registry is set.
func (p *Pool) RebuildIndexPreferRegistry(ctx context.Context) error {
	if p != nil && p.Registry() != nil {
		if err := p.LoadIndexFromRegistry(ctx); err == nil {
			return nil
		}
	}
	return p.RebuildIndex()
}

// LoadIndexFromRegistry builds byTH/dirCount from EntryRegistry (no disk walk).
func (p *Pool) LoadIndexFromRegistry(ctx context.Context) error {
	if p == nil || p.root == "" {
		return errors.New("coverpool: pool is unavailable")
	}
	r := p.Registry()
	if r == nil {
		return errors.New("coverpool: registry unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	byTH := make(map[string]int)
	dirCount := make(map[string]int)
	n := 0
	err := r.EachEntry(ctx, func(e PoolEntry) error {
		key := textSizeKey(e.Text, e.Width, e.Height)
		byTH[key] += e.Count
		if e.Dir != "" {
			dirCount[e.Dir] = e.Count
		}
		n++
		return nil
	})
	if err != nil {
		return err
	}
	p.indexMu.Lock()
	p.byTH = byTH
	p.dirCount = dirCount
	p.indexBuilt = true
	p.indexMu.Unlock()
	_ = n
	return nil
}

// RebuildIndex walks root/**/meta.json, counts images per dir, and aggregates
// by text+width+height in memory only. Prefer LoadIndexFromRegistry / PreferRegistry
// at startup when Mongo inventory is already synced.
func (p *Pool) RebuildIndex() error {
	if p == nil || p.root == "" {
		return errors.New("coverpool: pool is unavailable")
	}
	byTH := make(map[string]int)
	dirCount := make(map[string]int)

	err := p.walkMeta(func(spec Spec, dir string, n int) error {
		key := textSizeKey(spec.Text, spec.Width, spec.Height)
		byTH[key] += n
		dirCount[dir] = n
		return nil
	})
	if err != nil {
		return err
	}

	p.indexMu.Lock()
	p.byTH = byTH
	p.dirCount = dirCount
	p.indexBuilt = true
	p.indexMu.Unlock()
	return nil
}

const registrySyncBatch = 200

// SyncRegistryFromDisk walks meta.json and batch-Upserts inventory metadata
// (not image bytes) into the registry. No-op if registry nil.
func (p *Pool) SyncRegistryFromDisk(ctx context.Context) (upserted int, err error) {
	if p == nil || p.root == "" {
		return 0, errors.New("coverpool: pool is unavailable")
	}
	r := p.Registry()
	if r == nil {
		return 0, nil
	}
	batch := make([]PoolEntry, 0, registrySyncBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := r.UpsertMany(ctx, batch); err != nil {
			return err
		}
		upserted += len(batch)
		batch = batch[:0]
		return nil
	}
	err = p.walkMeta(func(spec Spec, dir string, n int) error {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		batch = append(batch, PoolEntry{
			ID:         SpecID(spec),
			Text:       spec.Text,
			Width:      spec.Width,
			Height:     spec.Height,
			Background: spec.Background,
			Format:     spec.Format,
			Count:      n,
			Dir:        dir,
		})
		if len(batch) >= registrySyncBatch {
			return flush()
		}
		return nil
	})
	if err != nil {
		return upserted, err
	}
	return upserted, flush()
}

// CleanupNonEmptyBackgrounds deletes on-disk pools whose meta.background != ""
// (legacy per-request-background dirs) and removes matching registry docs.
// Image files under empty-background pools are kept on disk.
func (p *Pool) CleanupNonEmptyBackgrounds(ctx context.Context) (deleted, failed int, errs []string) {
	if p == nil || p.root == "" {
		return 0, 0, nil
	}
	_ = p.walkMeta(func(spec Spec, dir string, n int) error {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.TrimSpace(spec.Background) == "" {
			return nil
		}
		e := PoolEntry{
			ID:         SpecID(spec),
			Text:       spec.Text,
			Width:      spec.Width,
			Height:     spec.Height,
			Background: spec.Background,
			Format:     spec.Format,
			Count:      n,
			Dir:        dir,
		}
		if err := p.RemoveEntry(ctx, e); err != nil {
			failed++
			if len(errs) < 20 {
				errs = append(errs, fmt.Sprintf("%s: %v", e.ID, err))
			}
			return nil
		}
		deleted++
		return nil
	})
	return deleted, failed, errs
}

// RemoveEntry deletes the pool directory and registry doc. Nil-safe.
func (p *Pool) RemoveEntry(ctx context.Context, e PoolEntry) error {
	if p == nil || p.root == "" {
		return errors.New("coverpool: pool is unavailable")
	}
	if e.Dir == "" {
		return errors.New("coverpool: empty entry dir")
	}
	absRoot, err := filepath.Abs(p.root)
	if err != nil {
		return err
	}
	absDir, err := filepath.Abs(e.Dir)
	if err != nil {
		return err
	}
	if absDir != absRoot && !strings.HasPrefix(absDir, absRoot+string(filepath.Separator)) {
		return errors.New("coverpool: entry dir outside pool root")
	}

	// Capture old count before removing.
	p.indexMu.Lock()
	old := 0
	if p.dirCount != nil {
		old = p.dirCount[absDir]
		if old == 0 {
			// Paths may be stored non-abs; try e.Dir as-is.
			old = p.dirCount[e.Dir]
		}
	}
	key := textSizeKey(e.Text, e.Width, e.Height)
	if old > 0 && p.byTH != nil {
		p.byTH[key] -= old
		if p.byTH[key] < 0 {
			p.byTH[key] = 0
		}
	}
	if p.dirCount != nil {
		delete(p.dirCount, absDir)
		delete(p.dirCount, e.Dir)
	}
	p.indexMu.Unlock()

	if err := os.RemoveAll(absDir); err != nil {
		return err
	}
	if r := p.Registry(); r != nil && e.ID != "" {
		_ = r.Delete(ctx, e.ID)
	}
	return nil
}

// reindexDir refreshes one pool directory into the aggregate index (call after Put).
func (p *Pool) reindexDir(spec Spec) {
	if p == nil || p.root == "" {
		return
	}
	dir, err := p.Paths(spec)
	if err != nil {
		return
	}
	n := countImagesInDir(dir)
	key := textSizeKey(spec.Text, spec.Width, spec.Height)

	p.indexMu.Lock()
	if p.byTH == nil {
		p.byTH = make(map[string]int)
	}
	if p.dirCount == nil {
		p.dirCount = make(map[string]int)
	}
	old := p.dirCount[dir]
	p.byTH[key] += n - old
	if p.byTH[key] < 0 {
		p.byTH[key] = 0
	}
	p.dirCount[dir] = n
	p.indexBuilt = true
	p.indexMu.Unlock()

	_ = p.upsertRegistry(context.Background(), spec, dir, n, time.Time{})
}

// reindexDirAt is reindexDir plus an optional last_rotated_at for the registry upsert.
func (p *Pool) reindexDirAt(spec Spec, lastRotated time.Time) {
	if p == nil || p.root == "" {
		return
	}
	dir, err := p.Paths(spec)
	if err != nil {
		return
	}
	n := countImagesInDir(dir)
	key := textSizeKey(spec.Text, spec.Width, spec.Height)

	p.indexMu.Lock()
	if p.byTH == nil {
		p.byTH = make(map[string]int)
	}
	if p.dirCount == nil {
		p.dirCount = make(map[string]int)
	}
	old := p.dirCount[dir]
	p.byTH[key] += n - old
	if p.byTH[key] < 0 {
		p.byTH[key] = 0
	}
	p.dirCount[dir] = n
	p.indexBuilt = true
	p.indexMu.Unlock()

	_ = p.upsertRegistry(context.Background(), spec, dir, n, lastRotated)
}

func (p *Pool) upsertRegistry(ctx context.Context, spec Spec, dir string, n int, lastRotated time.Time) error {
	r := p.Registry()
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	return r.Upsert(cctx, PoolEntry{
		ID:            SpecID(spec),
		Text:          spec.Text,
		Width:         spec.Width,
		Height:        spec.Height,
		Background:    spec.Background,
		Format:        spec.Format,
		Count:         n,
		Dir:           dir,
		LastRotatedAt: lastRotated,
	})
}

// walkMeta visits each meta.json under root and invokes fn with parsed Spec, dir, and image count.
func (p *Pool) walkMeta(fn func(spec Spec, dir string, n int) error) error {
	return p.walkMetaDetail(func(spec Spec, _ Meta, dir string, n int) error {
		return fn(spec, dir, n)
	})
}

// walkMetaDetail is like walkMeta but also passes the decoded Meta (timestamps included).
func (p *Pool) walkMetaDetail(fn func(spec Spec, meta Meta, dir string, n int) error) error {
	err := filepath.WalkDir(p.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || d.Name() != "meta.json" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		var meta Meta
		if json.Unmarshal(data, &meta) != nil {
			return nil
		}
		dir := filepath.Dir(path)
		n := countImagesInDir(dir)
		spec := Spec{
			Text:       meta.Text,
			Width:      meta.Width,
			Height:     meta.Height,
			Background: meta.Background,
			Format:     meta.Format,
		}
		return fn(spec, meta, dir, n)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// oldestImageModTime returns the oldest ModTime among images matching format in dir.
func oldestImageModTime(dir string, format ImageFormat) (time.Time, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	var oldest time.Time
	found := false
	for _, path := range imagePaths(dir, entries, format) {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		mod := info.ModTime()
		if !found || mod.Before(oldest) {
			oldest = mod
			found = true
		}
	}
	return oldest, found
}

func countImagesInDir(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		_, format, ok := imageName(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		valid, err := validImage(path, format)
		if err != nil || !valid {
			continue
		}
		n++
	}
	return n
}
