package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// storageKey is the field under which the credential is persisted inside the
// auth record's StorageJSON. The host round-trips this opaque blob into every
// executor call, so the plugin never needs to read the auth directory itself.
const storageKey = "miora_credential"

// credential is the Miora web session this plugin operates on. Miora only offers
// Google/GitHub OAuth, so the practical credential is the pair the web app keeps
// in localStorage: a 15-day RS256 JWT plus a rotating opaque refresh token.
type credential struct {
	AuthToken    string    `json:"auth_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	UserID       string    `json:"user_id,omitempty"`
	Email        string    `json:"email,omitempty"`
	Nickname     string    `json:"nickname,omitempty"`
	Plan         string    `json:"plan,omitempty"`
	Label        string    `json:"label,omitempty"`
	Source       string    `json:"source,omitempty"`
	BaseURL      string    `json:"base_url,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

func (c *credential) valid() bool {
	return c != nil && strings.TrimSpace(c.AuthToken) != ""
}

func (c *credential) displayLabel() string {
	switch {
	case c == nil:
		return ""
	case strings.TrimSpace(c.Label) != "":
		return strings.TrimSpace(c.Label)
	case strings.TrimSpace(c.Email) != "":
		return strings.TrimSpace(c.Email)
	case strings.TrimSpace(c.UserID) != "":
		return "user " + strings.TrimSpace(c.UserID)
	default:
		return "miora account"
	}
}

// expiry returns the access-token expiry, decoded from the JWT when the stored
// value is missing.
func (c *credential) expiry() time.Time {
	if c == nil {
		return time.Time{}
	}
	if !c.ExpiresAt.IsZero() {
		return c.ExpiresAt
	}
	if exp, ok := jwtExpiry(c.AuthToken); ok {
		return exp
	}
	return time.Time{}
}

// credentialFromStorage decodes the auth-file payload. It accepts three shapes so
// operators can drop in whatever they already have:
//
//	{"miora_credential": {...}}                        plugin storage form
//	{"type":"miora-provider","auth_token":...}         hand-written auth file
//	{"authToken":...,"refreshToken":...,"miora__user_info":{...}}  browser export
func credentialFromStorage(raw []byte) (*credential, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var probe map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &probe); errUnmarshal != nil {
		return nil, fmt.Errorf("credential json is invalid: %w", errUnmarshal)
	}
	if nested, ok := probe[storageKey]; ok {
		cred := &credential{}
		if errUnmarshal := json.Unmarshal(nested, cred); errUnmarshal != nil {
			return nil, fmt.Errorf("decode %s: %w", storageKey, errUnmarshal)
		}
		return normalizeCredential(cred), nil
	}

	cred := &credential{}
	authToken := firstJSONString(probe, "auth_token", "authToken", "access_token")
	refreshToken := firstJSONString(probe, "refresh_token", "refreshToken")
	if authToken == "" {
		return nil, nil
	}
	cred.AuthToken = authToken
	cred.RefreshToken = refreshToken
	cred.UserID = firstJSONString(probe, "user_id", "userId")
	cred.Email = firstJSONString(probe, "email")
	cred.Label = firstJSONString(probe, "label")
	cred.Source = firstJSONString(probe, "source")
	cred.Plan = firstJSONString(probe, "plan")
	if exp, ok := jwtExpiry(authToken); ok {
		cred.ExpiresAt = exp
	}
	// Browser export: miora__user_info holds id/email/plan.
	if info, ok := probe["miora__user_info"]; ok {
		var userInfo struct {
			ID    json.RawMessage `json:"id"`
			Email string          `json:"email"`
			Plan  string          `json:"plan"`
		}
		if errUnmarshal := json.Unmarshal(info, &userInfo); errUnmarshal == nil {
			if cred.UserID == "" {
				cred.UserID = strings.Trim(strings.TrimSpace(string(userInfo.ID)), `"`)
			}
			if cred.Email == "" {
				cred.Email = strings.TrimSpace(userInfo.Email)
			}
			if cred.Plan == "" {
				cred.Plan = strings.TrimSpace(userInfo.Plan)
			}
		}
	}
	return normalizeCredential(cred), nil
}

func normalizeCredential(cred *credential) *credential {
	if cred == nil {
		return nil
	}
	cred.AuthToken = strings.TrimSpace(cred.AuthToken)
	cred.RefreshToken = strings.TrimSpace(cred.RefreshToken)
	cred.Email = strings.TrimSpace(cred.Email)
	cred.UserID = strings.TrimSpace(cred.UserID)
	return cred
}

func firstJSONString(values map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		var text string
		if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// jwtExpiry decodes the `exp` claim without verifying the signature. The claim is
// only used to schedule refreshes; the upstream still rejects a bad token.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if errDecode != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}

// storageJSON encodes the credential blob the host keeps for this auth record.
func (c *credential) storageJSON() []byte {
	raw, _ := json.Marshal(map[string]any{storageKey: c})
	return raw
}

// authFileJSON renders a standalone auth file, so accounts can be created,
// exported and re-imported without the plugin's storage envelope.
func (c *credential) authFileJSON() []byte {
	payload := map[string]any{
		"type":       providerID,
		"auth_token": c.AuthToken,
		"user_id":    c.UserID,
		"email":      c.Email,
		"label":      c.displayLabel(),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if c.RefreshToken != "" {
		payload["refresh_token"] = c.RefreshToken
	}
	if exp := c.expiry(); !exp.IsZero() {
		payload["expires_at"] = exp.UTC().Format(time.RFC3339)
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	return raw
}

// authFileName derives a stable, safe auth file name for an account.
func authFileName(cred *credential) string {
	base := cred.Email
	if base == "" {
		base = cred.UserID
	}
	if base == "" {
		base = randomHex(6)
	}
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_' || r == '@':
			return r
		default:
			return '-'
		}
	}, base)
	return "miora-" + cleaned + ".json"
}

// nextRefreshAfter schedules a proactive refresh before the JWT expires.
func nextRefreshAfter(cfg *Config, cred *credential) time.Time {
	expiry := cred.expiry()
	if expiry.IsZero() {
		return time.Now().Add(12 * time.Hour)
	}
	hours := 24
	if cfg != nil && cfg.AutoRefreshHours > 0 {
		hours = cfg.AutoRefreshHours
	}
	next := expiry.Add(-time.Duration(hours) * time.Hour)
	if next.Before(time.Now().Add(5 * time.Minute)) {
		next = time.Now().Add(30 * time.Minute)
	}
	return next
}

// authDataFor maps a credential onto the host's AuthData contract.
func authDataFor(cfg *Config, cred *credential, fileName string) pluginapi.AuthData {
	if fileName == "" {
		fileName = authFileName(cred)
	}
	metadata := map[string]any{
		"email":   cred.Email,
		"user_id": cred.UserID,
		"label":   cred.displayLabel(),
	}
	if cred.Plan != "" {
		metadata["plan"] = cred.Plan
	}
	if expiry := cred.expiry(); !expiry.IsZero() {
		metadata["expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	if cred.Source != "" {
		metadata["source"] = cred.Source
	}
	// Attributes deliberately stay empty: the host treats an auth `base_url`
	// attribute as a request to execute against that OpenAI-compatible endpoint
	// itself, which would bypass this plugin's executor entirely.
	return pluginapi.AuthData{
		Provider:         providerID,
		ID:               fileName,
		FileName:         fileName,
		Label:            cred.displayLabel(),
		StorageJSON:      cred.storageJSON(),
		Metadata:         metadata,
		NextRefreshAfter: nextRefreshAfter(cfg, cred),
	}
}

// ---------------------------------------------------------------------------
// auth provider RPCs
// ---------------------------------------------------------------------------

func authParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth parse request: %w", errUnmarshal)
	}
	if normalizeProvider(req.Provider) != providerID {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	cred, errCredential := credentialFromStorage(req.RawJSON)
	if errCredential != nil || !cred.valid() {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    authDataFor(config(), cred, req.FileName),
	})
}

func authRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth refresh request: %w", errUnmarshal)
	}
	cred, errCredential := credentialFromStorage(req.StorageJSON)
	if errCredential != nil {
		return failEnvelope("invalid_credential", errCredential.Error(), http.StatusUnauthorized)
	}
	if !cred.valid() {
		return failEnvelope("missing_credential", "auth record has no Miora access token", http.StatusUnauthorized)
	}
	cfg := config()
	updated, errRefresh := refreshCredential(context.Background(), cfg, cred)
	if errRefresh != nil {
		return failEnvelope("refresh_failed", errRefresh.Error(), http.StatusUnauthorized)
	}
	fileName := fmt.Sprintf("%v", req.Metadata["name"])
	if strings.TrimSpace(fileName) == "" || !strings.HasSuffix(strings.ToLower(fileName), ".json") {
		fileName = authFileName(updated)
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             authDataFor(cfg, updated, fileName),
		NextRefreshAfter: nextRefreshAfter(cfg, updated),
	})
}

// refreshCredential refreshes a credential and merges identity fields. Miora
// rotates the refresh token on every use, so the returned credential must be
// persisted immediately and the old refresh token discarded.
func refreshCredential(ctx context.Context, cfg *Config, cred *credential) (*credential, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	client := newAPIClient(cfg, cred)
	fresh, errRefresh := client.refreshTokens(ctx, cred.RefreshToken)
	if errRefresh != nil {
		return nil, errRefresh
	}
	merged := *cred
	merged.AuthToken = fresh.AuthToken
	if fresh.RefreshToken != "" {
		merged.RefreshToken = fresh.RefreshToken
	}
	if !fresh.ExpiresAt.IsZero() {
		merged.ExpiresAt = fresh.ExpiresAt
	}
	merged.UpdatedAt = time.Now()
	// Fill identity/plan once, so panels show a useful label without an extra call.
	if merged.Email == "" || merged.UserID == "" || merged.Plan == "" {
		if profile, errProfile := newAPIClient(cfg, &merged).fetchProfile(ctx); errProfile == nil {
			if merged.Email == "" {
				merged.Email = profile.Email
			}
			if merged.UserID == "" {
				merged.UserID = profile.UserID
			}
			if merged.Plan == "" {
				merged.Plan = profile.Plan
			}
			if merged.Nickname == "" {
				merged.Nickname = profile.Nickname
			}
		}
	}
	return &merged, nil
}

// ---------------------------------------------------------------------------
// interactive login
// ---------------------------------------------------------------------------

// loginSession is one interactive sign-in flow. Miora has no public device-code
// endpoint, so the flow is a loopback page that either imports an existing
// browser session or accepts a pasted token pair.
type loginSession struct {
	State    string
	url      string
	listener net.Listener
	server   *http.Server
	expires  time.Time

	mu         sync.Mutex
	cred       *credential
	candidates []browserCandidate
	errMsg     string
	closed     bool
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

func authLoginStart(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode login start request: %w", errUnmarshal)
		}
	}
	session := &loginSession{
		State:   randomHex(16),
		expires: time.Now().Add(10 * time.Minute),
	}
	cfg := config()
	if errStart := session.start(cfg); errStart != nil {
		return failEnvelope("login_unavailable", "无法在本机启动登录页："+errStart.Error(), 0)
	}
	loginMu.Lock()
	loginSessions[session.State] = session
	loginMu.Unlock()

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerID,
		URL:       session.url,
		State:     session.State,
		ExpiresAt: session.expires,
		Metadata: map[string]any{
			"mode":     "local-page",
			"hint":     "在本机打开的页面里点“从浏览器导入”，或粘贴 authToken/refreshToken。",
			"base_url": cfg.BaseURL,
		},
	})
}

func authLoginPoll(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode login poll request: %w", errUnmarshal)
		}
	}
	loginMu.Lock()
	session := loginSessions[req.State]
	loginMu.Unlock()
	if session == nil || time.Now().After(session.expires) {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录。",
		})
	}
	session.mu.Lock()
	cred := session.cred
	errMsg := session.errMsg
	session.mu.Unlock()
	if cred.valid() {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "已导入 " + cred.displayLabel(),
			Auth:    authDataFor(config(), cred, authFileName(cred)),
		})
	}
	if errMsg != "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: errMsg,
		})
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "等待在登录页完成导入…",
	})
}

func (s *loginSession) start(cfg *Config) error {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		return errListen
	}
	s.listener = listener
	s.url = "http://" + listener.Addr().String() + "/?state=" + s.State
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/submit", s.handleSubmit)
	mux.HandleFunc("/api/browser-scan", s.handleBrowserScan)
	s.server = &http.Server{Handler: s.authorize(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.server.Serve(listener) }()
	time.AfterFunc(time.Until(s.expires), s.close)
	return nil
}

func (s *loginSession) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		s.mu.Lock()
		expired := s.closed || time.Now().After(s.expires)
		s.mu.Unlock()
		if expired {
			writeJSON(w, http.StatusGone, map[string]any{"ok": false, "error": "登录页已过期"})
			return
		}
		state := r.Header.Get("X-Login-State")
		if r.URL.Path == "/" {
			state = r.URL.Query().Get("state")
		}
		if state != s.State {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "登录 state 无效"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *loginSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cred = nil
	s.candidates = nil
	s.mu.Unlock()
	loginMu.Lock()
	delete(loginSessions, s.State)
	loginMu.Unlock()
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.server.Shutdown(ctx)
		cancel()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
}

func stopLoginSessions() {
	loginMu.Lock()
	sessions := make([]*loginSession, 0, len(loginSessions))
	for _, session := range loginSessions {
		sessions = append(sessions, session)
	}
	loginMu.Unlock()
	for _, session := range sessions {
		session.close()
	}
}

func (s *loginSession) succeed(cred *credential) {
	s.mu.Lock()
	s.cred = cred
	s.errMsg = ""
	s.mu.Unlock()
}

func (s *loginSession) fail(message string) {
	s.mu.Lock()
	s.errMsg = message
	s.mu.Unlock()
}

func (s *loginSession) handlePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(loginPageHTML(s.State, providerID)))
}

func (s *loginSession) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cred := s.cred
	errMsg := s.errMsg
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"done":    cred.valid(),
		"email":   safeString(cred, func(c *credential) string { return c.Email }),
		"user_id": safeString(cred, func(c *credential) string { return c.UserID }),
		"error":   errMsg,
	})
}

func safeString(cred *credential, pick func(*credential) string) string {
	if cred == nil {
		return ""
	}
	return pick(cred)
}

func (s *loginSession) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var body struct {
		AuthToken      string `json:"auth_token"`
		RefreshToken   string `json:"refresh_token"`
		Label          string `json:"label"`
		CandidateIndex *int   `json:"candidate_index"`
	}
	if errDecode := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); errDecode != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式错误：" + errDecode.Error()})
		return
	}
	cred := &credential{
		AuthToken:    strings.TrimSpace(body.AuthToken),
		RefreshToken: strings.TrimSpace(body.RefreshToken),
		Label:        strings.TrimSpace(body.Label),
		Source:       "manual",
	}
	if body.CandidateIndex != nil {
		s.mu.Lock()
		index := *body.CandidateIndex
		if index < 0 || index >= len(s.candidates) {
			s.mu.Unlock()
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请重新扫描浏览器"})
			return
		}
		candidate := s.candidates[index]
		s.mu.Unlock()
		cred.AuthToken, cred.RefreshToken = candidate.AuthToken, candidate.RefreshToken
		cred.Source = "browser"
	}
	imported, errImport := importCredential(r.Context(), config(), cred)
	if errImport != nil {
		s.fail(errImport.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": errImport.Error()})
		return
	}
	s.succeed(imported)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": imported.Email, "user_id": imported.UserID})
}

func (s *loginSession) handleBrowserScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if !config().EnableBrowserImport {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "浏览器导入已在插件配置中关闭"})
		return
	}
	candidates, errScan := scanBrowserSessions(config())
	if errScan != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": errScan.Error()})
		return
	}
	s.mu.Lock()
	s.candidates = candidates
	s.mu.Unlock()
	views := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		views = append(views, map[string]any{
			"browser":    candidate.Browser,
			"profile":    candidate.Profile,
			"email":      candidate.Email,
			"user_id":    candidate.UserID,
			"plan":       candidate.Plan,
			"expires_at": candidate.ExpiresAt,
			"valid":      candidate.Valid,
			"message":    candidate.Message,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "candidates": views})
}

// importCredential validates a credential against the upstream and enriches it
// with the account identity.
func importCredential(ctx context.Context, cfg *Config, cred *credential) (*credential, error) {
	if cred == nil || !cred.valid() {
		return nil, fmt.Errorf("需要 authToken（Miora 登录后的 access token）")
	}
	client := newAPIClient(cfg, cred)
	profile, errProfile := client.fetchProfile(ctx)
	if errProfile != nil {
		if authErr := asAuthError(errProfile); authErr != nil {
			return nil, fmt.Errorf("登录态已失效或被拒绝（%s），请重新登录 Miora 后再导入", authErr.Error())
		}
		return nil, fmt.Errorf("验证登录态失败：%w", errProfile)
	}
	cred.UserID = profile.UserID
	cred.Email = profile.Email
	cred.Nickname = profile.Nickname
	cred.Plan = profile.Plan
	if cred.Source == "" {
		cred.Source = "manual"
	}
	cred.UpdatedAt = time.Now()
	if exp, ok := jwtExpiry(cred.AuthToken); ok {
		cred.ExpiresAt = exp
	}
	return cred, nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, errRead := rand.Read(buf); errRead != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func normalizeProvider(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// ---------------------------------------------------------------------------
// background token keeper
// ---------------------------------------------------------------------------

// startBackground (re)starts the token keeper. The plugin ABI has no timer, so
// the plugin owns its own ticker; quiesce stops it before the host unloads us.
func startBackground(cfg *Config) {
	background.mu.Lock()
	defer background.mu.Unlock()
	stopBackgroundLocked()
	if cfg == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	background.cancel, background.done = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		first := time.NewTimer(45 * time.Second)
		defer first.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-first.C:
				refreshDueCredentials(ctx, config())
				prewarmAgentRuntimes(ctx, config())
			case <-ticker.C:
				refreshDueCredentials(ctx, config())
			}
		}
	}()
}

func stopBackground() {
	background.mu.Lock()
	defer background.mu.Unlock()
	stopBackgroundLocked()
}

func stopBackgroundLocked() {
	if background.cancel != nil {
		background.cancel()
		<-background.done
		background.cancel, background.done = nil, nil
	}
}

var background struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// refreshDueCredentials walks the host auth records of this provider and rotates
// the ones close to expiry. Persisting through host.auth.save is mandatory:
// Miora rotates refresh tokens, so an unsaved rotation locks the account out.
// prewarmAgentRuntimes provisions each account's sandbox in the background so the
// first user request does not pay the cold start (measured 20s vs 3s warm).
func prewarmAgentRuntimes(ctx context.Context, cfg *Config) {
	if cfg == nil || !cfg.AgentEnabled {
		return
	}
	raw, errList := hostCall(pluginabi.MethodHostAuthList, map[string]any{})
	if errList != nil {
		return
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"Files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &list); errUnmarshal != nil {
		return
	}
	for _, entry := range list.Files {
		if ctx.Err() != nil {
			return
		}
		if !strings.EqualFold(entry.Provider, providerID) && !strings.EqualFold(entry.Type, providerID) {
			continue
		}
		index := strings.TrimSpace(entry.AuthIndex)
		if index == "" {
			continue
		}
		cred, _, errCred := loadAccountCredential(entry)
		if errCred != nil || cred == nil {
			continue
		}
		warmCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
		runtime, errWarm := ensureAgentRuntime(warmCtx, cfg, cred)
		cancel()
		if errWarm != nil {
			logWarn("agent prewarm failed", map[string]any{"account": index, "error": errWarm.Error()})
			continue
		}
		logInfo("agent prewarmed", map[string]any{"account": index, "runtime": runtime.RuntimeID})
	}
}

func refreshDueCredentials(ctx context.Context, cfg *Config) {
	raw, errList := hostCall(pluginabi.MethodHostAuthList, map[string]any{})
	if errList != nil {
		return
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"Files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &list); errUnmarshal != nil {
		return
	}
	for _, entry := range list.Files {
		if ctx.Err() != nil {
			return
		}
		if !strings.EqualFold(entry.Provider, providerID) && !strings.EqualFold(entry.Type, providerID) {
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		getRaw, errGet := hostCall(pluginabi.MethodHostAuthGet, map[string]any{"auth_index": entry.AuthIndex})
		if errGet != nil {
			continue
		}
		var record struct {
			Name string          `json:"Name"`
			JSON json.RawMessage `json:"JSON"`
		}
		if errUnmarshal := json.Unmarshal(getRaw, &record); errUnmarshal != nil {
			continue
		}
		cred, errCred := credentialFromStorage(record.JSON)
		if errCred != nil || !cred.valid() || cred.RefreshToken == "" {
			continue
		}
		expiry := cred.expiry()
		horizon := time.Now().Add(time.Duration(cfg.AutoRefreshHours) * time.Hour)
		if !expiry.IsZero() && expiry.After(horizon) {
			continue
		}
		updated, errRefresh := refreshCredential(ctx, cfg, cred)
		if errRefresh != nil {
			logWarn("token refresh failed", map[string]any{"account": cred.displayLabel(), "error": errRefresh.Error()})
			continue
		}
		name := record.Name
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			name = authFileName(updated)
		}
		updated.Source = cred.Source
		if _, errSave := hostCall(pluginabi.MethodHostAuthSave, map[string]any{
			"name": name,
			"json": json.RawMessage(updated.authFileJSON()),
		}); errSave != nil {
			logWarn("token refresh could not be persisted", map[string]any{"account": updated.displayLabel(), "error": errSave.Error()})
			continue
		}
		logInfo("token refreshed", map[string]any{"account": updated.displayLabel()})
	}
}
