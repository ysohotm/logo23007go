package admin

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

//go:embed static/*
var ytmAdminFS embed.FS

var adminFiles = map[string]string{
	"":             "static/index.html",
	"index.html":   "static/index.html",
	"ipguard.html": "static/ipguard.html",
	"blocked.html": "static/blocked.html",
	"traffic.html": "static/traffic.html",
	"covers.html":  "static/covers.html",
	"account.html": "static/account.html",
	"common.css":   "static/common.css",
	"common.js":    "static/common.js",
}

func MountUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /ytmadmin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ytmadmin/", http.StatusFound)
	})
	mux.HandleFunc("GET /ytmadmin/", serveAdmin)
}

func serveAdmin(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/ytmadmin/")), "/")
	file, ok := adminFiles[rel]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := ytmAdminFS.ReadFile(file)
	if err != nil {
		http.Error(w, "admin ui missing", http.StatusInternalServerError)
		return
	}
	switch {
	case strings.HasSuffix(file, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(file, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write(b)
}

// IsExemptPath 后台页面与 API 不受 ipguard 限制（API 自身需登录）。
func IsExemptPath(p string) bool {
	return p == "/ytmadmin" || strings.HasPrefix(p, "/ytmadmin/") ||
		p == "/api" || strings.HasPrefix(p, "/api/")
}
