package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// defaultUserAgent is the desktop Chrome UA the Miora web client sends. Some
// gateways branch on it, and it is the value the reference implementation uses.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

// Upstream paths, verified against miora.design in October 2026.
const (
	pathMediaModels    = "/api/ai/public/cloud-agent/media-models"
	pathTiers          = "/api/ai/public/cloud-agent/tiers"
	pathProfile        = "/api/ai/user/profile"
	pathCredit         = "/api/ai/quota/credit"
	pathRefreshToken   = "/api/auth/refresh-token"
	pathGeneratePrefix = "/api/ai/media-generate/"
	pathImageQuery     = "/api/ai/async-task/batch-query"
	pathMediaProgress  = "/api/ai/media-generate/progress"
	pathSignURLs       = "/api/ai/cos/cdn-sign-url"
	pathSTSTokens      = "/api/ai/cos/sts-tokens"
)

// Media kinds, matching the Miora `contentType` values.
const (
	mediaImage  = "image"
	mediaVideo  = "video"
	mediaThreeD = "three_d"
)

// apiEnvelope is the common Miora response envelope.
type apiEnvelope struct {
	Code       json.RawMessage `json:"code"`
	Failed     bool            `json:"failed"`
	Data       json.RawMessage `json:"data"`
	Message    string          `json:"message"`
	HTTPStatus int             `json:"httpStatus"`
}

func (e *apiEnvelope) ok() bool {
	code := strings.TrimSpace(string(e.Code))
	return code == "" || code == "0" || code == `"0"` || code == "null"
}

// upstreamError is a decoded Miora business error.
type upstreamError struct {
	Code    string
	Message string
	Status  int
}

func (e *upstreamError) Error() string {
	switch {
	case e == nil:
		return ""
	case e.Code != "" && e.Message != "":
		return e.Code + ": " + e.Message
	case e.Message != "":
		return e.Message
	default:
		return fmt.Sprintf("upstream status %d", e.Status)
	}
}

// isAuthError reports whether the error means the credential is no longer usable.
func (e *upstreamError) isAuthError() bool {
	if e == nil {
		return false
	}
	if e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		return true
	}
	switch strings.TrimSpace(e.Code) {
	case "401", "403", "UNAUTHORIZED", "TOKEN_EXPIRED", "AUTH_EXPIRED":
		return true
	}
	return false
}

// httpResult is one completed upstream request.
type httpResult struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// doUpstream performs one HTTP request. It prefers the host HTTP bridge so the
// call inherits the host proxy, egress policy and request logging; when the
// bridge is unavailable (unit tests, ABI harness) it falls back to a direct
// client.
func doUpstream(ctx context.Context, cfg *Config, method, rawURL string, headers http.Header, body []byte) (*httpResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	useHost := cfg == nil || cfg.UseHostHTTP
	if useHost {
		result, errHost := doUpstreamViaHost(ctx, method, rawURL, headers, body)
		recordUpstreamTrace("host", method, rawURL, statusOf(result), errHost)
		if errHost == nil && !gatewayRejected(result) {
			return result, nil
		}
		// A timeout or callback failure may happen after a billable POST reached
		// Miora. Retrying through another transport can create a duplicate task.
		if errHost != nil && !errors.Is(errHost, errHostUnavailable) {
			return nil, errHost
		}
		if errHost == nil {
			// Some deployments put a gateway in front of the host transport that
			// answers POST with a bare 405 HTML page. The request never reached the
			// API, so retrying it directly is safe and keeps the plugin working.
			logWarn("host http bridge rejected the request; retrying directly", map[string]any{
				"method": method,
				"url":    redactURL(rawURL),
				"status": result.StatusCode,
			})
		}
	}
	result, errDirect := doUpstreamDirect(ctx, cfg, method, rawURL, headers, body)
	recordUpstreamTrace("direct", method, rawURL, statusOf(result), errDirect)
	return result, errDirect
}

func statusOf(result *httpResult) int {
	if result == nil {
		return 0
	}
	return result.StatusCode
}

// gatewayRejected reports a transport-level rejection that never reached the API:
// a 405/501 whose body is an HTML error page rather than the JSON envelope.
func gatewayRejected(result *httpResult) bool {
	if result == nil {
		return false
	}
	switch result.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
	default:
		return false
	}
	contentType := strings.ToLower(result.Headers.Get("Content-Type"))
	return strings.Contains(contentType, "html") || strings.HasPrefix(strings.TrimSpace(string(result.Body)), "<")
}

// redactURL keeps the path but drops the query string, which may carry a signature.
func redactURL(rawURL string) string {
	if index := strings.IndexByte(rawURL, '?'); index >= 0 {
		return rawURL[:index]
	}
	return rawURL
}

func doUpstreamViaHost(ctx context.Context, method, rawURL string, headers http.Header, body []byte) (*httpResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	type reply struct {
		result *httpResult
		err    error
	}
	done := make(chan reply, 1)
	go func() {
		raw, err := hostCall(pluginabi.MethodHostHTTPDo, pluginapi.HTTPRequest{
			Method:  method,
			URL:     rawURL,
			Headers: headers,
			Body:    body,
		})
		if err != nil {
			done <- reply{err: err}
			return
		}
		var resp pluginapi.HTTPResponse
		if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
			done <- reply{err: errUnmarshal}
			return
		}
		done <- reply{result: &httpResult{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		if r.result == nil {
			return nil, fmt.Errorf("host http bridge returned no response")
		}
		return r.result, nil
	}
}

var directTransportCache struct {
	sync.Mutex
	proxy     string
	transport *http.Transport
}

func directTransport(proxy string) *http.Transport {
	if proxy == "" {
		return http.DefaultTransport.(*http.Transport)
	}
	directTransportCache.Lock()
	defer directTransportCache.Unlock()
	if directTransportCache.transport != nil && directTransportCache.proxy == proxy {
		return directTransportCache.transport
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.EqualFold(proxy, "direct://") {
		transport.Proxy = nil
	} else if proxyURL, errParse := url.Parse(proxy); errParse == nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	if previous := directTransportCache.transport; previous != nil {
		previous.CloseIdleConnections()
	}
	directTransportCache.proxy, directTransportCache.transport = proxy, transport
	return transport
}

func doUpstreamDirect(ctx context.Context, cfg *Config, method, rawURL string, headers http.Header, body []byte) (*httpResult, error) {
	proxy := ""
	if cfg != nil {
		proxy = cfg.ProxyURL
	}
	client := &http.Client{Timeout: cfg.requestTimeout(), Transport: directTransport(proxy)}
	req, errNew := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if errNew != nil {
		return nil, errNew
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, errRead
	}
	return &httpResult{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: raw}, nil
}

// apiClient is one credential-bound view of the Miora HTTP API.
type apiClient struct {
	cfg  *Config
	cred *credential
}

func newAPIClient(cfg *Config, cred *credential) *apiClient {
	if cfg == nil {
		cfg = config()
	}
	return &apiClient{cfg: cfg, cred: cred}
}

func (c *apiClient) baseURL() string {
	if c == nil || c.cfg == nil || c.cfg.BaseURL == "" {
		return "https://miora.design"
	}
	return c.cfg.BaseURL
}

func (c *apiClient) token() string {
	if c == nil || c.cred == nil {
		return ""
	}
	return c.cred.AuthToken
}

// webHeaders builds the browser-equivalent header set the Miora web client sends.
func (c *apiClient) webHeaders(jsonBody bool) http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("User-Agent", defaultUserAgent)
	headers.Set("x-Source", "web")
	headers.Set("X-Product", "miora")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("Accept-Language", languageHeader(c.cfg))
	base := c.baseURL()
	headers.Set("Origin", base)
	headers.Set("Referer", base+"/")
	if jsonBody {
		headers.Set("Content-Type", "application/json")
	}
	if token := c.token(); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	return headers
}

func languageHeader(cfg *Config) string {
	lang := "zh-CN"
	if cfg != nil && strings.TrimSpace(cfg.Language) != "" {
		lang = strings.TrimSpace(cfg.Language)
	}
	if strings.EqualFold(lang, "en-US") {
		return "en-US,en;q=0.9"
	}
	return lang + "," + strings.SplitN(lang, "-", 2)[0] + ";q=0.9"
}

// requestJSON performs one JSON request and returns the envelope `data` blob. An
// empty body means no JSON payload.
func (c *apiClient) requestJSON(ctx context.Context, method, path string, query url.Values, payload any, auth bool) (json.RawMessage, error) {
	target := c.baseURL() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var body []byte
	var errMarshal error
	if payload != nil {
		body, errMarshal = json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
	}
	headers := c.webHeaders(payload != nil)
	if !auth {
		headers.Del("Authorization")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, c.cfg.requestTimeout())
	defer cancel()

	result, errDo := doUpstream(timeoutCtx, c.cfg, method, target, headers, body)
	if errDo != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, errDo)
	}
	if result.StatusCode == http.StatusUnauthorized || result.StatusCode == http.StatusForbidden {
		return nil, &upstreamError{Status: result.StatusCode, Message: upstreamSnippet(result.Body)}
	}
	env := &apiEnvelope{}
	if errUnmarshal := json.Unmarshal(result.Body, env); errUnmarshal != nil {
		return nil, &upstreamError{
			Status:  result.StatusCode,
			Message: fmt.Sprintf("unexpected upstream body (%d): %s", result.StatusCode, upstreamSnippet(result.Body)),
		}
	}
	if !env.ok() || env.Failed || result.StatusCode >= 400 {
		return nil, &upstreamError{
			Code:    strings.Trim(strings.TrimSpace(string(env.Code)), `"`),
			Message: strings.TrimSpace(env.Message),
			Status:  result.StatusCode,
		}
	}
	return env.Data, nil
}

func upstreamSnippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		text = "(empty body)"
	}
	return text
}

// ---------------------------------------------------------------------------
// catalogue
// ---------------------------------------------------------------------------

// catalogModel is one upstream media model entry.
type catalogModel struct {
	ID           string            `json:"id"`
	ModelID      string            `json:"modelId"`
	ContentType  string            `json:"contentType"`
	Label        string            `json:"label"`
	Description  string            `json:"description"`
	Badge        string            `json:"badge"`
	Tags         []string          `json:"tags"`
	Available    bool              `json:"available"`
	EditOnly     bool              `json:"editOnly"`
	Capabilities modelCapabilities `json:"capabilities"`
}

// modelCapabilities is the union of the three media capability shapes. Miora uses
// a different structure per media kind, so every field is optional.
type modelCapabilities struct {
	AspectRatios       []string `json:"aspectRatios"`
	AllowedResolutions []string `json:"allowedResolutions"`
	Resolutions        []string `json:"resolutions"`
	DefaultResolution  string   `json:"defaultResolution"`
	DefaultAspectRatio string   `json:"defaultAspectRatio"`
	SupportedTools     []string `json:"supportedTools"`
	ReferenceRange     *struct {
		Max int `json:"max"`
		Min int `json:"min"`
	} `json:"referenceImagesRange"`
	SecondsRange *struct {
		Max int `json:"max"`
		Min int `json:"min"`
	} `json:"secondsRange"`
	DefaultSeconds       int      `json:"defaultSeconds"`
	SupportsTextToVideo  bool     `json:"supportsTextToVideo"`
	SupportsImageURL     bool     `json:"supportsImageUrl"`
	SupportsLastImageURL bool     `json:"supportsLastImageUrl"`
	SupportsReference    bool     `json:"supportsReferenceImages"`
	DefaultResultFormat  string   `json:"defaultResultFormat"`
	ResultFormats        []string `json:"resultFormats"`
	PromptLengthLimit    int      `json:"promptLengthLimit"`
	EstimatedDuration    int      `json:"estimatedDurationSeconds"`
}

func (m catalogModel) mediaKind() string {
	switch strings.ToLower(strings.TrimSpace(m.ContentType)) {
	case mediaVideo:
		return mediaVideo
	case mediaThreeD, "3d", "three-d":
		return mediaThreeD
	case mediaImage, "":
		return mediaImage
	default:
		return mediaImage
	}
}

// supportsReferenceImages reports whether image inputs are meaningful for the
// model. Text-only models silently ignore them instead of failing the request.
func (m catalogModel) supportsReferenceImages() bool {
	if m.mediaKind() != mediaImage {
		return m.Capabilities.SupportsImageURL || m.Capabilities.SupportsReference
	}
	if m.Capabilities.ReferenceRange != nil && m.Capabilities.ReferenceRange.Max > 0 {
		return true
	}
	for _, tool := range m.Capabilities.SupportedTools {
		switch tool {
		case "image_to_image", "edit_image":
			return true
		}
	}
	return false
}

func (m catalogModel) maxReferenceImages() int {
	if m.Capabilities.ReferenceRange != nil && m.Capabilities.ReferenceRange.Max > 0 {
		return m.Capabilities.ReferenceRange.Max
	}
	return 1
}

// fetchCatalog returns the upstream media catalogue (42 models on the free tier).
func (c *apiClient) fetchCatalog(ctx context.Context) ([]catalogModel, string, error) {
	query := url.Values{}
	query.Set("lang", languageOrDefault(c.cfg))
	data, errFetch := c.requestJSON(ctx, http.MethodGet, pathMediaModels, query, nil, false)
	if errFetch != nil {
		return nil, "", errFetch
	}
	var payload struct {
		UserTier string         `json:"userTier"`
		Models   []catalogModel `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, "", fmt.Errorf("decode media catalogue: %w", errUnmarshal)
	}
	return payload.Models, payload.UserTier, nil
}

func languageOrDefault(cfg *Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Language) != "" {
		return strings.TrimSpace(cfg.Language)
	}
	return "zh-CN"
}

// ---------------------------------------------------------------------------
// account
// ---------------------------------------------------------------------------

type profileInfo struct {
	UserID    string  `json:"userId"`
	Username  string  `json:"username"`
	Email     string  `json:"email"`
	Nickname  string  `json:"nickname"`
	Avatar    string  `json:"avatar"`
	Plan      string  `json:"plan"`
	CR        float64 `json:"cr"`
	CreatedAt string  `json:"createdAt"`
}

func (c *apiClient) fetchProfile(ctx context.Context) (*profileInfo, error) {
	data, errFetch := c.requestJSON(ctx, http.MethodGet, pathProfile, nil, nil, true)
	if errFetch != nil {
		return nil, errFetch
	}
	profile := &profileInfo{}
	if errUnmarshal := json.Unmarshal(data, profile); errUnmarshal != nil {
		return nil, fmt.Errorf("decode profile: %w", errUnmarshal)
	}
	return profile, nil
}

// creditInfo is the authoritative balance view (/api/ai/quota/credit). The
// `cr` field in localStorage is not trustworthy, this endpoint is.
type creditInfo struct {
	TotalAmount  flexFloat `json:"total_amount"`
	UsedAmount   flexFloat `json:"used_amount"`
	NextExpiring string    `json:"nextExpiring"`
}

func (c *creditInfo) remaining() float64 {
	if c == nil {
		return 0
	}
	return c.TotalAmount.value() - c.UsedAmount.value()
}

func (c *apiClient) fetchCredits(ctx context.Context) (*creditInfo, error) {
	data, errFetch := c.requestJSON(ctx, http.MethodGet, pathCredit, nil, nil, true)
	if errFetch != nil {
		return nil, errFetch
	}
	credits := &creditInfo{}
	if errUnmarshal := json.Unmarshal(data, credits); errUnmarshal != nil {
		return nil, fmt.Errorf("decode credits: %w", errUnmarshal)
	}
	return credits, nil
}

// flexFloat accepts both 12.5 and "12.5", which Miora mixes across endpoints.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		*f = 0
		return nil
	}
	text = strings.Trim(text, `"`)
	value, errParse := strconv.ParseFloat(text, 64)
	if errParse != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(value)
	return nil
}

func (f flexFloat) value() float64 { return float64(f) }

// refreshTokens exchanges a refresh token for a fresh access token. The refresh
// token rotates on every call, so the caller must persist the new pair
// immediately; replaying an old refresh token yields HTTP 400.
func (c *apiClient) refreshTokens(ctx context.Context, refreshToken string) (*credential, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, &upstreamError{Message: "该账号没有 refresh token，需要重新登录导入"}
	}
	base := c.baseURL()
	payload, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", defaultUserAgent)

	timeoutCtx, cancel := context.WithTimeout(ctx, c.cfg.requestTimeout())
	defer cancel()
	result, errDo := doUpstream(timeoutCtx, c.cfg, http.MethodPost, base+pathRefreshToken, headers, payload)
	if errDo != nil {
		return nil, errDo
	}
	var body struct {
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if errUnmarshal := json.Unmarshal(result.Body, &body); errUnmarshal != nil {
		return nil, &upstreamError{Status: result.StatusCode, Message: "刷新令牌失败：" + upstreamSnippet(result.Body)}
	}
	if body.AccessToken == "" {
		return nil, &upstreamError{Status: result.StatusCode, Message: "刷新令牌失败：" + upstreamSnippet(result.Body)}
	}
	next := &credential{
		AuthToken:    body.AccessToken,
		RefreshToken: body.RefreshToken,
		UpdatedAt:    time.Now(),
	}
	if next.RefreshToken == "" {
		next.RefreshToken = refreshToken
	}
	if expiresIn := body.ExpiresIn; expiresIn > 0 {
		next.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	return next, nil
}

// ---------------------------------------------------------------------------
// generation
// ---------------------------------------------------------------------------

// generateRequest is the normalised generation intent, independent of the client
// protocol that produced it.
type generateRequest struct {
	Model         string
	Media         string
	Prompt        string
	ReferenceKeys []string
	AspectRatio   string
	Resolution    string
	Duration      int
	Footnote      string
	TaskType      string
}

func (r *generateRequest) buildBody() map[string]any {
	body := map[string]any{
		"type":   r.TaskType,
		"prompt": r.Prompt,
		"model":  r.Model,
	}
	switch r.Media {
	case mediaVideo:
		if len(r.ReferenceKeys) > 0 {
			body["firstImageFileKey"] = r.ReferenceKeys[0]
		}
		if r.Duration > 0 {
			body["duration"] = r.Duration
		}
	case mediaThreeD:
		if len(r.ReferenceKeys) > 0 {
			body["imageFileKeys"] = r.ReferenceKeys
		}
	default:
		if len(r.ReferenceKeys) > 0 {
			body["imageFileKeys"] = r.ReferenceKeys
		}
	}
	extra := map[string]any{}
	if r.Resolution != "" {
		extra["resolution"] = r.Resolution
	}
	if r.AspectRatio != "" {
		extra["aspectRatio"] = r.AspectRatio
	}
	if r.Footnote != "" {
		extra["footnote"] = r.Footnote
	}
	if len(extra) > 0 {
		body["extraParameters"] = extra
	}
	return body
}

// submitTask creates one media generation task and returns its upstream ID.
func (c *apiClient) submitTask(ctx context.Context, req generateRequest) (string, error) {
	path := pathGeneratePrefix + "image"
	switch req.Media {
	case mediaVideo:
		path = pathGeneratePrefix + "video"
	case mediaThreeD:
		path = pathGeneratePrefix + "3d"
	}
	data, errPost := c.requestJSON(ctx, http.MethodPost, path, nil, req.buildBody(), true)
	if errPost != nil {
		return "", errPost
	}
	// Image returns data.taskId; video/3D wrap it one level deeper (data.data).
	var flat struct {
		TaskID string `json:"taskId"`
	}
	if errUnmarshal := json.Unmarshal(data, &flat); errUnmarshal == nil && flat.TaskID != "" {
		return flat.TaskID, nil
	}
	var nested struct {
		Data struct {
			TaskID string `json:"taskId"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(data, &nested); errUnmarshal == nil && nested.Data.TaskID != "" {
		return nested.Data.TaskID, nil
	}
	return "", &upstreamError{Message: "上游没有返回 taskId：" + upstreamSnippet(data)}
}

// taskState is the subset of an async media task the bridge needs.
type taskState struct {
	TaskID          string       `json:"taskId"`
	Status          string       `json:"status"`
	Progress        float64      `json:"progress"`
	ErrorMessage    string       `json:"errorMessage"`
	ResultFileKey   string       `json:"resultFileKey"`
	ResultSignURL   string       `json:"resultSignUrl"`
	Assets          []taskAsset  `json:"assets"`
	GeneratedAssets []taskAsset  `json:"generatedAssets"`
	Metadata        taskMetadata `json:"metadata"`
	TaskType        string       `json:"taskType"`
	CreatedAt       string       `json:"createdAt"`
	UpdatedAt       string       `json:"updatedAt"`
}

type taskAsset struct {
	FileKey     string `json:"fileKey"`
	FileType    string `json:"fileType"`
	DisplayName string `json:"displayName"`
}

type taskMetadata struct {
	Size             string `json:"size"`
	Resolution       string `json:"resolution"`
	AspectRatio      string `json:"aspectRatio"`
	ModelDisplayName string `json:"modelDisplayName"`
	PromptSummary    string `json:"promptSummary"`
	Usage            struct {
		Credit      flexFloat `json:"credit"`
		TotalTokens int64     `json:"total_tokens"`
	} `json:"usage"`
}

// terminal reports whether the task reached a final state.
func (t *taskState) terminal() bool {
	switch strings.ToLower(strings.TrimSpace(t.Status)) {
	case "completed", "inserted", "failed", "cancelled", "canceled", "error":
		return true
	}
	return false
}

func (t *taskState) failed() bool {
	switch strings.ToLower(strings.TrimSpace(t.Status)) {
	case "failed", "cancelled", "canceled", "error":
		return true
	}
	return false
}

// fileKeys returns the task's result assets in display order.
func (t *taskState) fileKeys() []string {
	seen := map[string]struct{}{}
	keys := make([]string, 0, len(t.Assets)+1)
	appendKey := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	appendKey(t.ResultFileKey)
	for _, asset := range t.Assets {
		appendKey(asset.FileKey)
	}
	for _, asset := range t.GeneratedAssets {
		appendKey(asset.FileKey)
	}
	return keys
}

// pollTasks queries both task endpoints and merges the results, because image
// tasks only appear in batch-query while video/3D tasks only appear in
// media-generate/progress.
func (c *apiClient) pollTasks(ctx context.Context, media string, ids []string) (map[string]taskState, error) {
	if len(ids) == 0 {
		return map[string]taskState{}, nil
	}
	states := map[string]taskState{}

	if media == mediaImage {
		if data, errQuery := c.requestJSON(ctx, http.MethodPost, pathImageQuery, nil, map[string]any{"taskIds": ids}, true); errQuery == nil {
			mergeTaskMap(states, data)
			complete := true
			for _, id := range ids {
				if _, ok := states[id]; !ok {
					complete = false
					break
				}
			}
			if complete {
				return states, nil
			}
		} else {
			return nil, errQuery
		}
	}

	progressBody := map[string]any{
		"imageTaskIds":  []string{},
		"videoTaskIds":  []string{},
		"threeDTaskIds": []string{},
	}
	switch media {
	case mediaVideo:
		progressBody["videoTaskIds"] = ids
	case mediaThreeD:
		progressBody["threeDTaskIds"] = ids
	default:
		progressBody["imageTaskIds"] = ids
	}
	if data, errProgress := c.requestJSON(ctx, http.MethodPost, pathMediaProgress, nil, progressBody, true); errProgress == nil {
		var wrapped struct {
			Tasks json.RawMessage `json:"tasks"`
		}
		if errUnmarshal := json.Unmarshal(data, &wrapped); errUnmarshal == nil && len(wrapped.Tasks) > 0 {
			mergeTaskMap(states, wrapped.Tasks)
		} else {
			mergeTaskMap(states, data)
		}
	} else {
		return nil, errProgress
	}

	if len(states) == 0 {
		return nil, &upstreamError{Message: "任务查询未返回结果"}
	}
	return states, nil
}

func mergeTaskMap(dst map[string]taskState, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var single taskState
	if errUnmarshal := json.Unmarshal(raw, &single); errUnmarshal == nil && single.TaskID != "" {
		dst[single.TaskID] = single
		return
	}
	var batch struct {
		Tasks map[string]taskState `json:"tasks"`
	}
	if errUnmarshal := json.Unmarshal(raw, &batch); errUnmarshal == nil && len(batch.Tasks) > 0 {
		for id, state := range batch.Tasks {
			if state.TaskID == "" {
				state.TaskID = id
			}
			dst[id] = state
		}
		return
	}
	var direct map[string]taskState
	if errUnmarshal := json.Unmarshal(raw, &direct); errUnmarshal == nil {
		for id, state := range direct {
			if state.TaskID == "" {
				state.TaskID = id
			}
			dst[id] = state
		}
	}
}

func asAuthError(err error) *upstreamError {
	if err == nil {
		return nil
	}
	var upstream *upstreamError
	if errors.As(err, &upstream) && upstream.isAuthError() {
		return upstream
	}
	return nil
}

// signURLs asks the platform signer for a 3600s CDN URL. fileKeys are permanent,
// so an expired URL can always be re-signed instead of regenerating.
func (c *apiClient) signURLs(ctx context.Context, fileKeys []string) (map[string]string, error) {
	out := map[string]string{}
	if len(fileKeys) == 0 {
		return out, nil
	}
	data, errPost := c.requestJSON(ctx, http.MethodPost, pathSignURLs, nil, map[string]any{"filePaths": fileKeys}, true)
	if errPost != nil {
		return out, errPost
	}
	var payload struct {
		SignedURLs   json.RawMessage `json:"signedUrls"`
		FilePaths    []string        `json:"filePaths"`
		ExpireTime   flexFloat       `json:"expireTime"`
		ValidSeconds flexFloat       `json:"validDuration"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return out, fmt.Errorf("decode signed urls: %w", errUnmarshal)
	}
	var urls []string
	if errUnmarshal := json.Unmarshal(payload.SignedURLs, &urls); errUnmarshal != nil {
		var single string
		if errUnmarshal := json.Unmarshal(payload.SignedURLs, &single); errUnmarshal == nil && single != "" {
			urls = []string{single}
		}
	}
	for index, signed := range urls {
		if index < len(fileKeys) {
			out[fileKeys[index]] = signed
		} else if len(fileKeys) == 1 {
			out[fileKeys[0]] = signed
		}
	}
	return out, nil
}
