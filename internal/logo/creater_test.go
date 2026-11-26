package logo

import (
	"bytes"
	"image"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateBytesImage(t *testing.T) {
	root := findRoot(t)
	c, err := NewCreater(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.CreateBytesImage("测试标题六字", 400, 200, "sample.jpg", FormatJPEG)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Success || len(r.Data) < 100 || r.Format != FormatJPEG {
		t.Fatalf("unexpected result: success=%v len=%d format=%s", r.Success, len(r.Data), r.Format)
	}
	// Progressive JPEG starts with SOI (FFD8) then typically APP0/DQT; SOF2 marker is 0xFFC2.
	if !bytes.HasPrefix(r.Data, []byte{0xff, 0xd8}) || !bytes.Contains(r.Data[:min(512, len(r.Data))], []byte{0xff, 0xc2}) {
		t.Fatalf("expected progressive JPEG (SOF2), got magic=%x", r.Data[:min(16, len(r.Data))])
	}
}

func TestCreateBytesImageWebP(t *testing.T) {
	root := findRoot(t)
	c, err := NewCreater(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.CreateBytesImage("测试标题六字", 400, 200, "sample.jpg", FormatWebP)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Success || r.Format != FormatWebP || len(r.Data) < 100 {
		t.Fatalf("unexpected result: success=%v len=%d format=%s", r.Success, len(r.Data), r.Format)
	}
	if !bytes.HasPrefix(r.Data, []byte("RIFF")) || !bytes.Contains(r.Data[:min(16, len(r.Data))], []byte("WEBP")) {
		t.Fatalf("expected WebP RIFF header, got %x", r.Data[:min(16, len(r.Data))])
	}
}

func TestGetBackgroundImage2ByKeyStable(t *testing.T) {
	root := findRoot(t)
	c, err := NewCreater(root)
	if err != nil {
		t.Fatal(err)
	}
	a := c.GetBackgroundImage2ByKey("503135")
	b := c.GetBackgroundImage2ByKey("503135")
	if a == "" || a != b {
		t.Fatalf("expected stable pick, got %q vs %q", a, b)
	}
}

func TestDrawTextOnImageZeroCopy(t *testing.T) {
	c, err := NewCreater(findRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	rect := image.Rect(0, 0, 200, 100)

	opaque := image.NewNRGBA(rect)
	for i := 3; i < len(opaque.Pix); i += 4 {
		opaque.Pix[i] = 0xff
	}
	out, ok := c.drawTextOnImage(opaque, "测试标题六字", 40, 20, 18).(*image.RGBA)
	if !ok {
		t.Fatalf("opaque NRGBA: expected *image.RGBA")
	}
	if &out.Pix[0] != &opaque.Pix[0] {
		t.Fatalf("opaque NRGBA: expected zero-copy (shared Pix)")
	}
	if !out.Opaque() {
		t.Fatalf("opaque NRGBA: result must stay opaque")
	}

	rgba := image.NewRGBA(rect)
	if got := c.drawTextOnImage(rgba, "测试标题六字", 40, 20, 18).(*image.RGBA); &got.Pix[0] != &rgba.Pix[0] {
		t.Fatalf("RGBA: expected in-place draw")
	}

	translucent := image.NewNRGBA(rect)
	orig := append([]byte(nil), translucent.Pix...)
	got, ok := c.drawTextOnImage(translucent, "测试标题六字", 40, 20, 18).(*image.RGBA)
	if !ok || &got.Pix[0] == &translucent.Pix[0] || !got.Rect.Eq(rect) {
		t.Fatalf("non-opaque NRGBA: expected copy into new *image.RGBA")
	}
	if !bytes.Equal(translucent.Pix, orig) {
		t.Fatalf("non-opaque NRGBA: source must not be mutated")
	}

	ycc := image.NewYCbCr(rect, image.YCbCrSubsampleRatio420)
	got, ok = c.drawTextOnImage(ycc, "测试标题六字", 40, 20, 18).(*image.RGBA)
	if !ok || !got.Rect.Eq(rect) || !got.Opaque() {
		t.Fatalf("YCbCr: expected opaque *image.RGBA copy")
	}
}

type opaqueWrapper struct{ image.Image }

func TestDrawTextOnImageZeroCopyMatchesCopyPath(t *testing.T) {
	c, err := NewCreater(findRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	src := image.NewNRGBA(image.Rect(0, 0, 300, 120))
	for i := range src.Pix {
		src.Pix[i] = byte(rng.Intn(256))
		if i%4 == 3 {
			src.Pix[i] = 0xff
		}
	}
	clone := &image.NRGBA{Pix: append([]byte(nil), src.Pix...), Stride: src.Stride, Rect: src.Rect}

	c.rng = rand.New(rand.NewSource(42))
	want := c.drawTextOnImage(opaqueWrapper{clone}, "测试标题六字", 40, 20, 18).(*image.RGBA)
	c.rng = rand.New(rand.NewSource(42))
	got := c.drawTextOnImage(src, "测试标题六字", 40, 20, 18).(*image.RGBA)
	if &want.Pix[0] == &clone.Pix[0] {
		t.Fatalf("wrapper should force copy path")
	}
	if !bytes.Equal(got.Pix, want.Pix) {
		t.Fatalf("zero-copy output differs from copy path")
	}
}

func TestOpaqueNRGBAView(t *testing.T) {
	rect := image.Rect(0, 0, 4, 4)
	rgba := image.NewRGBA(rect)
	if _, ok := opaqueNRGBAView(rgba).(*image.RGBA); !ok {
		t.Fatalf("non-opaque RGBA must be passed through")
	}
	for i := 3; i < len(rgba.Pix); i += 4 {
		rgba.Pix[i] = 0xff
	}
	n, ok := opaqueNRGBAView(rgba).(*image.NRGBA)
	if !ok || &n.Pix[0] != &rgba.Pix[0] {
		t.Fatalf("opaque RGBA: expected zero-copy NRGBA view")
	}
}

func BenchmarkCreateBytesImage(b *testing.B) {
	wd, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	c, err := NewCreater(filepath.Clean(filepath.Join(wd, "..", "..")))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.CreateBytesImage("测试标题六字", 400, 200, "sample.jpg", FormatJPEG); err != nil {
			b.Fatal(err)
		}
	}
}

func findRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/logo -> repo root
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "app_data", "fonts", "msyhbd.ttf")); err != nil {
		t.Fatalf("font missing under %s: %v", root, err)
	}
	return root
}
