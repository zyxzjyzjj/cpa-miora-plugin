package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// autoModelID is a synthetic convenience model: it keeps any client working even
// when it does not know which Miora media model it wants, by routing to the
// configured defaults for the requested media kind.
const autoModelID = "miora-auto"

// registrationResponse advertises plugin metadata and the capabilities this
// plugin actually implements. Only implemented capabilities are declared: the
// host rejects or mis-routes an over-declaring plugin.
func registrationResponse() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "Miora",
			Version:          pluginVersion,
			Author:           "cpa-miora-plugin",
			GitHubRepository: pluginRepositoryURL,
			Logo:             "",
			ConfigFields:     registrationConfigFields(),
		},
		"capabilities": declaredCapabilities(),
	}
}

func registrationConfigFields() []pluginapi.ConfigField {
	field := func(name string, kind pluginapi.ConfigFieldType, description string) pluginapi.ConfigField {
		return pluginapi.ConfigField{Name: name, Type: kind, Description: description}
	}
	return []pluginapi.ConfigField{
		field("base_url", pluginapi.ConfigFieldTypeString, "Miora 站点地址，默认 https://miora.design。"),
		field("language", pluginapi.ConfigFieldTypeEnum, "模型目录语言。"),
		field("model_prefix", pluginapi.ConfigFieldTypeString, "对外暴露的模型 ID 前缀，例如 miora/。留空则使用上游模型名。"),
		field("include_models", pluginapi.ConfigFieldTypeArray, "只暴露这些上游模型 ID，留空表示全部。"),
		field("exclude_models", pluginapi.ConfigFieldTypeArray, "不暴露这些上游模型 ID。"),
		field("models", pluginapi.ConfigFieldTypeArray, "离线场景下的静态模型表（id/upstream/media/label）。"),
		field("default_image_model", pluginapi.ConfigFieldTypeString, "请求未指定可用模型时的默认图像模型。"),
		field("default_video_model", pluginapi.ConfigFieldTypeString, "默认视频模型。"),
		field("default_3d_model", pluginapi.ConfigFieldTypeString, "默认 3D 模型。"),
		field("default_video_seconds", pluginapi.ConfigFieldTypeInteger, "视频默认时长（秒）。"),
		field("poll_interval_seconds", pluginapi.ConfigFieldTypeInteger, "任务轮询间隔。"),
		field("task_timeout_seconds", pluginapi.ConfigFieldTypeInteger, "单次生成最长等待时间。"),
		field("max_concurrent_tasks", pluginapi.ConfigFieldTypeInteger, "并发等待中的生成任务上限（1-64）。"),
		field("reserve_credits", pluginapi.ConfigFieldTypeNumber, "余额低于此值直接拒绝请求，0 表示不检查。"),
		field("upload_reference_images", pluginapi.ConfigFieldTypeBoolean, "是否允许把 data:/http(s) 图片上传为参考图（实验特性）。"),
		field("enable_browser_import", pluginapi.ConfigFieldTypeBoolean, "是否允许面板读取本机 Edge/Chrome 登录态。"),
		field("browser_profiles", pluginapi.ConfigFieldTypeArray, "限定扫描的浏览器 profile，如 edge:Default。"),
		field("include_watermark", pluginapi.ConfigFieldTypeBoolean, "请求带水印的产物。"),
		field("response_footer", pluginapi.ConfigFieldTypeBoolean, "在回复末尾追加紧凑的生成信息。"),
		field("catalog_ttl_minutes", pluginapi.ConfigFieldTypeInteger, "模型目录缓存时长。"),
		field("auto_refresh_hours", pluginapi.ConfigFieldTypeInteger, "提前多少小时轮换即将过期的 token。"),
		field("event_log_size", pluginapi.ConfigFieldTypeInteger, "面板请求日志保留条数。"),
		field("proxy_url", pluginapi.ConfigFieldTypeString, "上游请求代理，direct:// 表示直连。"),
		field("use_host_http", pluginapi.ConfigFieldTypeBoolean, "通过宿主 HTTP 桥发送上游请求；被网关拦截时会自动回退直连。"),
		field("request_timeout_seconds", pluginapi.ConfigFieldTypeInteger, "单次上游请求超时。"),
	}
}

// declaredCapabilities is the single source of truth for the capability set; the
// panel renders it, so the plugin cannot advertise one set and register another.
func declaredCapabilities() map[string]any {
	return map[string]any{
		"model_provider":         true,
		"auth_provider":          true,
		"executor":               true,
		"executor_model_scope":   pluginapi.ExecutorModelScopeBoth,
		"executor_input_formats": []string{"chat-completions"},
		// The plugin renders OpenAI chat-completions bodies and bare JSON chunks.
		// Anthropic/Gemini clients are translated by the host into that format, so
		// no extra output format has to be declared.
		"executor_output_formats": []string{"chat-completions"},
		"management_api":          true,
		"quota_provider":          true,
		"usage_plugin":            true,
		"scheduler":               true,

		// Not implemented, stated explicitly so the decision is greppable.
		"model_registrar":             false,
		"frontend_auth_provider":      false,
		"model_router":                false,
		"request_translator":          false,
		"request_normalizer":          false,
		"request_interceptor":         false,
		"request_lifecycle_plugin":    false,
		"response_translator":         false,
		"response_before_translator":  false,
		"response_after_translator":   false,
		"response_interceptor":        false,
		"stream_chunk_interceptor":    false,
		"websocket_response_observer": false,
		"command_line_plugin":         false,
		"thinking_applier":            false,
	}
}

// modelRegistration returns the models contributed to the registry.
func modelRegistration() pluginapi.ModelRegistrationResponse {
	return pluginapi.ModelRegistrationResponse{
		Provider: providerID,
		Models:   modelInfos(),
	}
}

// staticModels returns the provider-native model list. The upstream catalogue is
// fetched lazily and cached, so this list is available without a credential.
func staticModels() pluginapi.ModelResponse {
	return pluginapi.ModelResponse{
		Provider: providerID,
		Models:   modelInfos(),
	}
}

// modelsForAuth returns the same catalogue for a specific account. Miora's media
// catalogue is not account specific (only the available flag differs), so the
// response is identical and needs no authenticated profile request.
func modelsForAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode model for auth request: %w", errUnmarshal)
		}
	}
	models := modelInfos()
	return okEnvelope(pluginapi.ModelResponse{Provider: providerID, Models: models})
}

func mediaLabel(media string) string {
	switch media {
	case mediaVideo:
		return "video generation"
	case mediaThreeD:
		return "3D generation"
	default:
		return "image generation"
	}
}

// modelInfos maps the Miora catalogue onto host model metadata.
func modelInfos() []pluginapi.ModelInfo {
	cfg := config()
	_ = loadCatalogCached()
	models := catalogueModels(cfg)
	now := time.Now().Unix()

	out := make([]pluginapi.ModelInfo, 0, len(models)+1)
	out = append(out, autoModelInfo(cfg, now))
	for _, model := range models {
		kind := model.mediaKind()
		modalities := []string{"text"}
		if model.supportsReferenceImages() {
			modalities = append(modalities, "image")
		}
		outputs := []string{kind}
		if kind == mediaThreeD {
			outputs = []string{"file"}
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         cfg.advertisedModelID(model.ModelID),
			Object:                     "model",
			Created:                    now,
			OwnedBy:                    providerID,
			Type:                       kind,
			DisplayName:                firstNonEmpty(model.Label, model.ModelID),
			Name:                       model.ModelID,
			Description:                firstNonEmpty(describeCatalogModel(model), "Miora "+mediaLabel(kind)),
			ContextLength:              int64(model.Capabilities.PromptLengthLimit),
			MaxCompletionTokens:        0,
			SupportedGenerationMethods: []string{"chat.completions"},
			SupportedInputModalities:   modalities,
			SupportedOutputModalities:  outputs,
			SupportedParameters:        generationParameters(kind),
		})
	}
	return out
}

func autoModelInfo(cfg *Config, now int64) pluginapi.ModelInfo {
	description := "按提示词自动挑选 Miora 模型；可用 family 参数指定 image/video/3d。"
	defaults := []string{}
	if cfg.DefaultImageModel != "" {
		defaults = append(defaults, "图像="+cfg.DefaultImageModel)
	}
	if cfg.DefaultVideoModel != "" {
		defaults = append(defaults, "视频="+cfg.DefaultVideoModel)
	}
	if cfg.Default3DModel != "" {
		defaults = append(defaults, "3D="+cfg.Default3DModel)
	}
	if len(defaults) > 0 {
		description += " 当前默认：" + joinComma(defaults) + "。"
	}
	return pluginapi.ModelInfo{
		ID:                         cfg.advertisedModelID(autoModelID),
		Object:                     "model",
		Created:                    now,
		OwnedBy:                    providerID,
		Type:                       "chat",
		DisplayName:                "Miora Auto",
		Name:                       autoModelID,
		Description:                description,
		SupportedGenerationMethods: []string{"chat.completions"},
		SupportedInputModalities:   []string{"text", "image"},
		SupportedOutputModalities:  []string{"image", "video", "file"},
		SupportedParameters: []string{
			"stream", "size", "aspect_ratio", "resolution", "duration", "family",
		},
	}
}

func describeCatalogModel(model catalogModel) string {
	parts := make([]string, 0, 4)
	if model.Description != "" {
		parts = append(parts, model.Description)
	}
	if model.Capabilities.DefaultAspectRatio != "" {
		parts = append(parts, "默认比例 "+model.Capabilities.DefaultAspectRatio)
	}
	if model.Capabilities.DefaultResolution != "" {
		parts = append(parts, "默认分辨率 "+model.Capabilities.DefaultResolution)
	}
	if model.Capabilities.SecondsRange != nil && model.Capabilities.SecondsRange.Max > 0 {
		parts = append(parts, fmt.Sprintf("时长 %d-%d 秒", model.Capabilities.SecondsRange.Min, model.Capabilities.SecondsRange.Max))
	}
	if model.supportsReferenceImages() {
		parts = append(parts, fmt.Sprintf("最多 %d 张参考图", model.maxReferenceImages()))
	}
	if model.Badge != "" {
		parts = append(parts, "标记 "+model.Badge)
	}
	return joinComma(parts)
}

func generationParameters(media string) []string {
	switch media {
	case mediaVideo:
		return []string{"stream", "aspect_ratio", "resolution", "duration", "size"}
	case mediaThreeD:
		return []string{"stream"}
	default:
		return []string{"stream", "aspect_ratio", "resolution", "size", "n"}
	}
}

func joinComma(values []string) string {
	out := ""
	for index, value := range values {
		if index > 0 {
			out += "，"
		}
		out += value
	}
	return out
}
