package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/basketikun/infinite-canvas/model"
	"github.com/basketikun/infinite-canvas/service"
)

func AIImagesGenerations(w http.ResponseWriter, r *http.Request) {
	proxyAIRequest(w, r, "/images/generations")
}

func AIImagesEdits(w http.ResponseWriter, r *http.Request) {
	proxyAIRequest(w, r, "/images/edits")
}

func AIChatCompletions(w http.ResponseWriter, r *http.Request) {
	proxyAIRequest(w, r, "/chat/completions")
}

func AIAudioSpeech(w http.ResponseWriter, r *http.Request) {
	proxyAIRequest(w, r, "/audio/speech")
}

func AIVideos(w http.ResponseWriter, r *http.Request) {
	proxyAIRequest(w, r, "/videos")
}

func AIVideo(w http.ResponseWriter, r *http.Request, id string) {
	proxyAIGetRequest(w, r, "/videos/"+id)
}

func AIVideoContent(w http.ResponseWriter, r *http.Request, id string) {
	proxyAIGetRequest(w, r, "/videos/"+id+"/content")
}

func proxyAIGetRequest(w http.ResponseWriter, r *http.Request, path string) {
	modelName := r.URL.Query().Get("model")
	if strings.TrimSpace(modelName) == "" {
		modelName = "grok-imagine-video"
	}
	channel, err := service.SelectModelChannel(modelName)
	if err != nil {
		log.Printf("AI proxy select channel failed: model=%s err=%v", modelName, err)
		Fail(w, aiStatusMessage(0))
		return
	}
	if isAgnesAI(channel.Protocol, channel.BaseURL, modelName) {
		proxyAgnesGetRequest(w, channel, modelName, path)
		return
	}
	path = resolveAIProxyPath(channel.BaseURL, modelName, path)
	request, err := http.NewRequest(http.MethodGet, service.BuildModelChannelURL(channel, path), nil)
	if err != nil {
		Fail(w, aiStatusMessage(0))
		return
	}
	request.Header.Set("Authorization", "Bearer "+channel.APIKey)
	copyAIResponse(w, request, nil)
}

func proxyAIRequest(w http.ResponseWriter, r *http.Request, path string) {
	body, contentType, modelName, err := readAIRequest(r)
	if err != nil {
		log.Printf("AI proxy request read failed: %v", err)
		Fail(w, aiStatusMessage(0))
		return
	}
	user, ok := service.UserFromContext(r.Context())
	if !ok {
		Fail(w, "未登录或权限不足")
		return
	}
	credits, err := service.ModelCost(modelName)
	if err != nil {
		log.Printf("AI proxy read model cost failed: model=%s err=%v", modelName, err)
		Fail(w, aiStatusMessage(0))
		return
	}
	credits *= readAIRequestCount(body, contentType)
	channel, err := service.SelectModelChannel(modelName)
	if err != nil {
		log.Printf("AI proxy select channel failed: model=%s err=%v", modelName, err)
		Fail(w, aiStatusMessage(0))
		return
	}
	if isAgnesAI(channel.Protocol, channel.BaseURL, modelName) {
		if err := service.ConsumeUserCredits(user.ID, modelName, credits, path); err != nil {
			FailError(w, err)
			return
		}
		proxyAgnesPostRequest(w, channel, path, body, contentType, func() {
			if err := service.RefundUserCredits(user.ID, modelName, credits, path); err != nil {
				log.Printf("AI proxy refund credits failed: user=%s model=%s credits=%d err=%v", user.ID, modelName, credits, err)
			}
		})
		return
	}
	path = resolveAIProxyPath(channel.BaseURL, modelName, path)
	request, err := http.NewRequest(http.MethodPost, service.BuildModelChannelURL(channel, path), bytes.NewReader(body))
	if err != nil {
		log.Printf("AI proxy build request failed: url=%s err=%v", service.BuildModelChannelURL(channel, path), err)
		Fail(w, aiStatusMessage(0))
		return
	}
	request.Header.Set("Authorization", "Bearer "+channel.APIKey)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if err := service.ConsumeUserCredits(user.ID, modelName, credits, path); err != nil {
		FailError(w, err)
		return
	}
	copyAIResponse(w, request, func() {
		if err := service.RefundUserCredits(user.ID, modelName, credits, path); err != nil {
			log.Printf("AI proxy refund credits failed: user=%s model=%s credits=%d err=%v", user.ID, modelName, credits, err)
		}
	})
}

func copyAIResponse(w http.ResponseWriter, request *http.Request, onFailure func()) {
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		log.Printf("AI proxy request failed: url=%s err=%v", request.URL.String(), err)
		if onFailure != nil {
			onFailure()
		}
		Fail(w, aiStatusMessage(0))
		return
	}
	defer response.Body.Close()

	if response.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		log.Printf("AI upstream error: url=%s status=%d", request.URL.String(), response.StatusCode)
		if onFailure != nil {
			onFailure()
		}
		Fail(w, aiUpstreamStatusMessage(response.StatusCode, body))
		return
	}

	for key, values := range response.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func readAIRequest(r *http.Request) ([]byte, string, string, error) {
	contentType := r.Header.Get("Content-Type")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, "", "", err
	}
	modelName := ""
	if strings.HasPrefix(contentType, "multipart/form-data") {
		modelName = readMultipartModel(body, contentType)
	} else {
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		modelName = payload.Model
	}
	if strings.TrimSpace(modelName) == "" {
		return nil, "", "", errMissingModel
	}
	return body, contentType, modelName, nil
}

func readMultipartModel(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	form, err := reader.ReadForm(32 << 20)
	if err != nil {
		return ""
	}
	defer form.RemoveAll()
	if values := form.Value["model"]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func readAIRequestCount(body []byte, contentType string) int {
	count := 1
	if strings.HasPrefix(contentType, "multipart/form-data") {
		_, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			return count
		}
		form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(32 << 20)
		if err != nil {
			return count
		}
		defer form.RemoveAll()
		if values := form.Value["n"]; len(values) > 0 {
			_, _ = fmt.Sscan(values[0], &count)
		}
	} else {
		var payload struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal(body, &payload)
		count = payload.N
	}
	if count < 1 {
		return 1
	}
	return count
}

var errMissingModel = &aiError{"缺少模型名称"}

func isAgnesAI(protocol string, baseURL string, modelName string) bool {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	baseURL = strings.ToLower(strings.TrimSpace(baseURL))
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	return protocol == "agnes" || strings.Contains(baseURL, "agnes-ai.com") || strings.HasPrefix(modelName, "agnes-")
}

func proxyAgnesPostRequest(w http.ResponseWriter, channel model.ModelChannel, path string, body []byte, contentType string, onFailure func()) {
	var requestBody []byte
	var targetPath string
	var err error
	if path == "/images/generations" {
		requestBody, err = agnesImageGenerationBody(body)
		targetPath = "/v1/images/generations"
	} else if path == "/images/edits" {
		requestBody, err = agnesImageEditBody(body, contentType)
		targetPath = "/v1/images/generations"
	} else if path == "/videos" {
		requestBody, err = agnesVideoBody(body, contentType)
		targetPath = "/v1/videos"
	} else {
		Fail(w, aiStatusMessage(0))
		return
	}
	if err != nil {
		log.Printf("Agnes proxy transform failed: path=%s err=%v", path, err)
		if onFailure != nil {
			onFailure()
		}
		Fail(w, aiStatusMessage(0))
		return
	}
	request, err := http.NewRequest(http.MethodPost, agnesChannelURL(channel.BaseURL, targetPath), bytes.NewReader(requestBody))
	if err != nil {
		if onFailure != nil {
			onFailure()
		}
		Fail(w, aiStatusMessage(0))
		return
	}
	request.Header.Set("Authorization", "Bearer "+channel.APIKey)
	request.Header.Set("Content-Type", "application/json")
	if path == "/videos" {
		copyAgnesVideoCreateResponse(w, request, onFailure)
		return
	}
	copyAIResponse(w, request, onFailure)
}

func proxyAgnesGetRequest(w http.ResponseWriter, channel model.ModelChannel, modelName string, path string) {
	if strings.HasPrefix(path, "/videos/") && strings.HasSuffix(path, "/content") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/videos/"), "/content")
		proxyAgnesVideoContent(w, channel, id, modelName)
		return
	}
	if strings.HasPrefix(path, "/videos/") {
		id := strings.TrimPrefix(path, "/videos/")
		request, err := http.NewRequest(http.MethodGet, agnesVideoStatusURL(channel.BaseURL, id, modelName), nil)
		if err != nil {
			Fail(w, aiStatusMessage(0))
			return
		}
		request.Header.Set("Authorization", "Bearer "+channel.APIKey)
		copyAIResponse(w, request, nil)
		return
	}
	Fail(w, aiStatusMessage(0))
}

func agnesImageGenerationBody(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	delete(payload, "response_format")
	delete(payload, "output_format")
	payload["return_base64"] = true
	return json.Marshal(payload)
}

func agnesImageEditBody(body []byte, contentType string) ([]byte, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(64 << 20)
	if err != nil {
		return nil, err
	}
	defer form.RemoveAll()
	payload := map[string]any{
		"model":         firstFormValue(form, "model"),
		"prompt":        firstFormValue(form, "prompt"),
		"return_base64": true,
	}
	if value := firstFormValue(form, "size"); value != "" {
		payload["size"] = value
	}
	if value := firstFormValue(form, "n"); value != "" {
		payload["n"] = value
	}
	images := []string{}
	for _, header := range form.File["image"] {
		image, err := multipartFileDataURL(header)
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	if len(images) > 0 {
		payload["image"] = images
	}
	return json.Marshal(payload)
}

func agnesVideoBody(body []byte, contentType string) ([]byte, error) {
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		return body, nil
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(64 << 20)
	if err != nil {
		return nil, err
	}
	defer form.RemoveAll()
	payload := map[string]any{
		"model":      firstFormValue(form, "model"),
		"prompt":     firstFormValue(form, "prompt"),
		"num_frames": 121,
		"frame_rate": 24,
	}
	if width, height := agnesVideoSize(firstFormValue(form, "size")); width > 0 && height > 0 {
		payload["width"] = width
		payload["height"] = height
	}
	images := []string{}
	for _, header := range form.File["input_reference[]"] {
		image, err := multipartFileDataURL(header)
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	if len(images) == 1 {
		payload["image"] = images[0]
	} else if len(images) > 1 {
		payload["extra_body"] = map[string]any{"image": images}
	}
	return json.Marshal(payload)
}

func copyAgnesVideoCreateResponse(w http.ResponseWriter, request *http.Request, onFailure func()) {
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		if onFailure != nil {
			onFailure()
		}
		Fail(w, aiStatusMessage(0))
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode >= http.StatusBadRequest {
		if onFailure != nil {
			onFailure()
		}
		Fail(w, aiUpstreamStatusMessage(response.StatusCode, body))
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		if videoID, ok := payload["video_id"].(string); ok && strings.TrimSpace(videoID) != "" {
			payload["id"] = videoID
			body, _ = json.Marshal(payload)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

func proxyAgnesVideoContent(w http.ResponseWriter, channel model.ModelChannel, id string, modelName string) {
	request, err := http.NewRequest(http.MethodGet, agnesVideoStatusURL(channel.BaseURL, id, modelName), nil)
	if err != nil {
		Fail(w, aiStatusMessage(0))
		return
	}
	request.Header.Set("Authorization", "Bearer "+channel.APIKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		Fail(w, aiStatusMessage(0))
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode >= http.StatusBadRequest {
		Fail(w, aiUpstreamStatusMessage(response.StatusCode, body))
		return
	}
	var payload struct {
		URL string `json:"remixed_from_video_id"`
	}
	_ = json.Unmarshal(body, &payload)
	if strings.TrimSpace(payload.URL) == "" {
		Fail(w, aiStatusMessage(0))
		return
	}
	download, err := http.Get(payload.URL)
	if err != nil {
		Fail(w, aiStatusMessage(0))
		return
	}
	defer download.Body.Close()
	if download.StatusCode >= http.StatusBadRequest {
		Fail(w, aiStatusMessage(download.StatusCode))
		return
	}
	for key, values := range download.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(download.StatusCode)
	_, _ = io.Copy(w, download.Body)
}

func agnesChannelURL(baseURL string, path string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + path
}

func agnesVideoStatusURL(baseURL string, id string, modelName string) string {
	values := url.Values{}
	values.Set("video_id", id)
	if strings.TrimSpace(modelName) != "" {
		values.Set("model_name", modelName)
	}
	return agnesChannelURL(baseURL, "/agnesapi?") + values.Encode()
}

func firstFormValue(form *multipart.Form, key string) string {
	if values := form.Value[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func multipartFileDataURL(header *multipart.FileHeader) (string, error) {
	file, err := header.Open()
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func agnesVideoSize(size string) (int, int) {
	var width, height int
	if _, err := fmt.Sscanf(size, "%dx%d", &width, &height); err == nil && width > 0 && height > 0 {
		return width, height
	}
	return 1152, 768
}
func resolveAIProxyPath(baseURL string, modelName string, path string) string {
	if !isArkSeedanceVideo(baseURL, modelName) {
		return path
	}
	if path == "/videos" {
		return "/contents/generations/tasks"
	}
	if strings.HasPrefix(path, "/videos/") && !strings.HasSuffix(path, "/content") {
		return "/contents/generations/tasks/" + strings.TrimPrefix(path, "/videos/")
	}
	return path
}

func isArkSeedanceVideo(baseURL string, modelName string) bool {
	base := strings.ToLower(baseURL)
	model := strings.ToLower(modelName)
	return strings.Contains(model, "seedance") || strings.Contains(model, "doubao-seedance") || strings.Contains(base, "/api/plan/v3")
}

func aiStatusMessage(statusCode int) string {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "AI 接口鉴权失败，请检查 API Key、套餐权限或模型权限"
	case http.StatusTooManyRequests:
		return "AI 接口限流或额度不足，请稍后重试或检查额度"
	default:
		return "AI 接口请求失败"
	}
}

func aiUpstreamStatusMessage(statusCode int, body []byte) string {
	base := aiStatusMessage(statusCode)
	detail := aiUpstreamErrorDetail(body)
	if detail == "" {
		return base
	}
	return base + "：" + detail
}

func aiUpstreamErrorDetail(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	var payload struct {
		Msg     string `json:"msg"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if payload.Error.Message != "" {
			if detail := friendlyUpstreamError(payload.Error.Code, payload.Error.Message); detail != "" {
				return safeUpstreamText(detail)
			}
			if payload.Error.Code != "" {
				return safeUpstreamText(payload.Error.Code + " " + payload.Error.Message)
			}
			return safeUpstreamText(payload.Error.Message)
		}
		if payload.Msg != "" {
			return safeUpstreamText(payload.Msg)
		}
		if payload.Message != "" {
			return safeUpstreamText(payload.Message)
		}
	}
	return safeUpstreamText(text)
}

func friendlyUpstreamError(code string, message string) string {
	lowerCode := strings.ToLower(strings.TrimSpace(code))
	if strings.Contains(lowerCode, "inputvideosensitivecontentdetected") || strings.Contains(lowerCode, "privacyinformation") {
		return strings.TrimSpace(code + " 参考视频疑似包含真人或隐私信息，火山方舟拒绝使用普通 URL 作为真人视频参考；请改用不含真人的视频、官方允许的模型产物，或已授权的 asset:// 素材。原始错误：" + message)
	}
	return ""
}

func safeUpstreamText(text string) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	runes := []rune(text)
	if len(runes) > 300 {
		return string(runes[:300]) + "..."
	}
	return text
}

type aiError struct {
	message string
}

func (err *aiError) Error() string {
	return err.message
}
