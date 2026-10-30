package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the plugin-owned configuration. The host preserves every field below
// `plugins.configs.miora-provider` verbatim and hands it to plugin.register /
// plugin.reconfigure as YAML, so this struct is the single source of truth for
// behaviour and for the panel's settings tab.
type Config struct {
	// BaseURL is the Miora origin. The intl site is https://miora.design.
	BaseURL string `yaml:"base_url" json:"base_url"`

	// Language selects the catalogue locale sent to Miora (zh-CN, en-US, ...).
	Language string `yaml:"language" json:"language"`

	// ProxyURL overrides the transport for upstream calls that do not go through
	// the host HTTP bridge, e.g. direct:// in tests. Empty follows host policy.
	ProxyURL string `yaml:"proxy_url" json:"proxy_url"`

	// UseHostHTTP routes upstream calls through host.http.do so they inherit the
	// host proxy, egress policy and request logging. Disable it when a deployment
	// has a gateway in front of the host transport that mangles POST bodies.
	UseHostHTTP bool `yaml:"use_host_http" json:"use_host_http"`

	// RequestTimeoutSeconds bounds one upstream HTTP round trip.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`

	// PollIntervalSeconds is how often a submitted media task is polled.
	PollIntervalSeconds int `yaml:"poll_interval_seconds" json:"poll_interval_seconds"`

	// TaskTimeoutSeconds bounds the whole wait for a generation task.
	TaskTimeoutSeconds int `yaml:"task_timeout_seconds" json:"task_timeout_seconds"`

	// MaxConcurrentTasks limits concurrently running generation waits.
	MaxConcurrentTasks int `yaml:"max_concurrent_tasks" json:"max_concurrent_tasks"`

	// DefaultImageModel / DefaultVideoModel / Default3DModel are used when a
	// request names a model that is not in the catalogue, or names the synthetic
	// model "miora-auto".
	DefaultImageModel string `yaml:"default_image_model" json:"default_image_model"`
	DefaultVideoModel string `yaml:"default_video_model" json:"default_video_model"`
	Default3DModel    string `yaml:"default_3d_model" json:"default_3d_model"`

	// DefaultVideoSeconds is the fallback clip length when a request omits it.
	DefaultVideoSeconds int `yaml:"default_video_seconds" json:"default_video_seconds"`

	// ModelPrefix is prepended to every advertised model ID, e.g. "miora/".
	ModelPrefix string `yaml:"model_prefix" json:"model_prefix"`

	// IncludeModels / ExcludeModels filter the upstream catalogue by model ID.
	IncludeModels []string `yaml:"include_models" json:"include_models"`
	ExcludeModels []string `yaml:"exclude_models" json:"exclude_models"`

	// Models are operator-defined models that are advertised even when the
	// upstream catalogue cannot be fetched (offline start, air-gapped CPA).
	Models []ModelConfig `yaml:"models" json:"models"`

	// CatalogTTLMinutes controls how long the upstream catalogue is cached.
	CatalogTTLMinutes int `yaml:"catalog_ttl_minutes" json:"catalog_ttl_minutes"`

	// ReserveCredits refuses a request when the account balance is below this
	// value. Zero disables the check.
	ReserveCredits float64 `yaml:"reserve_credits" json:"reserve_credits"`

	// UploadReferenceImages enables the experimental COS pre-signed upload for
	// image inputs supplied as data: URLs or http(s) URLs. When disabled only
	// miora file keys (image_file_keys) are forwarded for image-to-image.
	UploadReferenceImages bool `yaml:"upload_reference_images" json:"upload_reference_images"`

	// EnableBrowserImport allows the panel to read the local Edge/Chrome
	// Local Storage leveldb to import an already-signed-in Miora session.
	EnableBrowserImport bool `yaml:"enable_browser_import" json:"enable_browser_import"`

	// BrowserProfiles restricts the scan to specific browser profiles, written
	// as "browser:profile", e.g. "edge:Default". Empty scans every known browser.
	BrowserProfiles []string `yaml:"browser_profiles" json:"browser_profiles"`

	// AutoRefreshHours refreshes an account token this many hours before expiry.
	AutoRefreshHours int `yaml:"auto_refresh_hours" json:"auto_refresh_hours"`

	// EventLogSize bounds the in-memory request log shown in the panel.
	EventLogSize int `yaml:"event_log_size" json:"event_log_size"`

	// IncludeWatermark requests Miora's watermarked variant of the result.
	IncludeWatermark bool `yaml:"include_watermark" json:"include_watermark"`

	// --- Agent (ACP chat) settings -------------------------------------------
	// AgentEnabled exposes Miora's agent models (deepseek, glm, kimi, gpt …)
	// through the ACP bridge. Media models are unaffected either way.
	AgentEnabled bool `yaml:"agent_enabled" json:"agent_enabled"`

	// AgentDefaultModel serves requests that name a model outside the catalogue.
	AgentDefaultModel string `yaml:"agent_default_model" json:"agent_default_model"`

	// AgentModels adds model IDs to the advertised agent catalogue.
	AgentModels []string `yaml:"agent_models" json:"agent_models"`

	// AgentProjectID pins the Miora project used for the sandbox. Empty lets the
	// plugin reuse or create a project titled AgentProjectTitle.
	AgentProjectID string `yaml:"agent_project_id" json:"agent_project_id"`

	// AgentProjectTitle is the project name the plugin creates/owns.
	AgentProjectTitle string `yaml:"agent_project_title" json:"agent_project_title"`

	// AgentMode is the ACP session mode. bypassPermissions avoids the interactive
	// tool-approval prompt that a stateless proxy cannot answer.
	AgentMode string `yaml:"agent_mode" json:"agent_mode"`

	// AgentTimeoutSeconds bounds one agent turn.
	AgentTimeoutSeconds int `yaml:"agent_timeout_seconds" json:"agent_timeout_seconds"`

	// AgentRuntimeTTLMinutes is how long a provisioned sandbox is reused.
	AgentRuntimeTTLMinutes int `yaml:"agent_runtime_ttl_minutes" json:"agent_runtime_ttl_minutes"`

	// ResponseFooter appends a compact metadata line to the assistant message.
	ResponseFooter bool `yaml:"response_footer" json:"response_footer"`
}

// ModelConfig is one operator-defined catalogue entry.
type ModelConfig struct {
	ID       string `yaml:"id" json:"id"`
	Upstream string `yaml:"upstream" json:"upstream"`
	Media    string `yaml:"media" json:"media"` // image | video | three_d
	Label    string `yaml:"label" json:"label"`
}

func defaultConfig() *Config {
	return &Config{
		BaseURL:                "https://miora.design",
		Language:               "zh-CN",
		RequestTimeoutSeconds:  60,
		PollIntervalSeconds:    4,
		TaskTimeoutSeconds:     420,
		MaxConcurrentTasks:     4,
		DefaultVideoSeconds:    5,
		CatalogTTLMinutes:      30,
		AutoRefreshHours:       24,
		EventLogSize:           200,
		UploadReferenceImages:  false,
		AgentEnabled:           true,
		AgentDefaultModel:      agentDefaultModelID,
		AgentProjectTitle:      "CPA 会话",
		AgentMode:              agentBypassMode,
		AgentTimeoutSeconds:    180,
		AgentRuntimeTTLMinutes: 120,
		UseHostHTTP:            true,
		EnableBrowserImport:    true,
		ResponseFooter:         true,
	}
}

// normalize trims user input and fills derived defaults.
func (c *Config) normalize() {
	if c == nil {
		return
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		c.BaseURL = "https://miora.design"
	}
	c.Language = strings.TrimSpace(c.Language)
	if c.Language == "" {
		c.Language = "zh-CN"
	}
	c.ProxyURL = strings.TrimSpace(c.ProxyURL)
	c.ModelPrefix = strings.TrimSpace(c.ModelPrefix)
	if c.ModelPrefix != "" && !strings.HasSuffix(c.ModelPrefix, "/") {
		c.ModelPrefix += "/"
	}
	c.DefaultImageModel = strings.TrimSpace(c.DefaultImageModel)
	c.DefaultVideoModel = strings.TrimSpace(c.DefaultVideoModel)
	c.Default3DModel = strings.TrimSpace(c.Default3DModel)
	if c.RequestTimeoutSeconds <= 0 {
		c.RequestTimeoutSeconds = 60
	}
	if c.PollIntervalSeconds <= 0 {
		c.PollIntervalSeconds = 4
	}
	if c.TaskTimeoutSeconds <= 0 {
		c.TaskTimeoutSeconds = 420
	}
	if c.MaxConcurrentTasks <= 0 {
		c.MaxConcurrentTasks = 4
	}
	if c.DefaultVideoSeconds <= 0 {
		c.DefaultVideoSeconds = 5
	}
	if c.CatalogTTLMinutes <= 0 {
		c.CatalogTTLMinutes = 30
	}
	if c.AutoRefreshHours <= 0 {
		c.AutoRefreshHours = 24
	}
	if c.EventLogSize <= 0 {
		c.EventLogSize = 200
	}
	c.AgentDefaultModel = strings.TrimSpace(c.AgentDefaultModel)
	if c.AgentDefaultModel == "" {
		c.AgentDefaultModel = agentDefaultModelID
	}
	c.AgentProjectID = strings.TrimSpace(c.AgentProjectID)
	if strings.TrimSpace(c.AgentProjectTitle) == "" {
		c.AgentProjectTitle = "CPA 会话"
	}
	c.AgentMode = strings.TrimSpace(c.AgentMode)
	if c.AgentMode == "" {
		c.AgentMode = agentBypassMode
	}
	if c.AgentTimeoutSeconds <= 0 {
		c.AgentTimeoutSeconds = 180
	}
	if c.AgentRuntimeTTLMinutes <= 0 {
		c.AgentRuntimeTTLMinutes = 120
	}
	c.AgentModels = normalizeIDList(c.AgentModels)
	c.IncludeModels = normalizeIDList(c.IncludeModels)
	c.ExcludeModels = normalizeIDList(c.ExcludeModels)
	c.BrowserProfiles = normalizeIDList(c.BrowserProfiles)

	cleaned := make([]ModelConfig, 0, len(c.Models))
	for _, m := range c.Models {
		m.ID = strings.TrimSpace(m.ID)
		m.Upstream = strings.TrimSpace(m.Upstream)
		if m.Upstream == "" {
			m.Upstream = m.ID
		}
		m.Media = normalizeMedia(m.Media)
		if m.Media == "" {
			m.Media = "image"
		}
		m.Label = strings.TrimSpace(m.Label)
		if m.ID == "" {
			continue
		}
		cleaned = append(cleaned, m)
	}
	c.Models = cleaned
}

func normalizeIDList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func normalizeMedia(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "image", "img", "picture":
		return mediaImage
	case "video", "movie":
		return mediaVideo
	case "3d", "three_d", "threed", "model3d":
		return mediaThreeD
	default:
		return ""
	}
}

// parseConfig decodes the plugin.register / plugin.reconfigure request. The host
// sends the preserved YAML as a []byte, which is base64 on the JSON wire.
func parseConfig(request []byte) (*Config, error) {
	cfg := defaultConfig()
	if len(request) == 0 {
		return cfg, nil
	}
	var payload struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if errUnmarshal := json.Unmarshal(request, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	raw := payload.ConfigYAML
	if len(strings.TrimSpace(string(raw))) > 0 {
		if errUnmarshal := yaml.Unmarshal(raw, cfg); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	cfg.normalize()
	return cfg, nil
}

// config returns the active configuration, falling back to defaults before the
// first register call (the panel can be opened while the host is still loading).
func config() *Config {
	if cfg := currentConfig.Load(); cfg != nil {
		return cfg
	}
	return defaultConfig()
}

func (c *Config) requestTimeout() time.Duration {
	if c == nil || c.RequestTimeoutSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}

func (c *Config) pollInterval() time.Duration {
	if c == nil || c.PollIntervalSeconds <= 0 {
		return 4 * time.Second
	}
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

func (c *Config) taskTimeout() time.Duration {
	if c == nil || c.TaskTimeoutSeconds <= 0 {
		return 420 * time.Second
	}
	return time.Duration(c.TaskTimeoutSeconds) * time.Second
}

func (c *Config) catalogTTL() time.Duration {
	if c == nil || c.CatalogTTLMinutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(c.CatalogTTLMinutes) * time.Minute
}

// advertisedModelID maps an upstream model ID to the ID advertised to clients.
func (c *Config) advertisedModelID(upstream string) string {
	if c == nil {
		return upstream
	}
	return c.ModelPrefix + upstream
}

// upstreamModelID strips the configured prefix from a client-supplied model ID.
func (c *Config) upstreamModelID(clientModel string) string {
	if c == nil {
		return clientModel
	}
	name := strings.TrimSpace(clientModel)
	if c.ModelPrefix != "" && strings.HasPrefix(name, c.ModelPrefix) {
		return strings.TrimPrefix(name, c.ModelPrefix)
	}
	return name
}

// modelAllowed applies include/exclude filters by upstream model ID.
func (c *Config) modelAllowed(upstream string) bool {
	if c == nil {
		return true
	}
	if len(c.IncludeModels) > 0 {
		found := false
		for _, id := range c.IncludeModels {
			if strings.EqualFold(id, upstream) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, id := range c.ExcludeModels {
		if strings.EqualFold(id, upstream) {
			return false
		}
	}
	return true
}

// validate is used by the settings route to reject obviously broken values
// before they are written back to the host config.
func (c *Config) validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("base_url must start with http:// or https://")
	}
	if c.MaxConcurrentTasks > 64 {
		return fmt.Errorf("max_concurrent_tasks must be between 1 and 64")
	}
	if filepath.IsAbs(c.ProxyURL) {
		return fmt.Errorf("proxy_url must be an http(s) URL or direct://")
	}
	return nil
}
