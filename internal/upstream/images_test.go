package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testImagesSuccess = `{"code":0,"msg":"OK","data":{"created":1,"data":[{"url":"https://example.com/a.png"}],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"output_image_counts":1,"credit":6.8}}}`

func TestDoImagesSuccess(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(testImagesSuccess))
	}))
	defer srv.Close()
	ts := newTestTokenStore(t, "image-uid", "image-token", "refresh")
	c := New(ts, func() string { return "cn" }, 60)
	c.ChatBaseOverride = srv.URL

	res, err := c.DoImages(context.Background(), false, []byte(`{"model":"hunyuan-image-alpha","prompt":"a cat","n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v2/images/generations" || gotAuth != "Bearer image-token" {
		t.Errorf("path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["model"] != ImageGenerationModel {
		t.Errorf("body=%v", gotBody)
	}
	if res.Usage.TotalTokens != 30 || res.Usage.Credit != 6.8 || res.Usage.ImageCounts != 1 {
		t.Errorf("usage=%+v", res.Usage)
	}
	if !strings.Contains(string(res.Data), `"url"`) {
		t.Errorf("响应本体被改写: %s", res.Data)
	}
}

func TestDoImagesEditPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/images/edits" {
			t.Errorf("path=%q", r.URL.Path)
		}
		_, _ = w.Write([]byte(testImagesSuccess))
	}))
	defer srv.Close()
	c := New(newTestTokenStore(t, "uid", "token", "refresh"), func() string { return "cn" }, 60)
	c.ChatBaseOverride = srv.URL
	if _, err := c.DoImages(context.Background(), true, []byte(`{"model":"hunyuan-image-alpha-edit"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestDoImagesBusinessError(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"code":14401,"msg":"route config not found"}`},
		{http.StatusBadRequest, `{"code":14401,"msg":"bad image request"}`},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := New(newTestTokenStore(t, "uid", "token", "refresh"), func() string { return "cn" }, 60)
		c.ChatBaseOverride = srv.URL
		_, err := c.DoImages(context.Background(), false, []byte(`{}`))
		srv.Close()
		ue, ok := err.(*ImagesError)
		if !ok {
			t.Fatalf("status=%d error=%T %v，期望 ImagesError", tc.status, err, err)
		}
		if ue.Status != tc.status || !strings.Contains(ue.Error(), "14401") {
			t.Errorf("ImagesError=%+v", ue)
		}
	}
}

func TestDoImagesEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"data":[]}}`))
	}))
	defer srv.Close()
	c := New(newTestTokenStore(t, "uid", "token", "refresh"), func() string { return "cn" }, 60)
	c.ChatBaseOverride = srv.URL
	if _, err := c.DoImages(context.Background(), false, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "缺少图片") {
		t.Fatalf("空图片结果应报错，得到 %v", err)
	}
}
