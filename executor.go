package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// executorRequest mirrors the ExecutorRequest JSON the host sends, plus the
// plugin-only stream identifier the host adds for streaming calls.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func decodeExecutorRequest(request []byte) (*executorRequest, error) {
	req := &executorRequest{}
	if len(request) == 0 {
		return req, nil
	}
	if errUnmarshal := json.Unmarshal(request, req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	return req, nil
}

// parsedChat is the client request reduced to what a media generation needs.
type parsedChat struct {
	Model       string
	Prompt      string
	Images      []string
	AspectRatio string
	Resolution  string
	Size        string
	Duration    int
	Count       int
	Family      string
	Stream      bool
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseChatPayload accepts Chat Completions bodies (the declared input format)
// and tolerates the two neighbouring shapes CPA may forward: an OpenAI Responses
// body and an Images body.
func parseChatPayload(payload []byte) (*parsedChat, error) {
	parsed := &parsedChat{Count: 1}
	if len(payload) == 0 {
		return parsed, nil
	}
	var root map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(payload, &root); errUnmarshal != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON：%w", errUnmarshal)
	}

	parsed.Model = rawString(root["model"])
	parsed.Stream = rawBool(root["stream"])

	// Messages: standard chat completions.
	var messages []chatMessage
	if raw, ok := root["messages"]; ok {
		_ = json.Unmarshal(raw, &messages)
		parsed.Prompt = promptFromMessages(messages)
		parsed.Images = append(parsed.Images, imagesFromMessages(messages)...)
	}
	// Responses API: `input` is either a string or an array of items.
	if parsed.Prompt == "" {
		if raw, ok := root["input"]; ok {
			parsed.Prompt = promptFromInput(raw)
		}
	}
	// Images API: `prompt` plus `image`/`images`.
	if parsed.Prompt == "" {
		parsed.Prompt = firstNonEmpty(rawString(root["prompt"]))
	}
	parsed.Images = append(parsed.Images, stringsFromRaw(root["images"])...)

	if value := rawString(root["aspect_ratio"]); value != "" {
		parsed.AspectRatio = value
	}
	if value := rawString(root["resolution"]); value != "" {
		parsed.Resolution = value
	}
	if value := rawString(root["size"]); value != "" {
		parsed.Size = value
	}
	if duration := rawInt(root["duration"]); duration > 0 {
		parsed.Duration = duration
	}
	if duration := rawInt(root["seconds"]); duration > 0 {
		parsed.Duration = duration
	}
	if count := rawInt(root["n"]); count > 1 {
		if count > 4 {
			count = 4
		}
		parsed.Count = count
	}
	if family := rawString(root["family"]); family != "" {
		parsed.Family = family
	}

	// `miora` extension object wins over the flat fields.
	if raw, ok := root["miora"]; ok {
		var ext struct {
			AspectRatio string `json:"aspect_ratio"`
			Resolution  string `json:"resolution"`
			Size        string `json:"size"`
			Duration    int    `json:"duration"`
			Family      string `json:"family"`
			Model       string `json:"model"`
			N           int    `json:"n"`
		}
		if errUnmarshal := json.Unmarshal(raw, &ext); errUnmarshal == nil {
			if ext.AspectRatio != "" {
				parsed.AspectRatio = ext.AspectRatio
			}
			if ext.Resolution != "" {
				parsed.Resolution = ext.Resolution
			}
			if ext.Size != "" {
				parsed.Size = ext.Size
			}
			if ext.Duration > 0 {
				parsed.Duration = ext.Duration
			}
			if ext.Family != "" {
				parsed.Family = ext.Family
			}
			if ext.Model != "" {
				parsed.Model = ext.Model
			}
			if ext.N > 1 {
				parsed.Count = minInt(ext.N, 4)
			}
		}
	}

	parsed.Prompt = strings.TrimSpace(parsed.Prompt)
	if parsed.Prompt == "" {
		return nil, fmt.Errorf("请求里没有可用的提示词：请在 messages 里放一段文本，或直接给 prompt 字段")
	}
	return parsed, nil
}

func promptFromMessages(messages []chatMessage) string {
	parts := make([]string, 0, 4)
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "" && role != "user" {
			continue
		}
		text := textFromContent(message.Content)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	if len(parts) == 0 {
		// No user turn: fall back to any textual content so assistants proxying a
		// continued conversation still work.
		for _, message := range messages {
			if text := strings.TrimSpace(textFromContent(message.Content)); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func textFromContent(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil {
			return text
		}
		return ""
	}
	if strings.HasPrefix(trimmed, "[") {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if errUnmarshal := json.Unmarshal(raw, &parts); errUnmarshal == nil {
			texts := make([]string, 0, len(parts))
			for _, part := range parts {
				if part.Text != "" {
					texts = append(texts, part.Text)
				}
			}
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

func imagesFromMessages(messages []chatMessage) []string {
	images := make([]string, 0, 4)
	for _, message := range messages {
		trimmed := strings.TrimSpace(string(message.Content))
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		var parts []struct {
			Type     string `json:"type"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
			Image string `json:"image"`
		}
		if errUnmarshal := json.Unmarshal(message.Content, &parts); errUnmarshal != nil {
			continue
		}
		for _, part := range parts {
			if part.ImageURL.URL != "" {
				images = append(images, part.ImageURL.URL)
			}
			if part.Image != "" {
				images = append(images, part.Image)
			}
		}
	}
	return images
}

func promptFromInput(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		return rawString(raw)
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if errUnmarshal := json.Unmarshal(raw, &items); errUnmarshal != nil {
		return ""
	}
	messages := make([]chatMessage, 0, len(items))
	for _, item := range items {
		messages = append(messages, chatMessage{Role: item.Role, Content: item.Content})
	}
	return promptFromMessages(messages)
}

func stringsFromRaw(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if errUnmarshal := json.Unmarshal(raw, &list); errUnmarshal == nil {
		return list
	}
	var single string
	if errUnmarshal := json.Unmarshal(raw, &single); errUnmarshal == nil && single != "" {
		return []string{single}
	}
	return nil
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil {
		return strings.TrimSpace(text)
	}
	return ""
}

func rawBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value bool
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal == nil {
		return value
	}
	return false
}

func rawInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var number int
	if errUnmarshal := json.Unmarshal(raw, &number); errUnmarshal == nil {
		return number
	}
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil {
		parsed, errParse := strconv.Atoi(strings.TrimSpace(text))
		if errParse == nil {
			return parsed
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// request planning
// ---------------------------------------------------------------------------

type executionPlan struct {
	cfg         *Config
	cred        *credential
	parsed      *parsedChat
	generations []generateRequest
	model       string
	media       string
	stream      bool
	streamID    string
}

var aspectRatioPattern = regexp.MustCompile(`\b(\d{1,2}):(\d{1,2})\b`)

// planExecution normalises one executor call into concrete generation requests.
func planExecution(cfg *Config, req *executorRequest) (*executionPlan, error) {
	parsed, errParse := parseChatPayload(req.Payload)
	if errParse != nil {
		return nil, errParse
	}
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return nil, &upstreamError{Status: http.StatusUnauthorized, Message: "凭证解析失败：" + errCred.Error()}
	}
	if !cred.valid() {
		return nil, &upstreamError{
			Status:  http.StatusUnauthorized,
			Message: "没有可用的 Miora 登录态：请在插件的账号面板导入 Edge/Chrome 登录态或粘贴 authToken",
		}
	}
	model, media, errModel := resolveModel(cfg, parsed)
	if errModel != nil {
		return nil, errModel
	}

	plan := &executionPlan{
		cfg:      cfg,
		cred:     cred,
		parsed:   parsed,
		model:    model,
		media:    media,
		stream:   req.Stream,
		streamID: req.StreamID,
	}
	for index := 0; index < parsed.Count; index++ {
		generation, errPlan := buildGeneration(cfg, cred, parsed, model, media)
		if errPlan != nil {
			return nil, errPlan
		}
		plan.generations = append(plan.generations, generation)
	}
	return plan, nil
}

// resolveModel maps a client model ID onto an upstream model plus media kind.
func resolveModel(cfg *Config, parsed *parsedChat) (string, string, error) {
	clientModel := strings.TrimSpace(parsed.Model)
	upstream := cfg.upstreamModelID(clientModel)
	if isAutoModel(upstream) {
		media := mediaFamily(parsed.Family)
		if media == "" {
			media = mediaImage
		}
		fallback := defaultModelFor(cfg, media)
		if fallback == "" {
			fallback = firstAvailableModel(cfg, media)
		}
		if fallback == "" {
			return "", "", fmt.Errorf("没有可用的 Miora %s 模型：请先让插件拉取模型目录，或在配置里设置 default_%s_model", mediaLabel(media), media)
		}
		return fallback, media, nil
	}
	if upstream == "" {
		upstream = defaultModelFor(cfg, mediaImage)
		if upstream == "" {
			return "", "", fmt.Errorf("请求缺少 model 字段")
		}
	}
	return upstream, mediaKindForModel(cfg, upstream), nil
}

func isAutoModel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", autoModelID, "miora", "auto", "miora/auto":
		return true
	}
	return false
}

func mediaFamily(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case mediaImage, "img", "picture", "image_generation":
		return mediaImage
	case mediaVideo, "movie", "video_generation":
		return mediaVideo
	case mediaThreeD, "3d", "three-d", "model3d":
		return mediaThreeD
	default:
		return ""
	}
}

func firstAvailableModel(cfg *Config, media string) string {
	snapshot := peekCatalog()
	for _, model := range snapshot.Models {
		if model.mediaKind() != media {
			continue
		}
		if model.Available && cfg.modelAllowed(model.ModelID) {
			return model.ModelID
		}
	}
	for _, model := range snapshot.Models {
		if model.mediaKind() == media && cfg.modelAllowed(model.ModelID) {
			return model.ModelID
		}
	}
	return ""
}

// buildGeneration applies model capabilities to one concrete generation.
func buildGeneration(cfg *Config, cred *credential, parsed *parsedChat, model, media string) (generateRequest, error) {
	catalogModel, known := findCatalogModel(model)
	generation := generateRequest{
		Model:  model,
		Media:  media,
		Prompt: parsed.Prompt,
	}
	if known {
		generation.AspectRatio = normalizeAspectRatio(parsed.AspectRatio, parsed.Size, catalogModel, parsed.Prompt)
		generation.Resolution = normalizeResolution(parsed.Resolution, parsed.Size, catalogModel)
		if media == mediaVideo {
			generation.Duration = normalizeDuration(parsed.Duration, catalogModel, cfg)
		}
	} else {
		generation.AspectRatio = firstNonEmpty(parsed.AspectRatio, aspectRatioFromSize(parsed.Size), aspectRatioFromPrompt(parsed.Prompt))
		generation.Resolution = parsed.Resolution
		if media == mediaVideo {
			generation.Duration = parsed.Duration
			if generation.Duration <= 0 {
				generation.Duration = cfg.DefaultVideoSeconds
			}
		}
	}
	if !cfg.IncludeWatermark {
		generation.Footnote = "0"
	}

	keys, errKeys := resolveReferenceKeys(context.Background(), cfg, newAPIClient(cfg, cred), parsed.Images, catalogModel, media)
	if errKeys != nil {
		return generateRequest{}, errKeys
	}
	generation.ReferenceKeys = keys

	hasReference := len(keys) > 0
	switch media {
	case mediaVideo:
		if hasReference {
			generation.TaskType = "image_to_video"
		} else {
			generation.TaskType = "text_to_video"
		}
	case mediaThreeD:
		if hasReference {
			generation.TaskType = "image_to_3d"
		} else {
			generation.TaskType = "text_to_3d"
		}
	default:
		if hasReference {
			generation.TaskType = "image_to_image"
		} else {
			generation.TaskType = "text_to_image"
		}
	}
	return generation, nil
}

// resolveReferenceKeys turns client image inputs into Miora file keys. Miora only
// accepts its own COS keys for image-to-image, so raw URLs must be uploaded.
func resolveReferenceKeys(ctx context.Context, cfg *Config, client *apiClient, images []string, model catalogModel, media string) ([]string, error) {
	if len(images) == 0 {
		return nil, nil
	}
	limit := 1
	if model.maxReferenceImages() > 0 {
		limit = model.maxReferenceImages()
	}
	if media == mediaVideo {
		// Miora video models take a first-frame key, not a reference set.
		limit = 1
	}
	if len(images) > limit {
		images = images[:limit]
	}
	keys := make([]string, 0, len(images))
	for _, image := range images {
		trimmed := strings.TrimSpace(image)
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "file/"):
			keys = append(keys, trimmed)
		case strings.HasPrefix(trimmed, "miora://"):
			keys = append(keys, strings.TrimPrefix(trimmed, "miora://"))
		default:
			if !cfg.UploadReferenceImages {
				return nil, fmt.Errorf("参考图需要先转成 Miora 的文件，请在插件配置里开启 upload_reference_images 后重试")
			}
			key, errUpload := uploadReferenceImage(ctx, cfg, nil, trimmed)
			if errUpload != nil {
				return nil, fmt.Errorf("参考图上传失败：%w", errUpload)
			}
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func normalizeAspectRatio(requested, size string, model catalogModel, prompt string) string {
	allowed := model.Capabilities.AspectRatios
	candidate := firstNonEmpty(requested, aspectRatioFromSize(size), aspectRatioFromPrompt(prompt))
	if candidate == "" {
		candidate = model.Capabilities.DefaultAspectRatio
	}
	if len(allowed) == 0 {
		return candidate
	}
	for _, option := range allowed {
		if strings.EqualFold(option, candidate) {
			return option
		}
	}
	// Nearest allowed ratio by numeric value, so "4:3" degrades to "3:2" instead
	// of failing the request upstream.
	if target, ok := ratioValue(candidate); ok {
		best := ""
		bestDelta := 0.0
		for _, option := range allowed {
			if strings.EqualFold(option, "adaptive") {
				continue
			}
			value, okOption := ratioValue(option)
			if !okOption {
				continue
			}
			delta := value - target
			if delta < 0 {
				delta = -delta
			}
			if best == "" || delta < bestDelta {
				best = option
				bestDelta = delta
			}
		}
		if best != "" {
			return best
		}
	}
	if model.Capabilities.DefaultAspectRatio != "" {
		return model.Capabilities.DefaultAspectRatio
	}
	return firstNonEmpty(candidate, "1:1")
}

func ratioValue(ratio string) (float64, bool) {
	parts := strings.SplitN(strings.TrimSpace(ratio), ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	width, errWidth := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	height, errHeight := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errWidth != nil || errHeight != nil || height == 0 {
		return 0, false
	}
	return width / height, true
}

func aspectRatioFromSize(size string) string {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(size)), "x", 2)
	if len(parts) != 2 {
		return ""
	}
	width, errWidth := strconv.Atoi(strings.TrimSpace(parts[0]))
	height, errHeight := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errWidth != nil || errHeight != nil || width <= 0 || height <= 0 {
		return ""
	}
	gcd := greatestCommonDivisor(width, height)
	return fmt.Sprintf("%d:%d", width/gcd, height/gcd)
}

func greatestCommonDivisor(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

func aspectRatioFromPrompt(prompt string) string {
	match := aspectRatioPattern.FindStringSubmatch(prompt)
	if len(match) != 3 {
		return ""
	}
	return match[1] + ":" + match[2]
}

func normalizeResolution(requested, size string, model catalogModel) string {
	allowed := model.Capabilities.AllowedResolutions
	if len(allowed) == 0 {
		allowed = model.Capabilities.Resolutions
	}
	candidate := strings.TrimSpace(requested)
	if candidate == "" {
		candidate = resolutionFromSize(size)
	}
	if candidate == "" {
		return model.Capabilities.DefaultResolution
	}
	for _, option := range allowed {
		if strings.EqualFold(option, candidate) {
			return option
		}
	}
	if len(allowed) == 0 {
		return candidate
	}
	return model.Capabilities.DefaultResolution
}

func resolutionFromSize(size string) string {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(size)), "x", 2)
	longest := 0
	for _, part := range parts {
		value, errParse := strconv.Atoi(strings.TrimSpace(part))
		if errParse == nil && value > longest {
			longest = value
		}
	}
	switch {
	case longest == 0:
		return ""
	case longest <= 1280:
		return "1K"
	case longest <= 2048:
		return "1.5K"
	default:
		return "2K"
	}
}

func normalizeDuration(requested int, model catalogModel, cfg *Config) int {
	duration := requested
	if duration <= 0 {
		if model.Capabilities.DefaultSeconds > 0 {
			duration = model.Capabilities.DefaultSeconds
		} else if cfg != nil && cfg.DefaultVideoSeconds > 0 {
			duration = cfg.DefaultVideoSeconds
		} else {
			duration = 5
		}
	}
	if model.Capabilities.SecondsRange != nil {
		if min := model.Capabilities.SecondsRange.Min; min > 0 && duration < min {
			duration = min
		}
		if max := model.Capabilities.SecondsRange.Max; max > 0 && duration > max {
			duration = max
		}
	}
	return duration
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

func executorExecute(request []byte) ([]byte, error) {
	req, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := config()
	plan, errPlan := planExecution(cfg, req)
	if errPlan != nil {
		logWarn("executor plan failed", map[string]any{"error": errPlan.Error()})
		return executionFailure(errPlan)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.taskTimeout()+30*time.Second)
	defer cancel()
	release, errPermit := acquirePermit(ctx, cfg)
	if errPermit != nil {
		return failEnvelope("busy", "生成并发已满，请稍后重试", http.StatusTooManyRequests)
	}
	defer release()

	results := make([]*generationResult, 0, len(plan.generations))
	for _, generation := range plan.generations {
		result, errGeneration := runGeneration(ctx, cfg, plan.cred, generation, nil)
		if errGeneration != nil {
			return executionFailure(errGeneration)
		}
		results = append(results, result)
	}

	content := renderResults(cfg, results)
	body := chatCompletionBody(req.Model, content, results)
	logInfo("executor served", map[string]any{"model": req.Model, "media": plan.media, "files": len(results)})
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: body,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

func executorExecuteStream(request []byte) ([]byte, error) {
	req, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := config()
	plan, errPlan := planExecution(cfg, req)
	if errPlan != nil {
		logWarn("executor stream plan failed", map[string]any{"error": errPlan.Error()})
		if strings.TrimSpace(req.StreamID) != "" {
			emitStreamError(req.StreamID, errPlan.Error())
			closeStream(req.StreamID)
			return okEnvelope(map[string]any{"headers": streamHeaders()})
		}
		return executionFailure(errPlan)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.taskTimeout()+30*time.Second)
	defer cancel()
	release, errPermit := acquirePermit(ctx, cfg)
	if errPermit != nil {
		if strings.TrimSpace(req.StreamID) != "" {
			emitStreamError(req.StreamID, "生成并发已满，请稍后重试")
			closeStream(req.StreamID)
			return okEnvelope(map[string]any{"headers": streamHeaders()})
		}
		return failEnvelope("busy", "生成并发已满，请稍后重试", http.StatusTooManyRequests)
	}
	defer release()

	// Buffered mode: the host did not allocate a stream, so aggregate and return
	// the chunks inline exactly as a lower-level SSE translator would.
	if strings.TrimSpace(req.StreamID) == "" {
		results := make([]*generationResult, 0, len(plan.generations))
		for _, generation := range plan.generations {
			result, errGeneration := runGeneration(ctx, cfg, plan.cred, generation, nil)
			if errGeneration != nil {
				return executionFailure(errGeneration)
			}
			results = append(results, result)
		}
		chunks := make([]pluginapi.ExecutorStreamChunk, 0, 4)
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: chunkRole(req.Model)})
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: chunkContent(req.Model, renderResults(cfg, results))})
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: chunkFinal(req.Model, "stop")})
		return okEnvelope(map[string]any{"headers": streamHeaders(), "chunks": chunks})
	}

	streamID := req.StreamID
	if errEmit := hostStreamEmit(streamID, chunkRole(req.Model)); errEmit != nil {
		closeStream(streamID)
		return nil, fmt.Errorf("emit role chunk: %w", errEmit)
	}

	results := make([]*generationResult, 0, len(plan.generations))
	for index, generation := range plan.generations {
		progress := func(status string, percent float64, note string) {
			text := progressText(status, percent, index, len(plan.generations))
			if text == "" {
				return
			}
			_ = hostStreamEmit(streamID, chunkContent(req.Model, text))
		}
		result, errGeneration := runGeneration(ctx, cfg, plan.cred, generation, progress)
		if errGeneration != nil {
			emitStreamError(streamID, errGeneration.Error())
			closeStream(streamID)
			return okEnvelope(map[string]any{"headers": streamHeaders()})
		}
		results = append(results, result)
	}

	if errEmit := hostStreamEmit(streamID, chunkContent(req.Model, renderResults(cfg, results))); errEmit != nil {
		closeStream(streamID)
		return nil, fmt.Errorf("emit content chunk: %w", errEmit)
	}
	_ = hostStreamEmit(streamID, chunkFinal(req.Model, "stop"))
	closeStream(streamID)
	return okEnvelope(map[string]any{"headers": streamHeaders()})
}

// executionFailure maps a planner or generation error onto an RPC failure with a
// client-visible HTTP status.
func executionFailure(err error) ([]byte, error) {
	var upstream *upstreamError
	if asUpstream(err, &upstream) {
		status := upstream.Status
		switch {
		case upstream.isAuthError():
			return failEnvelope("credential_invalid", "Miora 登录态失效："+upstream.Message+
				"。请在插件面板重新导入 Edge/Chrome 登录态（Auth 页 → 从浏览器导入）。", http.StatusUnauthorized)
		case status == http.StatusTooManyRequests:
			return failEnvelope("rate_limited", upstream.Message, http.StatusTooManyRequests)
		case status >= 400 && status < 500:
			return failEnvelope("upstream_rejected", upstream.Message, http.StatusBadRequest)
		default:
			return failEnvelope("upstream_error", upstream.Error(), http.StatusBadGateway)
		}
	}
	return failEnvelope("generation_failed", err.Error(), http.StatusBadGateway)
}

func asUpstream(err error, target **upstreamError) bool {
	if err == nil {
		return false
	}
	if value, ok := err.(*upstreamError); ok {
		*target = value
		return true
	}
	return false
}

func streamHeaders() http.Header {
	return http.Header{"Content-Type": []string{"text/event-stream"}}
}

func progressText(status string, percent float64, index, total int) string {
	prefix := ""
	if total > 1 {
		prefix = fmt.Sprintf("[%d/%d] ", index+1, total)
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending":
		return prefix + "Miora 已排队…\n"
	case "processing", "running":
		if percent > 0 {
			return fmt.Sprintf("%s渲染中 %.0f%%\n", prefix, percent)
		}
		return prefix + "渲染中…\n"
	default:
		return ""
	}
}

// hostStreamEmit forwards one bare JSON chunk to the host stream pump.
func hostStreamEmit(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" || len(payload) == 0 {
		return nil
	}
	_, errCall := hostCall("host.stream.emit", map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	return errCall
}

// closeStream signals the end of an executor stream.
func closeStream(streamID string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	if _, errCall := hostCall("host.stream.close", map[string]any{"stream_id": streamID}); errCall != nil {
		logWarn("failed to close host stream", map[string]any{"error": errCall.Error()})
	}
}

func emitStreamError(streamID, message string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = hostCall("host.stream.emit", map[string]any{
		"stream_id": streamID,
		"error":     message,
	})
}

// ---------------------------------------------------------------------------
// OpenAI envelopes
// ---------------------------------------------------------------------------

type chatChoice struct {
	Index        int           `json:"index"`
	Message      chatChoiceMsg `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type chatChoiceMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func chatCompletionBody(model, content string, results []*generationResult) []byte {
	body := map[string]any{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []chatChoice{{
			Index:        0,
			Message:      chatChoiceMsg{Role: "assistant", Content: content},
			FinishReason: "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	if media := mediaExtension(results); media != nil {
		body["miora"] = media
	}
	raw, _ := json.Marshal(body)
	return raw
}

func mediaExtension(results []*generationResult) map[string]any {
	if len(results) == 0 {
		return nil
	}
	files := make([]mediaFile, 0, 4)
	taskIDs := make([]string, 0, len(results))
	totalCredit := 0.0
	for _, result := range results {
		files = append(files, result.Files...)
		taskIDs = append(taskIDs, result.TaskID)
		totalCredit += result.Credit
	}
	return map[string]any{
		"task_ids": taskIDs,
		"files":    files,
		"credits":  totalCredit,
		"media":    results[0].Media,
	}
}

func completionID() string {
	return "chatcmpl-miora-" + randomHex(8)
}

func chunkRole(model string) []byte {
	return chunkFrame(model, map[string]any{"role": "assistant"}, "")
}

func chunkContent(model, text string) []byte {
	return chunkFrame(model, map[string]any{"content": text}, "")
}

func chunkFinal(model, reason string) []byte {
	return chunkFrame(model, map[string]any{}, reason)
}

func chunkFrame(model string, delta map[string]any, finishReason string) []byte {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}
	frame := map[string]any{
		"id":      completionID(),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{choice},
	}
	raw, _ := json.Marshal(frame)
	return raw
}

func renderResults(cfg *Config, results []*generationResult) string {
	if len(results) == 1 {
		return renderResult(cfg, results[0], "")
	}
	builder := strings.Builder{}
	for index, result := range results {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(renderResult(cfg, result, ""))
	}
	return builder.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
