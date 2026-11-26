package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"logo23007go/internal/admin"
	"logo23007go/internal/coverpool"
	"logo23007go/internal/db"
	"logo23007go/internal/handlers"
	"logo23007go/internal/ipguard"
	"logo23007go/internal/logo"
	"logo23007go/internal/sysmetrics"
)

const defaultBgCacheSizes = "300x400,600x800,240x300,150x210"

func main() {
	// warmbg 子命令只做磁盘预热，必须在连接 Mongo 之前分流
	if len(os.Args) > 1 && os.Args[1] == "warmbg" {
		os.Exit(runWarmBg(os.Args[2:]))
	}

	if os.Getenv("DBCONN") == "" {
		_ = os.Setenv("DBCONN", "mongodb://sa:d4485920D%24@mubdev:19011/?serverSelectionTimeoutMS=5000&connectTimeoutMS=10000&authSource=admin&authMechanism=SCRAM-SHA-256")
		_ = os.Setenv("DBNAME", "cachelogomaker")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	root, err := contentRoot()
	if err != nil {
		logger.Error("content root", "err", err)
		os.Exit(1)
	}

	siteDB, err := db.NewSiteDbNoPing(context.Background())
	if err != nil {
		logger.Error("mongo connect failed", "err", err)
		os.Exit(1)
	}
	// 控制面（后台鉴权、ipguard）独立小连接池，避免被图片流量耗尽
	ctrlDB, err := db.NewSiteDbWithPool(context.Background(), 4)
	if err != nil {
		logger.Error("mongo ctrl connect failed", "err", err)
		os.Exit(1)
	}
	idxCtx, idxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	siteDB.CreateIndex(idxCtx)
	authStore := admin.MongoAuth{
		Users:    ctrlDB.Collection("admin_user"),
		Sessions: ctrlDB.Collection("admin_sessions"),
	}
	if err := authStore.EnsureIndexes(idxCtx); err != nil {
		logger.Warn("admin session index", "err", err)
	}
	idxCancel()

	flushCtx, flushCancel := context.WithCancel(context.Background())
	defer flushCancel()
	siteDB.StartUseCountFlusher(flushCtx, 10*time.Second)

	guard := ipguard.New(ipguard.MongoStore{Coll: ctrlDB.Collection("ipguard_config")}, logger)
	guardCtx, guardCancel := context.WithCancel(context.Background())
	defer guardCancel()
	go guard.Run(guardCtx, 30*time.Second)

	creater, err := logo.NewCreater(root)
	if err != nil {
		logger.Error("init logo creater", "err", err)
		os.Exit(1)
	}
	bgDir := bgCacheDir(root)
	bgSizesStr := os.Getenv("BG_CACHE_SIZES")
	if strings.TrimSpace(bgSizesStr) == "" {
		bgSizesStr = defaultBgCacheSizes
	}
	bgSizes, err := logo.ParseSizes(bgSizesStr)
	if err != nil {
		logger.Warn("invalid BG_CACHE_SIZES, using default", "value", bgSizesStr, "err", err)
		bgSizesStr = defaultBgCacheSizes
		bgSizes, _ = logo.ParseSizes(bgSizesStr)
	}
	if err := creater.EnableBgCache(logo.BgCacheConfig{Dir: bgDir, Sizes: bgSizes}); err != nil {
		logger.Warn("bg cache disabled", "err", err)
	} else {
		logger.Info("bg cache", "dir", bgDir, "sizes", bgSizesStr)
	}
	files := creater.GetImageFilesWithCache()
	fmt.Println("image files count:", len(files))

	imageLimit := envInt("IMAGE_CONCURRENCY", runtime.GOMAXPROCS(0))
	logger.Info("image concurrency", "limit", imageLimit)
	poolConfig := coverpool.NewConfig(&db.CoverPoolStore{Coll: ctrlDB.Collection("coverpool_config")}, logger)
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 3*time.Second)
	poolConfig.Load(loadCtx)
	loadCancel()
	go poolConfig.Run(guardCtx)
	var pool *coverpool.Pool
	poolDir := ""
	if !strings.EqualFold(os.Getenv("COVER_POOL_DIR"), "off") {
		poolDir = os.Getenv("COVER_POOL_DIR")
		if poolDir == "" {
			poolDir = filepath.Join(root, "app_data", "cache", "covers")
		}
		pool = coverpool.NewPool(poolDir)
	}
	h := &handlers.Home{
		Logger:     logger,
		Root:       root,
		DB:         siteDB,
		Creater:    creater,
		ImageSem:   make(chan struct{}, imageLimit),
		ImageWait:  envDuration("IMAGE_WAIT", 0),
		Pool:       pool,
		PoolConfig: poolConfig,
	}
	var poolCancel context.CancelFunc
	if pool != nil {
		var poolCtx context.Context
		poolCtx, poolCancel = context.WithCancel(context.Background())
		h.PoolScheduler = coverpool.NewScheduler(poolCtx, pool, envInt("COVER_POOL_WORKERS", 2), envInt("COVER_POOL_QUEUE", 256), h, logger)
		logger.Info("cover pool", "dir", poolDir, "count", poolConfig.Count(), "sizes", coverpool.FormatSizes(poolConfig.Sizes()))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.Index)
	// 统一小写路由；/Home/... 由 canonicalizeHomePath 归一化后命中
	mux.HandleFunc("GET /home/index", h.Index)
	mux.HandleFunc("GET /home/privacy", h.Privacy)
	mux.HandleFunc("GET /home/error", h.Error)
	mux.HandleFunc("GET /home/contact", h.Contact)
	mux.HandleFunc("GET /home/logo", h.Logo)
	mux.HandleFunc("GET /home/addimagekey", h.AddImageKey)
	mux.HandleFunc("GET /home/imagekey", h.ImageKey)
	mux.HandleFunc("GET /home/image", h.Image)
	mux.HandleFunc("GET /home/image/{text...}", h.Image)
	mux.HandleFunc("GET /vod/{text...}", h.Image)
	mux.HandleFunc("POST /home/upload", h.Upload)
	mux.HandleFunc("GET /home/fileisexits", h.FileIsExits)

	fs := http.FileServer(http.Dir(filepath.Join(root, "wwwroot")))
	mux.Handle("GET /static/", http.StripPrefix("/static/", fs))

	keywordStore := &db.CoverKeywordStore{Coll: ctrlDB.Collection("cover_keywords")}
	adminAPI := &admin.API{Auth: authStore, Guard: guard, CoverPool: poolConfig, Keywords: keywordStore, Pool: pool, Sys: sysmetrics.New()}
	if pool != nil {
		adminAPI.Prewarm = &coverpool.Prewarmer{
			Config:    poolConfig,
			Pool:      pool,
			Scheduler: h.PoolScheduler,
			Generate:  h,
		}
	}
	adminAPI.Mount(mux)
	admin.MountUI(mux)

	go memoryMonitor(logger, siteDB)

	addr := ":23107"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	if p := os.Getenv("ASPNETCORE_URLS"); p != "" {
		// ignore complex URLs; prefer PORT
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           guard.Middleware(canonicalizeHomePath(mux), admin.IsExemptPath),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		logger.Info("listening", "addr", addr, "root", root)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_ = srv.Shutdown(shutdownCtx)
	if poolCancel != nil {
		poolCancel()
		h.PoolScheduler.Wait()
	}
	flushCancel()
	if err := siteDB.FlushUseCounts(shutdownCtx); err != nil {
		logger.Warn("flush use counts", "err", err)
	}
	_ = siteDB.Close(shutdownCtx)
	guardCancel()
	_ = ctrlDB.Close(shutdownCtx)
}

// contentRoot 返回内容根目录：默认工作目录，可由 CONTENT_ROOT 覆盖。
func contentRoot() (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if v := os.Getenv("CONTENT_ROOT"); v != "" {
		root = v
	}
	return filepath.Abs(root)
}

// bgCacheDir 读取 BG_CACHE_DIR；"off" 表示禁用（返回空串）。
func bgCacheDir(root string) string {
	v, ok := os.LookupEnv("BG_CACHE_DIR")
	if !ok {
		return filepath.Join(root, "app_data", "cache", "bg")
	}
	if strings.EqualFold(strings.TrimSpace(v), "off") {
		return ""
	}
	return v
}

// runWarmBg 实现 warmbg 子命令：按尺寸预生成背景缓存，不连接 Mongo。
func runWarmBg(args []string) int {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	root, err := contentRoot()
	if err != nil {
		logger.Error("content root", "err", err)
		return 1
	}

	defSizes := os.Getenv("BG_WARM_SIZES")
	if strings.TrimSpace(defSizes) == "" {
		defSizes = "300x400"
	}
	fset := flag.NewFlagSet("warmbg", flag.ContinueOnError)
	sizesFlag := fset.String("sizes", defSizes, "comma-separated WxH sizes to warm, e.g. 300x400,600x800")
	workers := fset.Int("workers", runtime.GOMAXPROCS(0), "number of concurrent workers")
	dir := fset.String("dir", bgCacheDir(root), "bg cache directory")
	if err := fset.Parse(args); err != nil {
		return 1
	}

	sizes, err := logo.ParseSizes(*sizesFlag)
	if err != nil {
		logger.Error("parse sizes", "sizes", *sizesFlag, "err", err)
		return 1
	}
	if len(sizes) == 0 {
		logger.Error("no sizes to warm")
		return 1
	}
	if *dir == "" {
		logger.Error("bg cache dir is empty (BG_CACHE_DIR=off?)")
		return 1
	}

	creater, err := logo.NewCreater(root)
	if err != nil {
		logger.Error("init logo creater", "err", err)
		return 1
	}
	if err := creater.EnableBgCache(logo.BgCacheConfig{Dir: *dir, Sizes: sizes}); err != nil {
		logger.Error("enable bg cache", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	rate := func(st logo.WarmStats) float64 {
		sec := time.Since(start).Seconds()
		if sec <= 0 {
			return 0
		}
		return float64(st.Done+st.Skipped+st.Failed) / sec
	}
	logger.Info("warmbg start", "root", root, "dir", *dir, "sizes", *sizesFlag, "workers", *workers)
	st, err := creater.WarmBgCache(ctx, sizes, *workers, func(st logo.WarmStats) {
		logger.Info("warmbg progress",
			"total", st.Total, "done", st.Done, "skipped", st.Skipped, "failed", st.Failed,
			"per_sec", fmt.Sprintf("%.1f", rate(st)))
	})
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil || ctx.Err() != nil {
		logger.Error("warmbg aborted",
			"err", err, "total", st.Total, "done", st.Done, "skipped", st.Skipped, "failed", st.Failed,
			"elapsed", elapsed)
		return 1
	}
	logger.Info("warmbg finished",
		"total", st.Total, "done", st.Done, "skipped", st.Skipped, "failed", st.Failed,
		"per_sec", fmt.Sprintf("%.1f", rate(st)), "elapsed", elapsed)
	if st.Failed > 0 {
		logger.Warn("warmbg had failures", "failed", st.Failed)
	}
	return 0
}

// envInt 读取正整数环境变量，未设置或非法时返回 def。
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// envDuration 读取 time.ParseDuration 格式的环境变量，未设置或非法时返回 def。
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d < 0 {
		return def
	}
	return d
}

// canonicalizeHomePath 将 /Home/Image、/HOME/image 等归一为 /home/image，
// 仅小写 /home 与 action 段，保留其后的标题文本大小写。
func canonicalizeHomePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) < 5 || !strings.EqualFold(p[:5], "/home") {
			next.ServeHTTP(w, r)
			return
		}
		if len(p) == 5 {
			if p != "/home" {
				next.ServeHTTP(w, cloneReqPath(r, "/home"))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if p[5] != '/' {
			next.ServeHTTP(w, r)
			return
		}
		rest := p[6:]
		action, text, hasText := strings.Cut(rest, "/")
		canon := "/home/" + strings.ToLower(action)
		if hasText {
			canon += "/" + text
		}
		if canon == p {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, cloneReqPath(r, canon))
	})
}

func cloneReqPath(r *http.Request, path string) *http.Request {
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.Path = path
	r2.URL = &u
	return r2
}

// memoryMonitor 按 HeapInuse 判断阈值；Sys 只增不减，不适合作告警依据。
func memoryMonitor(logger *slog.Logger, siteDB *db.SiteDb) {
	const threshold = 1024 * 1024 * 1024
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		ps := siteDB.PoolStats()
		logger.Info("内存监控",
			"alloc_mb", float64(ms.Alloc)/1024/1024,
			"heap_inuse_mb", float64(ms.HeapInuse)/1024/1024,
			"heap_released_mb", float64(ms.HeapReleased)/1024/1024,
			"sys_mb", float64(ms.Sys)/1024/1024,
			"num_gc", ms.NumGC,
			"goroutines", runtime.NumGoroutine(),
			"mongo_open", ps.Open,
			"mongo_checked_out", ps.CheckedOut,
			"mongo_checkout_failed", ps.CheckoutFailed,
		)
		if ms.HeapInuse > threshold {
			logger.Warn("内存超过阈值",
				"heap_inuse_mb", float64(ms.HeapInuse)/1024/1024,
				"threshold_mb", threshold/1024/1024,
			)
		}
	}
}
