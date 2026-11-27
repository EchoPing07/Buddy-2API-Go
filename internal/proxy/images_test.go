package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/upstream"
)

func TestImagesDisabledByDefault(t *testing.T) {
	h, cfg := newEmbeddingsHandler(t)
	if cfg.Get().ImagesEnabled {
		t.Fatal("生图实验默认必须关闭")
	}
	rr := httptest.NewRecorder()
	h.ImagesGenerations(rr, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"cat"}`)))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "images_disabled") {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestImagesGlobalRegionUsesGlobalModel(t *testing.T) {
	h, cfg := newEmbeddingsHandler(t)
	if err := cfg.Update(func(c *config.Config) error { c.ImagesEnabled = true; c.Region = "global"; return nil }); err != nil {
		t.Fatal(err)
	}
	models := h.imagesFor("global")
	if got := upstream.DefaultImageModel(models, "text-to-image"); got != upstream.GlobalImageGenerationModel {
		t.Fatalf("国际区默认生图模型 = %q", got)
	}
	// 国际区默认名必须通过校验（不再因“仅国内区”被拒）。
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	got, err := parseImageRequest(req, []byte(`{"prompt":"a cat"}`), false, models)
	if err != nil {
		t.Fatalf("国际区生图参数被拒: %v", err)
	}
	if got["model"] != upstream.GlobalImageGenerationModel {
		t.Errorf("model=%v", got["model"])
	}
}

func TestParseImageRequestDefaults(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	got, err := parseImageRequest(req, []byte(`{"prompt":"一只猫"}`), false, upstream.BuiltinImageModels("cn"))
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != upstream.ImageGenerationModel || got["size"] != "1024x1024" || got["n"] != 1 || got["response_format"] != "url" {
		t.Errorf("默认参数异常: %#v", got)
	}
	if got["prompt"] != "一只猫" {
		t.Errorf("prompt 丢失: %#v", got)
	}
}

func TestParseImageRequestRejectsInvalidInput(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	cases := []string{
		`{"prompt":""}`,
		`{"prompt":"cat","model":"gpt-image-1"}`,
		`{"prompt":"cat","n":2}`,
		`{"prompt":"cat","size":"2560x2560"}`,
		`{"prompt":"cat","size":"10x10"}`,
		`{"prompt":"cat","response_format":"b64"}`,
	}
	for _, body := range cases {
		if _, err := parseImageRequest(req, []byte(body), false, upstream.BuiltinImageModels("cn")); err == nil {
			t.Errorf("应拒绝 %s", body)
		}
	}
	if _, err := parseImageRequest(req, []byte(`{"prompt":"cat"}`), true, upstream.BuiltinImageModels("cn")); err == nil {
		t.Error("改图缺图片应拒绝")
	}
}

func TestParseImageRequestEditDataURL(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	got, err := parseImageRequest(req, []byte(`{"prompt":"改成蓝色","image":"data:image/png;base64,aGVsbG8="}`), true, upstream.BuiltinImageModels("cn"))
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != upstream.ImageEditModel {
		t.Errorf("model=%v", got["model"])
	}
	if images, ok := got["image"].([]any); !ok || len(images) != 1 {
		t.Errorf("image 未归一为数组: %#v", got["image"])
	}
}

func TestMultipartImageRequest(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("prompt", "改成蓝色")
	_ = writer.WriteField("size", "1024x1024")
	_ = writer.WriteField("n", "1")
	part, err := writer.CreateFormFile("image", "pixel.png")
	if err != nil {
		t.Fatal(err)
	}
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{1}, 64)...)
	_, _ = part.Write(png)
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	got, err := parseImageRequest(req, body.Bytes(), true, upstream.BuiltinImageModels("cn"))
	if err != nil {
		t.Fatal(err)
	}
	images := got["image"].([]any)
	dataURL := images[0].(string)
	if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Errorf("上传图未转 data URL: %q", dataURL)
	}
	decoded, _ := base64.StdEncoding.DecodeString(strings.SplitN(dataURL, ",", 2)[1])
	if !bytes.Equal(decoded, png) {
		t.Error("上传图片字节被改变")
	}
}

func TestImagesBase64(t *testing.T) {
	raw := []byte(`{"created":1,"data":[{"url":"https://img.example/a.png"}]}`)
	out := imagesBase64(context.Background(), raw, func(context.Context, string) ([]byte, error) {
		return []byte("png-data"), nil
	})
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	item := obj["data"].([]any)[0].(map[string]any)
	if item["b64_json"] != base64.StdEncoding.EncodeToString([]byte("png-data")) {
		t.Errorf("b64_json=%v", item["b64_json"])
	}
	if _, exists := item["url"]; exists {
		t.Error("成功转换后不应同时返回 url")
	}
}

func TestImagesBase64FailureKeepsURL(t *testing.T) {
	raw := []byte(`{"created":1,"data":[{"url":"https://img.example/a.png"}]}`)
	out := imagesBase64(context.Background(), raw, func(context.Context, string) ([]byte, error) {
		return nil, errors.New("download failed")
	})
	if !strings.Contains(string(out), `"url"`) {
		t.Errorf("下载失败必须保留 URL: %s", out)
	}
}

func TestImagesSingleAccountSuccessAndUsage(t *testing.T) {
	h, cfg := newEmbeddingsHandler(t)
	if err := cfg.Update(func(c *config.Config) error { c.ImagesEnabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"created":1,"data":[{"url":"https://img.example/a.png"}],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"credit":6.8}}}`))
	}))
	defer srv.Close()
	h.client.ChatBaseOverride = srv.URL
	ts, err := auth.NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Save(&auth.Token{AccessToken: "test-token", UID: "test-uid", Domain: "www.codebuddy.cn", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	h.client = h.client.WithTokenStore(ts)
	rr := httptest.NewRecorder()
	h.ImagesGenerations(rr, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"cat","response_format":"url"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotBody["model"] != upstream.ImageGenerationModel || gotBody["n"] != float64(1) {
		t.Errorf("上游请求体=%v", gotBody)
	}
	if _, exists := gotBody["response_format"]; exists {
		t.Error("混元上游不应收到 response_format")
	}
	if !strings.Contains(rr.Body.String(), `"url"`) {
		t.Errorf("响应=%s", rr.Body.String())
	}
}

func TestImagesPooledRetriesOnlyRejectedAccounts(t *testing.T) {
	f := newPoolFixture(t, "bad", "good")
	if err := f.cfg.Update(func(c *config.Config) error {
		c.ImagesEnabled = true
		c.PoolStrategy = config.PoolStrategyCreditsFirst
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 高余额优先：「bad」额度更高，必定先被选中，再让它额度拒绝。
	f.m.SetCredits("bad", 100, time.Now().Unix())
	f.m.SetCredits("good", 1, time.Now().Unix())
	first, second := "bad", "good"
	f.up.plan(first, respErr(http.StatusPaymentRequired, `{"code":14018,"msg":"quota exhausted"}`))
	f.up.plan(second, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"created":1,"data":[{"url":"https://img.example/a.png"}],"usage":{"total_tokens":30,"credit":6.8}}}`))
	})
	rr := httptest.NewRecorder()
	f.h.ImagesGenerations(rr, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"cat"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("额度拒绝后应换号成功: %d %s", rr.Code, rr.Body.String())
	}
	if f.up.hits[first] != 1 || f.up.hits[second] != 1 {
		t.Errorf("换号次数异常: first=%s second=%s hits=%+v", first, second, f.up.hits)
	}
}

func TestImagesPooledBadParamsDoesNotRetry(t *testing.T) {
	f := newPoolFixture(t, "first", "second")
	if err := f.cfg.Update(func(c *config.Config) error { c.ImagesEnabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	f.up.plan("first", respErr(http.StatusBadRequest, `{"code":14401,"msg":"invalid image"}`))
	f.up.plan("second", func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"data":[{"url":"https://img.example/a.png"}]}}`))
	})
	rr := httptest.NewRecorder()
	f.h.ImagesGenerations(rr, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"cat"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("参数错误应直接返回: %d %s", rr.Code, rr.Body.String())
	}
	if f.up.hits["first"] != 1 || f.up.hits["second"] != 0 {
		t.Errorf("参数错误不得换号重试: %+v", f.up.hits)
	}
}

func TestModelsListConditionalImages(t *testing.T) {
	h, cfg := newEmbeddingsHandler(t)
	rr := httptest.NewRecorder()
	h.Models(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if strings.Contains(rr.Body.String(), upstream.ImageGenerationModel) {
		t.Fatal("关闭时不应列生图模型")
	}
	if err := cfg.Update(func(c *config.Config) error { c.ImagesEnabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	h.Models(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(rr.Body.String(), upstream.ImageGenerationModel) {
		t.Errorf("开启后应列国内回退生图模型: %s", rr.Body.String())
	}
}
