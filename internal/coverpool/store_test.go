package coverpool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var (
	testJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}
	testWebP = []byte{'R', 'I', 'F', 'F', 4, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}
)

func testSpec(text string) Spec {
	return Spec{Text: text, Width: 640, Height: 360, Background: "navy", Format: FormatJPEG}
}

func mustPaths(t *testing.T, p *Pool, spec Spec) string {
	t.Helper()
	dir, err := p.Paths(spec)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPathsSafeAndStable(t *testing.T) {
	root := t.TempDir()
	p := NewPool(root)
	spec := testSpec("../中文/../../escape")
	dir := mustPaths(t, p, spec)

	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 64 {
		t.Fatalf("path must be <2 hex>/<64 hex>, got %q", rel)
	}
	for _, part := range parts {
		if _, err := os.Stat(filepath.Join(root, part)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, r := range part {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("non-hex path component %q", part)
			}
		}
	}
	if strings.HasPrefix(parts[1], parts[0]) == false {
		t.Fatalf("prefix %q does not match hash %q", parts[0], parts[1])
	}
	if again := mustPaths(t, p, spec); again != dir {
		t.Fatalf("unstable paths: %q != %q", again, dir)
	}
}

func TestPathsLengthPrefixAndSpecIsolation(t *testing.T) {
	p := NewPool(t.TempDir())
	base := testSpec("相同关键词")
	basePath := mustPaths(t, p, base)

	variants := []Spec{
		{Text: base.Text, Width: base.Width + 1, Height: base.Height, Background: base.Background, Format: base.Format},
		{Text: base.Text, Width: base.Width, Height: base.Height + 1, Background: base.Background, Format: base.Format},
		{Text: base.Text, Width: base.Width, Height: base.Height, Background: "white", Format: base.Format},
		{Text: base.Text, Width: base.Width, Height: base.Height, Background: base.Background, Format: FormatWebP},
	}
	seen := map[string]bool{basePath: true}
	for _, spec := range variants {
		dir := mustPaths(t, p, spec)
		if seen[dir] {
			t.Errorf("spec %+v collides with another spec", spec)
		}
		seen[dir] = true
	}

	// Ambiguous concatenations must not produce the same cache key.
	left := Spec{Text: "a", Width: 1, Height: 2, Background: "bc", Format: FormatJPEG}
	right := Spec{Text: "a", Width: 1, Height: 2, Background: "b", Format: ImageFormat("cjpeg")}
	if leftDir, leftErr := p.Paths(left); leftErr == nil {
		if rightDir, rightErr := p.Paths(right); rightErr == nil && leftDir == rightDir {
			t.Fatal("ambiguous fields produced identical paths")
		}
	}
}

func TestImageFormatMetadata(t *testing.T) {
	tests := []struct {
		format      ImageFormat
		contentType string
		extension   string
	}{
		{FormatJPEG, "image/jpeg", "jpg"},
		{FormatWebP, "image/webp", "webp"},
		{ImageFormat("png"), "", ""},
	}
	for _, tt := range tests {
		if got := tt.format.ContentType(); got != tt.contentType {
			t.Errorf("%q ContentType() = %q, want %q", tt.format, got, tt.contentType)
		}
		if got := tt.format.Extension(); got != tt.extension {
			t.Errorf("%q Extension() = %q, want %q", tt.format, got, tt.extension)
		}
	}
}

func TestSaveMetaReadableAndAtomic(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := testSpec("原始关键词：../中文")
	if err := p.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(mustPaths(t, p, spec), "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("\n  \"text\"")) {
		t.Fatalf("meta is not human-readable: %s", data)
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Text != spec.Text || meta.Width != spec.Width || meta.Height != spec.Height ||
		meta.Background != spec.Background || meta.Format != spec.Format || meta.CreatedAt.IsZero() {
		t.Fatalf("incorrect metadata: %+v", meta)
	}

	var first Meta
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var second Meta
	if err := json.Unmarshal(after, &second); err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt not preserved on re-save: %v vs %v", second.CreatedAt, first.CreatedAt)
	}
	if !second.LastRotatedAt.Equal(first.LastRotatedAt) {
		t.Fatalf("LastRotatedAt not preserved on re-save: %v vs %v", second.LastRotatedAt, first.LastRotatedAt)
	}
	spec.Text = "更新后的关键词"
	if err := p.SaveMeta(spec); err != nil {
		t.Fatal(err)
	}
	newData, err := os.ReadFile(filepath.Join(mustPaths(t, p, spec), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(newData, []byte("更新后的关键词")) {
		t.Fatalf("metadata has incorrect keyword: %s", newData)
	}
	entries, err := os.ReadDir(filepath.Dir(metaPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestPutGetReplaceAndCoexistingFormats(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := testSpec("cover")
	if err := p.Put(spec, 7, testJPEG); err != nil {
		t.Fatal(err)
	}
	got, ok, err := p.Get(spec)
	if err != nil || !ok || !bytes.Equal(got, testJPEG) {
		t.Fatalf("Get() = %x, %v, %v", got, ok, err)
	}

	replacement := append([]byte(nil), testJPEG...)
	replacement = append(replacement, 1, 2, 3)
	if err := p.Put(spec, 7, replacement); err != nil {
		t.Fatal(err)
	}
	got, ok, err = p.Get(spec)
	if err != nil || !ok || !bytes.Equal(got, replacement) {
		t.Fatalf("replacement Get() = %x, %v, %v", got, ok, err)
	}

	webPSpec := spec
	webPSpec.Format = FormatWebP
	webPDir := mustPaths(t, p, webPSpec)
	if webPDir == mustPaths(t, p, spec) {
		t.Fatal("formats must be isolated")
	}
	if err := p.Put(webPSpec, 7, testWebP); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := p.Get(webPSpec); err != nil || !ok || !bytes.Equal(got, testWebP) {
		t.Fatalf("WebP Get() = %x, %v, %v", got, ok, err)
	}
	if got, ok, err := p.Get(spec); err != nil || !ok || !bytes.Equal(got, replacement) {
		t.Fatalf("JPEG was overwritten by WebP spec: %x, %v, %v", got, ok, err)
	}
}

func TestPutRejectsFormatMismatch(t *testing.T) {
	p := NewPool(t.TempDir())
	for _, tt := range []struct {
		format ImageFormat
		data   []byte
	}{
		{FormatJPEG, testWebP},
		{FormatWebP, testJPEG},
		{ImageFormat("png"), testJPEG},
	} {
		spec := testSpec("format-mismatch")
		spec.Format = tt.format
		if err := p.Put(spec, 0, tt.data); err == nil {
			t.Errorf("Put accepted format %q with data %x", tt.format, tt.data)
		}
	}
}

func TestGetRandomlySelectsAmongSlots(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := testSpec("random")
	first := append([]byte(nil), testJPEG...)
	second := append([]byte(nil), testJPEG...)
	second = append(second, 9)
	if err := p.Put(spec, 0, first); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(spec, 9, second); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		got, ok, err := p.Get(spec)
		if err != nil || !ok {
			t.Fatalf("Get() ok=%v err=%v", ok, err)
		}
		seen[string(got)] = true
	}
	if len(seen) != 2 {
		t.Fatalf("Get only returned %d of 2 slots", len(seen))
	}
}

func TestPutRejectsInvalidInput(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := testSpec("invalid")
	for _, slot := range []int{-1, 10000} {
		if err := p.Put(spec, slot, testJPEG); err == nil {
			t.Errorf("Put accepted slot %d", slot)
		}
	}
	invalid := [][]byte{nil, {0xff}, {0xff, 0xd9}, []byte("not an image"), testWebP[:11]}
	for _, data := range invalid {
		if err := p.Put(spec, 0, data); err == nil {
			t.Errorf("Put accepted invalid data %x", data)
		}
	}
	if _, ok, err := p.Get(spec); err != nil || ok {
		t.Fatalf("invalid Put created a usable image: ok=%v err=%v", ok, err)
	}
	if err := p.Put(spec, 0, testJPEG); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(spec, 0, []byte("broken")); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	got, ok, err := p.Get(spec)
	if err != nil || !ok || !bytes.Equal(got, testJPEG) {
		t.Fatalf("invalid replacement damaged existing image: %x, %v, %v", got, ok, err)
	}
}

func TestMissingAndCount(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := testSpec("counts")
	for _, count := range []int{-1, 10001} {
		if _, err := p.Missing(spec, count); err == nil {
			t.Errorf("Missing accepted count %d", count)
		}
	}
	if missing, err := p.Missing(spec, 0); err != nil || len(missing) != 0 {
		t.Fatalf("Missing(0) = %v, %v", missing, err)
	}
	if missing, err := p.Missing(spec, 4); err != nil || !reflect.DeepEqual(missing, []int{0, 1, 2, 3}) {
		t.Fatalf("empty Missing() = %v, %v", missing, err)
	}
	if err := p.Put(spec, 1, testJPEG); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(spec, 3, testJPEG); err != nil {
		t.Fatal(err)
	}
	if missing, err := p.Missing(spec, 4); err != nil || !reflect.DeepEqual(missing, []int{0, 2}) {
		t.Fatalf("Missing() = %v, %v", missing, err)
	}
	if count, err := p.Count(spec); err != nil || count != 2 {
		t.Fatalf("Count() = %d, %v", count, err)
	}
	if err := p.Put(spec, 2, testJPEG); err != nil {
		t.Fatal(err)
	}
	if count, err := p.Count(spec); err != nil || count != 3 {
		t.Fatalf("Count with three slots = %d, %v", count, err)
	}

	dir := mustPaths(t, p, spec)
	if err := os.WriteFile(filepath.Join(dir, "not-an-image.jpg"), testJPEG, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0000.jpg"), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if missing, err := p.Missing(spec, 4); err != nil || !reflect.DeepEqual(missing, []int{0}) {
		t.Fatalf("Missing with corrupt file = %v, %v", missing, err)
	}
	if count, err := p.Count(spec); err != nil || count != 3 {
		t.Fatalf("Count with corrupt file = %d, %v", count, err)
	}
}

func TestGetCorruptFileDegradesAndMissingDirectory(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := testSpec("corrupt")
	if got, ok, err := p.Get(spec); err != nil || ok || got != nil {
		t.Fatalf("missing directory Get() = %x, %v, %v", got, ok, err)
	}
	dir := mustPaths(t, p, spec)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(dir, "0002.jpg")
	if err := os.WriteFile(corruptPath, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := p.Get(spec); err != nil || ok || got != nil {
		t.Fatalf("corrupt Get() = %x, %v, %v", got, ok, err)
	}
	if _, err := os.Stat(corruptPath); !os.IsNotExist(err) {
		t.Fatalf("corrupt file not removed: %v", err)
	}
	if count, err := p.Count(spec); err != nil || count != 0 {
		t.Fatalf("missing Count() = %d, %v", count, err)
	}
}

func TestIOErrorsAreReturned(t *testing.T) {
	p := NewPool(t.TempDir())
	spec := testSpec("io-errors")
	dir := mustPaths(t, p, spec)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "0000.jpg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Get(spec); err == nil || ok {
		t.Fatalf("Get image I/O error: ok=%v err=%v", ok, err)
	}
	if _, err := p.Count(spec); err == nil {
		t.Fatal("Count ignored an image read error")
	}
	if _, err := p.Missing(spec, 1); err == nil {
		t.Fatal("Missing ignored an image read error")
	}
}

func TestNilAndEmptyPoolReturnSafeErrors(t *testing.T) {
	spec := testSpec("nil")
	pools := []*Pool{nil, NewPool("")}
	for _, p := range pools {
		if dir, err := p.Paths(spec); err == nil || dir != "" {
			t.Errorf("Paths() = %q, %v", dir, err)
		}
		if err := p.SaveMeta(spec); err == nil {
			t.Error("SaveMeta accepted unavailable pool")
		}
		if err := p.Put(spec, 0, testJPEG); err == nil {
			t.Error("Put accepted unavailable pool")
		}
		if data, ok, err := p.Get(spec); err == nil || ok || data != nil {
			t.Errorf("Get() = %x, %v, %v", data, ok, err)
		}
		if slots, err := p.Missing(spec, 1); err == nil || slots != nil {
			t.Errorf("Missing() = %v, %v", slots, err)
		}
		if count, err := p.Count(spec); err == nil || count != 0 {
			t.Errorf("Count() = %d, %v", count, err)
		}
	}
}
