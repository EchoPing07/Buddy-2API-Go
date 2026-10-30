package web

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// testVersion 测试用的版本号（走 New(version) 注入登录页）。
// 刻意不用 "dev"：那与空值回退后的默认展示同形，会让「注入值确实进了模板」
// 与「回退分支生效」两种情形无法区分。
const testVersion = "v9.9.9-test"

// newTestRouter 复刻 main.go 的路由装配：路由表本身来自 h.Mount（不再各处复刻），
// 这里只补 main.go 的中间件（nosniff 等）。
func newTestRouter(t *testing.T) (*Handler, chi.Router) {
	t.Helper()
	h, err := New(testVersion)
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	r := chi.NewRouter()
	r.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
	h.Mount(r)
	r.NotFound(func(w http.ResponseWriter, req *http.Request) { NotFoundJSON(w) })
	return h, r
}

// TestBrandLinksToRepo 品牌元素（登录页标题 / 顶栏 / 侧栏 logo）须链接项目仓库并以新标签页打开。
// 回归：品牌名与仓库地址原先硬编码于多处模板，改名/换仓库需逐处修改且无编译期提示，
// 故按 href 与品牌文案计数，校验模板确实使用了注入的 Brand / RepoURL。
func TestBrandLinksToRepo(t *testing.T) {
	h, _ := newTestRouter(t)
	for _, p := range h.Pages() {
		html := string(p.HTML)
		// 仓库地址本身含 “Buddy-2API-Go”，不能作为旧品牌名残留的证据
		if n := strings.Count(html, `href="`+RepoURL+`" target="_blank" rel="noreferrer"`); n != 3 {
			t.Errorf("%s: 指向仓库的新标签页链接数 = %d，期望 3（登录页标题 + 顶栏 + 侧栏）", p.Key, n)
		}
		if n := strings.Count(html, ">"+Brand+"</a>"); n != 3 {
			t.Errorf("%s: 品牌文案 %q 出现次数 = %d，期望 3（应为 Brand 常量注入，不是写死的字面量）", p.Key, Brand, n)
		}
		// 先移除仓库地址再检索旧品牌，避免 URL 中的 “Buddy-2API-Go” 被误判为残留
		if s := strings.ReplaceAll(html, RepoURL, ""); strings.Contains(s, "Buddy-2API") || strings.Contains(s, "Buddy2API") {
			t.Errorf("%s: 残留旧品牌名（应为 %q）", p.Key, Brand)
		}
		if !strings.Contains(html, `<title>`+Brand+` · `) {
			t.Errorf("%s: <title> 未以 %q 开头", p.Key, Brand)
		}
	}
}

// TestVersionInjected 登录页版本号须来自 New(version) 的注入值。
//
// 回归：登录页只发 /admin/session 一个请求（拿版本得再请求 /health），故版本必须在预渲染时
// 就写进 HTML；若注入点在后续重构中被弄丢，登录页会静默退回不显示版本（构建期无从发现）。
// 每一页都含登录页模板，因此所有页面都应带上版本串。
func TestVersionInjected(t *testing.T) {
	h, _ := newTestRouter(t)
	want := `<p class="login-ver"><span>` + testVersion + `</span></p>`
	for _, p := range h.Pages() {
		if !strings.Contains(string(p.HTML), want) {
			t.Errorf("%s: 缺少注入的版本号标记 %q", p.Key, want)
		}
	}
}

// TestVersionDisplay 版本号归一：空值回退 dev，非空补 v 前缀，已是 v / dev 则原样。
//
// 三个来源格式不一（release 是 git tag、Docker/本地构建是 "dev"、CI 非 tag 触发可能是分支名），
// 此处固定收敛规则，避免登录页出现 "v1.2.3" 与 "1.2.3" 两种写法。
func TestVersionDisplay(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "dev"},
		{"   ", "dev"},
		{"dev", "dev"},
		{"v0.1.8", "v0.1.8"},
		{"0.1.8", "v0.1.8"},
		{"main", "vmain"},
	} {
		if got := VersionDisplay(tc.in); got != tc.want {
			t.Errorf("VersionDisplay(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestLoginPageStructure 登录页结构约束：两列 2:1、分割线归品牌列右边界（不再是独立节点）、
// 单一 h1（品牌标题降为 p）、无遗留类。
//
// 回归：分割线原为网格中「1px」那一列的独立 div（居中），且高度写死 256px；
// 现改为品牌列的 ::after（位于右三分处、高度随行高自适应）。若有人把独立节点加回来，
// 文档里会同时出现两套分隔，且 1px 列会把 2:1 稀释成 1:1:1。
func TestLoginPageStructure(t *testing.T) {
	h, _ := newTestRouter(t)
	// 登录页模板在每份文档里各一份，取首个页面代表即可；逐页校验更稳妥
	for _, p := range h.Pages() {
		html := string(p.HTML)
		if strings.Contains(html, `class="login-divider"`) {
			t.Errorf("%s: 残留 login-divider 节点（分割线应由 .login-brand::after 绘制）", p.Key)
		}
		if strings.Contains(html, `class="login-form-col"`) && !strings.Contains(html, `class="login-form-sub"`) {
			t.Errorf("%s: 表单副标题应使用 login-form-sub（与品牌列描述 login-sub 分开，两者边距/宽度不同）", p.Key)
		}
		// 登录页只有一个 <h1>（表单标题）；品牌标题降为 <p>，否则一页两个 h1 破坏大纲。
		// 注意不能断言整份文档 h1 总数：文档内常驻全部 8 个页面，各自另有一个 h1。
		if n := strings.Count(html, `<h1 class="login-h1">`); n != 1 {
			t.Errorf("%s: 登录页 h1 数 = %d，期望 1（仅表单标题「登录」）", p.Key, n)
		}
		if strings.Contains(html, `<h1 class="login-brand-title"`) {
			t.Errorf("%s: 品牌标题仍是 h1（应与表单标题错开层级，降为 p）", p.Key)
		}
		if !strings.Contains(html, `<p class="login-brand-title">`) {
			t.Errorf("%s: 品牌标题应为 <p class=\"login-brand-title\">", p.Key)
		}
		// 内联样式不得回到登录页（现全走 css 类）
		if strings.Contains(html, `style="margin:8px 0 24px"`) || strings.Contains(html, `style="width:100%;height:36px"`) {
			t.Errorf("%s: 登录页残留内联样式", p.Key)
		}
	}
}

// TestPagesRendered 每页都应渲染出完整 HTML：含 data-page、标题、8 个 view 容器。
//
// 文档内常驻全部页面（软导航只切 .view 显隐，不重载文档），因此每份 HTML 都含 8 个
// <section class="space">；各页 <title> / data-page 仍然各自正确，深链与刷新不受影响。
func TestPagesRendered(t *testing.T) {
	h, _ := newTestRouter(t)
	if got, want := len(h.Keys()), 8; got != want {
		t.Fatalf("页面数量 = %d，期望 %d", got, want)
	}
	for _, p := range h.Pages() {
		html := string(p.HTML)
		for _, want := range []string{
			`<body data-page="` + p.Key + `">`,
			`href="/assets/app.css"`,
			`src="/assets/app.js"`,
			`src="/assets/alpine.js"`,
		} {
			if !strings.Contains(html, want) {
				t.Errorf("%s: 缺少 %q", p.Key, want)
			}
		}
		// 服务端渲染的标题只作为首帧标题（软导航后由 app.js 的 applyTitle 接管），
		// 故此处用正则匹配不带外部分组、与具体分隔符无关的写法。
		titleRe := regexp.MustCompile(`<title>[^<]*` + regexp.QuoteMeta(p.Title) + `</title>`)
		if !titleRe.MatchString(html) {
			t.Errorf("%s: <title> 未包含页名 %q", p.Key, p.Title)
		}
		// 每个 view 容器必须带 x-cloak：未激活的 view 在 Alpine 接管前不能闪现
		for _, k := range h.Keys() {
			marker := `<div class="view" x-cloak x-show="view==='` + k + `'">`
			if !strings.Contains(html, marker) {
				t.Errorf("%s: 缺少视图容器 %s", p.Key, marker)
			}
		}
		// 全部页面 section 常驻同一文档
		if n := strings.Count(html, `<section x-cloak class="space">`); n != len(h.Keys()) {
			t.Errorf("%s: 页面 section 数量 = %d，期望 %d", p.Key, n, len(h.Keys()))
		}
	}
}

// TestNavBinding 侧边栏导航：每页共用同一份，链接数等于页数，逐项校验 active 绑定与点击拦截。
// active 由 Alpine 按 view 绑定（服务端写死的高亮无法随软导航切换）；
// href 仍须为真实路径（无 JS / 新标签页可用），且 JS 侧由 href 推导页面 key。
func TestNavBinding(t *testing.T) {
	h, _ := newTestRouter(t)
	for _, p := range h.Pages() {
		html := string(p.HTML)
		if n := strings.Count(html, `<a class="nav-link" :class="{active:view===`); n != len(h.Keys()) {
			t.Errorf("%s: 导航链接数 = %d，期望 %d", p.Key, n, len(h.Keys()))
		}
		if strings.Contains(html, `class="nav-link active"`) {
			t.Errorf("%s: active 态不应由服务端写死", p.Key)
		}
		for i := range pages {
			href := "/" + pages[i].key
			if i == 0 {
				href = "/" // 首页走根路径
			}
			for _, want := range []string{
				`:class="{active:view==='` + pages[i].key + `'}"`,
				`href="` + href + `"`,
				`@click="navClick($event)"`,
			} {
				if !strings.Contains(html, want) {
					t.Errorf("%s: 导航缺少 %s", p.Key, want)
				}
			}
		}
	}
}

// TestPageKeyValidation 页面 key 会进入 URL、文件名与 Alpine 表达式字面量，字符集必须保守。
func TestPageKeyValidation(t *testing.T) {
	for _, k := range []string{"dashboard", "a", "keys", "growth2", "growth-travel", "growth_travel"} {
		if !validPageKey(k) {
			t.Errorf("validPageKey(%q) = false，期望 true", k)
		}
	}
	for _, k := range []string{"", "Dashboard", "1a", "a/b", "../etc", "a b", `a'b`, `a"b`, "a<b"} {
		if validPageKey(k) {
			t.Errorf("validPageKey(%q) = true，期望 false", k)
		}
	}
}

// TestScriptOrder app.js 必须排在 alpine.js 之前。
//
// alpine.min.js 末尾是 queueMicrotask(()=>Alpine.start())，微任务会在下一个 defer 脚本
// 执行前清空：若 Alpine 先加载，就会在 app() 定义前启动（控制台报 app is not defined、整页不渲染）。
func TestScriptOrder(t *testing.T) {
	h, _ := newTestRouter(t)
	html := string(h.pages["dashboard"].HTML)
	iApp := strings.Index(html, "/assets/app.js")
	iAlpine := strings.Index(html, "/assets/alpine.js")
	if iApp < 0 || iAlpine < 0 {
		t.Fatalf("未找到脚本标签：app.js=%d alpine.js=%d", iApp, iAlpine)
	}
	if iApp > iAlpine {
		t.Error("app.js 必须排在 alpine.js 之前（否则 Alpine 会在 app() 定义前自启动）")
	}
}

// TestMountRoutes 页面路由 / 规范跳转 / 静态资源 / 405 / 404 行为；一律经真实 chi router
// 发请求，直接调 h.PageHandler 会绕过路由层 method 分派（HEAD/405 仅在路由层成立）。
func TestMountRoutes(t *testing.T) {
	_, r := newTestRouter(t)

	cases := []struct {
		method, path string
		code         int
		location     string
	}{
		{http.MethodGet, "/", http.StatusOK, ""},
		{http.MethodGet, "/logs", http.StatusOK, ""},
		{http.MethodGet, "/settings", http.StatusOK, ""},
		// 尾斜杠仅作别名：308 到无尾斜杠版本，保证每个页面只有一个规范 URL
		{http.MethodGet, "/logs/", http.StatusPermanentRedirect, "/logs"},
		{http.MethodGet, "/settings/", http.StatusPermanentRedirect, "/settings"},
		{http.MethodGet, "/dashboard", http.StatusPermanentRedirect, "/"},  // 规范化为 /（308 永久）
		{http.MethodGet, "/dashboard/", http.StatusPermanentRedirect, "/"}, // 尾斜杠同样跳转
		{http.MethodGet, "/assets/app.js", http.StatusOK, ""},
		{http.MethodGet, "/assets/app.css", http.StatusOK, ""},
		{http.MethodGet, "/assets/alpine.js", http.StatusOK, ""},
		{http.MethodGet, "/assets/nope.js", http.StatusNotFound, ""},
		{http.MethodGet, "/assets/", http.StatusNotFound, ""}, // 不列目录
		{http.MethodGet, "/assets/pages/keys.js", http.StatusNotFound, ""},
		{http.MethodGet, "/nope", http.StatusNotFound, ""},
		{http.MethodGet, "/nope/x", http.StatusNotFound, ""},
		{http.MethodGet, "/index.html", http.StatusNotFound, ""}, // 旧单页入口不再对外
		{http.MethodPost, "/assets/app.js", http.StatusMethodNotAllowed, ""},
		{http.MethodDelete, "/assets/app.css", http.StatusMethodNotAllowed, ""},
		// 页面同样只接受 GET/HEAD：其他方法返回统一 JSON 405，而非 chi 的空 body
		{http.MethodPost, "/", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "/logs", http.StatusMethodNotAllowed, ""},
		// /logs/ 为别名：先 308 跳到 /logs（308 保留方法），到终点后由 pageMethodGuard 判方法
		{http.MethodPost, "/logs/", http.StatusPermanentRedirect, "/logs"},
		{http.MethodDelete, "/settings", http.StatusMethodNotAllowed, ""},
		// HEAD 须与 GET 同等待遇（链接检查器 / 预览抓取 / 代理预检均发 HEAD）
		{http.MethodHead, "/", http.StatusOK, ""},
		{http.MethodHead, "/logs", http.StatusOK, ""},
		{http.MethodHead, "/logs/", http.StatusPermanentRedirect, "/logs"},
		{http.MethodHead, "/dashboard", http.StatusPermanentRedirect, "/"},
		{http.MethodHead, "/nope", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.code {
			t.Errorf("%s %s = %d，期望 %d", c.method, c.path, rec.Code, c.code)
		}
		if c.location != "" && rec.Header().Get("Location") != c.location {
			t.Errorf("%s %s Location = %q，期望 %q", c.method, c.path, rec.Header().Get("Location"), c.location)
		}
	}
}

// TestHeadViaRouter HEAD 经真实路由须返回 200、空 body 与 Content-Length。
//
// 回归：页面路由曾用 chi 的 r.Get 注册（仅匹配 GET），HEAD 一律 405；旧测试直接调 PageHandler，绕过了路由层故未发现。
func TestHeadViaRouter(t *testing.T) {
	_, r := newTestRouter(t)
	for _, path := range []string{"/", "/logs", "/settings", "/assets/app.js", "/assets/app.css", "/assets/alpine.js"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("HEAD %s = %d，期望 200", path, rec.Code)
			continue
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD %s 返回了 %d 字节 body", path, rec.Body.Len())
		}
		if rec.Header().Get("Content-Length") == "" {
			t.Errorf("HEAD %s 缺少 Content-Length", path)
		}
	}
}

// TestPageMethodNotAllowedBody 页面路由的 405 由 pageMethodGuard 返回统一 JSON 错误体，Allow 为 GET, HEAD。
func TestPageMethodNotAllowedBody(t *testing.T) {
	_, r := newTestRouter(t)
	for _, path := range []string{"/", "/logs", "/settings"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d，期望 405", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("POST %s Allow = %q，期望 GET, HEAD", path, got)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("POST %s Content-Type = %q，期望 JSON", path, ct)
		}
		if body := rec.Body.String(); !strings.Contains(body, `"method_not_allowed"`) {
			t.Errorf("POST %s body = %q", path, body)
		}
	}
}

// TestMethodNotAllowedBody 静态资源处理器直接返回的 405 同样使用统一 JSON 错误体与 Allow。
func TestMethodNotAllowedBody(t *testing.T) {
	h, _ := newTestRouter(t)
	rec := httptest.NewRecorder()
	h.AssetHandler(rec, httptest.NewRequest(http.MethodPost, "/assets/app.js", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /assets/app.js = %d，期望 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q，期望 GET, HEAD", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("405 Content-Type = %q，期望 JSON", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"method_not_allowed"`) {
		t.Errorf("405 body = %q", body)
	}
}

// TestPageHandlerUnknownKey 未注册的页面 key 不能 panic、不能返回空 200。
func TestPageHandlerUnknownKey(t *testing.T) {
	h, _ := newTestRouter(t)
	rec := httptest.NewRecorder()
	h.PageHandler("nope")(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未注册页面 = %d，期望 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Errorf("404 body = %q", rec.Body.String())
	}
}

// TestNotFoundBody 路由层 404 与包内 404 用同一份字面量。
func TestNotFoundBody(t *testing.T) {
	_, r := newTestRouter(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if got, want := rec.Body.String(), `{"error":{"message":"not found","type":"invalid_request_error","code":"not_found"}}`; got != want {
		t.Errorf("404 body = %q，期望 %q", got, want)
	}
}

// TestNoSniff 中间件覆盖页面与静态资源（main.go 全局挂载）。
func TestNoSniff(t *testing.T) {
	_, r := newTestRouter(t)
	for _, path := range []string{"/", "/logs", "/assets/app.js", "/assets/app.css", "/nope"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q，期望 nosniff", path, got)
		}
	}
}

// TestAssetCaching 资源缓存语义：页面 no-cache、Alpine 长缓存、其余带内容哈希 Etag，
// 并校验 Content-Type（写错会直接导致样式/脚本失效）。
func TestAssetCaching(t *testing.T) {
	h, _ := newTestRouter(t)

	// 页面：no-cache + Etag + Content-Length，条件请求 → 304
	for _, key := range h.Keys() {
		rec := httptest.NewRecorder()
		h.PageHandler(key)(rec, httptest.NewRequest(http.MethodGet, "/"+key, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q，期望 no-cache", key, got)
		}
		if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type = %q", key, got)
		}
		if got := rec.Header().Get("Content-Length"); got == "" {
			t.Errorf("%s: 缺少 Content-Length（页面约 50KB，不该走 chunked）", key)
		}
		etag := rec.Header().Get("Etag")
		if etag == "" {
			t.Errorf("%s: 缺少 Etag", key)
			continue
		}
		req := httptest.NewRequest(http.MethodGet, "/"+key, nil)
		req.Header.Set("If-None-Match", etag)
		rec = httptest.NewRecorder()
		h.PageHandler(key)(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("%s: 条件请求 = %d，期望 304", key, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s: 304 不应带 body，实际 %d 字节", key, rec.Body.Len())
		}
	}

	// Alpine：沿用 1 天缓存（体积大、几乎不变）
	rec := httptest.NewRecorder()
	h.AssetHandler(rec, httptest.NewRequest(http.MethodGet, "/assets/alpine.js", nil))
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("alpine.js Cache-Control = %q，期望 public, max-age=86400", got)
	}

	// 其余资源：no-cache + Etag，且条件请求返回 304
	types := map[string]string{
		"/assets/app.js":  "application/javascript; charset=utf-8",
		"/assets/app.css": "text/css; charset=utf-8",
	}
	for path, ctype := range types {
		rec := httptest.NewRecorder()
		h.AssetHandler(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Content-Type"); got != ctype {
			t.Errorf("%s: Content-Type = %q，期望 %q", path, got, ctype)
		}
		etag := rec.Header().Get("Etag")
		if etag == "" {
			t.Errorf("%s: 缺少 Etag", path)
			continue
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
			t.Errorf("%s: Cache-Control = %q", path, got)
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("If-None-Match", etag)
		rec = httptest.NewRecorder()
		h.AssetHandler(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("%s: 条件请求 = %d，期望 304", path, rec.Code)
		}
	}
}

// TestHeadRequests HEAD 不返回 body 且带 Content-Length（处理器层）；仅覆盖 http.ServeContent
// 的行为，路由层 HEAD 由 TestHeadViaRouter 覆盖。
func TestHeadRequests(t *testing.T) {
	h, _ := newTestRouter(t)
	rec := httptest.NewRecorder()
	h.PageHandler("logs")(rec, httptest.NewRequest(http.MethodHead, "/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /logs = %d，期望 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD /logs 返回了 %d 字节 body", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("HEAD /logs 缺少 Content-Length")
	}
	rec = httptest.NewRecorder()
	h.AssetHandler(rec, httptest.NewRequest(http.MethodHead, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD /assets/app.js = %d，body %d 字节", rec.Code, rec.Body.Len())
	}
}

// TestAppMergesAllPages 总装必须合并全部页面层。
//
// 8 个 view 常驻同一文档，Alpine 会在 x-data 根作用域（即 app() 返回的实例）上求值
// 全部页面的指令；若只合并当前页，其他页的表达式会报 ReferenceError 且视图渲染不全。
func TestAppMergesAllPages(t *testing.T) {
	h, _ := newTestRouter(t)
	js := string(h.assets["/assets/app.js"])
	if !strings.Contains(js, "for(const id of Object.keys(PAGES))") {
		t.Error("app() 必须遍历全部 PAGES 合并页面层（否则常驻 view 的表达式会 ReferenceError）")
	}
	// 导航必须在 Alpine 求值中执行：在 app() 里直接给原始对象赋值不会触发重渲染
	if strings.Contains(js, "window.addEventListener('popstate'") {
		t.Error("popstate 应由 Alpine 绑定（@popstate.window）转发，直接监听会拿到非响应式 this")
	}
	shell := string(h.pages["dashboard"].HTML)
	if !strings.Contains(shell, `@popstate.window="onPop($event)"`) {
		t.Error("shell 缺少 popstate 绑定")
	}
}

// TestPageJSRegistered 每个页面都必须有对应的页面层脚本，且注册 key 与页面 key 一致。
func TestPageJSRegistered(t *testing.T) {
	h, _ := newTestRouter(t)
	js := string(h.assets["/assets/app.js"])
	if !strings.Contains(js, "function app()") {
		t.Fatal("app.js 缺少总装函数 app()")
	}
	// 共享层必须先于页面层注册（否则 PAGE 未定义）
	iRegistry := strings.Index(js, "function PAGE(")
	if iRegistry < 0 {
		t.Fatal("app.js 缺少 PAGE 注册表")
	}
	for _, k := range h.Keys() {
		marker := "PAGE('" + k + "', {"
		i := strings.Index(js, marker)
		if i < 0 {
			t.Errorf("app.js 缺少页面层注册 %s", marker)
			continue
		}
		if i < iRegistry {
			t.Errorf("%s 的注册早于 PAGE 注册表定义", k)
		}
	}
}

// TestEmbedShape 内嵌文件清单稳定：shell + 7 页 + 3 资源 + 7 页脚本。
func TestEmbedShape(t *testing.T) {
	if _, err := fs.ReadFile(pageFS, "shell.html"); err != nil {
		t.Fatalf("读取 shell.html: %v", err)
	}
	for _, k := range []string{"dashboard", "account", "keys", "resources", "growth", "logs", "settings"} {
		if _, err := fs.ReadFile(pageFS, "pages/"+k+".html"); err != nil {
			t.Errorf("缺少 pages/%s.html: %v", k, err)
		}
		if _, err := fs.ReadFile(assetFS, "assets/pages/"+k+".js"); err != nil {
			t.Errorf("缺少 assets/pages/%s.js: %v", k, err)
		}
	}
}

// TestNoTemplateLeak 渲染产物不得残留模板指令。
func TestNoTemplateLeak(t *testing.T) {
	h, _ := newTestRouter(t)
	for _, p := range h.Pages() {
		html := string(p.HTML)
		if strings.Contains(html, "{{") || strings.Contains(html, "}}") {
			t.Errorf("%s: 渲染产物残留模板指令", p.Key)
		}
	}
}

// harnessJS 前端行为测试 harness：用最小 DOM stub 驱动真实 app.js 与各页脚本，断言导航与
// 「进场加载」语义；这类行为无法在 Go 侧断言（只能对源码文本做 grep）。
const harnessJS = `
(async function () {
const fs = require('fs');
const src = process.argv.slice(2).map(function (f) { return fs.readFileSync(f, 'utf8'); }).join('\n');

const loads = [];
const real = {};   // 被 stub 换下的真实现（需要跑真实逻辑的断言用）
// 侧栏导航 DOM 的替代对象：applyTitle 依据 .nav-link 的 href 与文案建页名映射，
// 置空用于模拟壳层不在 DOM（会话过期、登录页展示中）。
let navLinks = [];
function makeNavLinks() {
  return [['/', ' 统计 '], ['/account', ' 账号 '], ['/keys', ' 密钥 '], ['/resources', ' 余额 '],
          ['/growth', ' 任务 '], ['/logs', ' 日志 '], ['/settings', ' 设置 ']].map(function (p) {
    return { getAttribute: function () { return p[0]; }, textContent: p[1] };
  });
}
navLinks = makeNavLinks();
global.localStorage = { getItem: function () { return null; }, setItem: function () {} };
global.document = {
  documentElement: { classList: { toggle: function () {} } },
  body: { dataset: { page: 'dashboard' } },
  querySelectorAll: function () { return navLinks; },
  title: 'Buddy 2API Go · 统计',
};
global.matchMedia = function () { return { matches: false, addEventListener: function () {} }; };
global.window = { addEventListener: function () {}, scrollTo: function () {} };
global.history = { state: null, replaceState: function () {}, pushState: function () {} };
global.location = { pathname: '/' };
global.requestAnimationFrame = function (fn) { fn(); };
global.fetch = async function () { return { status: 200, json: async function () { return {}; } }; };

// app.js / pages 里的 PAGES、app 都是 eval 作用域内的 const/function，不会泄到本模块，
// 因此在同一次 eval 里把它们取出来（不靠间接 eval 的全局语义）。
const api = eval(src + '\n;({ PAGES: PAGES, app: app });');
const PAGES = api.PAGES;
const inst = api.app();
// 记录两类事实：「页面进场」（enter 调用 PAGES[k].load）与「实际发出的加载动作」：
// 前者验证进场语义（首屏/切页/保活），后者验证 load() 确实发出了请求。
Object.keys(PAGES).forEach(function (k) {
  const orig = PAGES[k].load;
  PAGES[k].load = function () { loads.push('enter:' + k); if (orig) return orig.call(this); };
});
['loadStats', 'loadModels', 'loadKeys', 'loadAccount', 'loadAccounts', 'loadGrowth', 'loadCheckin',
 'loadSettings', 'loadResources', 'loadLogs', 'loadTasks', 'loadCompensation'].forEach(function (m) {
  // 记参数：loadResources/loadGrowth 的 force 标志要被断言（换账号必须强刷）
  if (typeof inst[m] === 'function') {
    real[m] = inst[m];   // 保留真实现，供 10c/12 直接跑
    inst[m] = function () {
      loads.push(arguments.length ? m + '(' + Array.prototype.join.call(arguments, ',') + ')' : m);
    };
  }
});
inst.loadHealth = function () { loads.push('loadHealth'); };
inst.$nextTick = function (fn) { fn(); }; // Alpine magic（onPop 的滚动回位用）
// api() 保留真实现：14f 要验证它把响应体挂到 error 上（明细靠这条链路传给 importBatch）
real.api = inst.api;

function fail(msg) {
  console.error('FAIL: ' + msg + ' | loads=' + JSON.stringify(loads) +
                ' | view=' + inst.view + ' | entered=' + JSON.stringify(inst.entered));
  process.exit(1);
}
function count(name) { return loads.filter(function (x) { return x === name; }).length; }

// 1) 首屏页必须进场并加载数据（回归：entered 预置首屏 key 会使 load() 被跳过）
inst.afterLogin();
if (count('enter:dashboard') !== 1) fail('首屏页 dashboard 应恰好进场一次');
if (count('loadStats') !== 1) fail('首屏页 dashboard 的 load() 未把数据请求发出去');

// 2) 切页：目标页进场一次，view 切换，移动端侧栏收起
inst.mobileNav = true;
inst.navigate('keys');
if (inst.view !== 'keys') fail('view 未切换到 keys');
if (count('enter:keys') !== 1) fail('切到 keys 后应恰好进场一次');
if (count('loadKeys') < 1) fail('切到 keys 后未加载 keys');
if (inst.mobileNav !== false) fail('navigate() 未收起移动端侧栏');

// 3) 切回已进场的页：不重复进场（保活语义不能被破坏）
inst.navigate('dashboard');
if (inst.view !== 'dashboard') fail('view 未切回 dashboard');
if (count('enter:dashboard') !== 1) fail('切回已进场页时重复进场');

// 4) 前进/后退：换页时同样收起侧栏并让未进场页进场
inst.mobileNav = true;
inst.onPop({ state: { view: 'logs' } });
if (inst.view !== 'logs') fail('onPop 未切换 view');
if (count('enter:logs') !== 1) fail('onPop 到未进场页时应进场');
if (inst.mobileNav !== false) fail('onPop 换页时未收起移动端侧栏');

// 5) 页名映射：壳层不在 DOM 时的空采集不得写入缓存（写入后不再更新）
inst._titles = undefined;
navLinks = [];
inst.applyTitle('logs');           // 空采集：不应写入 _titles
navLinks = makeNavLinks();
inst.applyTitle('logs');
if (!/日志$/.test(document.title)) {
  fail('空采集污染了页名映射，标题不再更新: ' + document.title);
}
inst.navigate('settings');         // 常规软导航也要更新标题
if (!/设置$/.test(document.title)) fail('navigate 后标题未更新: ' + document.title);


// 6) 会话边界：401 / 重新登录须清空「已进场」，否则当前页保留过期数据
inst.view = 'account';
inst.entered = { account: Date.now() };   // 模拟本次会话已进过 account 且数据已旧
inst.resetSession();                       // 会话边界：清空「已进场」
loads.length = 0;
inst.afterLogin();                         // 重新登录：首屏必须重新进场取新数据
if (count('enter:account') !== 1) fail('重新登录后当前页应重新进场取新数据');

// 7) 会话边界也须由 401 触发，而非仅显式 resetSession
const savedFetch = global.fetch;
global.fetch = async function () { return { status: 401, json: async function () { return {}; } }; };
inst.entered = { account: Date.now() };
let threw = false;
try { await inst.api('/admin/whoami'); } catch (e) { threw = true; }
if (!threw) fail('401 应抛错');
if (inst.loggedIn !== false) fail('401 后 loggedIn 应为 false');
if (Object.keys(inst.entered).length !== 0) fail('401 未清空 entered（旧数据会残留）');

// 7b) 登录端点自身的 401 是「密码错误」而非会话过期：必须保留服务端文案，
// 且不得把页面切回未登录态/清空进场缓存（此刻本就未登录，切态只会吞掉错误提示）
inst.loggedIn = false;
inst.entered = { account: Date.now() };
let loginErr = '';
global.fetch = async function () {
  return { ok: false, status: 401, json: async function () { return { error: '密码错误' }; } };
};
try { await inst.api('/admin/login', { method: 'POST' }); } catch (e) { loginErr = e.message; }
if (loginErr !== '密码错误') fail('/admin/login 的 401 应报服务端原文「密码错误」，实得: ' + JSON.stringify(loginErr));
if (Object.keys(inst.entered).length === 0) fail('登录端点的 401 不应清空 entered（与api 通道共用时不得误伤已进过页的缓存）');
global.fetch = savedFetch;

// 8) realtime 页每次进入都刷新，短间隔重复进入退化为保活；非 realtime 页始终保活
loads.length = 0;
inst.entered = {};
inst.navigate('logs');
if (count('enter:logs') !== 1) fail('logs 首次进入应进场');
inst.navigate('dashboard');
inst.entered.logs = Date.now() - 60 * 1000;   // 模拟 1 分钟前进过
inst.navigate('logs');
if (count('enter:logs') !== 2) fail('realtime 页（logs）再次进入应刷新');
inst.navigate('dashboard');
inst.navigate('logs');                        // 刚进过：间隔 < 5s，退化为保活
if (count('enter:logs') !== 2) fail('realtime 页短间隔重复进入不应反复请求');
inst.entered = { dashboard: Date.now() - 60 * 1000 };
loads.length = 0;
inst.navigate('dashboard');                   // 非 realtime：即使超过间隔也不重拉
if (count('enter:dashboard') !== 0) fail('非 realtime 页应保持保活（不重复拉取）');

// 9) 行内编辑（updateKey）只就地替换一行，不得整表重拉：整表重拉会丢弃其他行未提交的编辑
loads.length = 0;
inst.keys = [
  { id: 1, name: 'alpha', status: 'active', key_plain: 'sk-a' },
  { id: 2, name: 'beta',  status: 'active', key_plain: 'sk-b' },
];
// 第 2 行存在未提交编辑（尚未触发 @change），随后编辑第 1 行
inst.keys[1].name = 'beta-未提交';

// 9a) 成功：以服务端回传值为准，其他行的未提交编辑须原样保留
// （api() 依据 r.ok 判定，stub 须提供 ok）
global.fetch = async function () {
  return { ok: true, status: 200, json: async function () { return { key: { id: 1, name: 'alpha-new', status: 'disabled', key_plain: 'sk-a' } }; } };
};
await inst.updateKey(inst.keys[0], { name: 'alpha-new' });
if (count('loadKeys') !== 0) fail('updateKey 成功后不应整表重拉（会丢其他行的未提交编辑）');
if (inst.keys[0].name !== 'alpha-new') fail('updateKey 成功后第 1 行未更新为服务端回传值: ' + inst.keys[0].name);
if (inst.keys[1].name !== 'beta-未提交') fail('updateKey 成功后第 2 行的未提交编辑被冲掉了: ' + inst.keys[1].name);
if (inst.keys.length !== 2) fail('updateKey 改变了行数: ' + inst.keys.length);

// 9b) 失败：该行回滚到请求前的服务端值（而非保留被拒的 patch），其他行编辑仍须保留
const before0 = inst.keys[0].name;   // 9a 之后第 1 行是 'alpha-new'
global.fetch = async function () {
  return { ok: false, status: 400, json: async function () { return { error: '备注非法' }; } };
};
await inst.updateKey(inst.keys[0], { name: '被拒绝的值' });
if (count('loadKeys') !== 0) fail('updateKey 失败后不应整表重拉（会丢其他行的未提交编辑）');
if (inst.keys[0].name !== before0) fail('updateKey 失败后未回滚到请求前的值: ' + inst.keys[0].name + '（期望 ' + before0 + '）');
if (inst.keys[0].name === '被拒绝的值') fail('updateKey 失败后保留了被服务端拒绝的值（重试必然再失败）');
if (inst.keys[1].name !== 'beta-未提交') fail('updateKey 失败后第 2 行的未提交编辑被冲掉了: ' + inst.keys[1].name);

// 9c) 目标行已被移除（如并发刷新/删除）时须安全返回：不抛错、不凭空造行
const n = inst.keys.length;
const ghost = { id: 999, name: 'ghost', status: 'active' };
await inst.updateKey(ghost, { name: 'x' });
if (inst.keys.length !== n) fail('对不存在的行调用 updateKey 改变了行数: ' + inst.keys.length);
global.fetch = savedFetch;

// 10) 换账号（导入凭证 / OAuth 登录）后必须立即强刷「账号口径」数据
// 回归：后端清了缓存，但前端各页 keep-alive —— 余额/签到/任务会继续展示上一账号的数据，
// 直到手动刷新或重进页面。
inst.toast = function () {};      // 避免 toast 清理 timer 拖住进程退出
inst.oauthStop = function () {};  // 同上（内含 setTimeout）
const jwt = 'eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1In0.sig';
function accountDataLoaded(where, withModels) {
  if (count('loadResources(true)') !== 1) fail(where + ' 未强刷余额（loadResources(true)）: ' + JSON.stringify(loads));
  if (count('loadGrowth(true)') !== 1) fail(where + ' 未强刷任务（loadGrowth(true)）: ' + JSON.stringify(loads));
  if (count('loadTasks') !== 1) fail(where + ' 未刷新任务中心（loadTasks）: ' + JSON.stringify(loads));
  if (count('loadCompensation') !== 1) fail(where + ' 未刷新补偿状态（loadCompensation）: ' + JSON.stringify(loads));
  if (count('loadCheckin') !== 1) fail(where + ' 未刷新签到状态: ' + JSON.stringify(loads));
  if (count('loadAccount') !== 1) fail(where + ' 未刷新账号信息: ' + JSON.stringify(loads));
  if (count('loadHealth') !== 1) fail(where + ' 未刷新健康状态: ' + JSON.stringify(loads));
  if (withModels && count('loadModels') !== 1) fail(where + ' 未刷新模型列表: ' + JSON.stringify(loads));
  if (!withModels && count('loadModels') !== 0) fail(where + ' 不应拉模型列表（跟 region 走，不跟账号走）');
}

// 10a) 导入并启用（activate=true）：换账号 → 强刷账号口径数据，且不动 entered
loads.length = 0;
inst.entered = { resources: Date.now() - 60 * 1000 };  // 已进过余额页（keep-alive）
let importBodies = [];
global.fetch = async function (url, opts) {
  if (String(url).indexOf('/admin/account/import') === 0) importBodies.push(JSON.parse(opts.body));
  return { ok: true, status: 200, json: async function () { return { ok: true, uid: 'u2', nickname: '新账号' }; } };
};
inst.importForm = { access_token: jwt, refresh_token: '', domain: '', filename: '' };
await inst.importAccount(true);
await new Promise(function (r) { setTimeout(r, 0); });   // importAccount 未 await 内层 busy()，等一轮宏任务
accountDataLoaded('导入并启用后', false);
if (importBodies.length !== 1 || importBodies[0].activate !== true) {
  fail('导入并启用必须带 activate=true: ' + JSON.stringify(importBodies));
}
if (inst.importModal) fail('导入成功后应关闭弹窗');
if (Object.keys(inst.entered).length !== 1) fail('换账号是原地强刷，不应动 entered（重进页面会多拉一次）');

// 10a-2) 仅导入（activate=false）：账号未变，只刷账号档案列表，不得强刷账号口径数据
// （否则仅仅存一个备用账号也会把当前账号的余额/签到全清一遍）。
loads.length = 0;
importBodies = [];
inst.entered = { resources: Date.now() - 60 * 1000 };
inst.importForm = { access_token: jwt, refresh_token: '', domain: '', filename: '' };
await inst.importAccount(false);
await new Promise(function (r) { setTimeout(r, 0); });
if (importBodies.length !== 1 || importBodies[0].activate !== false) {
  fail('仅导入必须带 activate=false: ' + JSON.stringify(importBodies));
}
if (count('loadAccounts') !== 1) fail('仅导入后必须刷新账号列表（否则用户看不到导入结果）: ' + JSON.stringify(loads));
['loadResources(true)', 'loadResources(false)', 'loadGrowth(true)', 'loadTasks', 'loadCompensation', 'loadAccount', 'loadHealth']
  .forEach(function (m) { if (count(m) !== 0) fail('仅导入不得强刷「' + m + '」: ' + JSON.stringify(loads)); });
if (inst.importModal) fail('仅导入成功后应关闭弹窗');
if (Object.keys(inst.entered).length !== 1) fail('仅导入不应动 entered');

// 10b) OAuth 登录（同一体验，另加模型列表）
loads.length = 0;
inst.oauth = { state: 'st-1', auth_url: '', poll_msg: '', _timer: null, _deadline: Date.now() + 60000 };
global.fetch = async function () {
  return { ok: true, status: 200, json: async function () { return { status: 'success', account: { uid: 'u3', nickname: '扫码账号' } }; } };
};
await inst.oauthPoll();
accountDataLoaded('OAuth 登录后', true);

// 10c) 在途请求期间到来的强刷不得被静默丢弃
// 余额加载器用 _loading 去重，而在途那次拿的正是**上一账号**的余额，丢掉就把旧数据留在页面上；
// 真实现必须在在途请求落地后补发一次 force=1。
inst.loadResources = real.loadResources;   // 10a/10b 用的是记录 stub，这里跑真实现
let releaseFirst = null;
const urls = [];
global.fetch = function (url) {
  urls.push(String(url));
  if (urls.length === 1) {   // 第一个请求（旧账号）挂住不返回
    return new Promise(function (resolve) {
      releaseFirst = function () {
        resolve({ ok: true, status: 200, json: async function () {
          return { cached: false, data: { accounts: [{ package_name: '旧账号包', capacity_remain: 1 }] } };
        } });
      };
    });
  }
  return Promise.resolve({ ok: true, status: 200, json: async function () {
    return { cached: false, data: { accounts: [{ package_name: '新账号包', capacity_remain: 99 }] } };
  } });
};
const inflight = inst.loadResources(false);   // 旧账号余额：请求在途
const forced = inst.loadResources(true);     // 换账号后的强刷：被 _loading 去重挡下
releaseFirst();
await inflight;
await forced;
await new Promise(function (r) { setTimeout(r, 0); });   // 重取是 finally 里的 fire-and-forget
if (urls.length !== 2) fail('在途期间到来的强刷被丢弃了（应补发一次）: ' + JSON.stringify(urls));
if (!/force=1/.test(urls[1])) fail('补发的请求必须带 force=1: ' + urls[1]);
if (String(inst.resources.accounts[0].package_name) !== '新账号包') {
  fail('页面最终应显示新账号的余额，得到 ' + JSON.stringify(inst.resources.accounts));
}
global.fetch = savedFetch;

// 11) 删除凭证后账号口径缓存必须就地清空并撤销保活
// 回归：deleteAccount 原先只 loadAccount/loadHealth，余额/任务留在内存里仍是上一账号的数据，
// 且 entered 未撤销 —— 保活使两页不再重拉，被删账号的额度包/签到会一直显示。
// 这里也不能改成「重拉」：此刻已无凭证，只会失败弹错。
inst.resources = { accounts: [{ package_name: '已删账号包', capacity_remain: 1 }], loaded: true };
inst.growth = Object.assign({}, inst.growth, { loaded: true, streak: { days: 42 }, heatmap: { '2026-01-01': 1 } });
inst.checkin = { loaded: true, today_checked_in: true }; inst.checkinOk = true; inst.checkinErr = '旧错'; inst.checkinPending = true;
inst.growthErr = '旧错误';
inst.hideDepleted = true;   // 与账号无关的状态，作为「未被整块清空」的哨兵
inst.entered = { resources: Date.now(), growth: Date.now(), keys: Date.now() };
global.fetch = async function () {
  return { ok: true, status: 200, json: async function () { return { ok: true }; } };
};
inst.deleteAccount();
if (!inst.confirmBox.open) fail('deleteAccount 应先弹确认框');
inst.confirmBox.onOk();
await new Promise(function (r) { setTimeout(r, 0); });   // 内层 busy() 未被 deleteAccount await
if (inst.resources.accounts.length !== 0) fail('删除凭证后余额仍是被删账号的额度包: ' + JSON.stringify(inst.resources.accounts));
if (inst.resources.loaded !== false) fail('删除凭证后 resources 未回到未加载态（无凭证重拉只会失败）: ' + inst.resources.loaded);
if (inst.growth.loaded !== false || inst.growth.streak !== null || inst.growth.heatmap !== null) {
  fail('删除凭证后任务数据未清空: ' + JSON.stringify(inst.growth));
}
// 任务中心 / 补偿同属账号口径：删凭证后必须一起清空（否则会展示被删账号的进度与额度）
if ((inst.tasks || []).length !== 0 || inst.tasksLoaded !== false || inst.tasksErr) {
  fail('删除凭证后任务中心未清空: ' + JSON.stringify({ tasks: inst.tasks, loaded: inst.tasksLoaded, err: inst.tasksErr }));
}
if ((inst.tasksResults || []).length !== 0) fail('删除凭证后任务执行结果未清空');
if (inst.compLoaded !== false || (inst.comp && Object.keys(inst.comp).length)) {
  fail('删除凭证后补偿状态未清空: ' + JSON.stringify(inst.comp));
}
if (inst.checkin.loaded !== false || inst.checkinOk || inst.checkinErr || inst.checkinPending) {
  fail('删除凭证后签到状态/标志未清空: ' + JSON.stringify([inst.checkin, inst.checkinOk, inst.checkinErr, inst.checkinPending]));
}
if (inst.growthErr) fail('删除凭证后 growthErr 未清空: ' + inst.growthErr);
if (inst.hideDepleted !== true) fail('resetAccountData 误伤了非账号状态（hideDepleted）');
if ('resources' in inst.entered) fail('删除凭证后未撤销余额页保活（重进仍展示被删账号数据）');
if ('growth' in inst.entered) fail('删除凭证后未撤销任务页保活（重进仍展示被删账号数据）');
if (!('keys' in inst.entered)) fail('只应撤销账号口径两页的保活，不得清空整个 entered（密钥/日志/设置会白拉）');
global.fetch = savedFetch;

// 12) 换账号/删凭证时在途的旧账号响应必须作废（代次校验），不得覆盖新数据
// 回归：三个账号口径加载器无条件写回响应，而强刷是「先发新请求」的 —— 旧账号的响应只要
// 晚于新账号落地就会把新数据覆盖回旧值（last-write-wins）。
inst.loadResources = real.loadResources;
inst.loadGrowth = real.loadGrowth;
inst.loadCheckin = real.loadCheckin;

// 12a) 删除凭证时三个加载器的在途响应都不得把被删账号的数据写回
inst.resetAccountData();
let release12 = [];
let phase12 = 'old';
global.fetch = function (url) {
  const u = String(url); const ph = phase12;
  const payload = function () {
    if (u.indexOf('/admin/resources') === 0) {
      return {cached: false, data: {accounts: [{package_name: ph + '账号的包', capacity_remain: 5}]}};
    }
    if (u.indexOf('/admin/growth/overview') === 0) {
      return {available: true, streak: {days: ph === 'old' ? 11 : 22}, heatmap: {cells: []}, degraded: []};
    }
    return {random_target: ph === 'old' ? '08:00' : '20:00', data: {data: {today_checked_in: ph !== 'old', credit: 1}}};
  };
  if (ph === 'old') {
    return new Promise(function (res) {
      release12.push(function () { res({ok: true, status: 200, json: async function () { return payload(); }}); });
    });
  }
  return Promise.resolve({ok: true, status: 200, json: async function () { return payload(); }});
};
const d1 = inst.loadResources(true), d2 = inst.loadGrowth(true), d3 = inst.loadCheckin();
inst.resetAccountData();          // 在途期间用户删除了凭证
release12.forEach(function (f) { f(); });
await Promise.all([d1, d2, d3]);
await new Promise(function (r) { setTimeout(r, 0); });
if ((inst.resources.accounts || []).length !== 0) {
  fail('删除凭证后在途响应把被删账号的余额写回了: ' + JSON.stringify(inst.resources.accounts));
}
if (inst.growth.streak !== null || inst.growth.loaded !== false) {
  fail('删除凭证后在途响应把被删账号的任务数据写回了: ' + JSON.stringify(inst.growth));
}
if (inst.checkin.loaded !== false || inst.checkin.random_target) {
  fail('删除凭证后在途响应把被删账号的签到写回了: ' + JSON.stringify(inst.checkin));
}

// 12b) 换账号时旧账号响应「后落地」不得覆盖新账号数据
inst.resetAccountData();
release12 = [];
phase12 = 'old';
const o1 = inst.loadGrowth(true), o2 = inst.loadCheckin();   // 旧账号：挂住
phase12 = 'new';
inst.refreshAccountData();                                  // 换账号：新请求立即返回
await new Promise(function (r) { setTimeout(r, 30); });
release12.forEach(function (f) { f(); });                    // 旧响应最后落地
await Promise.all([o1, o2]);
await new Promise(function (r) { setTimeout(r, 30); });
if (!inst.growth.streak || inst.growth.streak.days !== 22) {
  fail('换账号后旧响应覆盖了新账号的任务数据: ' + JSON.stringify(inst.growth.streak));
}
if (inst.checkin.random_target !== '20:00') {
  fail('换账号后旧响应覆盖了新账号的签到状态，random_target=' + inst.checkin.random_target);
}
global.fetch = savedFetch;

// 13) 导出弹窗：三种范围（当前 / 全部 / 选择），统一走凭证包端点
// 回归：导出原先是「点一下就下载当前 token.json」，无法备份整批账号；现在开弹窗选范围。
inst.account = { logged_in: true, nickname: '当前账号', uid: 'uid-cur', has_refresh: true, expired: false };
inst.accounts = { loaded: true, items: [
  { uid: 'uid-cur', nickname: '当前账号', active: true },
  { uid: 'uid-b',   nickname: '账号B',  active: false },
  { uid: 'uid-c',   nickname: '账号C',  active: false, expired: true },
] };

// 13a) 打开弹窗默认「当前账号」（用户最常导的就是这一份）
inst.openExport();
if (!inst.exportModal) fail('openExport 未打开弹窗');
if (inst.exportForm.scope !== 'current') fail('导出弹窗默认方式应为单个导出（current），得到 ' + inst.exportForm.scope);
if (inst.exportCount() !== 1) fail('单个导出应导出 1 个账号，得到 ' + inst.exportCount());

// 13b) 切到「批量导出」自动全选（取消勾选比逐个勾更省事），
// 全选按钮的文案与 exportCount 随勾选变化
inst.setExportScope('selected');
if (inst.exportForm.uids.length !== 3) fail('切到批量导出应自动全选，得到 ' + JSON.stringify(inst.exportForm.uids));
if (inst.exportCount() !== 3) fail('全选后应导出 3 个，得到 ' + inst.exportCount());
if (!inst.exportAllSelected()) fail('exportAllSelected 应为 true（3/3）');
inst.toggleExportUID('uid-b');
if (inst.exportForm.uids.indexOf('uid-b') >= 0) fail('toggleExportUID 未能取消勾选');
if (inst.exportCount() !== 2) fail('取消一个后应剩 2 个，得到 ' + inst.exportCount());
if (inst.exportAllSelected()) fail('取消一项后 exportAllSelected 应为 false（全选按钮应显示「全选」）');
inst.exportSelectAll();   // 非全选 → 全选
if (inst.exportForm.uids.length !== 3) fail('全选按钮应补满：' + JSON.stringify(inst.exportForm.uids));
inst.exportSelectAll();   // 已全选 → 取消全选
if (inst.exportForm.uids.length !== 0) fail('全选按钮在全选状态应为「取消全选」：' + JSON.stringify(inst.exportForm.uids));
if (inst.exportCount() !== 0) fail('空选择时导出数应为 0（主按钮据此置灰），得到 ' + inst.exportCount());

// 13c) 未登录时默认退到批量导出（单个无内容可导）；单个导出数为 0
inst.account = { logged_in: false };
inst.openExport();
if (inst.exportForm.scope !== 'selected') fail('未登录时默认应为批量导出，得到 ' + inst.exportForm.scope);
inst.setExportScope('current');
if (inst.exportCount() !== 0) fail('无活动账号时单个导出数应为 0，得到 ' + inst.exportCount());
let idleUrls = [];
global.fetch = async function (url) { idleUrls.push(String(url)); return { ok: true, status: 200, json: async function () { return {}; } }; };
await inst.exportAccounts();
if (idleUrls.length !== 0) fail('0 个账号时不应发导出请求: ' + JSON.stringify(idleUrls));

// 13d) 导出请求带正确的 scope / uids，文件名取服务端 Content-Disposition，成功后关弹窗
inst.account = { logged_in: true, nickname: '当前账号', uid: 'uid-cur' };
let exportUrls = [];
let downloaded = null;
const realCreate = global.URL.createObjectURL;
global.URL.createObjectURL = function () { return 'blob:x'; };
global.URL.revokeObjectURL = function () {};
inst.download = function (blob, filename) { downloaded = filename; };   // 不碰真实 DOM
function exportFetch(headers) {
  global.fetch = async function (url) {
    exportUrls.push(String(url));
    return { ok: true, status: 200, blob: async function () { return 'x'; },
             headers: { get: function (k) {
               const h = headers || {};
               return h[k] || null;
             } } };
  };
}
exportFetch({'Content-Disposition': 'attachment; filename="buddy2api-account-uid-cur.json"', 'X-Export-Count': '1'});
inst.setExportScope('current');
await inst.exportAccounts();
await new Promise(function (r) { setTimeout(r, 0); });
if (exportUrls.length !== 1) fail('导出应发 1 个请求，得到 ' + JSON.stringify(exportUrls));
if (!/[?&]scope=current/.test(exportUrls[0])) fail('单个导出应带 scope=current: ' + exportUrls[0]);
if (/uids=/.test(exportUrls[0])) fail('单个导出不必带 uids: ' + exportUrls[0]);
if (downloaded !== 'buddy2api-account-uid-cur.json') fail('文件名应取服务端返回，得到 ' + downloaded);
if (inst.exportModal) fail('导出成功后应关闭弹窗');

exportUrls = [];
inst.setExportScope('selected');
inst.exportForm.uids = ['uid-b', 'uid-c'];
exportFetch({'X-Export-Count': '2'});
await inst.exportAccounts();
await new Promise(function (r) { setTimeout(r, 0); });
if (!/[?&]scope=selected/.test(exportUrls[0])) fail('批量导出应带 scope=selected: ' + exportUrls[0]);
if (!/uids=uid-b%2Cuid-c/.test(exportUrls[0])) fail('批量导出应带逗号分隔的 uids: ' + exportUrls[0]);
if (downloaded !== 'buddy2api-accounts.json') fail('无 Content-Disposition 时应回退通用文件名，得到 ' + downloaded);

// 13e) 导出失败（后台报错）应抛错且不下载、不关弹窗
exportUrls = [];
global.fetch = async function () { return { ok: false, status: 400, json: async function () { return { error: '所选账号不存在' }; } }; };
inst.openExport();
downloaded = null;
await inst.exportAccounts();
await new Promise(function (r) { setTimeout(r, 0); });
if (!inst.exportModal) fail('导出失败时不该关弹窗（用户要能重试/改选）');
if (downloaded !== null) fail('导出失败时不应触发下载，得到 ' + downloaded);
global.URL.createObjectURL = realCreate;

// 14) 批量导入：只入档、不切换当前账号；逐条结果展示
inst.openImport();
if (inst.importMode !== 'single') fail('导入弹窗默认应为单个账号模式，得到 ' + inst.importMode);
inst.setImportMode('batch');
if (inst.importMode !== 'batch') fail('setImportMode 未切到 batch');
if (inst.batchResult) fail('打开批量导入时不该有上一轮结果');

// 14a) 未选包时不得发请求
let batchBodies = [];
inst.api = async function (path, opts) {
  if (path === '/admin/account/import/batch') { batchBodies.push(JSON.parse(opts.body)); return { imported: 2, failed: 0, total: 2, active: false, items: [] }; }
  return {};
};
inst.toast = function () {};
await inst.importBatch();
if (batchBodies.length !== 0) fail('未选凭证包时就发了批量导入请求');

// 14b) 有包时 POST 原始凭证包，并刷新账号列表
loads.length = 0;
inst.batchForm = {
  filename: 'pkg.json', error: '',
  items: [{ uid: 'uid-b', nickname: '账号B', domain: 'copilot.tencent.com' }, { uid: 'uid-c', nickname: '账号C' }],
  bundle: { format: 'buddy2api.accounts', version: 1, accounts: [{ access_token: 'a' }, { access_token: 'b' }] },
};
inst.api = async function (path, opts) {
  if (path === '/admin/account/import/batch') {
    batchBodies.push(JSON.parse(opts.body));
    return { imported: 2, failed: 0, total: 2, active: false,
             items: [{ uid: 'uid-b', ok: true }, { uid: 'uid-c', ok: true }] };
  }
  if (path === '/admin/accounts') { loads.push('loadAccounts'); return { items: [] }; }
  return {};
};
await inst.importBatch();
await new Promise(function (r) { setTimeout(r, 0); });
if (batchBodies.length !== 1) fail('应发 1 次批量导入请求，得到 ' + batchBodies.length);
if (batchBodies[0].format !== 'buddy2api.accounts') fail('请求体应是原始凭证包: ' + JSON.stringify(batchBodies[0]));
if (batchBodies[0].activate !== undefined) fail('批量导入不得带 activate（恒不切换活动账号）: ' + JSON.stringify(batchBodies[0]));
if (count('loadAccounts') !== 1) fail('批量导入后必须刷新账号列表（唯一产出）');
['loadResources(true)', 'loadResources(false)', 'loadGrowth(true)', 'loadCheckin', 'loadAccount']
  .forEach(function (m) { if (count(m) !== 0) fail('批量导入不得强刷「' + m + '」（当前账号未变）: ' + JSON.stringify(loads)); });
if (!inst.batchResult || inst.batchResult.imported !== 2) fail('应保存逐条结果: ' + JSON.stringify(inst.batchResult));
if (inst.batchForm.items.length !== 0) fail('导入成功后应清空已选列表（防重复提交）');
if (!inst.importModal) fail('批量导入后应留在弹窗看逐条结果');

// 14c) 整包被拒（格式/版本错）时把错误留在弹窗里，而不是只弹一个飘走的 toast
inst.batchForm = { filename: 'bad.json', error: '', items: [{ uid: 'x' }], bundle: { format: 'other' } };
inst.api = async function () { throw new Error('不是凭证包（format 应为 "buddy2api.accounts"）'); };
await inst.importBatch();
await new Promise(function (r) { setTimeout(r, 0); });
if (!/不是凭证包/.test(inst.batchForm.error)) fail('整包失败应把原因写入 batchForm.error，得到 ' + JSON.stringify(inst.batchForm.error));

// 14d) 整包失败后重试成功，旧的红色错误必须被清掉
// 回归：error 只在抛错时写、成功路径从不清，于是弹窗里会同时显示「导入完成」和「网关超时」。
inst.batchForm = { filename: 'r.json', error: '', items: [{ uid: 'a' }], bundle: { format: 'buddy2api.accounts' } };
inst.api = async function () { throw new Error('网关超时'); };
await inst.importBatch();
await new Promise(function (r) { setTimeout(r, 0); });
if (!inst.batchForm.error) fail('首次失败应写入 error');
inst.batchForm = { filename: 'r.json', error: '网关超时', items: [{ uid: 'a' }], bundle: { format: 'buddy2api.accounts' } };
inst.api = async function () {
  return { imported: 1, failed: 0, total: 1, overwrote: 0, items: [{ uid: 'a', ok: true }] };
};
await inst.importBatch();
await new Promise(function (r) { setTimeout(r, 0); });
if (inst.batchForm.error) fail('重试成功后残留了旧错误（会与「导入完成」同时显示）: ' + JSON.stringify(inst.batchForm.error));

// 14e) 全部失败（400 + 逐条明细）时必须展示明细，而不是只报一个「HTTP 400」
// 回归：api() 只抛 error 字符串，400 响应里的 items（逐条失败原因）被丢弃 ——
// 而这恰恰是用户最需要看的内容（哪一条凭证坏了、为什么坏）。
const failErr = new Error('HTTP 400');
failErr.status = 400;
failErr.body = { ok: false, imported: 0, failed: 2, total: 2, active: false, items: [
  { ok: false, error: '第 1 条：access_token 不能为空' },
  { ok: false, error: '第 2 条：access_token 不是合法 JWT' },
] };
inst.batchForm = { filename: 'f.json', error: '', items: [{ uid: 'a' }, { uid: 'b' }], bundle: { format: 'buddy2api.accounts' } };
inst.api = async function () { throw failErr; };
await inst.importBatch();
await new Promise(function (r) { setTimeout(r, 0); });
if (!inst.batchResult || (inst.batchResult.items || []).length !== 2) {
  fail('全失败时应展示后端逐条明细，得到 ' + JSON.stringify(inst.batchResult));
}
if (!/不能为空/.test(JSON.stringify(inst.batchResult.items))) {
  fail('逐条明细应包含后端原文: ' + JSON.stringify(inst.batchResult.items));
}
if (inst.batchForm.error) fail('全失败已由明细展示时不应再写 error: ' + JSON.stringify(inst.batchForm.error));

// 14f) api() 必须把响应体挂在 error 上（14e 的前置能力），否则明细根本拿不到
inst.api = real.api;   // 跑真实现
let apiErrBody = null;
const savedFetchForErr = global.fetch;
global.fetch = async function () {
  return { ok: false, status: 400, json: async function () { return { error: '坏了', items: [{ ok: false }] }; } };
};
try { await inst.api('/admin/account/import/batch', { method: 'POST' }); }
catch (e) { apiErrBody = e.body; }
if (!apiErrBody || !Array.isArray(apiErrBody.items)) fail('api() 未把响应体挂到 error.body（逐条明细会丢失）: ' + JSON.stringify(apiErrBody));
inst.api = real.api;
global.fetch = savedFetchForErr;

// 14g) 用户「取消全选」后再切换导出方式，不得把选择静默还原成全选
// 回归：setExportScope 在 uids 为空时无条件全选，用户清空选择 → 切单个 → 切回批量，
// 选择被自动填满，一点导出就把所有账号导了出去。
inst.account = { logged_in: true, nickname: '当前', uid: 'uid-cur' };
inst.accounts = { loaded: true, items: [
  { uid: 'uid-cur', nickname: '当前', active: true },
  { uid: 'uid-b', nickname: 'B' },
] };
inst.openExport();
inst.setExportScope('selected');
if (inst.exportForm.uids.length !== 2) fail('首次进入批量应默认全选');
inst.exportSelectAll();                       // 取消全选（用户想自己挑）
if (inst.exportForm.uids.length !== 0) fail('取消全选后应为空');
inst.setExportScope('current');
inst.setExportScope('selected');              // 切回批量
if (inst.exportForm.uids.length !== 0) {
  fail('「取消全选」的意图被静默还原成全选（会误导出全部账号）: ' + JSON.stringify(inst.exportForm.uids));
}
if (inst.exportCount() !== 0) fail('用户清空后导出数应为 0，得到 ' + inst.exportCount());

// 15) 页面层与共享层不得重叠：openExport / exportAccounts / importBatch 只在页面层
// （appShell 里出现同名成员会被静默顶掉，由 shadowHarness 兜住；这里断言接线本身）
['openExport', 'exportAccounts', 'setExportScope', 'exportCount', 'importBatch', 'onBatchFile',
 'parseBatchBundle', 'openImport']
  .forEach(function (m) {
    if (typeof inst[m] !== 'function') fail('账号页缺少动作 ' + m);
  });

// 16) 选错文件时的自适应：单个模式选到凭证包 → 自动切批量；批量模式选到 token.json → 自动切单个。
// 否则用户得到的是一个「格式不对」的死胡同，还得自己回头改模式重选。
inst.openImport();
inst.parseBatchBundle('pkg.json', JSON.stringify({
  format: 'buddy2api.accounts', version: 1,
  accounts: [{ access_token: 'a.b.c' }],
}));
if (inst.batchForm.items.length !== 1) fail('合法凭证包应解析出预览项: ' + JSON.stringify(inst.batchForm));

inst.setImportMode('batch');
inst.parseBatchBundle('token.json', JSON.stringify({ access_token: 'aa.bb.cc', refresh_token: 'rt' }));
if (inst.importMode !== 'single') fail('批量模式选到 token.json 应自动切到单个模式，得到 ' + inst.importMode);
if (inst.importForm.access_token !== 'aa.bb.cc') fail('切回单个模式应回填 access_token: ' + JSON.stringify(inst.importForm));
if (inst.importForm.filename !== 'token.json') fail('应记住文件名: ' + inst.importForm.filename);

inst.setImportMode('batch');
inst.parseBatchBundle('weird.json', JSON.stringify({ hello: 'world' }));
if (!inst.batchForm.error) fail('缺 format 标识时应就地报错，不发请求');
if (inst.batchForm.items.length !== 0) fail('非法包不该产生预览项');

console.log('OK ' + JSON.stringify(loads));
})().catch(function (e) { console.error('FAIL: harness 异常: ' + ((e && e.stack) || e)); process.exit(1); });
`

// shadowHarnessJS 检查页面层与共享层成员重名（PAGES 各页 vs appShell 返回值）。
// 只取 appShell() 的返回值与 PAGES 键名做集合运算，无需 DOM stub。
const shadowHarnessJS = `
const fs = require('fs');
const src = process.argv.slice(2).map(function (f) { return fs.readFileSync(f, 'utf8'); }).join('\n');
// appShell() 体内读取 localStorage（theme 初值），故取成员名须实际调用该函数
global.localStorage = { getItem: function () { return null; }, setItem: function () {} };
const api = eval(src + '\n;({ PAGES: PAGES, appShell: appShell });');

const shellNames = Object.getOwnPropertyNames(api.appShell());
const shellSet = new Set(shellNames);
const bad = [];
Object.keys(api.PAGES).forEach(function (k) {
  Object.getOwnPropertyNames(api.PAGES[k]).forEach(function (m) {
    if (shellSet.has(m)) bad.push(k + '.' + m);
  });
});
if (bad.length) {
  console.error('FAIL: 页面层成员覆盖了共享层成员（共享层实现会被静默顶掉）: ' + bad.join(', '));
  process.exit(1);
}
console.log('OK 无重名覆盖（共享层 ' + shellNames.length + ' 个成员，页面层 ' + Object.keys(api.PAGES).length + ' 页）');
`

// TestPageLayerDoesNotShadowShell 页面层成员不得与共享层成员重名：app() 按 Object.keys(PAGES)
// 顺序将各页面层 defineProperty 到同一 x-data 根作用域，后注册者覆盖先注册者，同名即覆盖共享层实现。
// 跨页重名允许（load、realtime 等靠 PAGES[k] 显式索引调用），此处只禁页面层对共享层的覆盖。
func TestPageLayerDoesNotShadowShell(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("未找到 node，跳过前端成员重名检查")
	}
	harness := filepath.Join(t.TempDir(), "shadow.cjs")
	if err := os.WriteFile(harness, []byte(shadowHarnessJS), 0o600); err != nil {
		t.Fatalf("写入 harness 失败: %v", err)
	}
	args := []string{harness, filepath.Join("assets", "app.js")}
	for _, p := range pages {
		args = append(args, filepath.Join("assets", "pages", p.key+".js"))
	}
	if out, err := exec.Command(node, args...).CombinedOutput(); err != nil {
		t.Fatalf("页面层与共享层存在重名成员: %v\\n%s", err, out)
	}
}

// TestFrontendInitialLoad 用 node 驱动真实前端逻辑，守住「首屏页会加载数据」这一语义。
// Go 侧只能对 app.js 做源码文本 grep，而 P0 回归（entered 预置首屏 key 使 load() 被跳过）恰好是纯行为问题；
// 无 node 时跳过。
func TestFrontendInitialLoad(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("未找到 node，跳过前端行为测试")
	}
	if _, err := New(testVersion); err != nil { // 保证脚本清单与 New() 一致
		t.Fatalf("New() 失败: %v", err)
	}
	harness := filepath.Join(t.TempDir(), "harness.cjs")
	if err := os.WriteFile(harness, []byte(harnessJS), 0o600); err != nil {
		t.Fatalf("写入 harness 失败: %v", err)
	}
	args := []string{harness, filepath.Join("assets", "app.js")}
	for _, p := range pages {
		args = append(args, filepath.Join("assets", "pages", p.key+".js"))
	}
	if out, err := exec.Command(node, args...).CombinedOutput(); err != nil {
		t.Fatalf("前端行为测试失败: %v\n%s", err, out)
	}
}

// TestViewContainersBalanced .view 容器须平级（各自为 <main> 直接子节点），不得互相嵌套。
// 回归：HTML 注释误用 `*/` 收尾会吞掉后续真实标签，使部分 .view 被并入前一页的 <template
// x-if> 内：文档内 view 数量不减，进入 DOM 的减少，且无报错。
func TestViewContainersBalanced(t *testing.T) {
	h, _ := newTestRouter(t)
	for _, p := range h.Pages() {
		doc := string(p.HTML)
		// 扫描全文：整份文档本就应标签平衡（登录页/主界面模板亦需自洽），单独定位内容区反而复杂
		depth, viewDepths, err := scanTagBalance(doc)
		if err != nil {
			t.Errorf("%s: %v", p.Key, err)
			continue
		}
		if depth != 0 {
			t.Errorf("%s: 文档结束时标签深度 = %d，期望 0（有未闭合/多余闭合的标签，常见原因是注释吞掉了标签）", p.Key, depth)
		}
		if len(viewDepths) != len(h.Keys()) {
			t.Errorf("%s: .view 容器数 = %d，期望 %d", p.Key, len(viewDepths), len(h.Keys()))
			continue
		}
		want := viewDepths[0]
		for i, d := range viewDepths {
			if d != want {
				t.Errorf("%s: 第 %d 个 .view 的嵌套深度 = %d，与首个（%d）不同 —— 容器被嵌套了", p.Key, i, d, want)
			}
		}
	}
}

// scanTagBalance 扫描 HTML，返回收尾深度与各 <div class="view"> 出现时的嵌套深度。
// 须正确跳过注释：注释内的闭合标签仍会被朴素计数计入，深度看似平衡而结构已不同。
func scanTagBalance(doc string) (int, []int, error) {
	depth := 0
	var viewDepths []int
	// 参与平衡的标签：本前端仅用这三类嵌套（自闭合/空元素不参与）
	tracked := map[string]bool{"div": true, "section": true, "template": true}

	for i := 0; i < len(doc); {
		// 跳过 HTML 注释
		if strings.HasPrefix(doc[i:], "<!--") {
			end := strings.Index(doc[i+4:], "-->")
			if end < 0 {
				return depth, viewDepths, fmt.Errorf("位置 %d 起的 HTML 注释没有收尾（-->）——注释会吞掉后续标签", i)
			}
			i += 4 + end + 3
			continue
		}
		if doc[i] != '<' {
			i++
			continue
		}
		// 找到标签名
		j := i + 1
		closing := false
		if j < len(doc) && doc[j] == '/' {
			closing = true
			j++
		}
		k := j
		for k < len(doc) && (doc[k] >= 'a' && doc[k] <= 'z' || doc[k] >= 'A' && doc[k] <= 'Z') {
			k++
		}
		name := strings.ToLower(doc[j:k])
		tagEnd := strings.IndexByte(doc[i:], '>')
		if tagEnd < 0 {
			i++
			continue
		}
		tag := doc[i : i+tagEnd+1]

		if tracked[name] {
			if closing {
				depth--
				if depth < 0 {
					return depth, viewDepths, fmt.Errorf("位置 %d 的 </%s> 多余（没有对应开标签）", i, name)
				}
			} else {
				if name == "div" && strings.Contains(tag, `class="view"`) {
					viewDepths = append(viewDepths, depth)
				}
				depth++
			}
		}
		i += tagEnd + 1
	}
	return depth, viewDepths, nil
}

// TestGrowthPageTaskCenter 任务中心 UI 契约：页面含任务表、一键完成与补领清扫入口，
// 页面脚本含对应动作；设置页含新排程项。这类「功能是否真的接线」的问题只能靠源码断言。
func TestGrowthPageTaskCenter(t *testing.T) {
	html, err := pageFS.ReadFile("pages/growth.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := assetFS.ReadFile("assets/pages/growth.js")
	if err != nil {
		t.Fatal(err)
	}
	page, script := string(html), string(js)

	for _, want := range []string{
		"成长任务",              // 卡片标题
		"sortedTasks()",     // 任务表格数据源
		"runTasksAutoAll()", // 一键完成全部
		"runTasksSweep()",   // 补领清扫按钮
		"runTaskAuto(",      // 单任务一键完成
		"runTaskClaim(",     // 单任务领取
		"tasksResults",      // 逐项执行结果
		"活动补偿",              // 补偿卡片
		"runCompensationClaim()",
		"lastSweep", // 上一轮清扫结果（落库，刷新后仍在）
		"lastQueue", // 上一轮队列结果
		"taskAt(",   // 落库时间戳格式化
	} {
		if !strings.Contains(page, want) {
			t.Errorf("growth.html 缺少 %q", want)
		}
	}
	for _, want := range []string{
		"/admin/tasks'",          // 列表
		"/admin/tasks/auto'",     // 单任务一键完成
		"/admin/tasks/auto_all'", // 全部
		"/admin/tasks/claim'",    // 领奖
		"/admin/tasks/sweep'",    // 清扫
		"/admin/compensation'",   // 补偿状态
		"/admin/tasks/accept'",   // 接受
		"/admin/tasks/sweep'",    // 补领清扫
		"/admin/compensation'",   // 补偿状态
		"/admin/compensation/claim'",
		"loadTasks", "loadCompensation",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("growth.js 缺少 %q", want)
		}
	}
	// 动作执行必须有并发闸（重复点击不能起第二轮）
	if !strings.Contains(script, "tasksBusy") || !strings.Contains(script, "tasksRunning") {
		t.Error("growth.js 缺少任务并发闸（tasksBusy/tasksRunning）")
	}
	// 门控失败必须展示人话 detail 而不是裸 reason
	if !strings.Contains(script, "r.detail") {
		t.Error("growth.js 未展示服务端 detail（门控原因需人话）")
	}

	settingsHTML, err := pageFS.ReadFile("pages/settings.html")
	if err != nil {
		t.Fatal(err)
	}
	settingsJS, err := assetFS.ReadFile("assets/pages/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"growth_queue_cron", "blackcat_cron", "task_sweep_enabled", "task_sweep_minutes"} {
		if !strings.Contains(string(settingsHTML), want) {
			t.Errorf("settings.html 缺少 %q", want)
		}
		if !strings.Contains(string(settingsJS), want) {
			t.Errorf("settings.js 未下发 %q", want)
		}
	}
}

// TestAccountPageNoDanglingHandlers account.html 里绑定的 @click / @change 处理函数都必须在
// account.js 或共享层里定义（否则点击时静默报 Alpine 未定义错误）。
// 导出 / 导入弹窗新增了若干动作（openExport / setExportScope / toggleExportUID …），
// 之前的 page 无此守卫，改模板时漏改脚本不会被任何用例发现。
func TestAccountPageNoDanglingHandlers(t *testing.T) {
	html, err := pageFS.ReadFile("pages/account.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := assetFS.ReadFile("assets/pages/account.js")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := assetFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	page, pageScript, sharedScript := string(html), string(js), string(shared)

	re := regexp.MustCompile(`@(?:click|change|submit)(?:\.\w+)?="([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if strings.Contains(pageScript, name+"(") || strings.Contains(pageScript, name+":") {
			continue
		}
		if strings.Contains(sharedScript, name+"(") || strings.Contains(sharedScript, name+":") {
			continue
		}
		t.Errorf("account.html 绑定了 %s()，但页面脚本与共享层都没有定义", name)
	}
	if len(seen) == 0 {
		t.Error("未在 account.html 中解析到任何处理器（正则或页面结构变了？）")
	}
	// 弹窗内的模板方法（x-text / x-show / @change 里调用的取值函数）同样要存在
	for _, name := range []string{"exportCount", "toggleExportUID", "exportSelectAll"} {
		if !strings.Contains(page, name+"(") {
			continue // 模板未用即不要求
		}
		if !strings.Contains(pageScript, name+"(") {
			t.Errorf("account.html 调用了 %s()，但 account.js 未定义", name)
		}
	}
}

// TestAccountPageExportImportUI 导出 / 导入弹窗的结构契约：导出口含三种范围，
// 导入口含单个 / 批量两种模式，且两条请求分别打到 export（带 scope）与 import/batch。
func TestAccountPageExportImportUI(t *testing.T) {
	html, err := pageFS.ReadFile("pages/account.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := assetFS.ReadFile("assets/pages/account.js")
	if err != nil {
		t.Fatal(err)
	}
	page, script := string(html), string(js)

	for _, want := range []string{
		"导出凭证弹窗",
		"openExport()",
		"setExportScope('current')",
		"setExportScope('selected')",
		"单个导出",
		"批量导出",
		"exportAccounts()",
		"exportForm.uids",
		"exportSelectAll()",   // 全选按钮（取代了原先的「全部账号」第三种范围）
		"exportAllSelected()", // 全选按钮的文案依据
		"导入凭证弹窗",
		"setImportMode('batch')",
		"importBatch()",
		"onBatchFile($event)",
		"batchResult",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("account.html 缺少 %q", want)
		}
	}
	// 「全部账号」这种第三种导出范围已移除：批量导出默认全选 + 全选按钮已完全覆盖它
	if strings.Contains(page, "setExportScope('all')") || strings.Contains(page, "全部账号") {
		t.Error("account.html 仍存在「全部账号」导出范围（应由批量导出 + 全选按钮承担）")
	}
	for _, want := range []string{
		"/admin/account/export?scope=",
		"/admin/account/import/batch",
		"buddy2api.accounts", // 本地预校验用 format 标识
		"X-Export-Missing",   // 部分所选账号不存在时提示
		"importMode",         // 两种模式互斥
	} {
		if !strings.Contains(script, want) {
			t.Errorf("account.js 缺少 %q", want)
		}
	}
	// 导出不得再兼容旧的裸 token.json 下载（按钮必须开弹窗，而非直接下载）
	if strings.Contains(script, "a.download = 'token.json'") {
		t.Error("account.js 仍在下载旧格式 token.json（导出应统一为凭证包）")
	}
}

// TestGrowthPageNoDanglingHandlers growth.html 里绑定的 @click 处理函数都必须在
// growth.js 或共享层里定义（否则点击时静默报 Alpine 未定义错误）。
func TestGrowthPageNoDanglingHandlers(t *testing.T) {
	html, err := pageFS.ReadFile("pages/growth.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := assetFS.ReadFile("assets/pages/growth.js")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := assetFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	page, pageScript := string(html), string(js)
	sharedScript := string(shared)

	// 抓 @click="fnName(...)" / @click="fnName"
	re := regexp.MustCompile(`@click="([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		// 页面脚本里以 `name(` 或 `name:` 或 `async name(` 定义；共享层同理
		defined := strings.Contains(pageScript, name+"(") || strings.Contains(pageScript, name+":")
		if !defined {
			defined = strings.Contains(sharedScript, name+"(") || strings.Contains(sharedScript, name+":")
		}
		if !defined {
			t.Errorf("growth.html 绑定了 %s()，但页面脚本与共享层都没有定义", name)
		}
	}
	if len(seen) == 0 {
		t.Error("未在 growth.html 中解析到任何 @click 处理器（正则或页面结构变了？）")
	}
}
