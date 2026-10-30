package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The agent (chat) side of Miora is a completely different transport from media
// generation. A project-scoped sandbox is provisioned per account, then CodeBuddy
// ACP is spoken over HTTP: one POST to {acpLink.url}/bootstrap carrying an ordered
// batch of JSON-RPC requests, and an SSE stream of session/update notifications
// coming back.
//
//	POST /api/ai/workflow/create            {"title":"..."}      -> projectId
//	POST /api/ai/cloud-agent/runtimes       {"projectId":...}    -> id + links.acpLink{url,token}
//	POST {acpLink.url}/bootstrap            {initialize, sessionNew, ...}
//	POST {acpLink.url}/bootstrap            {initialize, sessionLoad, setModel, prompt}
//
// Media models stay on the REST media API; agent models never touch it.

const (
	pathWorkflowCreate       = "/api/ai/workflow/create"
	pathWorkflowList         = "/api/ai/workflow/list"
	pathAgentRuntimes        = "/api/ai/cloud-agent/runtimes"
	pathAgentTiers           = "/api/ai/public/cloud-agent/tiers"
	pathToolConfirmationMode = "/api/ai/tool-confirmation/mode"
	agentACPBootstrap        = "/bootstrap"
	agentPromptID            = 4
	agentSessionNewID        = 2
	agentDefaultModelID      = "deepseek-v4.1-flash"
	agentBypassMode          = "bypassPermissions"
	agentSSEScanMaxBytes     = 8 << 20
)

// agentModel is one model the agent sandbox accepts in session/set_model.
type agentModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Credits     string `json:"credits"`
	Vendor      string `json:"vendor"`
	MaxInput    int64  `json:"max_input_tokens"`
	MaxOutput   int64  `json:"max_output_tokens"`
	FromRuntime bool   `json:"from_runtime,omitempty"`
}

// agentRuntime caches one provisioned sandbox per account.
type agentRuntime struct {
	ProjectID    string
	RuntimeID    string
	ACPURL       string
	ACPToken     string
	TokenExpires time.Time
	Models       []agentModel
	// SessionID is the sandbox's single ACP session. The sandbox manager enforces
	// one session per runtime ("SingleSessionId"), so it is created once and reused;
	// opening a second one makes session/new wait for a manifest that never arrives.
	SessionID string
	// StateKey identifies the account in the persisted state file.
	StateKey string
	Created  time.Time
}

func (r *agentRuntime) usable(cfg *Config) bool {
	if r == nil || r.ACPURL == "" || r.ACPToken == "" || r.RuntimeID == "" {
		return false
	}
	if !r.TokenExpires.IsZero() && time.Now().After(r.TokenExpires.Add(-10*time.Minute)) {
		return false
	}
	ttl := 120 * time.Minute
	if cfg != nil && cfg.AgentRuntimeTTLMinutes > 0 {
		ttl = time.Duration(cfg.AgentRuntimeTTLMinutes) * time.Minute
	}
	return time.Since(r.Created) < ttl
}

// agentTurnLocks serialises turns per account: one sandbox serves one ACP session
// at a time, and a second concurrent session/new can block for minutes.
var agentTurnLocks = struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}{locks: map[string]*sync.Mutex{}}

func agentTurnLock(key string) *sync.Mutex {
	agentTurnLocks.mu.Lock()
	defer agentTurnLocks.mu.Unlock()
	lock, ok := agentTurnLocks.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		agentTurnLocks.locks[key] = lock
	}
	return lock
}

var agentRuntimes = struct {
	mu        sync.Mutex
	byAccount map[string]*agentRuntime
	models    []agentModel
}{byAccount: map[string]*agentRuntime{}}

func agentAccountKey(cfg *Config, cred *credential) string {
	base := "https://miora.design"
	if cfg != nil && cfg.BaseURL != "" {
		base = cfg.BaseURL
	}
	identity := cred.UserID
	if identity == "" {
		identity = cred.Email
	}
	if identity == "" {
		identity = randomHex(8)
	}
	return base + "|" + identity
}

// cachedAgentRuntime returns a still-valid runtime for this account.
func cachedAgentRuntime(cfg *Config, cred *credential) *agentRuntime {
	agentRuntimes.mu.Lock()
	defer agentRuntimes.mu.Unlock()
	runtime := agentRuntimes.byAccount[agentAccountKey(cfg, cred)]
	if runtime != nil && runtime.usable(cfg) {
		return runtime
	}
	return nil
}

func storeAgentRuntime(cfg *Config, cred *credential, runtime *agentRuntime) {
	agentRuntimes.mu.Lock()
	defer agentRuntimes.mu.Unlock()
	agentRuntimes.byAccount[agentAccountKey(cfg, cred)] = runtime
	if len(runtime.Models) > 0 {
		agentRuntimes.models = runtime.Models
	}
}

// knownAgentModels merges the public tier catalogue, runtime-learned models and
// operator overrides, de-duplicated by model ID.
func knownAgentModels(cfg *Config) []agentModel {
	out := make([]agentModel, 0, 32)
	seen := map[string]struct{}{}
	appendModel := func(model agentModel) {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return
		}
		if _, ok := seen[strings.ToLower(id)]; ok {
			return
		}
		seen[strings.ToLower(id)] = struct{}{}
		model.ID = id
		if model.Name == "" {
			model.Name = id
		}
		out = append(out, model)
	}

	agentRuntimes.mu.Lock()
	runtimeModels := append([]agentModel(nil), agentRuntimes.models...)
	agentRuntimes.mu.Unlock()
	for _, model := range runtimeModels {
		appendModel(model)
	}
	if len(agentModelCache.models) > 0 {
		for _, model := range agentModelCache.models {
			appendModel(model)
		}
	}
	if cfg != nil {
		for _, override := range cfg.AgentModels {
			appendModel(agentModel{ID: override, Name: override})
		}
	}
	return out
}

// agentModelCache holds the public tier catalogue (no sandbox needed).
var agentModelCache struct {
	mu        sync.RWMutex
	models    []agentModel
	fetchedAt time.Time
}

// loadAgentTiers refreshes the public agent model list. It is anonymous, so it can
// warm before any account exists.
func loadAgentTiers(ctx context.Context, cfg *Config) []agentModel {
	agentModelCache.mu.RLock()
	models := agentModelCache.models
	stale := time.Since(agentModelCache.fetchedAt) > 30*time.Minute
	agentModelCache.mu.RUnlock()
	if len(models) > 0 && !stale {
		return models
	}

	client := newAPIClient(cfg, nil)
	data, errFetch := client.agentJSON(ctx, http.MethodGet, pathAgentTiers, nil, nil)
	if errFetch != nil {
		logWarn("agent tier catalogue unavailable", map[string]any{"error": errFetch.Error()})
		return models
	}
	var payload []struct {
		TierKey       string  `json:"tierKey"`
		ModelID       string  `json:"modelId"`
		ModelName     string  `json:"modelName"`
		BillingFactor float64 `json:"billingFactor"`
		IsDefault     bool    `json:"isDefault"`
		Locales       map[string]struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"locales"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		logWarn("agent tier catalogue decode failed", map[string]any{"error": errUnmarshal.Error()})
		return models
	}
	lang := languageOrDefault(cfg)
	out := make([]agentModel, 0, len(payload))
	seen := map[string]struct{}{}
	for _, tier := range payload {
		id := strings.TrimSpace(tier.ModelID)
		if id == "" {
			continue
		}
		if _, ok := seen[strings.ToLower(id)]; ok {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		name := tier.ModelName
		description := ""
		if locale, ok := tier.Locales[lang]; ok {
			if locale.Name != "" {
				name = locale.Name
			}
			description = locale.Description
		}
		out = append(out, agentModel{
			ID:          id,
			Name:        firstNonEmpty(name, id),
			Description: description,
			Credits:     fmt.Sprintf("×%.2f", tier.BillingFactor),
		})
	}
	if len(out) > 0 {
		agentModelCache.mu.Lock()
		agentModelCache.models = out
		agentModelCache.fetchedAt = time.Now()
		agentModelCache.mu.Unlock()
	}
	return out
}

// isAgentModelID reports whether a model should be served by the ACP bridge.
func isAgentModelID(cfg *Config, upstream string) bool {
	name := strings.ToLower(strings.TrimSpace(upstream))
	if name == "" {
		return false
	}
	if name == strings.ToLower(autoModelID) || name == "auto" || name == "miora" {
		return false
	}
	if strings.HasPrefix(name, "custom-local:") {
		return true
	}
	for _, model := range knownAgentModels(cfg) {
		if strings.EqualFold(model.ID, upstream) {
			return true
		}
	}
	return false
}

// ensureAgentRuntime returns a live sandbox for this account. It walks candidate
// projects (configured id, this process's remembered project, existing plugin
// projects) and only creates a new project when none of them yields a usable
// runtime. A project can be left pointing at a deleted runtime, so a candidate
// that cannot be resolved is skipped rather than fatal.
func ensureAgentRuntime(ctx context.Context, cfg *Config, cred *credential) (*agentRuntime, error) {
	key := agentAccountKey(cfg, cred)
	if runtime := cachedAgentRuntime(cfg, cred); runtime != nil {
		if refreshed, errRefresh := refreshRuntimeLinks(ctx, cfg, cred, runtime); errRefresh == nil {
			storeAgentRuntime(cfg, cred, refreshed)
			return refreshed, nil
		}
		forgetAgentRuntime(cfg, cred)
	}

	client := newAPIClient(cfg, cred)
	if entry, ok := stateEntry(key); ok && entry.RuntimeID != "" {
		if runtime, errEntry := runtimeByID(ctx, client, cfg, entry.ProjectID, entry.RuntimeID); errEntry == nil {
			runtime.SessionID = entry.SessionID
			runtime.StateKey = key
			rememberProject(key, entry.ProjectID)
			storeAgentRuntime(cfg, cred, runtime)
			logInfo("agent runtime restored", map[string]any{
				"project": entry.ProjectID, "runtime": entry.RuntimeID,
			})
			return runtime, nil
		}
		forgetState(key)
	}

	candidates := make([]string, 0, 4)
	if cfg.AgentProjectID != "" {
		candidates = append(candidates, cfg.AgentProjectID)
	}
	if remembered := rememberedProject(key); remembered != "" {
		candidates = append(candidates, remembered)
	}
	if projects, errList := findAgentProjects(ctx, client, cfg); errList == nil {
		candidates = append(candidates, projects...)
	}

	retired := retiredProjects()
	for _, projectID := range candidates {
		if _, skip := retired[projectID]; skip {
			continue
		}
		runtime, errProject := runtimeForProject(ctx, client, cfg, projectID)
		if errProject != nil {
			continue
		}
		rememberProject(key, projectID)
		enableAutoConfirmation(ctx, client, projectID)
		runtime.StateKey = key
		rememberState(key, projectID, runtime.RuntimeID, runtime.SessionID)
		storeAgentRuntime(cfg, cred, runtime)
		logInfo("agent runtime ready", map[string]any{
			"project": projectID, "runtime": runtime.RuntimeID, "models": len(runtime.Models),
		})
		return runtime, nil
	}

	projectID, errCreate := createAgentProject(ctx, client, cfg)
	if errCreate != nil {
		return nil, errCreate
	}
	runtime, errRuntime := createRuntimeForProject(ctx, client, cfg, projectID)
	if errRuntime != nil {
		return nil, errRuntime
	}
	rememberProject(key, projectID)
	enableAutoConfirmation(ctx, client, projectID)
	runtime.StateKey = key
	rememberState(key, projectID, runtime.RuntimeID, runtime.SessionID)
	storeAgentRuntime(cfg, cred, runtime)
	logInfo("agent runtime provisioned", map[string]any{
		"project": projectID, "runtime": runtime.RuntimeID,
	})
	return runtime, nil
}

// agentJSON performs one provisioning call straight to Miora, bypassing the host
// HTTP bridge: a hung bridge callback would eat the whole agent request timeout.
func (c *apiClient) agentJSON(ctx context.Context, method, path string, query url.Values, payload any) (json.RawMessage, error) {
	return c.requestJSONDirect(ctx, method, path, query, payload, true, 25*time.Second)
}

// enableAutoConfirmation flips the project's media-tool confirmation mode to
// "auto". Without it every design tool call waits for a human approval that a
// stateless proxy cannot give, and the turn stalls until the upstream timeout.
func enableAutoConfirmation(ctx context.Context, client *apiClient, projectID string) {
	payload := map[string]any{"projectId": projectID, "mode": "auto"}
	if _, errPost := client.agentJSON(ctx, http.MethodPost, pathToolConfirmationMode, nil, payload); errPost != nil {
		logWarn("could not enable auto tool confirmation", map[string]any{
			"project": projectID, "error": errPost.Error(),
		})
		return
	}
	logInfo("auto tool confirmation enabled", map[string]any{"project": projectID})
}

// rememberedProjects maps an account to the project this process chose, so a
// restart-heavy deployment does not create a project per request.
var rememberedProjects = struct {
	mu    sync.Mutex
	byKey map[string]string
}{byKey: map[string]string{}}

func rememberProject(key, projectID string) {
	rememberedProjects.mu.Lock()
	rememberedProjects.byKey[key] = projectID
	rememberedProjects.mu.Unlock()
	if entry, ok := stateEntry(key); ok && entry.ProjectID == projectID {
		return
	}
	rememberState(key, projectID, "", "")
}

func rememberedProject(key string) string {
	rememberedProjects.mu.Lock()
	existing := rememberedProjects.byKey[key]
	rememberedProjects.mu.Unlock()
	if existing != "" {
		return existing
	}
	if entry, ok := stateEntry(key); ok {
		return entry.ProjectID
	}
	return ""
}

// findAgentProjects lists projects whose title starts with the configured prefix,
// newest first.
func findAgentProjects(ctx context.Context, client *apiClient, cfg *Config) ([]string, error) {
	title := strings.TrimSpace(cfg.AgentProjectTitle)
	if title == "" {
		return nil, nil
	}
	target := client.baseURL() + pathWorkflowList + "?page=1&pageSize=50"
	headers := client.webHeaders(false)
	timeoutCtx, cancel := context.WithTimeout(ctx, cfg.requestTimeout())
	defer cancel()
	result, errDo := doUpstreamDirect(timeoutCtx, cfg, http.MethodGet, target, headers, nil)
	if errDo != nil {
		return nil, errDo
	}
	var payload struct {
		Data struct {
			List []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"list"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(result.Body, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	out := make([]string, 0, len(payload.Data.List))
	for _, item := range payload.Data.List {
		if strings.HasPrefix(strings.TrimSpace(item.Title), title) {
			out = append(out, item.ID)
		}
	}
	return out, nil
}

// findAgentProject reuses a project the plugin created earlier.
func findAgentProject(ctx context.Context, client *apiClient, cfg *Config) (string, error) {
	title := strings.TrimSpace(cfg.AgentProjectTitle)
	if title == "" {
		return "", nil
	}
	query := map[string]string{"page": "1", "pageSize": "50"}
	target := client.baseURL() + pathWorkflowList + "?" + encodeQuery(query)
	headers := client.webHeaders(false)
	timeoutCtx, cancel := context.WithTimeout(ctx, cfg.requestTimeout())
	defer cancel()
	result, errDo := doUpstreamDirect(timeoutCtx, cfg, http.MethodGet, target, headers, nil)
	if errDo != nil {
		return "", errDo
	}
	var payload struct {
		Data struct {
			List []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"list"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(result.Body, &payload); errUnmarshal != nil {
		return "", errUnmarshal
	}
	for _, item := range payload.Data.List {
		if strings.TrimSpace(item.Title) == title {
			return item.ID, nil
		}
	}
	return "", nil
}

func createAgentProject(ctx context.Context, client *apiClient, cfg *Config) (string, error) {
	title := strings.TrimSpace(cfg.AgentProjectTitle)
	if title == "" {
		title = "CPA 会话"
	}
	// Keep titles distinguishable: several CPA deployments can share one account,
	// and a stale project must not be mistaken for the new one.
	if existing, errExisting := findAgentProjects(ctx, client, cfg); errExisting == nil && len(existing) > 0 {
		title = title + " " + randomHex(3)
	}
	data, errPost := client.agentJSON(ctx, http.MethodPost, pathWorkflowCreate, nil, map[string]any{"title": title})
	if errPost != nil {
		return "", fmt.Errorf("创建 Miora 项目失败：%w", errPost)
	}
	var id string
	if errUnmarshal := json.Unmarshal(data, &id); errUnmarshal == nil && id != "" {
		return id, nil
	}
	var wrapped struct {
		ID string `json:"id"`
	}
	if errUnmarshal := json.Unmarshal(data, &wrapped); errUnmarshal == nil && wrapped.ID != "" {
		return wrapped.ID, nil
	}
	return "", &upstreamError{Message: "创建项目没有返回 id：" + upstreamSnippet(data)}
}

// createRuntimeForProject provisions the sandbox and returns its ACP link.
// refreshRuntimeLinks re-reads the runtime so the ACP token is fresh: the platform
// rotates it, and a stale link is rejected as 401 by the sandbox.
func refreshRuntimeLinks(ctx context.Context, cfg *Config, cred *credential, runtime *agentRuntime) (*agentRuntime, error) {
	client := newAPIClient(cfg, cred)
	data, errGet := client.agentJSON(ctx, http.MethodGet, pathAgentRuntimes+"/"+runtime.RuntimeID, nil, nil)
	if errGet != nil {
		return nil, errGet
	}
	var payload struct {
		Status string `json:"status"`
		Links  struct {
			ACPLink struct {
				Token          string `json:"token"`
				URL            string `json:"url"`
				TokenExpiresAt int64  `json:"tokenExpiresAt"`
			} `json:"acpLink"`
		} `json:"links"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if payload.Links.ACPLink.URL == "" || payload.Links.ACPLink.Token == "" {
		return nil, fmt.Errorf("运行时链接不完整")
	}
	clone := *runtime
	clone.ACPURL = strings.TrimRight(payload.Links.ACPLink.URL, "/")
	clone.ACPToken = payload.Links.ACPLink.Token
	if payload.Links.ACPLink.TokenExpiresAt > 0 {
		clone.TokenExpires = time.Unix(payload.Links.ACPLink.TokenExpiresAt, 0)
	}
	clone.Created = time.Now()
	runtime.ACPURL = clone.ACPURL
	runtime.ACPToken = clone.ACPToken
	runtime.TokenExpires = clone.TokenExpires
	return runtime, nil
}

// closeAgentSession releases a session so the sandbox can serve the next turn.
func (r *agentRuntime) closeAgentSession(cfg *Config, sessionID string) {
	if sessionID == "" {
		return
	}
	ctx, cancel := contextWithTimeout(8 * time.Second)
	defer cancel()
	batch := struct {
		Initialize   acpRequest `json:"initialize"`
		SessionClose acpRequest `json:"sessionClose"`
	}{
		Initialize:   acpRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"},
		SessionClose: acpRequest{JSONRPC: "2.0", ID: 2, Method: "session/close", Params: acpSessionLoadParams{SessionID: sessionID}},
	}
	if errClose := r.bootstrap(ctx, cfg, batch, 15*time.Second, func(frame *acpFrame) bool {
		return frame.ID != nil && *frame.ID == 2
	}); errClose != nil {
		logWarn("agent session close failed", map[string]any{"error": errClose.Error()})
	}
}

// runtimeForProject prefers the runtime already bound to the project and only
// provisions a new one when the project has none.
func runtimeForProject(ctx context.Context, client *apiClient, cfg *Config, projectID string) (*agentRuntime, error) {
	if runtime, errExisting := existingRuntimeForProject(ctx, client, cfg, projectID); errExisting == nil && runtime != nil {
		return runtime, nil
	}
	return createRuntimeForProject(ctx, client, cfg, projectID)
}

// existingRuntimeForProject resolves project -> runtimeId -> ACP links.
func existingRuntimeForProject(ctx context.Context, client *apiClient, cfg *Config, projectID string) (*agentRuntime, error) {
	data, errGet := client.agentJSON(ctx, http.MethodGet, pathAgentRuntimes+"/by-project/"+projectID, nil, nil)
	if errGet != nil {
		return nil, errGet
	}
	var payload struct {
		RuntimeID string `json:"runtimeId"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil || payload.RuntimeID == "" {
		return nil, fmt.Errorf("项目 %s 还没有运行时", projectID)
	}
	return runtimeByID(ctx, client, cfg, projectID, payload.RuntimeID)
}

// runtimeByID reads one runtime's links.
func runtimeByID(ctx context.Context, client *apiClient, cfg *Config, projectID, runtimeID string) (*agentRuntime, error) {
	data, errGet := client.agentJSON(ctx, http.MethodGet, pathAgentRuntimes+"/"+runtimeID, nil, nil)
	if errGet != nil {
		return nil, errGet
	}
	var payload struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Links  struct {
			ACPLink struct {
				Token          string `json:"token"`
				URL            string `json:"url"`
				TokenExpiresAt int64  `json:"tokenExpiresAt"`
			} `json:"acpLink"`
		} `json:"links"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if payload.ID == "" || payload.Links.ACPLink.URL == "" || payload.Links.ACPLink.Token == "" {
		return nil, fmt.Errorf("运行时信息不完整")
	}
	runtime := &agentRuntime{
		ProjectID: projectID,
		RuntimeID: payload.ID,
		ACPURL:    strings.TrimRight(payload.Links.ACPLink.URL, "/"),
		ACPToken:  payload.Links.ACPLink.Token,
		Created:   time.Now(),
	}
	if payload.Links.ACPLink.TokenExpiresAt > 0 {
		runtime.TokenExpires = time.Unix(payload.Links.ACPLink.TokenExpiresAt, 0)
	}
	return runtime, nil
}

// projectRuntimeID returns the runtime id currently bound to a project.
func projectRuntimeID(ctx context.Context, client *apiClient, projectID string) (string, error) {
	data, errGet := client.agentJSON(ctx, http.MethodGet, pathAgentRuntimes+"/by-project/"+projectID, nil, nil)
	if errGet != nil {
		return "", errGet
	}
	var payload struct {
		RuntimeID string `json:"runtimeId"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return "", errUnmarshal
	}
	return payload.RuntimeID, nil
}

// deleteRuntime removes a runtime record so a wedged sandbox does not block the
// project forever.
func deleteRuntime(ctx context.Context, client *apiClient, runtimeID string) error {
	_, errPost := client.agentJSON(ctx, http.MethodPost, pathAgentRuntimes+"/"+runtimeID+"/delete", nil, map[string]any{})
	return errPost
}

func createRuntimeForProject(ctx context.Context, client *apiClient, cfg *Config, projectID string) (*agentRuntime, error) {
	return createRuntimeForProjectAttempt(ctx, client, cfg, projectID, true)
}

func createRuntimeForProjectAttempt(ctx context.Context, client *apiClient, cfg *Config, projectID string, mayReplace bool) (*agentRuntime, error) {
	data, errPost := client.agentJSON(ctx, http.MethodPost, pathAgentRuntimes, nil, map[string]any{
		"projectId": projectID,
	})
	if errPost != nil {
		if strings.Contains(errPost.Error(), "RUNTIME_ALREADY_EXISTS") {
			// The project already owns a runtime (often one the web app created):
			// reuse it when it is healthy.
			if runtime, errExisting := existingRuntimeForProject(ctx, client, cfg, projectID); errExisting == nil && runtime != nil {
				return runtime, nil
			}
			// Otherwise the binding is stale or the sandbox is wedged: delete and
			// provision once more.
			if mayReplace {
				if staleID, errID := projectRuntimeID(ctx, client, projectID); errID == nil && staleID != "" {
					logWarn("replacing a stale agent runtime", map[string]any{"runtime": staleID})
					_ = deleteRuntime(ctx, client, staleID)
					if runtime, errRetry := createRuntimeForProjectAttempt(ctx, client, cfg, projectID, false); errRetry == nil {
						return runtime, nil
					}
				}
			}
		}
		return nil, fmt.Errorf("申请 Miora Agent 运行时失败：%w", errPost)
	}
	var payload struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Links  struct {
			ACPLink struct {
				Token          string `json:"token"`
				URL            string `json:"url"`
				TokenExpiresAt int64  `json:"tokenExpiresAt"`
			} `json:"acpLink"`
		} `json:"links"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode runtime: %w", errUnmarshal)
	}
	if payload.ID == "" || payload.Links.ACPLink.URL == "" || payload.Links.ACPLink.Token == "" {
		return nil, &upstreamError{Message: "运行时信息不完整：" + upstreamSnippet(data)}
	}
	runtime := &agentRuntime{
		ProjectID: projectID,
		RuntimeID: payload.ID,
		ACPURL:    strings.TrimRight(payload.Links.ACPLink.URL, "/"),
		ACPToken:  payload.Links.ACPLink.Token,
		Created:   time.Now(),
	}
	if payload.Links.ACPLink.TokenExpiresAt > 0 {
		runtime.TokenExpires = time.Unix(payload.Links.ACPLink.TokenExpiresAt, 0)
	}
	return runtime, nil
}

// ---------------------------------------------------------------------------
// ACP over HTTP + SSE
// ---------------------------------------------------------------------------

// acpRequest is one JSON-RPC request in the bootstrap batch.
type acpRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type acpSessionLoadParams struct {
	SessionID string         `json:"sessionId"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

type acpSetModelParams struct {
	ModelID string `json:"modelId"`
}

type acpSetModeParams struct {
	ModeID string `json:"modeId"`
}

type acpPromptParams struct {
	SessionID string           `json:"sessionId"`
	Prompt    []acpPromptBlock `json:"prompt"`
}

type acpPromptBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// acpFrame is one SSE `data:` payload.
type acpFrame struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Params struct {
		// Artifact notifications (_codebuddy.ai/artifact) carry their payload at the
		// params level, next to `update` for session/update notifications.
		Event    string `json:"event"`
		Artifact struct {
			URI   string `json:"uri"`
			Type  string `json:"type"`
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"artifact"`
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Status        string          `json:"status"`
			Title         string          `json:"title"`
			ToolName      string          `json:"toolName"`
			RawOutput     json.RawMessage `json:"rawOutput"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			InputTokens      int64  `json:"inputTokens"`
			OutputTokens     int64  `json:"outputTokens"`
			CachedReadTokens int64  `json:"cachedReadTokens"`
			Model            string `json:"model"`
			Used             int64  `json:"used"`
			Size             int64  `json:"size"`
		} `json:"update"`
	} `json:"params"`
}

type agentUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CachedReadTokens int64
	Model            string
}

// agentSession is the result of session/new.
type agentSession struct {
	SessionID string
	Models    []agentModel
	ModeID    string
}

// startAgentSession opens a fresh ACP session on the runtime's sandbox.
func (r *agentRuntime) startAgentSession(ctx context.Context, cfg *Config) (*agentSession, error) {
	if existing := strings.TrimSpace(r.SessionID); existing != "" {
		return &agentSession{SessionID: existing}, nil
	}
	// A freshly provisioned sandbox needs tens of seconds before its session
	// manifest exists; the manager answers "no session available after 40s" until
	// then, so retry instead of failing the user's request.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		session, errSession := r.createAgentSession(ctx, cfg)
		if errSession == nil {
			return session, nil
		}
		lastErr = errSession
		if !strings.Contains(errSession.Error(), "session") {
			return nil, errSession
		}
		if errWait := sleepCtx(ctx, 15*time.Second); errWait != nil {
			return nil, errWait
		}
	}
	return nil, lastErr
}

// createAgentSession performs one session/new round trip.
func (r *agentRuntime) createAgentSession(ctx context.Context, cfg *Config) (*agentSession, error) {
	batch := struct {
		Initialize acpRequest `json:"initialize"`
		SessionNew acpRequest `json:"sessionNew"`
	}{
		Initialize: acpRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"},
		SessionNew: acpRequest{JSONRPC: "2.0", ID: agentSessionNewID, Method: "session/new", Params: map[string]any{
			"cwd":        "/home/oai/share",
			"mcpServers": []any{},
		}},
	}
	session := &agentSession{}
	errStream := r.bootstrap(ctx, cfg, batch, 120*time.Second, func(frame *acpFrame) bool {
		if frame.ID != nil && *frame.ID == agentSessionNewID {
			var result struct {
				SessionID string `json:"sessionId"`
				Models    struct {
					AvailableModels []struct {
						ModelID string `json:"modelId"`
						Name    string `json:"name"`
					} `json:"availableModels"`
				} `json:"models"`
				Modes struct {
					CurrentModeID string `json:"currentModeId"`
				} `json:"modes"`
			}
			if errUnmarshal := json.Unmarshal(frame.Result, &result); errUnmarshal == nil {
				session.SessionID = result.SessionID
				session.ModeID = result.Modes.CurrentModeID
				models := make([]agentModel, 0, len(result.Models.AvailableModels))
				for _, item := range result.Models.AvailableModels {
					models = append(models, agentModel{ID: item.ModelID, Name: item.Name, FromRuntime: true})
				}
				session.Models = models
			}
			return true
		}
		return false
	})
	if errStream != nil {
		return nil, errStream
	}
	if session.SessionID == "" {
		return nil, fmt.Errorf("沙箱暂时没有可用会话（SingleSessionId 被占用），稍后重试")
	}
	r.SessionID = session.SessionID
	if r.StateKey != "" {
		rememberState(r.StateKey, r.ProjectID, r.RuntimeID, r.SessionID)
	}
	if len(session.Models) > 0 {
		r.Models = session.Models
		agentRuntimes.mu.Lock()
		agentRuntimes.models = session.Models
		agentRuntimes.mu.Unlock()
	}
	return session, nil
}

// prompt sends one user turn and streams the assistant text back.
func (r *agentRuntime) prompt(ctx context.Context, cfg *Config, session *agentSession, modelID, text string, onDelta func(string) error) (*agentUsage, string, []string, error) {
	batch := struct {
		Initialize  acpRequest `json:"initialize"`
		SessionLoad acpRequest `json:"sessionLoad"`
		SetModel    acpRequest `json:"setModel"`
		SetMode     acpRequest `json:"setMode"`
		Prompt      acpRequest `json:"prompt"`
	}{
		Initialize: acpRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"},
		SessionLoad: acpRequest{JSONRPC: "2.0", ID: 2, Method: "session/load", Params: acpSessionLoadParams{
			SessionID: session.SessionID,
			Meta:      map[string]any{"codebuddy.ai": map[string]any{"round": -1}},
		}},
		SetModel: acpRequest{JSONRPC: "2.0", ID: 3, Method: "session/set_model", Params: acpSetModelParams{ModelID: modelID}},
		SetMode:  acpRequest{JSONRPC: "2.0", ID: 5, Method: "session/set_mode", Params: acpSetModeParams{ModeID: firstNonEmpty(cfg.AgentMode, agentBypassMode)}},
		Prompt: acpRequest{JSONRPC: "2.0", ID: agentPromptID, Method: "session/prompt", Params: acpPromptParams{
			SessionID: session.SessionID,
			Prompt:    []acpPromptBlock{{Type: "text", Text: text}},
		}},
	}

	usage := &agentUsage{Model: modelID}
	stopReason := ""
	permissionDenied := false
	updateCounts := map[string]int{}
	artifacts := make([]string, 0, 2)
	promptTimeout := 300 * time.Second
	if cfg != nil && cfg.AgentTimeoutSeconds > 0 {
		promptTimeout = time.Duration(cfg.AgentTimeoutSeconds) * time.Second
	}
	errStream := r.bootstrap(ctx, cfg, batch, promptTimeout, func(frame *acpFrame) bool {
		switch {
		case frame.Method == "session/update":
			update := frame.Params.Update
			updateCounts[update.SessionUpdate]++
			switch update.SessionUpdate {
			case "agent_message_chunk":
				if update.Content.Text != "" && onDelta != nil {
					if errDelta := onDelta(update.Content.Text); errDelta != nil {
						return true
					}
				}
			case "tool_call", "tool_call_update":
				// Design tools (image / video / 3D) report completion here; when the
				// payload carries artifact links they are forwarded to the caller.
				if strings.EqualFold(update.Status, "completed") {
					found := mediaURLsFromToolOutput(update.RawOutput)
					if len(found) > 0 {
						artifacts = append(artifacts, found...)
						logInfo("agent tool produced artifacts: "+strconv.Itoa(len(found)), map[string]any{
							"tool": firstNonEmpty(update.ToolName, update.Title),
						})
					}
				}
			case "usage_update":
				if update.InputTokens > 0 {
					usage.InputTokens = update.InputTokens
				}
				if update.OutputTokens > 0 {
					usage.OutputTokens = update.OutputTokens
				}
				if update.CachedReadTokens > 0 {
					usage.CachedReadTokens = update.CachedReadTokens
				}
			}
		case frame.Method == "_codebuddy.ai/artifact":
			// Design tools publish their results here (uri + type), not through
			// tool_call_update. Without this the reply says "image generated" but the
			// client never receives a link.
			artifact := frame.Params.Artifact
			if frame.Params.Event != "deleted" {
				for _, url := range artifactURLs(artifact.URI, artifact.Text) {
					artifacts = append(artifacts, url)
				}
				if artifact.URI != "" {
					logInfo("agent artifact: "+artifact.Type, map[string]any{"uri": truncateRunes(artifact.URI, 120)})
				}
			}
		case frame.Method == "session/request_permission":
			// Tool approval cannot be answered inside a batch bootstrap call; the
			// session runs in bypassPermissions by default, so seeing this means the
			// operator pinned a stricter mode.
			permissionDenied = true
		case frame.ID != nil && *frame.ID == agentPromptID:
			var result struct {
				StopReason string `json:"stopReason"`
				Usage      struct {
					InputTokens      int64  `json:"inputTokens"`
					OutputTokens     int64  `json:"outputTokens"`
					CachedReadTokens int64  `json:"cachedReadTokens"`
					Model            string `json:"model"`
				} `json:"usage"`
			}
			if errUnmarshal := json.Unmarshal(frame.Result, &result); errUnmarshal == nil {
				stopReason = result.StopReason
				if result.Usage.InputTokens > 0 {
					usage.InputTokens = result.Usage.InputTokens
				}
				if result.Usage.OutputTokens > 0 {
					usage.OutputTokens = result.Usage.OutputTokens
				}
				if result.Usage.CachedReadTokens > 0 {
					usage.CachedReadTokens = result.Usage.CachedReadTokens
				}
				if result.Usage.Model != "" {
					usage.Model = result.Usage.Model
				}
			}
			return true
		}
		return false
	})
	if len(updateCounts) > 0 {
		logInfo("agent updates: "+formatUpdateCounts(updateCounts), map[string]any{"model": modelID})
	}
	if errStream != nil {
		// The cached session may have been reclaimed by the sandbox: drop it so the
		// caller's retry opens a fresh one.
		if strings.Contains(errStream.Error(), "session") {
			r.SessionID = ""
		}
		return usage, stopReason, artifacts, errStream
	}
	if permissionDenied && stopReason == "" {
		return usage, stopReason, artifacts, fmt.Errorf("该会话触发了工具调用审批：请把 agent_mode 设为 bypassPermissions，或改用网页端操作")
	}
	return usage, stopReason, artifacts, nil
}

// bootstrap posts one ACP batch and consumes the SSE stream until done() returns
// true, the stream ends, or the context expires.
func (r *agentRuntime) bootstrap(ctx context.Context, cfg *Config, batch any, timeout time.Duration, done func(*acpFrame) bool) error {
	payload, errMarshal := json.Marshal(batch)
	if errMarshal != nil {
		return errMarshal
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+r.ACPToken)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("User-Agent", defaultUserAgent)

	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type openResult struct {
		resp *http.Response
		err  error
	}
	opened := make(chan openResult, 1)
	go func() {
		req, errNew := http.NewRequestWithContext(streamCtx, http.MethodPost, r.ACPURL+agentACPBootstrap, bytes.NewReader(payload))
		if errNew != nil {
			opened <- openResult{err: errNew}
			return
		}
		for key, values := range headers {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		// Streaming: no overall timeout (the context governs), but reuse the v4-first
		// transport so a broken IPv6 route cannot stall the SSE handshake.
		client := &http.Client{Transport: directTransport(proxySettingOf(cfg))}
		resp, errDo := client.Do(req)
		opened <- openResult{resp: resp, err: errDo}
	}()

	var resp *http.Response
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-opened:
		if result.err != nil {
			return result.err
		}
		resp = result.resp
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &upstreamError{Status: resp.StatusCode, Message: "ACP 连接被拒绝（token 可能已过期）"}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := readAllLimited(resp.Body, 4096)
		return &upstreamError{Status: resp.StatusCode, Message: "ACP bootstrap 失败：" + upstreamSnippet(body)}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), agentSSEScanMaxBytes)
	for scanner.Scan() {
		if streamCtx.Err() != nil {
			return streamCtx.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		frame := &acpFrame{}
		if errUnmarshal := json.Unmarshal([]byte(raw), frame); errUnmarshal != nil {
			continue
		}
		if frame.Error != nil && frame.Error.Message != "" {
			return &upstreamError{Message: "ACP 错误：" + frame.Error.Message}
		}
		if done != nil && done(frame) {
			return nil
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	return nil
}

func readAllLimited(body interface{ Read([]byte) (int, error) }, limit int64) ([]byte, error) {
	buffer := make([]byte, 0, 1024)
	chunk := make([]byte, 1024)
	for int64(len(buffer)) < limit {
		read, errRead := body.Read(chunk)
		if read > 0 {
			buffer = append(buffer, chunk[:read]...)
		}
		if errRead != nil {
			return buffer, nil
		}
	}
	return buffer, nil
}

func encodeQuery(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, "&")
}

// formatUpdateCounts renders the ACP update histogram for the host log, e.g.
// "session_info_update=4,agent_message_chunk=33,tool_call=2".
func formatUpdateCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

// mediaURLsFromToolOutput pulls artifact links out of a completed tool result.
// Miora's design tools answer with a JSON envelope that is sometimes nested twice,
// so the payload is unwrapped first and then scanned for signed asset URLs.
var (
	toolURLPattern  = regexp.MustCompile(`https://sign\.miorausercontent\.com/[^"\s\\]+`)
	toolFilePattern = regexp.MustCompile(`"file_key"\s*:\s*"([^"]+)"`)
)

// unwrapToolOutput flattens {"text":"[{...}]"} into the innermost JSON text.
func unwrapToolOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var envelope struct {
		Text string `json:"text"`
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal == nil && envelope.Text != "" {
		var blocks []struct {
			Text string `json:"text"`
		}
		if errBlocks := json.Unmarshal([]byte(envelope.Text), &blocks); errBlocks == nil && len(blocks) > 0 {
			return blocks[0].Text
		}
		return envelope.Text
	}
	return string(raw)
}

func mediaURLsFromToolOutput(raw json.RawMessage) []string {
	text := unwrapToolOutput(raw)
	if text == "" {
		return nil
	}
	found := make([]string, 0, 2)
	seen := map[string]struct{}{}
	for _, match := range toolURLPattern.FindAllString(text, 4) {
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		found = append(found, match)
	}
	return found
}

// toolFileKeys exposes file keys from a tool result for callers that prefer
// re-signing over a short-lived URL.
func toolFileKeys(raw json.RawMessage) []string {
	text := unwrapToolOutput(raw)
	if text == "" {
		return nil
	}
	keys := make([]string, 0, 2)
	for _, match := range toolFilePattern.FindAllStringSubmatch(text, 4) {
		if len(match) > 1 {
			keys = append(keys, match[1])
		}
	}
	return keys
}

// artifactURLs normalises an artifact notification into displayable links. The
// uri is usually the signed asset URL; when it is a bare file key the text field
// or a signed URL pattern inside the payload supplies the link.
func artifactURLs(uri, text string) []string {
	out := make([]string, 0, 2)
	seen := map[string]struct{}{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "http://") {
		add(uri)
	}
	for _, match := range toolURLPattern.FindAllString(text, 4) {
		add(match)
	}
	return out
}
