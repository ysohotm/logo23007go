package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"logo23007go/internal/coverpool"
	"logo23007go/internal/ipguard"
)

const sessionTTL = 7 * 24 * time.Hour

// CoverKeywordRegistry stores manually added cover keywords for listing/status.
type CoverKeywordRegistry interface {
	UpsertMany(ctx context.Context, texts []string) error
	List(ctx context.Context) ([]string, error)
}

type API struct {
	Auth      AuthStore
	Guard     *ipguard.Guard
	CoverPool *coverpool.Config
	Prewarm   *coverpool.Prewarmer
	Keywords  CoverKeywordRegistry
	Pool      *coverpool.Pool
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return false
	}
	return true
}

func sessionToken(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("X-Session-Token")); k != "" {
		return k
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func (a *API) authed(w http.ResponseWriter, r *http.Request) bool {
	ok, err := a.Auth.ValidSession(r.Context(), sessionToken(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (a *API) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("GET /api/auth/status", a.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/setup", a.handleAuthSetup)
	mux.HandleFunc("POST /api/auth/login", a.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", a.handleAuthLogout)
	mux.HandleFunc("GET /api/auth/me", a.handleAuthMe)
	mux.HandleFunc("POST /api/auth/change", a.handleAuthChange)
	mux.HandleFunc("GET /api/ipguard", a.handleGetConfig)
	mux.HandleFunc("PUT /api/ipguard", a.handlePutConfig)
	mux.HandleFunc("GET /api/cover-pool", a.handleGetCoverPool)
	mux.HandleFunc("PUT /api/cover-pool", a.handlePutCoverPool)
	mux.HandleFunc("POST /api/cover-pool/generate", a.handleGenerateCoverPool)
	mux.HandleFunc("GET /api/cover-pool/keywords", a.handleListCoverKeywords)
	mux.HandleFunc("GET /api/ipguard/stats", a.handleStats)
	mux.HandleFunc("POST /api/ipguard/stats/reset", a.handleStatsReset)
	mux.HandleFunc("GET /api/ipguard/check", a.handleCheck)
	mux.HandleFunc("GET /api/ipguard/traffic", a.handleTraffic)
	mux.HandleFunc("POST /api/ipguard/block", a.handleBlock)
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	has, err := a.Auth.HasAdmin(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "setup_required": !has})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *API) issueSession(w http.ResponseWriter, r *http.Request, username string) {
	tok, err := a.Auth.CreateSession(r.Context(), sessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": tok, "username": strings.TrimSpace(username)})
}

func (a *API) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := a.Auth.CreateAdmin(r.Context(), body.Username, body.Password); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrAdminExists) {
			code = http.StatusConflict
		}
		writeErr(w, code, err.Error())
		return
	}
	a.issueSession(w, r, body.Username)
}

func (a *API) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !decodeJSON(w, r, &body) {
		return
	}
	ok, err := a.Auth.VerifyAdmin(r.Context(), body.Username, body.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	a.issueSession(w, r, body.Username)
}

func (a *API) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if tok := sessionToken(r); tok != "" {
		_ = a.Auth.DeleteSession(r.Context(), tok)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	name, err := a.Auth.AdminUsername(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": name})
}

// handleAuthChange 校验当前密码后修改用户名/密码；所有旧会话失效，返回新 token。
func (a *API) handleAuthChange(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		Username    string `json:"username"`
		Password    string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	current, err := a.Auth.AdminUsername(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok, err := a.Auth.VerifyAdmin(r.Context(), current, body.OldPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusForbidden, "当前密码错误")
		return
	}
	if strings.TrimSpace(body.Username) == "" {
		body.Username = current
	}
	if body.Password == "" {
		body.Password = body.OldPassword
	}
	if err := a.Auth.UpdateAdmin(r.Context(), body.Username, body.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.issueSession(w, r, body.Username)
}

func (a *API) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config":   a.Guard.Config(),
		"defaults": ipguard.DefaultConfig(),
	})
}

func (a *API) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	var c ipguard.Config
	if !decodeJSON(w, r, &c) {
		return
	}
	saved, err := a.Guard.Save(r.Context(), c)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": saved})
}

func (a *API) handleGetCoverPool(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	if a.CoverPool == nil {
		writeErr(w, http.StatusServiceUnavailable, "关键词图片池配置暂不可用")
		return
	}
	writeCoverPoolConfig(w, a.CoverPool.Snapshot())
}

func (a *API) handlePutCoverPool(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	if a.CoverPool == nil {
		writeErr(w, http.StatusServiceUnavailable, "关键词图片池配置暂不可用")
		return
	}
	var body struct {
		Count int      `json:"count"`
		Sizes []string `json:"sizes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Count < 1 || body.Count > 100 {
		writeErr(w, http.StatusBadRequest, "每个关键词的图片数量需为 1-100 的整数")
		return
	}
	sizes, err := parseCoverPoolSizeList(body.Sizes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.CoverPool.Set(r.Context(), coverpool.Settings{Count: body.Count, Sizes: sizes}); err != nil {
		if strings.Contains(err.Error(), "outside") || strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "too many") || strings.Contains(err.Error(), "too large") {
			writeErr(w, http.StatusBadRequest, "关键词图片池配置无效: "+err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "保存关键词图片池配置失败")
		return
	}
	writeCoverPoolConfig(w, a.CoverPool.Snapshot())
}

func parseCoverPoolSizeList(parts []string) ([][2]int, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	joined := strings.Join(parts, ",")
	sizes, err := coverpool.ParseSizes(joined)
	if err != nil {
		return nil, fmt.Errorf("尺寸格式需为 WIDTHxHEIGHT，例如 600x800")
	}
	return sizes, nil
}

func writeCoverPoolConfig(w http.ResponseWriter, settings coverpool.Settings) {
	sizes := make([]string, 0, len(settings.Sizes))
	for _, size := range settings.Sizes {
		sizes = append(sizes, fmt.Sprintf("%dx%d", size[0], size[1]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"config": map[string]any{
			"count": settings.Count,
			"sizes": sizes,
		},
	})
}

func (a *API) handleGenerateCoverPool(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	if a.Prewarm == nil || a.CoverPool == nil {
		writeErr(w, http.StatusServiceUnavailable, "关键词图片池暂不可用")
		return
	}
	var body struct {
		Keywords []string `json:"keywords"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	normalized := coverpool.NormalizeKeywords(body.Keywords)
	if len(normalized) == 0 {
		writeErr(w, http.StatusBadRequest, "请至少填写一个关键词")
		return
	}
	if a.Keywords != nil {
		if err := a.Keywords.UpsertMany(r.Context(), normalized); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	result := a.Prewarm.Enqueue(r.Context(), body.Keywords)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

func (a *API) handleListCoverKeywords(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	if a.CoverPool == nil {
		writeErr(w, http.StatusServiceUnavailable, "关键词图片池配置暂不可用")
		return
	}
	if a.Keywords == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keywords": []any{}})
		return
	}
	texts, err := a.Keywords.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	statuses := coverpool.StatusFor(a.Pool, a.CoverPool, texts)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keywords": statuses})
}

func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stats":  a.Guard.Stats.Snapshot(),
		"recent": a.Guard.Stats.Recent(limit),
	})
}

func (a *API) handleStatsReset(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	a.Guard.Stats.Reset()
	a.Guard.Traffic.Reset()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTraffic 返回最近 minutes 分钟的按分钟计数，以及最近访问按网段的聚合。
func (a *API) handleTraffic(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	q := r.URL.Query()
	minutes, err := strconv.Atoi(q.Get("minutes"))
	if err != nil {
		minutes = 10
	}
	minutes = max(1, min(minutes, 60))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 300
	}
	limit = min(limit, 10000)
	writeJSON(w, http.StatusOK, map[string]any{
		"minutes": a.Guard.Traffic.Minutes(time.Now(), minutes),
		"recent":  a.Guard.RecentSubnets(limit),
	})
}

// handleBlock 把 IP/CIDR 追加到黑名单并立即生效。
func (a *API) handleBlock(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	var body struct {
		CIDRs []string `json:"cidrs"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	saved, err := a.Guard.AddBlockIPs(r.Context(), body.CIDRs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": saved})
}

// handleCheck 用当前规则评估管理员自己的这次请求（不计入统计）。
func (a *API) handleCheck(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, a.Guard.Decide(r))
}
