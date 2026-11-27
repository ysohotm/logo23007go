package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"logo23007go/internal/coverpool"
	"logo23007go/internal/logo"
)

const (
	coverCacheControl      = "public, max-age=86400, s-maxage=2592000"
	cloudflareCacheControl = "public, max-age=2592000"
	backgroundImageWait    = 30 * time.Second
)

func (h *Home) poolEnabled() bool {
	return h != nil && h.Pool != nil && h.Creater != nil
}

// poolForSize is true only for admin-allowlisted sizes (default 600x800).
func (h *Home) poolForSize(width, height int) bool {
	return h.poolEnabled() && h.PoolConfig != nil && h.PoolConfig.Allows(width, height)
}

// poolSpec keys the disk pool by text×size×format only. Request background is
// ignored so one keyword shares a single prewarm pool (CreateBytesImage picks a
// random background when Background is empty). Matches manual prewarm.
func (h *Home) poolSpec(text string, width, height int, _ string, format logo.OutputFormat) coverpool.Spec {
	return coverpool.Spec{Text: text, Width: width, Height: height, Background: "", Format: coverpool.ImageFormat(format)}
}

// servePoolHit runs before the image semaphore in Image and ImageKey. Reading
// a finished cover is cheap and must not wait behind CPU-heavy generation.
func (h *Home) servePoolHit(w http.ResponseWriter, text string, width, height int, file string, format logo.OutputFormat) ([]byte, bool) {
	if !h.poolForSize(width, height) {
		return nil, false
	}
	spec := h.poolSpec(text, width, height, file, format)
	data, ok, err := h.Pool.Get(spec)
	if err != nil {
		h.Logger.Warn("cover pool read failed; regenerating", "err", err)
	}
	if !ok {
		return nil, false
	}
	h.PoolScheduler.Ensure(spec, h.PoolConfig.Count())
	writeCover(w, data, format.ContentType())
	h.SizeStats.RecordPoolHit()
	return data, true
}

func (h *Home) writePooledResult(w http.ResponseWriter, text string, width, height int, file string, format logo.OutputFormat, data []byte) {
	if err := h.storePooledResult(w, text, width, height, file, format, data); err != nil && h.Logger != nil {
		h.Logger.Warn("cover pool write failed; serving generated image", "err", err)
	}
}

func (h *Home) storePooledResult(w http.ResponseWriter, text string, width, height int, file string, format logo.OutputFormat, data []byte) error {
	if h.poolForSize(width, height) {
		spec := h.poolSpec(text, width, height, file, format)
		if err := h.Pool.Put(spec, 0, data); err != nil {
			return err
		}
		if err := h.Pool.SaveMeta(spec); err != nil {
			h.Logger.Warn("cover pool metadata write failed", "err", err)
		}
		h.PoolScheduler.Ensure(spec, h.PoolConfig.Count())
	}
	writeCover(w, data, format.ContentType())
	return nil
}

// Generate implements coverpool.Generator. Every fill shares the request image
// semaphore and cannot bypass the service's CPU/memory protection.
func (h *Home) Generate(ctx context.Context, spec coverpool.Spec, slot int) error {
	if !h.poolEnabled() {
		return fmt.Errorf("coverpool generator is not configured")
	}
	release, ok := h.acquireBackgroundImageSlot(ctx)
	if !ok {
		return fmt.Errorf("image slot unavailable")
	}
	defer release()
	result, err := h.Creater.CreateBytesImage(spec.Text, spec.Width, spec.Height, spec.Background, logo.OutputFormat(spec.Format))
	if err != nil {
		return err
	}
	return h.Pool.Put(spec, slot, result.Data)
}

func (h *Home) acquireBackgroundImageSlot(ctx context.Context) (func(), bool) {
	if h.ImageSem == nil {
		return func() {}, true
	}
	if err := ctx.Err(); err != nil {
		return nil, false
	}
	timer := time.NewTimer(backgroundImageWait)
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

func writeCover(w http.ResponseWriter, data []byte, contentType string) {
	w.Header().Set("Cache-Control", coverCacheControl)
	w.Header().Set("Cloudflare-CDN-Cache-Control", cloudflareCacheControl)
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}
