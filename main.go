// Buddy2API-Go：CodeBuddy → OpenAI 兼容代理网关（单账号、单二进制、内置 Web 管理面板）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"buddy2api-go/internal/admin"
	"buddy2api-go/internal/apikey"
	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/proxy"
	"buddy2api-go/internal/scheduler"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
	"buddy2api-go/internal/web"
)

func main() {
	var (
		dataDir = flag.String("data", envOr("BUDDY2API_DATA_DIR", "data"), "数据目录（token.json / config.json / buddy2api.db）")
		showVer = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(proxy.Version)
		return
	}

	// slog 标准日志
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dataDirAbs, err := filepath.Abs(*dataDir)
	if err != nil {
		slog.Error("解析数据目录失败", "error", err)
		os.Exit(1)
	}
	slog.Info("启动 Buddy2API", "version", proxy.Version, "data_dir", dataDirAbs)

	// ── 依赖装配 ──
	cfg, err := config.Load(dataDirAbs)
	if err != nil {
		slog.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	st, err := store.Open(filepath.Join(dataDirAbs, "buddy2api.db"))
	if err != nil {
		slog.Error("打开数据库失败", "error", err)
		os.Exit(1)
	}

	toks, err := auth.NewTokenStore(dataDirAbs)
	if err != nil {
		slog.Error("加载凭证失败", "error", err)
		os.Exit(1)
	}

	client := upstream.New(toks, func() string { return cfg.Get().Region }, cfg.Get().ChatTimeoutSeconds)
	models := upstream.NewModelCache(cfg.Get().Region, client)
	keys := apikey.New(st)
	sched := scheduler.New(cfg, client, st)

	session, err := admin.NewSession(dataDirAbs)
	if err != nil {
		slog.Error("初始化会话失败", "error", err)
		os.Exit(1)
	}

	proxyH := proxy.New(client, models, keys, st, cfg)
	adminH := admin.New(cfg, st, toks, client, models, keys, sched, session)

	// ── 路由 ──
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)

	r.Get("/health", proxyH.Health)

	// OpenAI 兼容业务端点（API Key 鉴权）
	r.Group(func(r chi.Router) {
		r.Use(keys.Authenticate)
		r.Post("/v1/chat/completions", proxyH.Chat)
		r.Get("/v1/models", proxyH.Models)
	})

	// 管理后台 API
	r.Route("/admin", adminH.Routes())

	// 前端
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(web.Index())
	})
	r.Get("/assets/alpine.js", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(web.AlpineJS())
	})
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"not found","type":"invalid_request_error","code":"not_found"}}`))
	})

	// ── HTTP 服务 ──
	listen := cfg.Get().Listen
	srv := &http.Server{
		Addr:              listen,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan struct{}, 1) // ListenAndServe 异常退出信号
	go func() {
		slog.Info("HTTP 服务已启动", "listen", listen, "region", cfg.Get().Region)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP 服务异常退出", "error", err)
			serveErr <- struct{}{}
		}
	}()

	// 优雅退出（收到信号或 HTTP 服务异常退出时走统一关闭路径，确保资源释放）
	fatal := false
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
		slog.Info("正在退出…")
	case <-serveErr:
		fatal = true
		slog.Info("HTTP 服务异常，正在退出…")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	sched.Stop()
	if err := st.Close(); err != nil { // 显式关闭，避免 os.Exit 跳过 defer
		slog.Error("关闭数据库失败", "error", err)
	}
	slog.Info("已退出")
	if fatal {
		os.Exit(1)
	}
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// requestLogger 极简访问日志（不记录请求/响应正文）。
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"ms", time.Since(start).Milliseconds(),
			"bytes", ww.BytesWritten(),
		)
	})
}
