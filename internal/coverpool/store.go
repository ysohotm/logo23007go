package coverpool

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ImageFormat identifies the encoding used by an image in the pool.
type ImageFormat string

const (
	FormatJPEG ImageFormat = "jpeg"
	FormatWebP ImageFormat = "webp"
)

// ContentType returns the HTTP media type for the image format.
func (f ImageFormat) ContentType() string {
	switch f {
	case FormatJPEG:
		return "image/jpeg"
	case FormatWebP:
		return "image/webp"
	default:
		return ""
	}
}

// Extension returns the file extension without a leading dot.
func (f ImageFormat) Extension() string {
	switch f {
	case FormatJPEG:
		return "jpg"
	case FormatWebP:
		return "webp"
	default:
		return ""
	}
}

// Spec identifies one generated-image pool.
type Spec struct {
	Text       string
	Width      int
	Height     int
	Background string
	Format     ImageFormat
}

// Meta is stored alongside the images for inspection and recovery.
type Meta struct {
	Text          string      `json:"text"`
	Width         int         `json:"width"`
	Height        int         `json:"height"`
	Background    string      `json:"background"`
	Format        ImageFormat `json:"format"`
	CreatedAt     time.Time   `json:"created_at"`
	LastRotatedAt time.Time   `json:"last_rotated_at,omitempty"`
}

// Pool stores generated images under a content-addressed directory tree.
type Pool struct {
	root string

	registry EntryRegistry

	indexMu    sync.RWMutex
	byTH       map[string]int // text\x00WxH → total images across backgrounds/formats
	dirCount   map[string]int // pool dir path → image count (for reindex deltas)
	indexBuilt bool
	indexOnce  sync.Once
}

// NewPool creates a pool rooted at root. An empty root is unavailable.
func NewPool(root string) *Pool {
	return &Pool{root: root}
}

// SetRegistry attaches an optional EntryRegistry for inventory persistence.
func (p *Pool) SetRegistry(r EntryRegistry) {
	if p == nil {
		return
	}
	p.registry = r
}

// Registry returns the attached EntryRegistry, or nil.
func (p *Pool) Registry() EntryRegistry {
	if p == nil {
		return nil
	}
	return p.registry
}

// Paths returns the directory for spec without creating it.
func (p *Pool) Paths(spec Spec) (string, error) {
	if p == nil || p.root == "" {
		return "", errors.New("coverpool: pool is unavailable")
	}
	key := hashKey(spec)
	return filepath.Join(p.root, key[:2], key), nil
}

// SaveMeta atomically records the original image-generation parameters.
// If meta.json already exists, CreatedAt and LastRotatedAt are preserved;
// only Text/Width/Height/Background/Format are refreshed. A new file gets
// CreatedAt=now and a zero LastRotatedAt.
func (p *Pool) SaveMeta(spec Spec) error {
	dir, err := p.Paths(spec)
	if err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "meta.json")
	meta := Meta{
		Text:       spec.Text,
		Width:      spec.Width,
		Height:     spec.Height,
		Background: spec.Background,
		Format:     spec.Format,
		CreatedAt:  time.Now(),
	}
	if existing, err := os.ReadFile(metaPath); err == nil {
		var prev Meta
		if json.Unmarshal(existing, &prev) == nil {
			meta.CreatedAt = prev.CreatedAt
			meta.LastRotatedAt = prev.LastRotatedAt
		}
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(metaPath, data)
}

// RotateOldest deletes up to n oldest image files (by mtime) in the pool dir for spec.
// Updates meta last_rotated_at=now, reindexes, upserts registry. Returns deleted count.
func (p *Pool) RotateOldest(spec Spec, n int) (deleted int, err error) {
	if n <= 0 {
		return 0, nil
	}
	if p == nil || p.root == "" {
		return 0, errors.New("coverpool: pool is unavailable")
	}
	dir, err := p.Paths(spec)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	type timedPath struct {
		path string
		mod  time.Time
	}
	files := make([]timedPath, 0, len(entries))
	for _, path := range imagePaths(dir, entries, spec.Format) {
		info, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return deleted, statErr
		}
		files = append(files, timedPath{path: path, mod: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].mod.Before(files[j].mod)
	})
	if n > len(files) {
		n = len(files)
	}
	for i := 0; i < n; i++ {
		if remErr := os.Remove(files[i].path); remErr != nil && !errors.Is(remErr, fs.ErrNotExist) {
			return deleted, remErr
		}
		deleted++
	}

	metaPath := filepath.Join(dir, "meta.json")
	now := time.Now()
	meta := Meta{
		Text:          spec.Text,
		Width:         spec.Width,
		Height:        spec.Height,
		Background:    spec.Background,
		Format:        spec.Format,
		CreatedAt:     now,
		LastRotatedAt: now,
	}
	if existing, readErr := os.ReadFile(metaPath); readErr == nil {
		var prev Meta
		if json.Unmarshal(existing, &prev) == nil {
			if !prev.CreatedAt.IsZero() {
				meta.CreatedAt = prev.CreatedAt
			}
		}
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return deleted, err
	}
	data = append(data, '\n')
	if err := writeAtomic(metaPath, data); err != nil {
		return deleted, err
	}
	p.reindexDir(spec)
	return deleted, nil
}

// Put validates data and atomically stores it in slot, replacing the same
// slot and format if it already exists.
func (p *Pool) Put(spec Spec, slot int, data []byte) error {
	if p == nil || p.root == "" {
		return errors.New("coverpool: pool is unavailable")
	}
	if slot < 0 || slot > 9999 {
		return fmt.Errorf("coverpool: slot %d out of range", slot)
	}
	format, ok := imageMagic(data)
	if !ok {
		return errors.New("coverpool: invalid JPEG or WebP image data")
	}
	if format != spec.Format {
		return fmt.Errorf("coverpool: image magic %q does not match format %q", format, spec.Format)
	}
	dir, err := p.Paths(spec)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%04d.%s", slot, format.Extension())
	if err := writeAtomic(filepath.Join(dir, name), data); err != nil {
		return err
	}
	p.reindexDir(spec)
	return nil
}

// Get reads a randomly selected image. Invalid images are removed and treated
// as cache misses rather than operational failures.
func (p *Pool) Get(spec Spec) ([]byte, bool, error) {
	dir, err := p.Paths(spec)
	if err != nil {
		return nil, false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	paths := imagePaths(dir, entries, spec.Format)
	for len(paths) > 0 {
		i := rand.IntN(len(paths))
		path := paths[i]
		paths = append(paths[:i], paths[i+1:]...)
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, false, err
		}
		_, format, _ := imageName(filepath.Base(path))
		if actual, ok := imageMagic(data); !ok || actual != format {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, false, err
			}
			continue
		}
		return data, true, nil
	}
	return nil, false, nil
}

// Missing returns absent slots in the half-open range [0, count).
func (p *Pool) Missing(spec Spec, count int) ([]int, error) {
	if count < 0 || count > 10000 {
		return nil, fmt.Errorf("coverpool: count %d out of range", count)
	}
	dir, err := p.Paths(spec)
	if err != nil {
		return nil, err
	}
	present, err := presentSlots(dir, count, spec.Format)
	if err != nil {
		return nil, err
	}
	missing := make([]int, 0, count-len(present))
	for slot := 0; slot < count; slot++ {
		if !present[slot] {
			missing = append(missing, slot)
		}
	}
	return missing, nil
}

// Count returns the number of occupied slots with valid image data.
func (p *Pool) Count(spec Spec) (int, error) {
	dir, err := p.Paths(spec)
	if err != nil {
		return 0, err
	}
	present, err := presentSlots(dir, 10000, spec.Format)
	if err != nil {
		return 0, err
	}
	return len(present), nil
}

// SpecID returns the content-addressed id for a pool Spec (sha256 hex).
func SpecID(spec Spec) string { return hashKey(spec) }

func hashKey(spec Spec) string {
	h := sha256.New()
	writeKeyField := func(value []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		h.Write(length[:])
		h.Write(value)
	}
	writeKeyField([]byte(spec.Text))
	var width [8]byte
	binary.BigEndian.PutUint64(width[:], uint64(int64(spec.Width)))
	writeKeyField(width[:])
	var height [8]byte
	binary.BigEndian.PutUint64(height[:], uint64(int64(spec.Height)))
	writeKeyField(height[:])
	writeKeyField([]byte(spec.Background))
	writeKeyField([]byte(spec.Format))
	return hex.EncodeToString(h.Sum(nil))
}

func imageMagic(data []byte) (ImageFormat, bool) {
	switch {
	case len(data) >= 2 && data[0] == 0xff && data[1] == 0xd8:
		return FormatJPEG, true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return FormatWebP, true
	default:
		return "", false
	}
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func imagePaths(dir string, entries []fs.DirEntry, format ImageFormat) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if slot, fileFormat, ok := imageName(entry.Name()); ok && slot >= 0 && fileFormat == format {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return paths
}

func imageName(name string) (int, ImageFormat, bool) {
	if len(name) != 8 && len(name) != 9 || name[4] != '.' {
		return 0, "", false
	}
	slot, err := strconv.Atoi(name[:4])
	if err != nil {
		return 0, "", false
	}
	var format ImageFormat
	switch name[5:] {
	case "jpg":
		format = FormatJPEG
	case "webp":
		format = FormatWebP
	default:
		return 0, "", false
	}
	return slot, format, true
}

func presentSlots(dir string, limit int, format ImageFormat) (map[int]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[int]bool{}, nil
		}
		return nil, err
	}
	present := make(map[int]bool)
	for _, path := range imagePaths(dir, entries, format) {
		slot, _, _ := imageName(filepath.Base(path))
		if slot >= limit || present[slot] {
			continue
		}
		valid, err := validImage(path, format)
		if err != nil {
			return nil, err
		}
		if valid {
			present[slot] = true
		} else if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return present, nil
}

func validImage(path string, format ImageFormat) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	header := make([]byte, 12)
	n, readErr := f.Read(header)
	data := header[:n]
	actual, ok := imageMagic(data)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, readErr
	}
	return ok && actual == format, nil
}
