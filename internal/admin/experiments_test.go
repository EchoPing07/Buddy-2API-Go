package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExperimentsDefaultOff 读接口默认必须回 false（实验特性出厂关闭），且不再返回 pool_* 字段。
func TestExperimentsDefaultOff(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	req := httptest.NewRequest(http.MethodGet, "/admin/experiments", nil)
	rr := httptest.NewRecorder()
	h.getExperiments(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"embeddings_enabled":false`) || !strings.Contains(body, `"images_enabled":false`) {
		t.Errorf("默认应关闭，得到 %s", body)
	}
	for _, legacy := range []string{"pool_enabled", "pool_session_ttl", "pool_max_retry", "pool_cool_max_min", "pool_probe_cron"} {
		if strings.Contains(body, legacy) {
			t.Errorf("GET /admin/experiments 不应再返回 %s：%s", legacy, body)
		}
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

	if rr := put(`{"images_enabled":true}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"images_enabled":true`) {
		t.Fatalf("开启生图失败: %d %s", rr.Code, rr.Body.String())
	}
	if !h.cfg.Get().ImagesEnabled {
		t.Error("开启生图后配置未落库")
	}
	if rr := put(`{"images_enabled":false}`); rr.Code != http.StatusOK || h.cfg.Get().ImagesEnabled {
		t.Fatalf("关闭生图失败: %d %s", rr.Code, rr.Body.String())
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
	if h.cfg.Get().EmbeddingsEnabled || h.cfg.Get().ImagesEnabled {
		t.Error("失败请求不得改变开关状态")
	}
}

// TestPutExperimentsEnvLocked env 强制开启时，PUT false 不改值，响应仍回服务端真值 true。
func TestPutExperimentsEnvLocked(t *testing.T) {
	t.Setenv("BUDDY2API_EMBEDDINGS_ENABLED", "true")
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

func TestPutExperimentsImagesEnvLocked(t *testing.T) {
	t.Setenv("BUDDY2API_IMAGES_ENABLED", "true")
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	rr := httptest.NewRecorder()
	h.putExperiments(rr, httptest.NewRequest(http.MethodPut, "/admin/experiments", strings.NewReader(`{"images_enabled":false}`)))
	if rr.Code != http.StatusOK || !h.cfg.Get().ImagesEnabled || !strings.Contains(rr.Body.String(), `"images_enabled":true`) {
		t.Fatalf("env 锁定生图失败: %d %s", rr.Code, rr.Body.String())
	}
}

// TestPutExperimentsIgnoresLegacyPoolKeys 兼容旧客户端传入的 pool_* 字段：不报 400，也不改配置。
func TestPutExperimentsIgnoresLegacyPoolKeys(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	beforeTTL := h.cfg.Get().PoolSessionTTL

	// 1) 仅传旧版 pool_* 字段 → 200，忽略且不改写
	rr := httptest.NewRecorder()
	h.putExperiments(rr, httptest.NewRequest(http.MethodPut, "/admin/experiments",
		strings.NewReader(`{"pool_enabled":true,"pool_session_ttl":60,"pool_probe_cron":"invalid-cron"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("传入旧版 pool_* 字段不应 400，得到 %d: %s", rr.Code, rr.Body.String())
	}
	if h.cfg.Get().PoolSessionTTL != beforeTTL {
		t.Errorf("旧版 pool_session_ttl 不应改写配置: got=%d want=%d", h.cfg.Get().PoolSessionTTL, beforeTTL)
	}
	if strings.Contains(rr.Body.String(), "pool_") {
		t.Errorf("响应不应返回 pool_* 字段: %s", rr.Body.String())
	}

	// 2) embeddings_enabled + 旧版 pool_* 混传 → embeddings_enabled 生效，pool_* 忽略
	rr2 := httptest.NewRecorder()
	h.putExperiments(rr2, httptest.NewRequest(http.MethodPut, "/admin/experiments",
		strings.NewReader(`{"embeddings_enabled":true,"pool_enabled":false}`)))
	if rr2.Code != http.StatusOK || !h.cfg.Get().EmbeddingsEnabled {
		t.Fatalf("混传时 embeddings_enabled 应正常生效: code=%d body=%s", rr2.Code, rr2.Body.String())
	}
}
