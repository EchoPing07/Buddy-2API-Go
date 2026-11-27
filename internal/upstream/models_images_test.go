package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestFetchConfigDiscoversImageModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/config" {
			t.Errorf("path=%q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"auto","name":"Auto","tags":["craft"]},{"id":"hunyuan-image-alpha","name":"Image","tags":["text-to-image"]},{"id":"hunyuan-image-alpha-edit","name":"Edit","tags":["image-to-image"]}],"agents":[{"name":"craft","models":["auto"]}]}}`))
	}))
	defer srv.Close()
	c := New(newTestTokenStore(t, "uid", "token", "refresh"), func() string { return "cn" }, 60)
	c.ChatBaseOverride = srv.URL
	c.ConfigBaseOverride = srv.URL
	cfg, err := c.FetchConfig()
	if err != nil {
		t.Fatal(err)
	}
	if want := []ImageModel{{ID: ImageGenerationModel, Kind: "text-to-image"}, {ID: ImageEditModel, Kind: "image-to-image"}}; !reflect.DeepEqual(cfg.Images, want) {
		t.Errorf("Images=%v want %v", cfg.Images, want)
	}
	if !reflect.DeepEqual(cfg.Craft, []string{"auto"}) {
		t.Errorf("Craft=%v", cfg.Craft)
	}
	// 生图模型不能混入 craft 对话可用集。
	for _, id := range cfg.Craft {
		if id == ImageGenerationModel || id == ImageEditModel {
			t.Errorf("生图模型误入对话集: %v", cfg.Craft)
		}
	}
}

func TestImageListFallbackPerRegion(t *testing.T) {
	mc := &ModelCache{}
	mc.applyFallback("cn")
	if got := mc.ImageList(); !reflect.DeepEqual(got, []string{ImageGenerationModel, ImageEditModel}) {
		t.Errorf("国内回退=%v", got)
	}

	// 国际区有自己已实测的文生图模型，不是“不支持生图”。
	mc.applyFallback("global")
	if got := mc.ImageModels(); !reflect.DeepEqual(got, []ImageModel{{ID: GlobalImageGenerationModel, Kind: "text-to-image"}}) {
		t.Errorf("国际区回退=%v", got)
	}
	if got := DefaultImageModel(mc.ImageModels(), "text-to-image"); got != GlobalImageGenerationModel {
		t.Errorf("国际区默认文生图=%q", got)
	}
	// 国际区尚无实测改图模型：不得用国内模型名冒顶。
	if got := imageModelsOfKind(mc.ImageModels(), "image-to-image"); len(got) != 0 {
		t.Errorf("国际区无改图模型时不应回落国内: %v", got)
	}
}

func imageModelsOfKind(models []ImageModel, kind string) []string {
	var out []string
	for _, m := range models {
		if m.Kind == kind {
			out = append(out, m.ID)
		}
	}
	return out
}

func TestImagesResultDataIsValidJSON(t *testing.T) {
	res, err := parseImagesResponse(http.StatusOK, []byte(testImagesSuccess))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(res.Data, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["created"] != float64(1) {
		t.Errorf("created=%v", obj["created"])
	}
}
