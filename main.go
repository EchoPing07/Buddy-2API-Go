// Buddy 2API Go：CodeBuddy → OpenAI 兼容代理网关（单账号、单二进制、内置 Web 管理面板）。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	slog.Info("启动 Buddy 2API Go", "version", proxy.Version, "data_dir", dataDirAbs)

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
	sched := scheduler.New(cfg, client, st, models)

	session, err := admin.NewSession(dataDirAbs)
	if err != nil {
		slog.Error("初始化会话失败", "error", err)
		os.Exit(1)
	}

	proxyH := proxy.New(client, models, keys, st, cfg)
	adminH := admin.New(cfg, st, toks, client, models, keys, sched, session)

	// 前端：启动时解析模板并预渲染全部页面。
	// app.js 必须排在 alpine.js 之前：alpine.min.js 末尾以 queueMicrotask(Alpine.start())
	// 自启动，微任务会在下一个 defer 脚本执行前清空，Alpine 先加载即在 app() 定义前启动。
	webH, err := web.New(proxy.Version)
	if err != nil {
		slog.Error("初始化前端失败", "error", err)
		os.Exit(1)
	}

	// ── 路由 ──
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	// 全站禁止 MIME 嗅探：页面 / 脚本 / JSON 均由本进程产出且显式声明 Content-Type，
	// 不应由浏览器猜测（否则被嗅探为 HTML 的响应可成为 XSS 载体）。
	r.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
	// 405 统一走 JSON 错误体（与页面、静态资源一致）。必须全局挂载：chi 要求中间件在
	// 路由注册前定义（路由后 Use 会 panic），且 API 路由的 405 同样需统一 body。
	r.Use(methodNotAllowedJSONBody)
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

	// ── 前端（多页 + 软导航：每页独立路径，刷新保持当前页） ──
	// 路由表在 internal/web.Mount 内，测试复用同一份定义。
	//
	// 压缩仅加在页面与静态资源这一支：
	//
	//   - /v1/chat/completions 的流式响应为 text/event-stream，浏览器不支持对其解压，
	//     且压缩中间件会包装 ResponseWriter、改变 Flush 行为，而代理的 SSE 需逐块原样透传。
	//   - 页面与 app.js/app.css 均为 52KB 级文本，gzip 后约为 1/4；alpine.js 体积大但走
	//     1 天强缓存，一并压缩。
	//
	// Range 与压缩冲突：ServeContent 按未压缩长度返回 Content-Range，而压缩会连同 206 的
	// body 一并 gzip，使实体与 Content-Range 不符。带 Range 的请求绕过压缩，见 skipCompressOnRange。
	r.Group(func(r chi.Router) {
		// skipCompressOnRange 必须排在 Compress 之前：chi 按注册顺序执行中间件，Compress 在
		// 调用下游前已根据 Accept-Encoding 选好编码器，排在其后删头无效。
		r.Use(skipCompressOnRange)
		r.Use(middleware.Compress(5, "text/html", "text/css", "text/javascript", "application/javascript", "application/json", "image/svg+xml"))
		webH.Mount(r)
	})
	r.NotFound(func(w http.ResponseWriter, req *http.Request) { web.NotFoundJSON(w) })

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

// skipCompressOnRange 让带 Range 的请求绕过压缩（见挂载处说明）。
//
// 从 Accept-Encoding 中移除 gzip/deflate：Compress 仅依据该头选择编码器，移除后不包装
// writer（不置 Content-Encoding/Vary，也不改 Content-Length），ServeContent 的 206 与实体
// 保持一致。中间件内判断「响应是否已为 206」不可行：写出前就需选定 writer。
func skipCompressOnRange(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			r.Header.Del("Accept-Encoding")
		}
		next.ServeHTTP(w, r)
	})
}

// methodNotAllowedJSONBody 将 chi 默认 405 的空 body 替换为全站统一的 JSON 错误体，
// 同时保留 chi 算出的 Allow。
//
// 不直接设置自定义 MethodNotAllowed 处理器：chi 的默认 405 处理器会先写 Allow（每个允许的
// 方法一条）、再 WriteHeader(405)、最后 Write(nil)，而设置自定义处理器后 chi 不再预先写
// Allow（见 chi mux.go：非默认时直接调用自定义 handler）。故保留默认处理器，用一层包装器
// 拦截 405 的头部提交并自行补 JSON body——此时 Allow 已在 header map 中，会随响应发出。
//
// 包装器虽只处理 405，仍会包住全部响应，故须原样透传 ResponseWriter 的可选能力
// （Flusher / Hijacker / ReaderFrom / Pusher）：
//
//   - /v1/chat/completions 的 SSE 逐块透传依赖 w.(http.Flusher) 直接断言；
//   - requestLogger 的 NewWrapResponseWriter 亦依据直接断言选择代理类型。
//
// Go 1.20+ 的 http.ResponseController 可沿 Unwrap 链查找能力，但业务代码使用直接断言
// （见 internal/proxy/proxy.go 的 `w.(http.Flusher)`），故此处需显式补齐。
func methodNotAllowedJSONBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&methodNotAllowedJSONWriter{ResponseWriter: w}, r)
	})
}

// methodNotAllowedBody 取 internal/web 的同一份错误体字面量，不在此重复定义。
//
// 只能取其字符串而不能直接调用 web.MethodNotAllowedJSON：后者自带 WriteHeader，
// 而此处须在写好 Allow 头之后、于同一次 WriteHeader 调用中写 body。
// TestMethodNotAllowedJSONAppWide 锁定两处一致。
func methodNotAllowedBody() string { return web.MethodNotAllowedBodyJSON() }

type methodNotAllowedJSONWriter struct {
	http.ResponseWriter
	is405       bool // 已确定为 405：需自写 JSON body，并吞掉 chi 的空 body
	wroteHeader bool
}

// WriteHeader 405 时自写 JSON 错误体，其余状态码原样透传。
//
// 在此处写 body 的原因：chi 的 405 路径为「写 Allow → WriteHeader(405) → Write(nil)」。
// 若仅在 handler 返回后补 body，此时头已提交，需绕过自身包装且 WriteHeader 已无效。
func (m *methodNotAllowedJSONWriter) WriteHeader(code int) {
	if code == http.StatusMethodNotAllowed {
		// 此时 chi 已将 Allow 写入 header map（每个允许的方法 Add 一条），归一为单条
		// （重复头合法，但 "GET, HEAD" 可读性更好）。
		if vals := m.Header().Values("Allow"); len(vals) > 1 {
			m.Header().Del("Allow")
			m.Header().Set("Allow", strings.Join(vals, ", "))
		}
		m.Header().Set("Content-Type", "application/json; charset=utf-8")
		m.is405 = true
		m.wroteHeader = true
		m.ResponseWriter.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = m.ResponseWriter.Write([]byte(methodNotAllowedBody()))
		return
	}
	m.writeHeaderOnce(code)
}

func (m *methodNotAllowedJSONWriter) writeHeaderOnce(code int) {
	if m.wroteHeader {
		return
	}
	m.wroteHeader = true
	m.ResponseWriter.WriteHeader(code)
}

// Write 只在 405 时吞掉 chi 的空 body；其余响应（包括 SSE 的逐块写入）原样透传。
func (m *methodNotAllowedJSONWriter) Write(p []byte) (int, error) {
	if m.is405 {
		return len(p), nil
	}
	m.writeHeaderOnce(http.StatusOK)
	return m.ResponseWriter.Write(p)
}

// Flush 透传给底层（SSE 逐块刷出依赖它）。
func (m *methodNotAllowedJSONWriter) Flush() {
	m.writeHeaderOnce(http.StatusOK)
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 透传（WebSocket / 裸 TCP 升级场景）。
func (m *methodNotAllowedJSONWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := m.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// ReadFrom 透传（sendfile 快速路径，否则 io.Copy 退化为用户态拷贝）。
func (m *methodNotAllowedJSONWriter) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := m.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(m.ResponseWriter, src)
}

// Push 透传（HTTP/2 server push）。
func (m *methodNotAllowedJSONWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := m.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap 供 http.ResponseController 与中间件链回溯。
func (m *methodNotAllowedJSONWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

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
