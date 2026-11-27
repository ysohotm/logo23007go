package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"logo23007go/internal/coverpool"
	"logo23007go/internal/db"
	"logo23007go/internal/ipguard"
	"logo23007go/internal/sysmetrics"
)

type fakeCoverPoolStore struct {
	settings coverpool.Settings
}

func (f *fakeCoverPoolStore) Get(context.Context) (coverpool.Settings, error) {
	if f.settings.Count == 0 && len(f.settings.Sizes) == 0 {
		return coverpool.DefaultSettings(), nil
	}
	return coverpool.Settings{
		Count:   f.settings.Count,
		Sizes:   append([][2]int(nil), f.settings.Sizes...),
		Workers: f.settings.Workers,
	}, nil
}
func (f *fakeCoverPoolStore) Set(_ context.Context, settings coverpool.Settings) error {
	f.settings = coverpool.Settings{
		Count:   settings.Count,
		Sizes:   append([][2]int(nil), settings.Sizes...),
		Workers: settings.Workers,
	}
	return nil
}

type fakeAuth struct {
	user, pass string
	sessions   map[string]bool
}

func (f *fakeAuth) HasAdmin(context.Context) (bool, error) { return f.user != "", nil }
func (f *fakeAuth) CreateAdmin(_ context.Context, u, p string) error {
	if f.user != "" {
		return ErrAdminExists
	}
	u, err := validateCredentials(u, p)
	if err != nil {
		return err
	}
	f.user, f.pass = u, p
	return nil
}
func (f *fakeAuth) VerifyAdmin(_ context.Context, u, p string) (bool, error) {
	return f.user != "" && u == f.user && p == f.pass, nil
}
func (f *fakeAuth) UpdateAdmin(_ context.Context, u, p string) error {
	u, err := validateCredentials(u, p)
	if err != nil {
		return err
	}
	f.user, f.pass = u, p
	f.sessions = map[string]bool{}
	return nil
}
func (f *fakeAuth) AdminUsername(context.Context) (string, error) { return f.user, nil }
func (f *fakeAuth) CreateSession(context.Context, time.Duration) (string, error) {
	tok, _ := newToken()
	f.sessions[tok] = true
	return tok, nil
}
func (f *fakeAuth) ValidSession(_ context.Context, t string) (bool, error) { return f.sessions[t], nil }
func (f *fakeAuth) DeleteSession(_ context.Context, t string) error {
	delete(f.sessions, t)
	return nil
}

func newTestMux() *http.ServeMux {
	_, mux := newTestAPI()
	return mux
}

func newTestAPI() (*API, *http.ServeMux) {
	mux := http.NewServeMux()
	a := &API{Auth: &fakeAuth{sessions: map[string]bool{}}, Guard: ipguard.New(nil, nil)}
	a.Mount(mux)
	MountUI(mux)
	return a, mux
}

func setupToken(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	code, out := do(t, mux, "POST", "/api/auth/setup", "", credentials{"admin", "secret1"})
	if code != 200 {
		t.Fatalf("setup: %d %v", code, out)
	}
	return out["token"].(string)
}

// hitGuard 以 remote 为对端地址经过 Guard.Middleware 发 n 次请求，返回各状态码。
func hitGuard(a *API, remote string, n int) []int {
	h := a.Guard.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), nil)
	codes := make([]int, n)
	for i := range codes {
		r := httptest.NewRequest("GET", "/vod/x", nil)
		r.RemoteAddr = remote
		r.Header.Set("User-Agent", "Mozilla/5.0 test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		codes[i] = w.Code
	}
	return codes
}

func findGroup(t *testing.T, out map[string]any, subnet string) map[string]any {
	t.Helper()
	recent, _ := out["recent"].(map[string]any)
	groups, _ := recent["groups"].([]any)
	for _, g := range groups {
		if m, _ := g.(map[string]any); m["subnet"] == subnet {
			return m
		}
	}
	return nil
}

// sumTotal 返回 minutes 中 total 之和与最后一个 total。
func sumTotal(t *testing.T, out map[string]any) (sum, last float64, n int) {
	t.Helper()
	ms, ok := out["minutes"].([]any)
	if !ok {
		t.Fatalf("minutes missing: %v", out)
	}
	for _, m := range ms {
		last = m.(map[string]any)["total"].(float64)
		sum += last
	}
	return sum, last, len(ms)
}

func do(t *testing.T, mux *http.ServeMux, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	if token != "" {
		r.Header.Set("X-Session-Token", token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestAuthFlowAndConfig(t *testing.T) {
	mux := newTestMux()

	if code, out := do(t, mux, "GET", "/api/auth/status", "", nil); code != 200 || out["setup_required"] != true {
		t.Fatalf("status: %d %v", code, out)
	}
	if code, _ := do(t, mux, "GET", "/api/ipguard", "", nil); code != 401 {
		t.Fatalf("unauthenticated config should be 401, got %d", code)
	}
	code, out := do(t, mux, "POST", "/api/auth/setup", "", credentials{"admin", "secret1"})
	if code != 200 {
		t.Fatalf("setup: %d %v", code, out)
	}
	tok := out["token"].(string)
	if code, _ := do(t, mux, "POST", "/api/auth/setup", "", credentials{"x", "secret1"}); code != 409 {
		t.Fatalf("second setup should 409, got %d", code)
	}
	if code, _ := do(t, mux, "POST", "/api/auth/login", "", credentials{"admin", "wrong!!"}); code != 401 {
		t.Fatalf("bad login should 401, got %d", code)
	}

	code, out = do(t, mux, "GET", "/api/ipguard", tok, nil)
	if code != 200 || out["config"] == nil || out["defaults"] == nil {
		t.Fatalf("get config: %d %v", code, out)
	}

	code, out = do(t, mux, "PUT", "/api/ipguard", tok, ipguard.Config{Enabled: true, Countries: []string{"bad"}})
	if code != 400 || !strings.Contains(out["error"].(string), "国家码") {
		t.Fatalf("invalid config should 400: %d %v", code, out)
	}
	code, out = do(t, mux, "PUT", "/api/ipguard", tok, ipguard.Config{Enabled: true, Countries: []string{"jp"}, IPs: []string{"1.1.1.1"}})
	if code != 200 {
		t.Fatalf("put config: %d %v", code, out)
	}
	cfg := out["config"].(map[string]any)
	if cfg["countries"].([]any)[0] != "JP" {
		t.Fatalf("countries not normalized: %v", cfg)
	}

	if code, _ := do(t, mux, "GET", "/api/ipguard/stats", tok, nil); code != 200 {
		t.Fatalf("stats: %d", code)
	}
	if code, out := do(t, mux, "GET", "/api/ipguard/check", tok, nil); code != 200 || out["ip"] == nil {
		t.Fatalf("check: %d %v", code, out)
	}

	do(t, mux, "POST", "/api/auth/logout", tok, nil)
	if code, _ := do(t, mux, "GET", "/api/ipguard", tok, nil); code != 401 {
		t.Fatalf("after logout should 401, got %d", code)
	}
}

func TestChangeCredentials(t *testing.T) {
	mux := newTestMux()
	_, out := do(t, mux, "POST", "/api/auth/setup", "", credentials{"admin", "secret1"})
	tok := out["token"].(string)

	if code, out := do(t, mux, "GET", "/api/auth/me", tok, nil); code != 200 || out["username"] != "admin" {
		t.Fatalf("me: %d %v", code, out)
	}
	type change struct {
		OldPassword string `json:"old_password"`
		Username    string `json:"username"`
		Password    string `json:"password"`
	}
	if code, _ := do(t, mux, "POST", "/api/auth/change", tok, change{"wrong!!", "boss", "newpass1"}); code != 403 {
		t.Fatalf("wrong old password should 403, got %d", code)
	}
	if code, _ := do(t, mux, "POST", "/api/auth/change", tok, change{"secret1", "boss", "123"}); code != 400 {
		t.Fatalf("short password should 400, got %d", code)
	}
	code, out := do(t, mux, "POST", "/api/auth/change", tok, change{"secret1", "boss", "newpass1"})
	if code != 200 {
		t.Fatalf("change: %d %v", code, out)
	}
	newTok := out["token"].(string)
	if code, _ := do(t, mux, "GET", "/api/ipguard", tok, nil); code != 401 {
		t.Fatalf("old session should be revoked, got %d", code)
	}
	if code, _ := do(t, mux, "GET", "/api/ipguard", newTok, nil); code != 200 {
		t.Fatalf("new session should work, got %d", code)
	}
	if code, _ := do(t, mux, "POST", "/api/auth/login", "", credentials{"boss", "newpass1"}); code != 200 {
		t.Fatalf("login with new credentials failed: %d", code)
	}

	code, out = do(t, mux, "POST", "/api/auth/change", newTok, change{"newpass1", "", "another1"})
	if code != 200 || out["username"] != "boss" {
		t.Fatalf("password-only change should keep username: %d %v", code, out)
	}
}

func TestTrafficAndBlockRequireAuth(t *testing.T) {
	mux := newTestMux()
	if code, _ := do(t, mux, "GET", "/api/ipguard/traffic", "", nil); code != 401 {
		t.Fatalf("unauthenticated traffic should be 401, got %d", code)
	}
	if code, _ := do(t, mux, "POST", "/api/ipguard/block", "", map[string]any{"cidrs": []string{"1.2.3.0/24"}}); code != 401 {
		t.Fatalf("unauthenticated block should be 401, got %d", code)
	}
}

func TestTraffic(t *testing.T) {
	a, mux := newTestAPI()
	tok := setupToken(t, mux)

	before := time.Now().Truncate(time.Minute)
	for _, c := range hitGuard(a, "8.8.8.8:1", 3) {
		if c != 200 {
			t.Fatalf("guard should allow untrusted peer without country, got %d", c)
		}
	}
	code, out := do(t, mux, "GET", "/api/ipguard/traffic?minutes=10", tok, nil)
	if code != 200 {
		t.Fatalf("traffic: %d %v", code, out)
	}
	sum, last, n := sumTotal(t, out)
	if n != 10 || sum != 3 {
		t.Fatalf("minutes: len=%d sum=%v, want 10 / 3", n, sum)
	}
	if time.Now().Truncate(time.Minute).Equal(before) && last != 3 {
		t.Fatalf("last minute total = %v, want 3", last)
	}
	g := findGroup(t, out, "8.8.8.0/24")
	if g == nil || g["count"].(float64) != 3 || g["blacklisted"] != false {
		t.Fatalf("recent group 8.8.8.0/24: %v", out["recent"])
	}

	for q, want := range map[string]int{"": 10, "?minutes=0": 1, "?minutes=999": 60, "?minutes=abc": 10} {
		_, out := do(t, mux, "GET", "/api/ipguard/traffic"+q, tok, nil)
		if _, _, n := sumTotal(t, out); n != want {
			t.Fatalf("traffic%s: len=%d want %d", q, n, want)
		}
	}
	if code, out := do(t, mux, "GET", "/api/ipguard/traffic?limit=-5", tok, nil); code != 200 || findGroup(t, out, "8.8.8.0/24") == nil {
		t.Fatalf("limit<=0 should use default: %d %v", code, out)
	}
}

func TestBlock(t *testing.T) {
	a, mux := newTestAPI()
	tok := setupToken(t, mux)
	hitGuard(a, "8.8.8.8:1", 2)

	code, out := do(t, mux, "POST", "/api/ipguard/block", tok, map[string]any{"cidrs": []string{"8.8.8.0/24"}})
	if code != 200 || out["ok"] != true {
		t.Fatalf("block: %d %v", code, out)
	}
	code, out = do(t, mux, "GET", "/api/ipguard", tok, nil)
	if code != 200 {
		t.Fatalf("get config: %d %v", code, out)
	}
	blocks, _ := out["config"].(map[string]any)["block_ips"].([]any)
	found := false
	for _, b := range blocks {
		found = found || b == "8.8.8.0/24"
	}
	if !found {
		t.Fatalf("block_ips missing 8.8.8.0/24: %v", out["config"])
	}
	_, out = do(t, mux, "GET", "/api/ipguard/traffic", tok, nil)
	if g := findGroup(t, out, "8.8.8.0/24"); g == nil || g["blacklisted"] != true {
		t.Fatalf("group should be blacklisted: %v", out["recent"])
	}
	if c := hitGuard(a, "8.8.8.9:1", 1)[0]; c != 403 {
		t.Fatalf("blacklisted subnet should get 403, got %d", c)
	}

	if code, out := do(t, mux, "POST", "/api/ipguard/block", tok, map[string]any{"cidrs": []string{"not-a-cidr"}}); code != 400 || out["error"] == nil {
		t.Fatalf("invalid cidr should 400: %d %v", code, out)
	}
	if code, _ := do(t, mux, "POST", "/api/ipguard/block", tok, map[string]any{"cidrs": []string{}}); code != 400 {
		t.Fatalf("empty cidrs should 400, got %d", code)
	}
}

func TestStatsResetClearsTraffic(t *testing.T) {
	a, mux := newTestAPI()
	tok := setupToken(t, mux)
	hitGuard(a, "8.8.8.8:1", 2)

	if code, _ := do(t, mux, "POST", "/api/ipguard/stats/reset", tok, nil); code != 200 {
		t.Fatalf("stats reset: %d", code)
	}
	_, out := do(t, mux, "GET", "/api/ipguard/traffic?minutes=10", tok, nil)
	if sum, last, _ := sumTotal(t, out); sum != 0 || last != 0 {
		t.Fatalf("after reset totals should be 0, sum=%v last=%v", sum, last)
	}
	if g := findGroup(t, out, "8.8.8.0/24"); g != nil {
		t.Fatalf("recent should be empty after reset: %v", out["recent"])
	}
}

func TestCoverPoolConfig(t *testing.T) {
	mux := http.NewServeMux()
	store := &fakeCoverPoolStore{}
	a := &API{
		Auth:      &fakeAuth{sessions: map[string]bool{}},
		CoverPool: coverpool.NewConfig(store, nil),
	}
	a.Mount(mux)

	if code, _ := do(t, mux, "GET", "/api/cover-pool", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated cover pool should be 401, got %d", code)
	}
	tok := setupToken(t, mux)

	assertConfig := func(method string, body any, wantCount, wantWorkers int, wantSizes ...string) {
		t.Helper()
		code, out := do(t, mux, method, "/api/cover-pool", tok, body)
		config, _ := out["config"].(map[string]any)
		if code != http.StatusOK || out["ok"] != true || config["count"] != float64(wantCount) {
			t.Fatalf("%s cover pool: %d %v, want count %d", method, code, out, wantCount)
		}
		if config["workers"] != float64(wantWorkers) {
			t.Fatalf("%s workers = %v, want %d", method, config["workers"], wantWorkers)
		}
		gotSizes, _ := config["sizes"].([]any)
		if len(gotSizes) != len(wantSizes) {
			t.Fatalf("%s sizes = %v, want %v", method, gotSizes, wantSizes)
		}
		for i, want := range wantSizes {
			if gotSizes[i] != want {
				t.Fatalf("%s sizes[%d] = %v, want %s", method, i, gotSizes[i], want)
			}
		}
	}
	assertConfig("GET", nil, 10, coverpool.DefaultSettings().Workers, "600x800")
	assertConfig("PUT", map[string]any{"count": 20, "sizes": []string{"600x800", "300x400"}, "workers": 8}, 20, 8, "600x800", "300x400")
	if store.settings.Count != 20 || len(store.settings.Sizes) != 2 || store.settings.Workers != 8 {
		t.Fatalf("stored = %+v", store.settings)
	}
	assertConfig("GET", nil, 20, 8, "600x800", "300x400")

	for _, count := range []int{0, 101} {
		code, out := do(t, mux, "PUT", "/api/cover-pool", tok, map[string]any{"count": count, "sizes": []string{"600x800"}})
		err, _ := out["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(err, "1") || !strings.Contains(err, "100") {
			t.Fatalf("count %d should be rejected: %d %v", count, code, out)
		}
	}
	for _, workers := range []int{-1, 33} {
		code, out := do(t, mux, "PUT", "/api/cover-pool", tok, map[string]any{"count": 10, "sizes": []string{"600x800"}, "workers": workers})
		if code != http.StatusBadRequest || out["error"] == nil {
			t.Fatalf("workers %d should be rejected: %d %v", workers, code, out)
		}
	}
	if code, out := do(t, mux, "PUT", "/api/cover-pool", tok, map[string]any{"count": 10, "sizes": []string{"bad"}}); code != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("bad size should 400: %d %v", code, out)
	}
	assertConfig("GET", nil, 20, 8, "600x800", "300x400")
}

func TestCoverPoolConfigUnavailable(t *testing.T) {
	mux := newTestMux()
	tok := setupToken(t, mux)
	for _, method := range []string{"GET", "PUT"} {
		var body any
		if method == "PUT" {
			body = map[string]any{"count": 10}
		}
		if code, out := do(t, mux, method, "/api/cover-pool", tok, body); code != http.StatusServiceUnavailable {
			t.Fatalf("%s unavailable cover pool: %d %v, want 503", method, code, out)
		}
	}
	if code, out := do(t, mux, "POST", "/api/cover-pool/generate", tok, map[string]any{"keywords": []string{"词"}}); code != http.StatusServiceUnavailable {
		t.Fatalf("generate unavailable cover pool: %d %v, want 503", code, out)
	}
}

type stubCoverGen struct {
	pool *coverpool.Pool
}

var testCoverWebP = []byte{'R', 'I', 'F', 'F', 4, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}

func (g *stubCoverGen) Generate(_ context.Context, spec coverpool.Spec, slot int) error {
	return g.pool.Put(spec, slot, testCoverWebP)
}

type fakeKeywords struct {
	texts []string
}

func (f *fakeKeywords) UpsertMany(_ context.Context, texts []string) error {
	seen := make(map[string]struct{}, len(f.texts))
	for _, t := range f.texts {
		seen[t] = struct{}{}
	}
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		f.texts = append(f.texts, t)
	}
	return nil
}

func (f *fakeKeywords) List(context.Context) ([]string, error) {
	return append([]string(nil), f.texts...), nil
}

func (f *fakeKeywords) ListPage(_ context.Context, page, pageSize int, q string) (db.KeywordListResult, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	} else if pageSize > 200 {
		pageSize = 200
	}
	q = strings.TrimSpace(q)
	filtered := make([]string, 0, len(f.texts))
	for _, t := range f.texts {
		if q == "" || strings.Contains(t, q) {
			filtered = append(filtered, t)
		}
	}
	total := int64(len(filtered))
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	texts := append([]string{}, filtered[start:end]...)
	if texts == nil {
		texts = []string{}
	}
	return db.KeywordListResult{Texts: texts, Total: total, Page: page, PageSize: pageSize}, nil
}

func TestCoverPoolGenerate(t *testing.T) {
	muxUnauth := newTestMux()
	if code, _ := do(t, muxUnauth, "POST", "/api/cover-pool/generate", "", map[string]any{"keywords": []string{"词"}}); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated generate should be 401, got %d", code)
	}

	muxNoPrewarm := http.NewServeMux()
	aNoPrewarm := &API{
		Auth:      &fakeAuth{sessions: map[string]bool{}},
		CoverPool: coverpool.NewConfig(&fakeCoverPoolStore{}, nil),
	}
	aNoPrewarm.Mount(muxNoPrewarm)
	tokNoPrewarm := setupToken(t, muxNoPrewarm)
	if code, out := do(t, muxNoPrewarm, "POST", "/api/cover-pool/generate", tokNoPrewarm, map[string]any{"keywords": []string{"词"}}); code != http.StatusServiceUnavailable {
		t.Fatalf("CoverPool without Prewarm should 503: %d %v", code, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := coverpool.NewPool(t.TempDir())
	store := &fakeCoverPoolStore{}
	cfg := coverpool.NewConfig(store, nil)
	if err := cfg.Set(ctx, coverpool.Settings{Count: 3, Sizes: [][2]int{{600, 800}}}); err != nil {
		t.Fatal(err)
	}
	gen := &stubCoverGen{pool: pool}
	scheduler := coverpool.NewScheduler(ctx, pool, 1, 8, gen, nil)
	defer func() {
		cancel()
		scheduler.Wait()
	}()
	kw := &fakeKeywords{}
	mux := http.NewServeMux()
	a := &API{
		Auth:      &fakeAuth{sessions: map[string]bool{}},
		CoverPool: cfg,
		Keywords:  kw,
		Pool:      pool,
		Prewarm: &coverpool.Prewarmer{
			Config:    cfg,
			Pool:      pool,
			Scheduler: scheduler,
			Generate:  gen,
		},
	}
	a.Mount(mux)
	tok := setupToken(t, mux)

	if code, out := do(t, mux, "POST", "/api/cover-pool/generate", tok, map[string]any{"keywords": []string{}}); code != http.StatusBadRequest {
		t.Fatalf("empty keywords should 400: %d %v", code, out)
	}
	if code, out := do(t, mux, "POST", "/api/cover-pool/generate", tok, map[string]any{"keywords": []string{"  ", ""}}); code != http.StatusBadRequest {
		t.Fatalf("whitespace-only keywords should 400: %d %v", code, out)
	}

	code, out := do(t, mux, "POST", "/api/cover-pool/generate", tok, map[string]any{
		"keywords": []string{"词A", "词A", "  "},
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("generate: %d %v", code, out)
	}
	result, _ := out["result"].(map[string]any)
	if result["keywords"] != float64(1) {
		t.Fatalf("result.keywords = %v, want 1", result["keywords"])
	}
	accepted, _ := result["accepted"].(float64)
	if accepted < 1 {
		t.Fatalf("result.accepted = %v, want >= 1", accepted)
	}
	if len(kw.texts) != 1 || kw.texts[0] != "词A" {
		t.Fatalf("Keywords.UpsertMany = %v, want [词A]", kw.texts)
	}
}

func TestCoverPoolProgress(t *testing.T) {
	muxUnauth := newTestMux()
	if code, _ := do(t, muxUnauth, "GET", "/api/cover-pool/progress", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated progress should be 401, got %d", code)
	}

	muxNil := http.NewServeMux()
	aNil := &API{Auth: &fakeAuth{sessions: map[string]bool{}}}
	aNil.Mount(muxNil)
	tokNil := setupToken(t, muxNil)
	code, out := do(t, muxNil, "GET", "/api/cover-pool/progress", tokNil, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("nil Prewarm progress: %d %v", code, out)
	}
	progNil, _ := out["progress"].(map[string]any)
	if progNil["workers"] != float64(2) {
		t.Fatalf("nil Prewarm progress.workers = %v, want 2", progNil["workers"])
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := coverpool.NewPool(t.TempDir())
	cfg := coverpool.NewConfig(&fakeCoverPoolStore{}, nil)
	gen := &stubCoverGen{pool: pool}
	const queueCap = 8
	scheduler := coverpool.NewScheduler(ctx, pool, 1, queueCap, gen, nil)
	defer func() {
		cancel()
		scheduler.Wait()
	}()
	mux := http.NewServeMux()
	a := &API{
		Auth:      &fakeAuth{sessions: map[string]bool{}},
		CoverPool: cfg,
		Pool:      pool,
		Prewarm: &coverpool.Prewarmer{
			Config:    cfg,
			Pool:      pool,
			Scheduler: scheduler,
			Generate:  gen,
		},
	}
	a.Mount(mux)
	tok := setupToken(t, mux)

	code, out = do(t, mux, "GET", "/api/cover-pool/progress", tok, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("progress: %d %v", code, out)
	}
	prog, ok := out["progress"].(map[string]any)
	if !ok {
		t.Fatalf("progress missing or wrong type: %v", out["progress"])
	}
	for _, key := range []string{"queue_len", "queue_cap", "inflight", "active", "workers", "worker_procs", "completed", "failed", "busy"} {
		if _, exists := prog[key]; !exists {
			t.Fatalf("progress missing field %q: %v", key, prog)
		}
	}
	if prog["queue_cap"] != float64(queueCap) {
		t.Fatalf("progress.queue_cap = %v, want %d", prog["queue_cap"], queueCap)
	}
	if prog["workers"] != float64(1) {
		t.Fatalf("progress.workers = %v, want 1", prog["workers"])
	}
	if _, ok := prog["busy"].(bool); !ok {
		t.Fatalf("progress.busy should be bool, got %T %v", prog["busy"], prog["busy"])
	}
	if _, ok := prog["completed"].(float64); !ok {
		t.Fatalf("progress.completed should be number, got %T %v", prog["completed"], prog["completed"])
	}
}

func TestCoverPoolKeywordsList(t *testing.T) {
	muxUnauth := newTestMux()
	if code, _ := do(t, muxUnauth, "GET", "/api/cover-pool/keywords", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated keywords list should be 401, got %d", code)
	}

	muxEmpty := http.NewServeMux()
	aEmpty := &API{
		Auth:      &fakeAuth{sessions: map[string]bool{}},
		CoverPool: coverpool.NewConfig(&fakeCoverPoolStore{}, nil),
	}
	aEmpty.Mount(muxEmpty)
	tokEmpty := setupToken(t, muxEmpty)
	code, out := do(t, muxEmpty, "GET", "/api/cover-pool/keywords", tokEmpty, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("empty keywords (nil registry): %d %v", code, out)
	}
	kws, _ := out["keywords"].([]any)
	if len(kws) != 0 {
		t.Fatalf("keywords = %v, want empty", kws)
	}
	if out["total"] != float64(0) || out["page"] != float64(1) || out["page_size"] != float64(50) {
		t.Fatalf("empty page fields = %v", out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := coverpool.NewPool(t.TempDir())
	store := &fakeCoverPoolStore{}
	cfg := coverpool.NewConfig(store, nil)
	if err := cfg.Set(ctx, coverpool.Settings{Count: 3, Sizes: [][2]int{{600, 800}}}); err != nil {
		t.Fatal(err)
	}
	gen := &stubCoverGen{pool: pool}
	scheduler := coverpool.NewScheduler(ctx, pool, 1, 8, gen, nil)
	defer func() {
		cancel()
		scheduler.Wait()
	}()
	kw := &fakeKeywords{}
	mux := http.NewServeMux()
	a := &API{
		Auth:      &fakeAuth{sessions: map[string]bool{}},
		CoverPool: cfg,
		Keywords:  kw,
		Pool:      pool,
		Prewarm: &coverpool.Prewarmer{
			Config:    cfg,
			Pool:      pool,
			Scheduler: scheduler,
			Generate:  gen,
		},
	}
	a.Mount(mux)
	tok := setupToken(t, mux)

	code, out = do(t, mux, "POST", "/api/cover-pool/generate", tok, map[string]any{
		"keywords": []string{"词B"},
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("generate: %d %v", code, out)
	}

	code, out = do(t, mux, "GET", "/api/cover-pool/keywords?page=1&page_size=50", tok, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("list keywords: %d %v", code, out)
	}
	if out["total"] != float64(1) || out["page"] != float64(1) || out["page_size"] != float64(50) {
		t.Fatalf("page fields = total=%v page=%v page_size=%v", out["total"], out["page"], out["page_size"])
	}
	list, _ := out["keywords"].([]any)
	if len(list) != 1 {
		t.Fatalf("keywords len = %d, want 1: %v", len(list), out)
	}
	item, _ := list[0].(map[string]any)
	if item["text"] != "词B" {
		t.Fatalf("keyword text = %v, want 词B", item["text"])
	}
	sizes, _ := item["sizes"].([]any)
	if len(sizes) != 1 {
		t.Fatalf("sizes = %v, want 1 entry", sizes)
	}
	sz, _ := sizes[0].(map[string]any)
	if sz["w"] != float64(600) || sz["h"] != float64(800) || sz["target"] != float64(3) {
		t.Fatalf("size progress = %v, want w=600 h=800 target=3", sz)
	}

	kw.texts = []string{"alpha", "beta", "gamma", "alpine"}
	code, out = do(t, mux, "GET", "/api/cover-pool/keywords?page=1&page_size=1&q=alp", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("paged keywords: %d %v", code, out)
	}
	if out["total"] != float64(2) || out["page"] != float64(1) || out["page_size"] != float64(1) {
		t.Fatalf("filter page fields = %v", out)
	}
	list, _ = out["keywords"].([]any)
	if len(list) != 1 {
		t.Fatalf("page size slice = %d, want 1: %v", len(list), out)
	}
	code, out = do(t, mux, "GET", "/api/cover-pool/keywords?page=2&page_size=1&q=alp", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("page 2: %d %v", code, out)
	}
	list, _ = out["keywords"].([]any)
	if len(list) != 1 {
		t.Fatalf("page 2 len = %d, want 1: %v", len(list), out)
	}
}

type memEntryRegistry struct {
	entries []coverpool.PoolEntry
}

func (m *memEntryRegistry) Upsert(_ context.Context, e coverpool.PoolEntry) error {
	for i, existing := range m.entries {
		if existing.ID == e.ID {
			m.entries[i] = e
			return nil
		}
	}
	m.entries = append(m.entries, e)
	return nil
}

func (m *memEntryRegistry) UpsertMany(ctx context.Context, entries []coverpool.PoolEntry) error {
	for _, e := range entries {
		if err := m.Upsert(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (m *memEntryRegistry) Delete(_ context.Context, id string) error {
	out := m.entries[:0]
	for _, e := range m.entries {
		if e.ID != id {
			out = append(out, e)
		}
	}
	m.entries = out
	return nil
}

func (m *memEntryRegistry) SumCount(context.Context, string, int, int, bool) (int, error) {
	return 0, nil
}

func (m *memEntryRegistry) ListBackground(_ context.Context, nonempty bool, limit int) ([]coverpool.PoolEntry, error) {
	var out []coverpool.PoolEntry
	for _, e := range m.entries {
		hasBG := strings.TrimSpace(e.Background) != ""
		if nonempty != hasBG {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func TestCoverPoolCleanupAndSync(t *testing.T) {
	muxUnauth := newTestMux()
	if code, _ := do(t, muxUnauth, "POST", "/api/cover-pool/cleanup-backgrounds", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated cleanup should be 401, got %d", code)
	}
	if code, _ := do(t, muxUnauth, "POST", "/api/cover-pool/sync-registry", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated sync should be 401, got %d", code)
	}

	muxNoPool := newTestMux()
	tokNoPool := setupToken(t, muxNoPool)
	if code, out := do(t, muxNoPool, "POST", "/api/cover-pool/cleanup-backgrounds", tokNoPool, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("nil Pool cleanup should 503: %d %v", code, out)
	}
	if code, out := do(t, muxNoPool, "POST", "/api/cover-pool/sync-registry", tokNoPool, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("nil Pool sync should 503: %d %v", code, out)
	}

	// Pool without Registry: cleanup walks disk and succeeds (0 if no nonempty metas).
	muxNoReg := http.NewServeMux()
	aNoReg := &API{
		Auth: &fakeAuth{sessions: map[string]bool{}},
		Pool: coverpool.NewPool(t.TempDir()),
	}
	aNoReg.Mount(muxNoReg)
	tokNoReg := setupToken(t, muxNoReg)
	if code, out := do(t, muxNoReg, "POST", "/api/cover-pool/cleanup-backgrounds", tokNoReg, nil); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("Pool without Registry cleanup should succeed: %d %v", code, out)
	}

	root := t.TempDir()
	pool := coverpool.NewPool(root)
	reg := &memEntryRegistry{}
	pool.SetRegistry(reg)

	bgDir := filepath.Join(root, "bg-entry")
	emptyDir := filepath.Join(root, "empty-entry")
	if err := os.MkdirAll(bgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bgMeta := []byte(`{"text":"a","width":600,"height":800,"background":"x.webp","format":"webp"}`)
	emptyMeta := []byte(`{"text":"b","width":600,"height":800,"background":"","format":"webp"}`)
	if err := os.WriteFile(filepath.Join(bgDir, "meta.json"), bgMeta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(emptyDir, "meta.json"), emptyMeta, 0o644); err != nil {
		t.Fatal(err)
	}
	bgSpec := coverpool.Spec{Text: "a", Width: 600, Height: 800, Background: "x.webp", Format: coverpool.FormatWebP}
	emptySpec := coverpool.Spec{Text: "b", Width: 600, Height: 800, Background: "", Format: coverpool.FormatWebP}
	bgID := coverpool.SpecID(bgSpec)
	emptyID := coverpool.SpecID(emptySpec)
	reg.entries = []coverpool.PoolEntry{
		{ID: bgID, Text: "a", Width: 600, Height: 800, Background: "x.webp", Format: coverpool.FormatWebP, Dir: bgDir},
		{ID: emptyID, Text: "b", Width: 600, Height: 800, Background: "", Format: coverpool.FormatWebP, Dir: emptyDir},
	}

	mux := http.NewServeMux()
	a := &API{
		Auth: &fakeAuth{sessions: map[string]bool{}},
		Pool: pool,
	}
	a.Mount(mux)
	tok := setupToken(t, mux)

	code, out := do(t, mux, "POST", "/api/cover-pool/cleanup-backgrounds", tok, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("cleanup: %d %v", code, out)
	}
	if out["deleted"] != float64(1) || out["failed"] != float64(0) {
		t.Fatalf("cleanup counts = deleted=%v failed=%v, want 1/0", out["deleted"], out["failed"])
	}
	if _, err := os.Stat(bgDir); !os.IsNotExist(err) {
		t.Fatalf("bg dir should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(emptyDir); err != nil {
		t.Fatalf("empty-bg dir should remain: %v", err)
	}
	if len(reg.entries) != 1 || reg.entries[0].ID != emptyID {
		t.Fatalf("registry after cleanup = %+v, want only empty-bg entry", reg.entries)
	}

	code, out = do(t, mux, "POST", "/api/cover-pool/sync-registry", tok, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("sync: %d %v", code, out)
	}
	if out["upserted"] == nil {
		t.Fatalf("sync missing upserted: %v", out)
	}
}

func TestCoverSizeStats(t *testing.T) {
	muxUnauth := newTestMux()
	if code, _ := do(t, muxUnauth, "GET", "/api/cover-pool/size-stats", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated size-stats should be 401, got %d", code)
	}
	if code, _ := do(t, muxUnauth, "POST", "/api/cover-pool/size-stats/reset", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated size-stats reset should be 401, got %d", code)
	}

	ctx := context.Background()
	store := &fakeCoverPoolStore{}
	cfg := coverpool.NewConfig(store, nil)
	if err := cfg.Set(ctx, coverpool.Settings{Count: 10, Sizes: [][2]int{{600, 800}}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a := &API{
		Auth:      &fakeAuth{sessions: map[string]bool{}},
		CoverPool: cfg,
		SizeStats: coverpool.NewSizeStats(),
	}
	a.Mount(mux)
	tok := setupToken(t, mux)

	code, out := do(t, mux, "GET", "/api/cover-pool/size-stats", tok, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("empty size-stats: %d %v", code, out)
	}
	if out["total"] != float64(0) {
		t.Fatalf("total = %v, want 0", out["total"])
	}
	if out["pool_hits"] != float64(0) || out["live_ok"] != float64(0) {
		t.Fatalf("pool_hits/live_ok = %v/%v, want 0/0", out["pool_hits"], out["live_ok"])
	}
	sizes, _ := out["sizes"].([]any)
	if len(sizes) != 0 {
		t.Fatalf("sizes = %v, want empty", sizes)
	}
	if since, _ := out["since"].(string); since == "" {
		t.Fatalf("since should be set when SizeStats non-nil: %v", out)
	}

	a.SizeStats.Record(600, 800)
	a.SizeStats.Record(600, 800)
	a.SizeStats.Record(300, 400)
	a.SizeStats.RecordPoolHit()
	a.SizeStats.RecordPoolHit()
	a.SizeStats.RecordLive()

	code, out = do(t, mux, "GET", "/api/cover-pool/size-stats", tok, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("size-stats after record: %d %v", code, out)
	}
	if out["total"] != float64(3) {
		t.Fatalf("total = %v, want 3", out["total"])
	}
	if out["pool_hits"] != float64(2) || out["live_ok"] != float64(1) {
		t.Fatalf("pool_hits/live_ok = %v/%v, want 2/1", out["pool_hits"], out["live_ok"])
	}
	sizes, _ = out["sizes"].([]any)
	if len(sizes) != 2 {
		t.Fatalf("sizes len = %d, want 2: %v", len(sizes), sizes)
	}
	top, _ := sizes[0].(map[string]any)
	if top["w"] != float64(600) || top["h"] != float64(800) || top["count"] != float64(2) {
		t.Fatalf("top size = %v, want 600x800 count=2", top)
	}
	if top["pooled"] != true {
		t.Fatalf("600x800 pooled = %v, want true", top["pooled"])
	}
	other, _ := sizes[1].(map[string]any)
	if other["w"] != float64(300) || other["h"] != float64(400) || other["count"] != float64(1) {
		t.Fatalf("second size = %v, want 300x400 count=1", other)
	}
	if other["pooled"] != false {
		t.Fatalf("300x400 pooled = %v, want false", other["pooled"])
	}

	if code, out := do(t, mux, "POST", "/api/cover-pool/size-stats/reset", tok, nil); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("reset: %d %v", code, out)
	}
	code, out = do(t, mux, "GET", "/api/cover-pool/size-stats", tok, nil)
	if code != http.StatusOK || out["total"] != float64(0) {
		t.Fatalf("after reset: %d %v", code, out)
	}
	if out["pool_hits"] != float64(0) || out["live_ok"] != float64(0) {
		t.Fatalf("after reset pool_hits/live_ok = %v/%v, want 0/0", out["pool_hits"], out["live_ok"])
	}
	sizes, _ = out["sizes"].([]any)
	if len(sizes) != 0 {
		t.Fatalf("after reset sizes = %v, want empty", sizes)
	}
}

func TestSystemStats(t *testing.T) {
	mux := http.NewServeMux()
	a := &API{
		Auth: &fakeAuth{sessions: map[string]bool{}},
		Sys:  sysmetrics.New(),
	}
	a.Mount(mux)

	if code, _ := do(t, mux, "GET", "/api/system/stats", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated system stats should be 401, got %d", code)
	}
	tok := setupToken(t, mux)
	code, out := do(t, mux, "GET", "/api/system/stats", tok, nil)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("system stats: %d %v", code, out)
	}
	stats, _ := out["stats"].(map[string]any)
	if stats == nil || stats["mem_total_bytes"] == nil {
		t.Fatalf("stats missing mem_total_bytes: %v", out)
	}
	code, out = do(t, mux, "GET", "/api/system/stats", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("second sample: %d %v", code, out)
	}
	stats, _ = out["stats"].(map[string]any)
	if stats["cpu_percent"] == nil {
		t.Fatalf("second sample should include cpu_percent: %v", stats)
	}

	muxNil := http.NewServeMux()
	aNil := &API{Auth: &fakeAuth{sessions: map[string]bool{}}}
	aNil.Mount(muxNil)
	tok2 := setupToken(t, muxNil)
	if code, out := do(t, muxNil, "GET", "/api/system/stats", tok2, nil); code != http.StatusServiceUnavailable || out["error"] == nil {
		t.Fatalf("nil Sys should 503: %d %v", code, out)
	}
}

func TestServeUI(t *testing.T) {
	mux := newTestMux()
	for _, p := range []string{"/ytmadmin/", "/ytmadmin/ipguard.html", "/ytmadmin/blocked.html", "/ytmadmin/covers.html", "/ytmadmin/system.html", "/ytmadmin/account.html", "/ytmadmin/common.css", "/ytmadmin/common.js"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/ytmadmin/../go.mod", nil))
	if w.Code == 200 {
		t.Fatal("path traversal should not serve")
	}
}

func TestIsExemptPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/ytmadmin": true, "/ytmadmin/x": true, "/api/health": true,
		"/vod/abc": false, "/ytmadminx": false, "/apix": false, "/home/image": false,
	} {
		if IsExemptPath(p) != want {
			t.Fatalf("IsExemptPath(%q) != %v", p, want)
		}
	}
}
