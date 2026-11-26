package logo

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KarpelesLab/gowebp"
	"github.com/disintegration/imaging"
	"github.com/dlecorfec/progjpeg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"

	"logo23007go/internal/cache"
)

// OutputFormat selects the encoded image format for cover endpoints.
type OutputFormat string

const (
	FormatJPEG OutputFormat = "jpeg"
	FormatWebP OutputFormat = "webp"
)

type FileCreateResult struct {
	Message  string
	Success  bool
	FileName string
	Data     []byte
	Format   OutputFormat
}

type Creater struct {
	ContentRootPath string
	font            *truetype.Font
	fileCache       *cache.FileCache
	rng             *rand.Rand
	mu              sync.Mutex
	bg              atomic.Pointer[bgCache]
}

func NewCreater(contentRootPath string) (*Creater, error) {
	fontPath := filepath.Join(contentRootPath, "app_data", "fonts", "msyhbd.ttf")
	fontBytes, err := os.ReadFile(fontPath)
	if err != nil {
		return nil, fmt.Errorf("load font: %w", err)
	}
	ttf, err := truetype.Parse(fontBytes)
	if err != nil {
		return nil, fmt.Errorf("parse font: %w", err)
	}
	return &Creater{
		ContentRootPath: contentRootPath,
		font:            ttf,
		fileCache:       cache.NewFileCache(5 * time.Minute),
		rng:             rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

func (c *Creater) LoadImage(imagePath string, targetWidth, targetHeight int) (image.Image, error) {
	if _, err := os.Stat(imagePath); err != nil {
		return nil, fmt.Errorf("图像文件未找到: %s", imagePath)
	}
	img, err := imaging.Open(imagePath)
	if err != nil {
		return nil, err
	}
	if targetWidth > 0 && targetHeight > 0 {
		// Approximate ImageSharp DecoderOptions.TargetSize (fit within size, bicubic).
		img = imaging.Fit(img, targetWidth, targetHeight, imaging.Lanczos)
	}
	return img, nil
}

func (c *Creater) Create(text string, siteID int) error {
	imageBfile := c.GetBackgroundImage()
	if imageBfile == "" {
		return fmt.Errorf("背景图片不可用（文件夹为空或不存在）")
	}
	img, err := c.LoadImage(imageBfile, 0, 0)
	if err != nil {
		return err
	}
	fontSize, x, y := getTextPosition(text, img.Bounds().Dx())
	out := c.drawTextOnImage(img, text, fontSize, x, y)
	fn := c.GetLogoPath(siteID)
	if err := os.MkdirAll(filepath.Dir(fn), 0o755); err != nil {
		return err
	}
	f, err := os.Create(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, out)
}

func (c *Creater) CreateBytes(text string) ([]byte, error) {
	imageBfile := c.GetBackgroundImage()
	if imageBfile == "" {
		return nil, fmt.Errorf("背景图片不可用（文件夹为空或不存在）")
	}
	img, err := c.LoadImage(imageBfile, 0, 0)
	if err != nil {
		return nil, err
	}
	fontSize, x, y := getTextPosition(text, img.Bounds().Dx())
	out := c.drawTextOnImage(img, text, fontSize, x, y)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *Creater) CreateBytesImage(text string, desWidth, desHeight int, bfile string, format OutputFormat) (*FileCreateResult, error) {
	var imageBfile string
	if bfile == "" {
		imageBfile = c.GetBackgroundImage2()
	} else {
		imageBfile = filepath.Join(c.ContentRootPath, "app_data", "images", bfile)
	}
	if imageBfile == "" || !fileExists(imageBfile) {
		imageBfile = c.GetBackgroundImage2()
		if imageBfile == "" {
			return nil, fmt.Errorf("背景图片不可用（文件夹为空或不存在）")
		}
	}

	img, err := c.loadBackground(imageBfile, desWidth, desHeight)
	if err != nil {
		return nil, err
	}
	fontSize, x, y := getTextPosition(text, img.Bounds().Dx())
	out := c.drawTextOnImage(img, text, fontSize, x, y)

	if format != FormatWebP {
		format = FormatJPEG
	}

	var buf bytes.Buffer
	buf.Grow(estimateEncodedSize(out.Bounds()))
	switch format {
	case FormatWebP:
		// gowebp's lossy encoder only reads *image.NRGBA directly; anything else is
		// copied into a fresh NRGBA twice (alpha extraction + YUV conversion).
		if err := gowebp.Encode(&buf, opaqueNRGBAView(out), &gowebp.Options{Lossy: true, Quality: 80}); err != nil {
			return nil, err
		}
	default:
		if err := progjpeg.Encode(&buf, out, &progjpeg.Options{Quality: 80, Progressive: true}); err != nil {
			return nil, err
		}
		format = FormatJPEG
	}
	return &FileCreateResult{
		Success:  true,
		FileName: imageBfile,
		Data:     buf.Bytes(),
		Format:   format,
	}, nil
}

// ContentType returns the HTTP Content-Type for an encoded cover image.
func (f OutputFormat) ContentType() string {
	if f == FormatWebP {
		return "image/webp"
	}
	return "image/jpeg"
}

func (c *Creater) GetLogoPath(siteID int) string {
	return filepath.Join(c.ContentRootPath, "app_data", "logos", fmt.Sprintf("%04d.png", siteID))
}

const (
	minEncodeBufSize = 32 << 10
	maxEncodeBufSize = 4 << 20
)

func estimateEncodedSize(r image.Rectangle) int {
	n := r.Dx() * r.Dy() / 4
	if n < minEncodeBufSize {
		return minEncodeBufSize
	}
	if n > maxEncodeBufSize {
		return maxEncodeBufSize
	}
	return n
}

// opaqueNRGBAView returns a zero-copy *image.NRGBA over an opaque *image.RGBA
// (premultiplied and straight alpha are byte-identical when every A is 0xff).
func opaqueNRGBAView(img image.Image) image.Image {
	if r, ok := img.(*image.RGBA); ok && r.Opaque() {
		return &image.NRGBA{Pix: r.Pix, Stride: r.Stride, Rect: r.Rect}
	}
	return img
}

// toDrawableRGBA returns an *image.RGBA to draw text on. img must be owned by
// the caller (never a shared/cached image): RGBA inputs and opaque NRGBA inputs
// are drawn on in place, everything else is copied.
func toDrawableRGBA(img image.Image) *image.RGBA {
	switch m := img.(type) {
	case *image.RGBA:
		return m
	case *image.NRGBA:
		// Opaque NRGBA has the same byte layout as RGBA; RGBA is what progjpeg fast-paths.
		if m.Opaque() {
			return &image.RGBA{Pix: m.Pix, Stride: m.Stride, Rect: m.Rect}
		}
	}
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	return rgba
}

func (c *Creater) drawTextOnImage(img image.Image, text string, fontSize, x, y int) image.Image {
	rgba := toDrawableRGBA(img)

	c.mu.Lock()
	col := color.RGBA{
		R: uint8(c.rng.Intn(256)),
		G: uint8(c.rng.Intn(256)),
		B: uint8(c.rng.Intn(256)),
		A: 255,
	}
	c.mu.Unlock()

	face := truetype.NewFace(c.font, &truetype.Options{
		Size:    float64(fontSize),
		DPI:     72,
		Hinting: font.HintingFull,
	})
	defer face.Close()

	// ImageSharp DrawText PointF is top-left of glyph box; Drawer.Dot is baseline.
	d := &font.Drawer{
		Dst:  rgba,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x, y+fontSize),
	}
	d.DrawString(text)
	return rgba
}

func getTextPosition(text string, imageWidth int) (fontSize, x, y int) {
	lenText := len([]rune(text))
	switch lenText {
	case 6:
		fontSize, x, y = 40, 20, 18
	case 7:
		fontSize, x, y = 36, 12, 20
	default:
		fontSize, x, y = 40, 35, 18
	}
	if imageWidth >= 1000 {
		fontSize = 60
	}
	return
}

func (c *Creater) getCachedDirectoryFiles(cacheKey, relativePath string) []string {
	return c.fileCache.GetOrLoad(cacheKey, func() []string {
		path := filepath.Join(c.ContentRootPath, relativePath)
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil
		}
		var files []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			files = append(files, filepath.Join(path, e.Name()))
		}
		return files
	})
}

func (c *Creater) pickRandomFile(files []string) string {
	if len(files) == 0 {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return files[c.rng.Intn(len(files))]
}

func (c *Creater) GetBackgroundImage() string {
	return c.pickRandomFile(c.getCachedDirectoryFiles(
		"logobackgroudimages",
		filepath.Join("app_data", "configs", "logobackgroudimages"),
	))
}

func (c *Creater) GetBackgroundImage2() string {
	return c.pickRandomFile(c.getCachedDirectoryFiles(
		"imagefiles",
		filepath.Join("app_data", "images"),
	))
}

func (c *Creater) GetBackgroundImage2ByKey(key string) string {
	files := c.getCachedDirectoryFiles("imagefiles", filepath.Join("app_data", "images"))
	if len(files) == 0 {
		return ""
	}
	var index int
	if numericID, err := strconv.Atoi(key); err == nil {
		if numericID < 0 {
			numericID = -numericID
		}
		index = numericID % len(files)
	} else {
		h := stableHash(key)
		if h < 0 {
			h = -h
		}
		index = h % len(files)
	}
	return files[index]
}

func stableHash(input string) int {
	sum := md5.Sum([]byte(input))
	return int(int32(binary.LittleEndian.Uint32(sum[0:4])))
}

func (c *Creater) GetImageFilesWithCache() []string {
	return c.getCachedDirectoryFiles("imagefiles", filepath.Join("app_data", "images"))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
