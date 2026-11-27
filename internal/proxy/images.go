// images.go OpenAI Images 兼容端点（实验特性，默认关闭）。
// 国内区与国际区共用同一套上游协议，模型按区域由 /v3/config 能力标签发现。
package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"buddy2api-go/internal/apikey"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

const (
	defaultImageSize = "1024x1024"
	maxImagePixels   = 4 << 20
	maxInputImage    = 8 << 20
	maxOutputImage   = 32 << 20
)

var imageSizeRE = regexp.MustCompile(`^([1-9][0-9]{1,4})x([1-9][0-9]{1,4})$`)

// ImagesGenerations / ImagesEdits 分别是文生图与图生图入口。
func (h *Handler) ImagesGenerations(w http.ResponseWriter, r *http.Request) { h.images(w, r, false) }
func (h *Handler) ImagesEdits(w http.ResponseWriter, r *http.Request)       { h.images(w, r, true) }

func (h *Handler) images(w http.ResponseWriter, r *http.Request, edit bool) {
	if !h.cfg.Get().ImagesEnabled {
		openaiError(w, http.StatusForbidden, "invalid_request_error", "images_disabled",
			"生图端点未开启：请在管理后台「实验」页开启（默认关闭）")
		return
	}
	start := time.Now()
	key := apikey.FromContext(r.Context())
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "read_body_failed", "读取请求体失败: "+err.Error())
		return
	}
	req, err := parseImageRequest(r, raw, edit, h.imagesFor(h.cfg.Get().Region))
	if err != nil {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_image_request", err.Error())
		return
	}
	model, _ := req["model"].(string)
	responseFormat, _ := req["response_format"].(string)
	// 混元官方 CLI 不传 response_format；该字段只控制本网关的回程转换。
	delete(req, "response_format")
	upBody, _ := json.Marshal(req)

	logEntry := &store.LogEntry{APIKeyID: keyID(key), APIKeyName: keyName(key), Model: model}
	defer func() {
		logEntry.DurationMs = time.Since(start).Milliseconds()
		h.writeLog(logEntry)
		if key != nil {
			h.keys.RecordUsage(key.ID, int64(logEntry.TotalTokens))
		}
	}()

	if h.pool == nil {
		result, err := h.client.DoImages(r.Context(), edit, upBody)
		if err != nil {
			h.writeImagesFailure(w, logEntry, err)
			return
		}
		h.writeImagesSuccess(w, r, logEntry, result, responseFormat)
		return
	}
	h.imagesPooled(w, r, edit, upBody, model, responseFormat, logEntry)
}

func (h *Handler) imagesPooled(w http.ResponseWriter, r *http.Request, edit bool, body []byte, model, responseFormat string, logEntry *store.LogEntry) {
	tried := map[string]bool{}
	maxRetry := h.pool.MaxRetry()
	for attempt := 0; ; attempt++ {
		uid, ok := h.pool.Resolve("", model, tried)
		if !ok {
			h.writePoolExhausted(w, logEntry, model, len(tried))
			return
		}
		tried[uid] = true
		logEntry.AccountUID = uid
		ts, err := h.pool.TokenStore(uid)
		if err != nil {
			h.writePoolExhausted(w, logEntry, model, len(tried))
			return
		}
		h.pool.AcquireInflight(uid)
		result, err := h.client.DoImagesWith(r.Context(), ts, edit, body)
		h.pool.ReleaseInflight(uid)
		if err == nil {
			h.pool.NoteSuccess(uid, model)
			h.writeImagesSuccess(w, r, logEntry, result, responseFormat)
			return
		}

		var ue *upstream.ImagesError
		if !errors.As(err, &ue) {
			h.pool.NoteError(uid, model, upstream.ErrKindTransport, nil)
			h.writeImagesFailure(w, logEntry, err)
			return
		}
		kind := imageErrorKind(ue)
		h.pool.NoteError(uid, model, kind, ue.Raw)
		if !kind.Retryable() || attempt >= maxRetry {
			h.writeImagesFailure(w, logEntry, err)
			return
		}
		slog.Info("生图换号重试", "model", model, "failed_uid", shortUID(uid), "attempt", attempt+1)
	}
}

func imageErrorKind(err *upstream.ImagesError) upstream.ChatErrKind {
	kind := upstream.ClassifyChatError(err.Status, err.Raw)
	if kind == upstream.ErrKindNone {
		return upstream.ErrKindBadParams // HTTP 200 + 非零业务 code
	}
	return kind
}

func (h *Handler) writeImagesFailure(w http.ResponseWriter, logEntry *store.LogEntry, err error) {
	status := http.StatusServiceUnavailable
	raw := []byte(err.Error())
	var ue *upstream.ImagesError
	if errors.As(err, &ue) {
		status = ue.Status
		raw = ue.Raw
		if status == http.StatusOK { // 业务包络错误没有可透传的 HTTP 错误码
			status = http.StatusBadRequest
			switch imageErrorKind(ue) {
			case upstream.ErrKindCredential:
				status = http.StatusUnauthorized
			case upstream.ErrKindQuotaExhausted:
				status = http.StatusPaymentRequired
			case upstream.ErrKindModelRate:
				status = http.StatusTooManyRequests
			}
		}
	}
	logEntry.StatusCode = status
	logEntry.ErrorMsg = trunc(string(raw), 500)
	openaiError(w, status, "upstream_error", "upstream_error", upstreamErrorMessage(raw, status))
}

func (h *Handler) writeImagesSuccess(w http.ResponseWriter, r *http.Request, logEntry *store.LogEntry, result *upstream.ImagesResult, responseFormat string) {
	logEntry.StatusCode = http.StatusOK
	logEntry.PromptTokens = result.Usage.InputTokens
	logEntry.CompletionTokens = result.Usage.OutputTokens
	logEntry.TotalTokens = result.Usage.TotalTokens
	logEntry.Credit = result.Usage.Credit
	logEntry.UsagePresent = true
	logEntry.FinishReason = "stop"
	data := result.Data
	if responseFormat == "b64_json" {
		data = imagesBase64(r.Context(), data, fetchGeneratedImage)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

// imagesFor 当前区域的图像模型：优先用目录发现结果，目录为空（拉取失败 / 隔离单测）
// 时回退到该区的内置实测表，绝不把“没发现”当成“不支持”。
func (h *Handler) imagesFor(region string) []upstream.ImageModel {
	if list := h.models.ImageModels(); len(list) > 0 {
		return list
	}
	return upstream.BuiltinImageModels(region)
}

// parseImageRequest 校验公共参数，并把 multipart 改图请求归一为上游 JSON。
// models 是当前区域发现的图像模型（空则用内置回退表）。
func parseImageRequest(r *http.Request, raw []byte, edit bool, models []upstream.ImageModel) (map[string]any, error) {
	if len(models) == 0 {
		models = upstream.BuiltinImageModels("cn")
	}
	var req map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if !edit {
			return nil, errors.New("文生图请使用 application/json")
		}
		var err error
		req, err = multipartImageRequest(raw, r.Header.Get("Content-Type"))
		if err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(raw, &req); err != nil {
		return nil, errors.New("请求体不是合法 JSON")
	}
	if req == nil {
		return nil, errors.New("请求体必须是 JSON 对象")
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt 不能为空")
	}
	model, _ := req["model"].(string)
	want := imageKind(edit)
	if model == "" {
		model = upstream.DefaultImageModel(models, want)
	}
	if !containsImageModel(models, model, want) {
		return nil, fmt.Errorf("%s 模型 %q 不可用，当前可用：%s", wantLabel(want), model, strings.Join(imageModelIDs(models, want), "、"))
	}
	req["model"] = model
	size, err := normalizeImageSize(req["size"])
	if err != nil {
		return nil, err
	}
	req["size"] = size
	if n, exists := req["n"]; exists && !imageCountOne(n) {
		return nil, errors.New("上游一次只生成一张图，n 仅支持 1")
	}
	req["n"] = 1
	format, _ := req["response_format"].(string)
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return nil, errors.New("response_format 仅支持 url 或 b64_json")
	}
	req["response_format"] = format
	if edit {
		if mask, ok := req["mask"]; ok {
			if !isDataImageURL(mask) {
				return nil, errors.New("mask 必须为 data:image/*;base64,...")
			}
		}
		if err := normalizeEditImages(req); err != nil {
			return nil, err
		}
	}
	return req, nil
}

func imageCountOne(v any) bool {
	switch n := v.(type) {
	case float64:
		return n == 1
	case string:
		return n == "1"
	default:
		return false
	}
}

func normalizeImageSize(v any) (string, error) {
	size, _ := v.(string)
	if size == "" || size == "auto" {
		return defaultImageSize, nil
	}
	m := imageSizeRE.FindStringSubmatch(size)
	if m == nil {
		return "", errors.New("size 须为 宽x高，例如 1024x1024")
	}
	width, _ := strconv.Atoi(m[1])
	height, _ := strconv.Atoi(m[2])
	if width < 16 || height < 16 || width*height > maxImagePixels {
		return "", fmt.Errorf("size 超出范围：宽高至少 16，总像素不得超过 %d", maxImagePixels)
	}
	return size, nil
}

func imageKind(edit bool) string {
	if edit {
		return "image-to-image"
	}
	return "text-to-image"
}

func wantLabel(kind string) string {
	if kind == "image-to-image" {
		return "改图"
	}
	return "文生图"
}

func containsImageModel(models []upstream.ImageModel, id, kind string) bool {
	for _, m := range models {
		if m.ID == id && m.Kind == kind {
			return true
		}
	}
	return false
}

func imageModelIDs(models []upstream.ImageModel, kind string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if m.Kind == kind {
			out = append(out, m.ID)
		}
	}
	return out
}

func isDataImageURL(v any) bool {
	s, ok := v.(string)
	return ok && strings.HasPrefix(s, "data:image/")
}

func normalizeEditImages(req map[string]any) error {
	images, ok := req["image"].([]any)
	if !ok {
		if one, ok := req["image"].(string); ok && one != "" {
			images = []any{one}
		}
	}
	if len(images) == 0 {
		return errors.New("改图需要 image 文件或 data:image URL")
	}
	for _, item := range images {
		if !isDataImageURL(item) {
			return errors.New("image 必须为 data:image/*;base64,... 或其数组")
		}
	}
	req["image"] = images
	return nil
}

func multipartImageRequest(raw []byte, contentType string) (map[string]any, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil, errors.New("multipart/form-data 缺少有效 boundary")
	}
	form, err := multipart.NewReader(bytes.NewReader(raw), params["boundary"]).ReadForm(maxInputImage)
	if err != nil {
		return nil, fmt.Errorf("解析 multipart 失败: %w", err)
	}
	defer form.RemoveAll()
	out := map[string]any{}
	for key, values := range form.Value {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	var images []any
	for key, files := range form.File {
		for _, header := range files {
			dataURL, err := imageFileDataURL(header)
			if err != nil {
				return nil, err
			}
			switch key {
			case "image", "images":
				images = append(images, dataURL)
			case "mask":
				out["mask"] = dataURL
			}
		}
	}
	if len(images) > 0 {
		out["image"] = images
	}
	return out, nil
}

func imageFileDataURL(header *multipart.FileHeader) (string, error) {
	file, err := header.Open()
	if err != nil {
		return "", err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxInputImage+1))
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || len(raw) > maxInputImage {
		return "", fmt.Errorf("单张输入图片须为 1 字节至 %d MiB", maxInputImage>>20)
	}
	contentType := http.DetectContentType(raw)
	if !strings.HasPrefix(contentType, "image/") {
		if declared := header.Header.Get("Content-Type"); strings.HasPrefix(declared, "image/") {
			contentType = declared
		} else {
			return "", errors.New("上传文件不是受支持的图片")
		}
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// imagesBase64 只转换已经生成的图片。下载失败保留 URL，避免成品已扣费却被网关丢弃。
func imagesBase64(ctx context.Context, raw []byte, fetch func(context.Context, string) ([]byte, error)) []byte {
	var response map[string]any
	if json.Unmarshal(raw, &response) != nil {
		return raw
	}
	items, _ := response["data"].([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		imageURL, _ := m["url"].(string)
		if imageURL == "" {
			continue
		}
		data, err := fetch(ctx, imageURL)
		if err != nil {
			slog.Warn("生图 Base64 转换失败，保留 URL", "error", err)
			continue
		}
		m["b64_json"] = base64.StdEncoding.EncodeToString(data)
		delete(m, "url")
	}
	out, err := json.Marshal(response)
	if err != nil {
		return raw
	}
	return out
}

func fetchGeneratedImage(ctx context.Context, location string) ([]byte, error) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".myqcloud.com") {
		return nil, errors.New("图片 URL 不是受信任的腾讯云 COS HTTPS 地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 45 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("图片下载不允许重定向")
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("图片下载返回 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOutputImage+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxOutputImage {
		return nil, errors.New("生成图片超过 32 MiB")
	}
	return raw, nil
}
