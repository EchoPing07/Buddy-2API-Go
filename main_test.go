package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"buddy2api-go/internal/proxy"
	"buddy2api-go/internal/web"
)

// newFrontendRouter 复刻 main.go 前端分支的中间件链（顺序须一致）。
// main() 的装配无可导出入口，故逐行复刻；其中 skipCompressOnRange 须先于 Compress 注册正是守护对象。
func newFrontendRouter(t *testing.T) chi.Router {
	t.Helper()
	h, err := web.New(proxy.Version)
	if err != nil {
		t.Fatalf("web.New() 失败: %v", err)
	}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
	r.Group(func(r chi.Router) {
		r.Use(skipCompressOnRange)
		r.Use(middleware.Compress(5, "text/html", "text/css", "text/javascript", "application/javascript", "application/json", "image/svg+xml"))
		h.Mount(r)
	})
	r.NotFound(func(w http.ResponseWriter, req *http.Request) { web.NotFoundJSON(w) })
	return r
}

// TestCompressSkipsRange 带 Range 的请求不得被压缩：Compress 会连 206 响应体一并 gzip，而 Content-Range
// 仍按未压缩长度声明，字节范围即错误；故 skipCompressOnRange 须先于 Compress 注册（编码器在调用下游前选定）。
func TestCompressSkipsRange(t *testing.T) {
	r := newFrontendRouter(t)

	for _, path := range []string{"/logs", "/assets/app.js", "/assets/app.css"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Range", "bytes=0-99")
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusPartialContent {
			t.Errorf("%s: Range 请求 = %d，期望 206", path, rec.Code)
			continue
		}
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("%s: Range 响应被压缩（Content-Encoding=%q），实体与 Content-Range 不符", path, enc)
		}
		if got := rec.Body.Len(); got != 100 {
			t.Errorf("%s: Range 实体 = %d 字节，期望 100（Content-Range 声明 0-99）", path, got)
		}
		if cr := rec.Header().Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-99/") {
			t.Errorf("%s: Content-Range = %q", path, cr)
		}
	}
}

// TestCompressStillAppliesWithoutRange 不带 Range 时压缩仍须生效，避免修复 Range 时误关压缩。
func TestCompressStillAppliesWithoutRange(t *testing.T) {
	r := newFrontendRouter(t)

	plain := httptest.NewRecorder()
	r.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/logs", nil))

	gz := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/logs", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(gz, req)

	if got := gz.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q，期望 gzip", got)
	}
	if gz.Body.Len() >= plain.Body.Len() {
		t.Errorf("压缩后 %d 字节 >= 未压缩 %d 字节（压缩没生效？）", gz.Body.Len(), plain.Body.Len())
	}
	// 压缩后长度未知：须删除 Content-Length 并声明 Vary，否则中间缓存会误用同一副本
	if cl := gz.Header().Get("Content-Length"); cl != "" {
		t.Errorf("压缩响应不应带 Content-Length（实际 %q）", cl)
	}
	if v := gz.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("Vary = %q，期望包含 Accept-Encoding", v)
	}
}

// TestSSEStreamingNotWrapped 流式端点不得被压缩中间件包装。
// 前端分支的压缩只挂在 webH.Mount 一组上；event-stream 一旦被包装 writer，Flush 语义与逐块透传即遭破坏。
func TestSSEStreamingNotWrapped(t *testing.T) {
	h, err := web.New(proxy.Version)
	if err != nil {
		t.Fatalf("web.New() 失败: %v", err)
	}
	// 与 main.go 同构：压缩组只包前端，不包 /v1
	r := chi.NewRouter()
	r.Use(middleware.Compress(5, "text/html", "text/javascript", "application/json"))
	r.Post("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"x\":1}\n\n"))
	})
	r.Group(func(r chi.Router) { h.Mount(r) })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Compress 仅压缩白名单内的 Content-Type，event-stream 不在其中；
	// 该断言不依赖此巧合：一旦 event-stream 被加入白名单即失败。
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("event-stream 被压缩了（Content-Encoding=%q），会破坏逐块透传", enc)
	}
	if !strings.Contains(rec.Body.String(), "data: ") {
		t.Errorf("SSE body 非原文: %q", rec.Body.String())
	}
}

// TestMethodNotAllowedJSONAppWide 全站 405 返回统一 JSON 错误体，并保留 chi 算出的 Allow。
// 回归：chi 默认 405 只有 Allow 而无 body；自定义 MethodNotAllowed 处理器会丢失 Allow，
// 故实现为包装 ResponseWriter，拦下默认 405 的空 body 并替换为 JSON。
func TestMethodNotAllowedJSONAppWide(t *testing.T) {
	// 复刻 main.go 的全站中间件（顺序一致：405 包装在 Recoverer/nosniff 之后）
	h, err := web.New(proxy.Version)
	if err != nil {
		t.Fatalf("web.New() 失败: %v", err)
	}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
	r.Use(methodNotAllowedJSONBody)
	r.Get("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) })
	r.Post("/only-post", func(w http.ResponseWriter, req *http.Request) {})
	h.Mount(r)
	r.NotFound(func(w http.ResponseWriter, req *http.Request) { web.NotFoundJSON(w) })

	cases := []struct {
		method, path string
		wantAllow    string
	}{
		{http.MethodPost, "/health", "GET"},
		{http.MethodGet, "/only-post", "POST"},
		{http.MethodPost, "/logs", "GET, HEAD"},
		{http.MethodPost, "/assets/app.js", "GET, HEAD"},
		{http.MethodPost, "/", "GET, HEAD"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d，期望 405", c.method, c.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Allow"); got != c.wantAllow {
			t.Errorf("%s %s Allow = %q，期望 %q", c.method, c.path, got, c.wantAllow)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s Content-Type = %q，期望 JSON", c.method, c.path, ct)
		}
		if body := rec.Body.String(); !strings.Contains(body, `"method_not_allowed"`) {
			t.Errorf("%s %s body = %q，期望含 method_not_allowed", c.method, c.path, body)
		}
	}

	// 非 405 响应不受该包装影响：状态码与 body 原样透传
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("GET /health = %d body=%q（包装器误伤了正常响应）", rec.Code, rec.Body.String())
	}
}

// TestResponseWriterCapabilitiesPassthrough 全局中间件的包装器须透传 ResponseWriter 的可选能力。
// 回归：包装器若不实现 http.Flusher，internal/proxy 的断言即失败、SSE 无法逐块刷出，
// requestLogger 亦会丢失能力；此类问题在非流式 httptest 中无法暴露。
func TestResponseWriterCapabilitiesPassthrough(t *testing.T) {
	r := chi.NewRouter()
	r.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
	r.Use(methodNotAllowedJSONBody)
	r.Use(requestLogger)

	var sawFlusher bool
	r.Post("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		fl, ok := w.(http.Flusher)
		sawFlusher = ok
		if ok {
			fl.Flush() // 必须不 panic
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {}\n\n"))
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !sawFlusher {
		t.Error("处理器拿不到 http.Flusher：SSE 逐块透传会被破坏")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200", rec.Code)
	}
	if rec.Body.String() != "data: {}\n\n" {
		t.Errorf("body = %q，包装器改动了非 405 响应", rec.Body.String())
	}
}

// TestSSEArrivesIncrementally 流式响应须逐块到达，不得被中间件缓冲至末尾一次性发出。
// 仅断言 `w.(http.Flusher)` 存在尚不足够：包装器仍可能实现 Flusher 却滞留数据，
// 故此处以真实 HTTP 服务逐块写入并延时，验证客户端在 handler 结束前即收到第一块。
func TestSSEArrivesIncrementally(t *testing.T) {
	release := make(chan struct{})
	firstChunkSeen := make(chan string, 1)

	// 与 main.go 同构的中间件链（含包装 ResponseWriter 的全局中间件）
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
	r.Use(methodNotAllowedJSONBody)
	r.Use(requestLogger)
	r.Post("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: chunk-1\n\n"))
		if fl != nil {
			fl.Flush()
		}
		// 待测试确认第一块已到达客户端后再写第二块：
		// 若中间件缓冲响应，此处将阻塞至超时并使测试失败。
		<-release
		_, _ = w.Write([]byte("data: chunk-2\n\n"))
		if fl != nil {
			fl.Flush()
		}
	})

	srv := httptest.NewServer(r)
	defer srv.Close()
	// 确保 handler 必被放行：否则失败路径上 srv.Close() 会等待阻塞的处理函数，
	// 将 3 秒的断言失败拖长至数十秒（defer 后注册者先执行，此处先于 srv.Close 生效）。
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	go func() {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
		if err != nil {
			firstChunkSeen <- "ERR: " + err.Error()
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		firstChunkSeen <- string(buf[:n])
		unblock() // 放行第二块
	}()

	select {
	case got := <-firstChunkSeen:
		if !strings.Contains(got, "chunk-1") {
			t.Fatalf("第一块内容 = %q，期望含 chunk-1", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("3 秒内未收到第一块：流式响应被中间件缓冲了（Flush 未生效）")
	}
}
