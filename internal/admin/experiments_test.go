package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
