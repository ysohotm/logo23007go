package handlers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"logo23007go/internal/coverpool"
	"logo23007go/internal/db"
	"logo23007go/internal/logo"
)

var (
	sizeRegex            = regexp.MustCompile(`_(\d+)x(\d+)`)
	md5SuffixRegex       = regexp.MustCompile(`_([a-fA-F0-9]{32})$`)
	numericIdSuffixRegex = regexp.MustCompile(`_(\d+)$`)
	extensionRegex       = regexp.MustCompile(`(?i)\.(jpg|jpeg|png|webp)$`)
)

const (
	defaultWidth  = 600
	defaultHeight = 800
)

// normalizeSize 把旧版大图 1200x1520 强制降为 600x800，其它尺寸原样返回。
func normalizeSize(width, height int) (int, int) {
	if width == 1200 && height == 1520 {
		return defaultWidth, defaultHeight
	}
	return width, height
}

type Home struct {
	Logger  *slog.Logger
	Root    string
	DB      *db.SiteDb
	Creater *logo.Creater

	ImageSem  chan struct{} // buffered channel used as a semaphore; nil = unlimited
	ImageWait time.Duration // max time to wait for a slot; 0 = default 3s

	Pool          *coverpool.Pool
	PoolConfig    *coverpool.Config
	PoolScheduler *coverpool.Scheduler
	SizeStats     *coverpool.SizeStats

	busyCount atomic.Int64
	busyLogAt atomic.Int64
}

const (
	defaultImageWait = 3 * time.Second
	slowImageHold    = 3 * time.Second
	busyLogEvery     = time.Minute
)

// rejectBusy 回 503，并按分钟汇总被拒次数，避免逐条刷日志。
func (h *Home) rejectBusy(w http.ResponseWriter) {
	h.busyCount.Add(1)
	now := time.Now().UnixNano()
	last := h.busyLogAt.Load()
	if now-last >= int64(busyLogEvery) && h.busyLogAt.CompareAndSwap(last, now) && h.Logger != nil {
		h.Logger.Warn("image slots busy, rejected", "count", h.busyCount.Swap(0), "limit", cap(h.ImageSem))
	}
	writeBusy(w)
}

func (h *Home) logSlowImage(r *http.Request, start time.Time) {
	if held := time.Since(start); held >= slowImageHold && h.Logger != nil {
		h.Logger.Warn("slow image", "path", r.URL.Path, "query", r.URL.RawQuery, "held", held.String())
	}
}

func (h *Home) acquireImageSlot(ctx context.Context) (release func(), ok bool) {
	if h.ImageSem == nil {
		return func() {}, true
	}
	if err := ctx.Err(); err != nil {
		return nil, false
	}
	select {
	case h.ImageSem <- struct{}{}:
		return h.releaseImageSlot(), true
	default:
	}
	wait := h.ImageWait
	if wait <= 0 {
		wait = defaultImageWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case h.ImageSem <- struct{}{}:
		return h.releaseImageSlot(), true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

func (h *Home) releaseImageSlot() func() {
	var once sync.Once
	sem := h.ImageSem
	return func() {
		once.Do(func() { <-sem })
	}
}

func writeBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "busy", http.StatusServiceUnavailable)
}

func (h *Home) Index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>LogoMaker</title></head><body><h1>LogoMakerCore23007 (Go)</h1><p>API ready.</p></body></html>`))
}

func (h *Home) Privacy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`Privacy`))
}

func (h *Home) Error(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`Error`))
}

func (h *Home) Contact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`Your contact page.`))
}

func (h *Home) Logo(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	bytes, err := h.Creater.CreateBytes(text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(bytes)
}

func (h *Home) AddImageKey(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	key := r.URL.Query().Get("key")
	id, err := h.DB.AutoAddFileKeyAllowDouble(r.Context(), domain, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(id))
}

func (h *Home) ImageKey(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	width, _ := strconv.Atoi(r.URL.Query().Get("width"))
	height, _ := strconv.Atoi(r.URL.Query().Get("height"))
	format := negotiateImageFormat(r.Header.Get("Accept"))

	// Without a cover pool, keep the old order: take a generation slot before
	// touching Mongo so busy tests and overload protection stay unchanged.
	var release func()
	if !h.poolEnabled() {
		var ok bool
		release, ok = h.acquireImageSlot(r.Context())
		if !ok {
			h.rejectBusy(w)
			return
		}
		defer release()
		defer h.logSlowImage(r, time.Now())
	}

	obj, err := h.DB.GetFileKey(r.Context(), id)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn("GetFileKey failed", "id", id, "err", err)
		}
		writeBusy(w)
		return
	}
	if obj == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("文件不存在:" + id))
		return
	}

	var file string
	if f, ok := obj["file"].(string); ok && strings.TrimSpace(f) != "" {
		file = f
		_ = h.DB.UpdateFileKeyUseCount(r.Context(), id)
	} else {
		bg := h.Creater.GetBackgroundImage2ByKey(id)
		file = filepath.Base(bg)
		_ = h.DB.UpdateFileKeyFile(r.Context(), id, file)
	}

	text, _ := obj["key"].(string)
	width, height = normalizeSize(width, height)
	h.SizeStats.Record(width, height)
	if h.poolEnabled() {
		if _, cached := h.servePoolHit(w, text, width, height, file, format); cached {
			return
		}
		var ok bool
		release, ok = h.acquireImageSlot(r.Context())
		if !ok {
			h.rejectBusy(w)
			return
		}
		defer release()
		defer h.logSlowImage(r, time.Now())
	}

	result, err := h.Creater.CreateBytesImage(text, width, height, file, format)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	h.writePooledResult(w, text, width, height, file, format, result.Data)
}

func (h *Home) Image(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		text = r.PathValue("text")
	}
	if text == "" {
		text = strings.TrimPrefix(r.URL.Path, "/vod/")
		text = strings.TrimPrefix(text, "/home/image/")
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("width"))
	height, _ := strconv.Atoi(r.URL.Query().Get("height"))

	text, width, height, file, err := h.resolveImage(r, text, width, height)
	if err != nil {
		h.Logger.Error("解析图片参数失败", "err", err)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.SizeStats.Record(width, height)
	format := negotiateImageFormat(r.Header.Get("Accept"))
	if h.poolEnabled() {
		if _, cached := h.servePoolHit(w, text, width, height, file, format); cached {
			return
		}
	}

	release, ok := h.acquireImageSlot(r.Context())
	if !ok {
		h.rejectBusy(w)
		return
	}
	defer release()
	defer h.logSlowImage(r, time.Now())
	defer func() {
		if rec := recover(); rec != nil {
			h.Logger.Error("panic generating image", "err", rec)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}()
	result, err := h.Creater.CreateBytesImage(text, width, height, file, format)
	if err != nil {
		h.Logger.Error("生成图片失败", "text", text, "width", width, "height", height, "file", file, "err", err)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	h.writePooledResult(w, text, width, height, file, format, result.Data)
}

func (h *Home) resolveImage(r *http.Request, text string, width, height int) (string, int, int, string, error) {
	if strings.Contains(text, "-") {
		text = strings.ReplaceAll(text, "-", "%")
		if decoded, err := url.QueryUnescape(text); err == nil {
			text = decoded
		}
	}
	text = extensionRegex.ReplaceAllString(text, "")
	if width == 0 {
		if m := sizeRegex.FindStringSubmatch(text); m != nil {
			width, _ = strconv.Atoi(m[1])
			height, _ = strconv.Atoi(m[2])
			text = strings.Replace(text, m[0], "", 1)
		} else if !strings.Contains(text, "orig") {
			width = defaultWidth
			height = defaultHeight
		} else {
			text = strings.ReplaceAll(text, "_orig", "")
		}
	}
	width, height = normalizeSize(width, height)
	var file string
	if fileKey := tryExtractFileKey(&text); fileKey != "" {
		f, err := h.DB.GetFileByMd5(r.Context(), fileKey)
		if err != nil {
			h.Logger.Warn("GetFileByMd5 failed, fallback by key", "key", fileKey, "err", err)
		}
		if strings.TrimSpace(f) == "" {
			bg := h.Creater.GetBackgroundImage2ByKey(fileKey)
			f = filepath.Base(bg)
			if err == nil {
				_ = h.DB.AddFileByMd5(r.Context(), fileKey, f)
			}
		}
		file = f
	}
	return text, width, height, file, nil
}

func (h *Home) Upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 104857600)
	if err := r.ParseMultipartForm(104857600); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("No file selected!"))
		return
	}
	defer file.Close()

	fileName := r.URL.Query().Get("filename")
	if strings.TrimSpace(fileName) == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("文件名不能为空"))
		return
	}
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("Only .png .jpg .jpeg files are allowed!"))
		return
	}

	imagesDir := filepath.Join(h.Root, "app_data", "images")
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dest := filepath.Join(imagesDir, fileName)
	if _, err := os.Stat(dest); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("文件已存在"))
		return
	}

	out, err := os.Create(dest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("File uploaded successfully!"))
}

func (h *Home) FileIsExits(w http.ResponseWriter, r *http.Request) {
	fileName := r.URL.Query().Get("fileName")
	if strings.ContainsAny(fileName, `/\`) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("文件路径错误"))
		return
	}
	path := filepath.Join(h.Root, "app_data", "images", fileName)
	exists := "false"
	if _, err := os.Stat(path); err == nil {
		exists = "true"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(exists))
}

func tryExtractFileKey(text *string) string {
	if m := md5SuffixRegex.FindStringSubmatch(*text); m != nil {
		key := m[1]
		*text = (*text)[:len(*text)-len("_"+key)]
		return key
	}
	if m := numericIdSuffixRegex.FindStringSubmatch(*text); m != nil {
		key := m[1]
		*text = (*text)[:len(*text)-len("_"+key)]
		return key
	}
	return ""
}

// negotiateImageFormat picks WebP when the client Accept header lists image/webp;
// otherwise Progressive JPEG.
func negotiateImageFormat(accept string) logo.OutputFormat {
	accept = strings.ToLower(accept)
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if media == "image/webp" {
			return logo.FormatWebP
		}
	}
	return logo.FormatJPEG
}
