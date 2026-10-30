package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buddy2api-go/internal/config"
)

// TestExperimentsDefaultOff 读接口默认必须回 false（实验特性出厂关闭）。
func TestExperimentsDefaultOff(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	req := httptest.NewRequest(http.MethodGet, "/admin/experiments", nil)
	rr := httptest.NewRecorder()
	h.getExperiments(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"embeddings_enabled":false`) {
		t.Errorf("默认应关闭，得到 %s", rr.Body.String())
	}
}

// TestPutExperimentsToggle PUT 开→关→开均落库并回显服务端真值。
func TestPutExperimentsToggle(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})

	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/admin/experiments", strings.NewReader(body))
		rr := httptest.NewRecorder()
		h.putExperiments(rr, req)
		return rr
	}

	if rr := put(`{"embeddings_enabled":true}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"embeddings_enabled":true`) {
		t.Fatalf("开启失败: %d %s", rr.Code, rr.Body.String())
	}
	if !h.cfg.Get().EmbeddingsEnabled {
		t.Error("开启后配置未落库")
	}
	if rr := put(`{"embeddings_enabled":false}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"embeddings_enabled":false`) {
		t.Fatalf("关闭失败: %d %s", rr.Code, rr.Body.String())
	}
	if h.cfg.Get().EmbeddingsEnabled {
		t.Error("关闭后配置未落库")
	}
}

// TestPutExperimentsMissingField 缺字段 / 非法 JSON 应 400 而非误开。
func TestPutExperimentsMissingField(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	for _, body := range []string{`{}`, `not json`} {
		req := httptest.NewRequest(http.MethodPut, "/admin/experiments", strings.NewReader(body))
		rr := httptest.NewRecorder()
		h.putExperiments(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%q 应 400，得到 %d", body, rr.Code)
		}
	}
	if h.cfg.Get().EmbeddingsEnabled {
		t.Error("失败请求不得改变开关状态")
	}
}

// TestPutExperimentsEnvLocked env 强制开启时，PUT false 不改值（与设置页 env 锁定同口径），
// 响应仍回服务端真值 true，前端据此回显、开关弹回。
func TestPutExperimentsEnvLocked(t *testing.T) {
	t.Setenv("BUDDY2API_EMBEDDINGS_ENABLED", "true") // 须在 newGrowthTestHandler 内 config.Load 前生效
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})

	req := httptest.NewRequest(http.MethodPut, "/admin/experiments", strings.NewReader(`{"embeddings_enabled":false}`))
	rr := httptest.NewRecorder()
	h.putExperiments(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"embeddings_enabled":true`) {
		t.Errorf("env 锁定下响应应保持 true，得到 %s", rr.Body.String())
	}
	if !h.cfg.Get().EmbeddingsEnabled {
		t.Error("env 锁定下配置不应被 PUT 关闭")
	}
}

// TestPutExperimentsPartialFields 逐字段可选：只提交一个字段不应 400。
// 实验开关是按特性逐个挂的，多挂一个特性不能让老前端提交失败。
func TestPutExperimentsPartialFields(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})

	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/admin/experiments", strings.NewReader(body))
		rr := httptest.NewRecorder()
		h.putExperiments(rr, req)
		return rr
	}

	// 只开号池，不动嵌入
	if rr := put(`{"pool_enabled":true}`); rr.Code != http.StatusOK {
		t.Fatalf("只提交 pool_enabled 应成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	if !h.cfg.Get().PoolEnabled {
		t.Error("pool_enabled 未落库")
	}
	if h.cfg.Get().EmbeddingsEnabled {
		t.Error("未提交的 embeddings_enabled 不应被改动")
	}

	// 只改参数，不动开关
	if rr := put(`{"pool_session_ttl":60}`); rr.Code != http.StatusOK {
		t.Fatalf("只提交 ttl 应成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	if got := h.cfg.Get().PoolSessionTTL; got != 60 {
		t.Errorf("pool_session_ttl = %d，期望 60", got)
	}
	if !h.cfg.Get().PoolEnabled {
		t.Error("未提交的 pool_enabled 不应被改动")
	}
}

// TestPutExperimentsPoolParamsClamped 参数超界被钳制（envInt 只挡 <=0）。
func TestPutExperimentsPoolParamsClamped(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	req := httptest.NewRequest(http.MethodPut, "/admin/experiments",
		strings.NewReader(`{"pool_session_ttl":999999,"pool_max_retry":999,"pool_cool_max_min":999999}`))
	rr := httptest.NewRecorder()
	h.putExperiments(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rr.Code, rr.Body.String())
	}
	cfg := h.cfg.Get()
	if cfg.PoolSessionTTL > config.PoolSessionTTLMax {
		t.Errorf("ttl 未钳制：%d", cfg.PoolSessionTTL)
	}
	if cfg.PoolMaxRetry > config.PoolMaxRetryMax {
		t.Errorf("max_retry 未钳制：%d", cfg.PoolMaxRetry)
	}
	if cfg.PoolCoolMaxMin > config.PoolCoolMaxMinMax {
		t.Errorf("cool_max_min 未钳制：%d", cfg.PoolCoolMaxMin)
	}
}

// TestPutExperimentsInvalidProbeCron 非法 cron 应 400 且不改配置。
func TestPutExperimentsInvalidProbeCron(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	before := h.cfg.Get().PoolProbeCron
	req := httptest.NewRequest(http.MethodPut, "/admin/experiments",
		strings.NewReader(`{"pool_probe_cron":"not a cron"}`))
	rr := httptest.NewRecorder()
	h.putExperiments(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法 cron 应 400，得到 %d", rr.Code)
	}
	if h.cfg.Get().PoolProbeCron != before {
		t.Error("失败请求不得改变 cron")
	}
}

// TestGetExperimentsIncludesPool 读接口应带号池字段，默认全关。
func TestGetExperimentsIncludesPool(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	req := httptest.NewRequest(http.MethodGet, "/admin/experiments", nil)
	rr := httptest.NewRecorder()
	h.getExperiments(rr, req)
	body := rr.Body.String()
	for _, want := range []string{`"pool_enabled":false`, `"pool_session_ttl":180`,
		`"pool_max_retry":2`, `"pool_cool_max_min":360`, `"pool_probe_cron"`} {
		if !strings.Contains(body, want) {
			t.Errorf("响应缺少 %s：%s", want, body)
		}
	}
}
