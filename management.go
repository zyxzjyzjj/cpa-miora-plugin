package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

// managementRoutes lists the plugin-owned Management API surface. Paths are
// joined onto /v0/management by the host, so the plugin namespace is included
// here; every one of these routes requires the admin key.
func managementRoutes() []map[string]any {
	prefix := "/" + providerID
	return []map[string]any{
		{"Method": http.MethodGet, "Path": prefix + "/status", "Description": "插件运行状态：版本、上游地址、模型数、账号数。"},
		{"Method": http.MethodGet, "Path": prefix + "/accounts", "Description": "列出 Miora 账号、登录态有效期与额度（query: live=1 现场拉取额度）。"},
		{"Method": http.MethodPost, "Path": prefix + "/accounts/import", "Description": "粘贴 authToken/refreshToken 导入账号（body: {auth_token, refresh_token, label}）。"},
		{"Method": http.MethodPost, "Path": prefix + "/accounts/scan", "Description": "扫描本机 Edge/Chrome 的 miora.design 登录态并验证。"},
		{"Method": http.MethodPost, "Path": prefix + "/accounts/browser-import", "Description": "导入一条扫描到的浏览器登录态（body: {auth_token, refresh_token, source}）。"},
		{"Method": http.MethodPost, "Path": prefix + "/accounts/test", "Description": "用指定账号请求 profile/额度验证可用性（body: {auth_index}）。"},
		{"Method": http.MethodPost, "Path": prefix + "/accounts/refresh", "Description": "立即轮换该账号的 token 并写回 auth 文件（body: {auth_index}）。"},
		{"Method": http.MethodPost, "Path": prefix + "/accounts/delete", "Description": "删除该账号的 auth 文件（body: {auth_index}）。"},
		{"Method": http.MethodGet, "Path": prefix + "/models", "Description": "Miora 模型目录（含能力、参考图上限、默认参数）。"},
		{"Method": http.MethodPost, "Path": prefix + "/models/refresh", "Description": "强制重新拉取上游模型目录。"},
		{"Method": http.MethodPost, "Path": prefix + "/generate", "Description": "在面板里发起一次生成（body: {prompt, model, media, aspect_ratio, resolution, duration}）。"},
		{"Method": http.MethodGet, "Path": prefix + "/tasks", "Description": "最近的生成任务（query: limit）。"},
		{"Method": http.MethodGet, "Path": prefix + "/media", "Description": "已生成的产物列表（query: limit）。"},
		{"Method": http.MethodPost, "Path": prefix + "/media/sign", "Description": "用 fileKey 重新签名产物 URL（body: {file_key}）。"},
		{"Method": http.MethodGet, "Path": prefix + "/logs", "Description": "请求日志（query: limit）。"},
		{"Method": http.MethodGet, "Path": prefix + "/settings", "Description": "当前插件配置（只读视图，修改请走 CPA 的 plugins.configs）。"},
		{"Method": http.MethodGet, "Path": prefix + "/diagnostics", "Description": "上游连通性自检：目录、登录态、额度。"},
	}
}

// managementRegistration declares routes and browser resources.
func managementRegistration() map[string]any {
	return map[string]any{
		"routes": managementRoutes(),
		"resources": []map[string]any{
			{
				"Path":        "/panel",
				"Menu":        "Miora",
				"Description": "Miora 控制台：登录态导入、生成台、模型、产物、日志。",
			},
			{
				"Path":        "/status",
				"Menu":        "",
				"Description": "机器可读的插件状态 JSON（不含凭证）。",
			},
		},
	}
}

// managementHandle dispatches one management or resource request.
func managementHandle(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id"`
	}
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
		}
	}
	route := managementRouteSuffix(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	query := map[string][]string(req.Query)

	switch {
	case route == "/" || route == "/status":
		return okEnvelope(jsonResponse(http.StatusOK, statusPayload()))

	case route == "/panel":
		return okEnvelope(htmlResponse(panelHTML()))

	case route == "/accounts" && method == http.MethodGet:
		live := firstQueryValue(query, "live") == "1"
		return okEnvelope(jsonResponse(http.StatusOK, accountsPayload(live)))

	case route == "/accounts/import" && method == http.MethodPost:
		return okEnvelope(handleAccountImport(req.Body, "manual"))

	case route == "/accounts/scan" && method == http.MethodPost:
		return okEnvelope(handleAccountScan(req.Body))

	case route == "/accounts/browser-import" && method == http.MethodPost:
		return okEnvelope(handleAccountImport(req.Body, "browser"))

	case route == "/accounts/test" && method == http.MethodPost:
		return okEnvelope(handleAccountTest(req.Body))

	case route == "/accounts/refresh" && method == http.MethodPost:
		return okEnvelope(handleAccountRefresh(req.Body))

	case route == "/accounts/delete" && method == http.MethodPost:
		return okEnvelope(handleAccountDelete(req.Body))

	case route == "/models" && method == http.MethodGet:
		return okEnvelope(jsonResponse(http.StatusOK, modelsPayload()))

	case route == "/models/refresh" && method == http.MethodPost:
		invalidateCatalog()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		snapshot := loadCatalog(ctx)
		payload := modelsPayload()
		payload["tier"] = snapshot.Tier
		if snapshot.Err != "" {
			payload["error"] = snapshot.Err
		}
		return okEnvelope(jsonResponse(http.StatusOK, payload))

	case route == "/generate" && method == http.MethodPost:
		return okEnvelope(handlePanelGenerate(req.Body))

	case route == "/tasks" && method == http.MethodGet:
		limit := queryInt(query, "limit", 50)
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"tasks": listTasks(limit)}))

	case route == "/media" && method == http.MethodGet:
		limit := queryInt(query, "limit", 60)
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"media": mediaPayload(limit)}))

	case route == "/media/sign" && method == http.MethodPost:
		return okEnvelope(handleMediaSign(req.Body))

	case route == "/logs" && method == http.MethodGet:
		limit := queryInt(query, "limit", 100)
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"events": listEvents(limit)}))

	case route == "/settings" && method == http.MethodGet:
		return okEnvelope(jsonResponse(http.StatusOK, settingsPayload()))

	case route == "/diagnostics":
		return okEnvelope(jsonResponse(http.StatusOK, diagnosticsPayload()))

	default:
		return okEnvelope(errorJSON(http.StatusNotFound, "未知的路由 "+method+" "+route))
	}
}

func managementRouteSuffix(path string) string {
	trimmed := strings.TrimSpace(path)
	for _, prefix := range []string{
		"/v0/management/" + providerID,
		"/v0/resource/plugins/" + providerID,
	} {
		if strings.HasPrefix(trimmed, prefix) {
			trimmed = strings.TrimPrefix(trimmed, prefix)
			break
		}
	}
	if trimmed == "" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		body, _ = json.Marshal(map[string]any{"ok": false, "error": errMarshal.Error()})
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func htmlResponse(page string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":           []string{"text/html; charset=utf-8"},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
		},
		Body: []byte(page),
	}
}

func errorJSON(status int, message string) pluginapi.ManagementResponse {
	return jsonResponse(status, map[string]any{"ok": false, "error": message})
}

func firstQueryValue(query map[string][]string, key string) string {
	if values, ok := query[key]; ok && len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	return ""
}

func queryInt(query map[string][]string, key string, fallback int) int {
	raw := firstQueryValue(query, key)
	if raw == "" {
		return fallback
	}
	value, errParse := strconv.Atoi(raw)
	if errParse != nil || value <= 0 {
		return fallback
	}
	return value
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func statusPayload() map[string]any {
	cfg := config()
	snapshot := peekCatalog()
	accounts, _ := hostAccounts()
	return map[string]any{
		"ok":             true,
		"provider":       providerID,
		"version":        pluginVersion,
		"repository":     pluginRepositoryURL,
		"base_url":       cfg.BaseURL,
		"language":       cfg.Language,
		"model_prefix":   cfg.ModelPrefix,
		"models":         len(catalogueModels(cfg)),
		"tier":           snapshot.Tier,
		"catalog_age_s":  int(time.Since(snapshot.FetchedAt).Seconds()),
		"catalog_error":  snapshot.Err,
		"accounts":       len(accounts),
		"capabilities":   declaredCapabilities(),
		"poll_interval":  cfg.PollIntervalSeconds,
		"task_timeout":   cfg.TaskTimeoutSeconds,
		"max_concurrent": cfg.MaxConcurrentTasks,
	}
}

// ---------------------------------------------------------------------------
// accounts
// ---------------------------------------------------------------------------

// hostAccounts lists auth files owned by this provider through the host bridge.
func hostAccounts() ([]pluginapi.HostAuthFileEntry, error) {
	raw, errList := hostCall(pluginabi.MethodHostAuthList, map[string]any{})
	if errList != nil {
		return nil, errList
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"Files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &list); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(list.Files))
	for _, entry := range list.Files {
		if strings.EqualFold(entry.Provider, providerID) || strings.EqualFold(entry.Type, providerID) {
			out = append(out, entry)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// accountSnapshot caches an account's identity and balance, so opening the panel
// does not fan out one upstream call per account per refresh.
type accountSnapshot struct {
	Email     string  `json:"email"`
	UserID    string  `json:"user_id"`
	Plan      string  `json:"plan"`
	Nickname  string  `json:"nickname"`
	Credits   float64 `json:"credits"`
	Total     float64 `json:"total_credits"`
	Used      float64 `json:"used_credits"`
	ExpiresAt string  `json:"expires_at"`
	// CreditExpires is when the granted credits lapse (billing service), which is
	// independent of the token expiry above.
	CreditExpires string    `json:"credit_expires_at"`
	CheckedAt     time.Time `json:"checked_at"`
	Error         string    `json:"error,omitempty"`
}

var accountCache = struct {
	mu      sync.Mutex
	entries map[string]accountSnapshot
}{entries: map[string]accountSnapshot{}}

func cachedAccount(index string) (accountSnapshot, bool) {
	accountCache.mu.Lock()
	defer accountCache.mu.Unlock()
	snapshot, ok := accountCache.entries[index]
	if !ok || time.Since(snapshot.CheckedAt) > 2*time.Minute {
		return accountSnapshot{}, false
	}
	return snapshot, true
}

func storeAccount(index string, snapshot accountSnapshot) {
	accountCache.mu.Lock()
	defer accountCache.mu.Unlock()
	accountCache.entries[index] = snapshot
}

func invalidateAccount(index string) {
	accountCache.mu.Lock()
	defer accountCache.mu.Unlock()
	delete(accountCache.entries, index)
}

// loadAccountCredential reads one auth file through the host and decodes it.
func loadAccountCredential(entry pluginapi.HostAuthFileEntry) (*credential, string, error) {
	index := strings.TrimSpace(entry.AuthIndex)
	if index == "" {
		return nil, "", fmt.Errorf("auth_index 缺失")
	}
	raw, errGet := hostCall(pluginabi.MethodHostAuthGet, map[string]any{"auth_index": index})
	if errGet != nil {
		return nil, "", errGet
	}
	var record struct {
		Name string          `json:"Name"`
		Path string          `json:"Path"`
		JSON json.RawMessage `json:"JSON"`
	}
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return nil, "", errUnmarshal
	}
	cred, errCred := credentialFromStorage(record.JSON)
	if errCred != nil {
		return nil, record.Name, errCred
	}
	if !cred.valid() {
		return nil, record.Name, fmt.Errorf("auth 文件里没有 auth_token")
	}
	cred.Source = firstNonEmpty(cred.Source, "host")
	return cred, firstNonEmpty(record.Name, entry.Name), nil
}

func accountsPayload(live bool) map[string]any {
	entries, errList := hostAccounts()
	if errList != nil {
		return map[string]any{"ok": false, "error": "读取 host 凭证失败：" + errList.Error(), "accounts": []any{}}
	}
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		index := strings.TrimSpace(entry.AuthIndex)
		view := map[string]any{
			"auth_index":   index,
			"name":         entry.Name,
			"label":        firstNonEmpty(entry.Label, entry.Name),
			"status":       entry.Status,
			"disabled":     entry.Disabled,
			"email":        entry.Email,
			"path":         entry.Path,
			"success":      entry.Success,
			"failed":       entry.Failed,
			"next_retry":   formatTime(entry.NextRetryAfter),
			"last_refresh": formatTime(entry.LastRefresh),
			"runtime_note": entry.StatusMessage,
			"base_url":     entry.BaseURL,
		}
		snapshot, cached := cachedAccount(index)
		cred, fileName, errCred := loadAccountCredential(entry)
		if fileName != "" {
			view["name"] = fileName
		}
		if errCred == nil && cred != nil {
			view["email"] = firstNonEmpty(cred.Email, entry.Email)
			view["user_id"] = cred.UserID
			view["plan"] = cred.Plan
			view["has_refresh_token"] = cred.RefreshToken != ""
			if expiry := cred.expiry(); !expiry.IsZero() {
				view["expires_at"] = expiry.UTC().Format(time.RFC3339)
				view["expires_in_hours"] = int(time.Until(expiry).Hours())
			}
		}
		if cached {
			view["credits"] = snapshot
		}
		if firstNonEmpty(strOf(view["plan"]), strOf(view["email"])) != "" && strings.TrimSpace(strOf(view["plan"])) == "" {
			// Miora's profile has no plan field; the catalogue tier is the honest label.
			if tier := peekCatalog().Tier; tier != "" {
				view["plan"] = tier
			}
		}
		if live {
			snapshot := reflectAccount(context.Background(), index, cred)
			view["credits"] = snapshot
			if snapshot.Email != "" {
				view["email"] = snapshot.Email
			}
			if snapshot.Plan != "" {
				view["plan"] = snapshot.Plan
			}
		}
		out = append(out, view)
	}
	return map[string]any{"ok": true, "accounts": out}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// reflectAccount calls the upstream for one account and caches the result.
func reflectAccount(ctx context.Context, index string, cred *credential) accountSnapshot {
	snapshot := accountSnapshot{CheckedAt: time.Now()}
	if cred == nil || !cred.valid() {
		snapshot.Error = "没有可用的 auth_token"
		storeAccount(index, snapshot)
		return snapshot
	}
	client := newAPIClient(config(), cred)
	profile, errProfile := client.fetchProfile(ctx)
	if errProfile != nil {
		snapshot.Error = errProfile.Error()
		storeAccount(index, snapshot)
		return snapshot
	}
	snapshot.Email = profile.Email
	snapshot.UserID = profile.UserID
	snapshot.Plan = profile.Plan
	if snapshot.Plan == "" {
		if credits, errCredits := client.fetchBillingSubscription(ctx); errCredits == nil && credits != nil {
			snapshot.Plan = credits.TierName
		}
	}
	snapshot.Nickname = profile.Nickname
	if expiry := cred.expiry(); !expiry.IsZero() {
		snapshot.ExpiresAt = expiry.UTC().Format(time.RFC3339)
	}
	if credits, errCredits := client.fetchCredits(ctx); errCredits == nil {
		snapshot.Total = credits.TotalAmount.value()
		snapshot.Used = credits.UsedAmount.value()
		snapshot.Credits = credits.remaining()
		snapshot.CreditExpires = credits.ExpiresAt
		if snapshot.Plan == "" {
			snapshot.Plan = credits.TierName
		}
	}
	storeAccount(index, snapshot)
	return snapshot
}

func handleAccountScan(body []byte) pluginapi.ManagementResponse {
	cfg := config()
	if !cfg.EnableBrowserImport {
		return errorJSON(http.StatusForbidden, "浏览器导入已关闭：请在插件配置里设置 enable_browser_import: true")
	}
	candidates, errScan := scanBrowserSessions(cfg)
	if errScan != nil {
		return errorJSON(http.StatusOK, errScan.Error())
	}
	views := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		views = append(views, map[string]any{
			"browser":       candidate.Browser,
			"profile":       candidate.Profile,
			"auth_token":    candidate.AuthToken,
			"refresh_token": candidate.RefreshToken,
			"email":         candidate.Email,
			"user_id":       candidate.UserID,
			"plan":          candidate.Plan,
			"expires_at":    candidate.ExpiresAt,
			"valid":         candidate.Valid,
			"message":       candidate.Message,
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "candidates": views})
}

func handleAccountImport(body []byte, source string) pluginapi.ManagementResponse {
	var payload struct {
		AuthToken    string `json:"auth_token"`
		RefreshToken string `json:"refresh_token"`
		Label        string `json:"label"`
		Raw          string `json:"raw"`
	}
	if len(body) > 0 {
		if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
			return errorJSON(http.StatusBadRequest, "请求体不是合法 JSON："+errUnmarshal.Error())
		}
	}
	// The panel can also submit the whole browser export JSON.
	if strings.TrimSpace(payload.AuthToken) == "" && strings.TrimSpace(payload.Raw) != "" {
		cred, errCred := credentialFromStorage([]byte(payload.Raw))
		if errCred != nil || cred == nil {
			return errorJSON(http.StatusBadRequest, "粘贴内容里没有找到 auth_token")
		}
		payload.AuthToken = cred.AuthToken
		payload.RefreshToken = cred.RefreshToken
	}
	cred := &credential{
		AuthToken:    strings.TrimSpace(payload.AuthToken),
		RefreshToken: strings.TrimSpace(payload.RefreshToken),
		Label:        strings.TrimSpace(payload.Label),
		Source:       source,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	imported, errImport := importCredential(ctx, config(), cred)
	if errImport != nil {
		return errorJSON(http.StatusBadRequest, errImport.Error())
	}
	name := authFileName(imported)
	raw := imported.authFileJSON()
	if _, errSave := hostCall(pluginabi.MethodHostAuthSave, map[string]any{
		"name": name,
		"json": json.RawMessage(raw),
	}); errSave != nil {
		return errorJSON(http.StatusBadGateway, "写入 auth 文件失败："+errSave.Error())
	}
	logInfo("account imported", map[string]any{"account": imported.displayLabel(), "source": source})
	snapshot := accountSnapshot{CheckedAt: time.Now(), Email: imported.Email, UserID: imported.UserID, Plan: imported.Plan}
	if credits, errCredits := newAPIClient(config(), imported).fetchCredits(ctx); errCredits == nil {
		snapshot.Total = credits.TotalAmount.value()
		snapshot.Used = credits.UsedAmount.value()
		snapshot.Credits = credits.remaining()
	}
	storeAccount(strings.TrimSpace(imported.UserID), snapshot)
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":         true,
		"name":       name,
		"email":      imported.Email,
		"user_id":    imported.UserID,
		"plan":       imported.Plan,
		"expires_at": formatTime(imported.expiry()),
		"credits":    snapshot,
		"message":    "已导入 " + imported.displayLabel() + "，CPA 会自动加载该账号",
	})
}

func handleAccountTest(body []byte) pluginapi.ManagementResponse {
	entry, errEntry := resolveAccountEntry(body)
	if errEntry != nil {
		return errorJSON(http.StatusBadRequest, errEntry.Error())
	}
	index := strings.TrimSpace(entry.AuthIndex)
	cred, _, errCred := loadAccountCredential(entry)
	if errCred != nil {
		return errorJSON(http.StatusBadRequest, "读取凭证失败："+errCred.Error())
	}
	invalidateAccount(index)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	snapshot := reflectAccount(ctx, index, cred)
	status := http.StatusOK
	ok := snapshot.Error == ""
	if !ok {
		status = http.StatusBadGateway
	}
	return jsonResponse(status, map[string]any{"ok": ok, "account": snapshot})
}

func handleAccountRefresh(body []byte) pluginapi.ManagementResponse {
	entry, errEntry := resolveAccountEntry(body)
	if errEntry != nil {
		return errorJSON(http.StatusBadRequest, errEntry.Error())
	}
	cred, fileName, errCred := loadAccountCredential(entry)
	if errCred != nil {
		return errorJSON(http.StatusBadRequest, "读取凭证失败："+errCred.Error())
	}
	if cred.RefreshToken == "" {
		return errorJSON(http.StatusBadRequest, "该账号没有 refresh token，只能重新从浏览器导入")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	updated, errRefresh := refreshCredential(ctx, config(), cred)
	if errRefresh != nil {
		return errorJSON(http.StatusBadGateway, "轮换 token 失败："+errRefresh.Error())
	}
	updated.Source = cred.Source
	if _, errSave := hostCall(pluginabi.MethodHostAuthSave, map[string]any{
		"name": firstNonEmpty(fileName, authFileName(updated)),
		"json": json.RawMessage(updated.authFileJSON()),
	}); errSave != nil {
		return errorJSON(http.StatusBadGateway, "写回 auth 文件失败："+errSave.Error())
	}
	invalidateAccount(strings.TrimSpace(entry.AuthIndex))
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":         true,
		"expires_at": formatTime(updated.expiry()),
		"message":    "已轮换 token 并写回 auth 文件",
	})
}

func handleAccountDelete(body []byte) pluginapi.ManagementResponse {
	entry, errEntry := resolveAccountEntry(body)
	if errEntry != nil {
		return errorJSON(http.StatusBadRequest, errEntry.Error())
	}
	path := strings.TrimSpace(entry.Path)
	if path == "" {
		return errorJSON(http.StatusBadRequest, "该账号没有物理文件路径，无法删除")
	}
	if errRemove := os.Remove(path); errRemove != nil {
		return errorJSON(http.StatusBadGateway, "删除 auth 文件失败："+errRemove.Error())
	}
	invalidateAccount(strings.TrimSpace(entry.AuthIndex))
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "message": "已删除 " + path})
}

func resolveAccountEntry(body []byte) (pluginapi.HostAuthFileEntry, error) {
	var payload struct {
		AuthIndex string `json:"auth_index"`
		Name      string `json:"name"`
	}
	if len(body) > 0 {
		if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
			return pluginapi.HostAuthFileEntry{}, errUnmarshal
		}
	}
	entries, errList := hostAccounts()
	if errList != nil {
		return pluginapi.HostAuthFileEntry{}, errList
	}
	wanted := strings.TrimSpace(payload.AuthIndex)
	byName := strings.TrimSpace(payload.Name)
	for _, entry := range entries {
		if wanted != "" && strings.EqualFold(entry.AuthIndex, wanted) {
			return entry, nil
		}
		if wanted == "" && byName != "" && strings.EqualFold(entry.Name, byName) {
			return entry, nil
		}
	}
	if wanted == "" && len(entries) == 1 {
		return entries[0], nil
	}
	return pluginapi.HostAuthFileEntry{}, fmt.Errorf("找不到指定的账号（auth_index=%s）", wanted)
}

// pickCredentialForPanel resolves which account the panel's generation should use.
func pickCredentialForPanel(authIndex string) (*credential, string, error) {
	entries, errList := hostAccounts()
	if errList != nil {
		return nil, "", errList
	}
	if len(entries) == 0 {
		return nil, "", fmt.Errorf("还没有 Miora 账号：请先在“账号”页导入浏览器登录态")
	}
	for _, entry := range entries {
		if authIndex != "" && !strings.EqualFold(entry.AuthIndex, authIndex) {
			continue
		}
		cred, fileName, errCred := loadAccountCredential(entry)
		if errCred != nil {
			continue
		}
		return cred, fileName, nil
	}
	return nil, "", fmt.Errorf("指定账号不可用（auth_index=%s）", authIndex)
}

// ---------------------------------------------------------------------------
// models / generate / media
// ---------------------------------------------------------------------------

func modelsPayload() map[string]any {
	cfg := config()
	snapshot := peekCatalog()
	models := make([]map[string]any, 0, len(snapshot.Models))
	for _, model := range snapshot.Models {
		models = append(models, map[string]any{
			"id":              cfg.advertisedModelID(model.ModelID),
			"upstream":        model.ModelID,
			"media":           model.mediaKind(),
			"label":           model.Label,
			"description":     model.Description,
			"badge":           model.Badge,
			"tags":            model.Tags,
			"available":       model.Available,
			"aspect_ratios":   model.Capabilities.AspectRatios,
			"resolutions":     firstNonEmptyList(model.Capabilities.AllowedResolutions, model.Capabilities.Resolutions),
			"default_ratio":   model.Capabilities.DefaultAspectRatio,
			"default_res":     model.Capabilities.DefaultResolution,
			"seconds_range":   model.Capabilities.SecondsRange,
			"default_seconds": model.Capabilities.DefaultSeconds,
			"max_references":  model.maxReferenceImages(),
			"supports_refs":   model.supportsReferenceImages(),
		})
	}
	if cfg.AgentEnabled {
		for _, model := range knownAgentModels(cfg) {
			models = append(models, map[string]any{
				"id":          cfg.advertisedModelID(model.ID),
				"upstream":    model.ID,
				"media":       "chat",
				"label":       model.Name,
				"description": model.Description,
				"badge":       "agent",
				"credits":     model.Credits,
				"available":   true,
			})
		}
	}
	return map[string]any{
		"ok":      true,
		"tier":    snapshot.Tier,
		"agent":   agentStatusView(cfg),
		"fetched": formatTime(snapshot.FetchedAt),
		"error":   snapshot.Err,
		"models":  models,
		"defaults": map[string]string{
			"image":   cfg.DefaultImageModel,
			"video":   cfg.DefaultVideoModel,
			"three_d": cfg.Default3DModel,
		},
		"auto_model": cfg.advertisedModelID(autoModelID),
	}
}

func firstNonEmptyList(values ...[]string) []string {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func handlePanelGenerate(body []byte) pluginapi.ManagementResponse {
	var payload struct {
		Prompt      string `json:"prompt"`
		Model       string `json:"model"`
		Media       string `json:"media"`
		AspectRatio string `json:"aspect_ratio"`
		Resolution  string `json:"resolution"`
		Duration    int    `json:"duration"`
		AuthIndex   string `json:"auth_index"`
		Size        string `json:"size"`
	}
	if len(body) > 0 {
		if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
			return errorJSON(http.StatusBadRequest, "请求体不是合法 JSON："+errUnmarshal.Error())
		}
	}
	if strings.TrimSpace(payload.Prompt) == "" {
		return errorJSON(http.StatusBadRequest, "提示词不能为空")
	}
	cred, _, errCred := pickCredentialForPanel(strings.TrimSpace(payload.AuthIndex))
	if errCred != nil {
		return errorJSON(http.StatusBadRequest, errCred.Error())
	}
	parsed := &parsedChat{
		Model:       strings.TrimSpace(payload.Model),
		Prompt:      strings.TrimSpace(payload.Prompt),
		AspectRatio: payload.AspectRatio,
		Resolution:  payload.Resolution,
		Size:        payload.Size,
		Duration:    payload.Duration,
		Count:       1,
		Family:      payload.Media,
	}
	cfg := config()
	model, media, errModel := resolveModel(cfg, parsed)
	if errModel != nil {
		return errorJSON(http.StatusBadRequest, errModel.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.taskTimeout()+30*time.Second)
	release, acquired := generationPermits.acquire(cfg.MaxConcurrentTasks)
	if !acquired {
		cancel()
		return errorJSON(http.StatusTooManyRequests, "生成并发已满，请稍后重试")
	}
	generation, errBuild := buildGeneration(ctx, cfg, cred, parsed, model, media)
	if errBuild != nil {
		release()
		cancel()
		return errorJSON(http.StatusBadRequest, errBuild.Error())
	}

	cfgCopy := *cfg
	go func() {
		defer cancel()
		defer release()
		_, errRun := runGeneration(ctx, &cfgCopy, cred, generation, nil)
		if errRun != nil {
			logWarn("panel generation failed", map[string]any{"error": errRun.Error()})
		}
	}()

	return jsonResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"model":   model,
		"media":   media,
		"message": "已提交生成，可在此页查看任务进度",
	})
}

func mediaPayload(limit int) []map[string]any {
	tasks := listTasks(limit)
	out := make([]map[string]any, 0, limit)
	for _, task := range tasks {
		if len(task.Files) == 0 {
			continue
		}
		for _, file := range task.Files {
			out = append(out, map[string]any{
				"task_id":  task.TaskID,
				"model":    task.Model,
				"media":    file.Type,
				"prompt":   task.Prompt,
				"file_key": file.FileKey,
				"url":      file.URL,
				"credit":   task.Credit,
				"at":       task.FinishedAt,
			})
		}
	}
	return out
}

func handleMediaSign(body []byte) pluginapi.ManagementResponse {
	var payload struct {
		FileKey   string   `json:"file_key"`
		FileKeys  []string `json:"file_keys"`
		AuthIndex string   `json:"auth_index"`
	}
	if len(body) > 0 {
		if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
			return errorJSON(http.StatusBadRequest, errUnmarshal.Error())
		}
	}
	keys := payload.FileKeys
	if len(keys) == 0 && payload.FileKey != "" {
		keys = []string{payload.FileKey}
	}
	if len(keys) == 0 {
		return errorJSON(http.StatusBadRequest, "需要 file_key")
	}
	cred, _, errCred := pickCredentialForPanel(strings.TrimSpace(payload.AuthIndex))
	if errCred != nil {
		return errorJSON(http.StatusBadRequest, errCred.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	signed, errSign := newAPIClient(config(), cred).signURLs(ctx, keys)
	if errSign != nil {
		return errorJSON(http.StatusBadGateway, "重新签名失败："+errSign.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "urls": signed})
}

// ---------------------------------------------------------------------------
// settings / diagnostics
// ---------------------------------------------------------------------------

func settingsPayload() map[string]any {
	cfg := config()
	return map[string]any{
		"ok":       true,
		"config":   cfg,
		"defaults": defaultConfig(),
		"note":     "修改请通过 CPA 的 plugins.configs.miora-provider（PATCH /v0/management/plugins/miora-provider/config），插件会收到 plugin.reconfigure。",
	}
}

func diagnosticsPayload() map[string]any {
	cfg := config()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	report := map[string]any{"ok": true, "checks": []map[string]any{}}
	checks := make([]map[string]any, 0, 4)

	snapshot := loadCatalog(ctx)
	checks = append(checks, map[string]any{
		"name":   "模型目录",
		"ok":     len(snapshot.Models) > 0,
		"detail": fmt.Sprintf("%d 个模型，tier=%s", len(snapshot.Models), snapshot.Tier),
		"error":  snapshot.Err,
	})

	entries, errList := hostAccounts()
	if errList != nil {
		checks = append(checks, map[string]any{"name": "账号列表", "ok": false, "error": errList.Error()})
	} else {
		checks = append(checks, map[string]any{"name": "账号列表", "ok": len(entries) > 0, "detail": fmt.Sprintf("%d 个账号", len(entries))})
		for _, entry := range entries {
			cred, _, errCred := loadAccountCredential(entry)
			if errCred != nil {
				checks = append(checks, map[string]any{"name": "账号 " + entry.Name, "ok": false, "error": errCred.Error()})
				continue
			}
			client := newAPIClient(cfg, cred)
			profile, errProfile := client.fetchProfile(ctx)
			if errProfile != nil {
				checks = append(checks, map[string]any{"name": "账号 " + entry.Name, "ok": false, "error": errProfile.Error()})
				continue
			}
			credits, errCredits := client.fetchCredits(ctx)
			detail := profile.Email
			if errCredits == nil {
				detail += fmt.Sprintf("，剩余 %.2f credits", credits.remaining())
			}
			checks = append(checks, map[string]any{"name": "账号 " + entry.Name, "ok": true, "detail": detail})
		}
	}
	report["checks"] = checks
	report["upstream"] = listUpstreamTraces()
	report["agent"] = agentStatusView(cfg)
	if snapshot.Err != "" {
		report["ok"] = false
	}
	return report
}

// strOf renders a stored view field for comparisons without panicking on nil.
func strOf(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
