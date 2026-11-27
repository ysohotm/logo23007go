package coverpool

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

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
		_ = p.RebuildIndex()
	})
}

// RebuildIndex walks root/**/meta.json, counts images per dir, and aggregates
// by text+width+height. Prefer calling this once at process startup.
func (p *Pool) RebuildIndex() error {
	if p == nil || p.root == "" {
		return errors.New("coverpool: pool is unavailable")
	}
	byTH := make(map[string]int)
	dirCount := make(map[string]int)

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
		key := textSizeKey(meta.Text, meta.Width, meta.Height)
		byTH[key] += n
		dirCount[dir] = n
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	p.indexMu.Lock()
	p.byTH = byTH
	p.dirCount = dirCount
	p.indexBuilt = true
	p.indexMu.Unlock()
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
	defer p.indexMu.Unlock()
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
