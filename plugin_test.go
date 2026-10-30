package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeJWT(t *testing.T, expiresAt time.Time) string {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{"exp": expiresAt.Unix(), "sub": "test"})
	if errMarshal != nil {
		t.Fatalf("marshal claims: %v", errMarshal)
	}
	encode := func(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
	return encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(payload) + "." + encode([]byte("signature-signature-signature"))
}

func TestParseConfig(t *testing.T) {
	raw := []byte("base_url: https://example.test/\nlanguage: en-US\nmodel_prefix: miora\npoll_interval_seconds: 7\ndefault_video_seconds: 9\nexclude_models: [seedance-2.5]\n")
	payload, _ := json.Marshal(map[string]any{"config_yaml": raw})
	cfg, errParse := parseConfig(payload)
	if errParse != nil {
		t.Fatalf("parseConfig: %v", errParse)
	}
	if cfg.BaseURL != "https://example.test" {
		t.Fatalf("base_url not trimmed: %q", cfg.BaseURL)
	}
	if cfg.ModelPrefix != "miora/" {
		t.Fatalf("model_prefix not normalised: %q", cfg.ModelPrefix)
	}
	if cfg.PollIntervalSeconds != 7 || cfg.DefaultVideoSeconds != 9 {
		t.Fatalf("numeric overrides lost: %+v", cfg)
	}
	if cfg.advertisedModelID("gem-3.1") != "miora/gem-3.1" {
		t.Fatalf("advertised id wrong: %q", cfg.advertisedModelID("gem-3.1"))
	}
	if cfg.upstreamModelID("miora/gem-3.1") != "gem-3.1" {
		t.Fatalf("upstream id wrong: %q", cfg.upstreamModelID("miora/gem-3.1"))
	}
	if cfg.modelAllowed("seedance-2.5") {
		t.Fatal("exclude_models not applied")
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, errParse := parseConfig(nil)
	if errParse != nil {
		t.Fatalf("parseConfig(nil): %v", errParse)
	}
	if cfg.BaseURL != "https://miora.design" || cfg.MaxConcurrentTasks != 4 || cfg.TaskTimeoutSeconds != 420 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	cred := &credential{
		AuthToken:    fakeJWT(t, time.Now().Add(96*time.Hour)),
		RefreshToken: "def50200abcdef",
		Email:        "user@example.test",
		UserID:       "741996331787679",
	}
	storage := cred.storageJSON()
	decoded, errDecode := credentialFromStorage(storage)
	if errDecode != nil || decoded == nil {
		t.Fatalf("decode storage: %v", errDecode)
	}
	if decoded.AuthToken != cred.AuthToken || decoded.Email != cred.Email {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
	if hours := int(time.Until(decoded.expiry()).Hours()); hours < 90 || hours > 100 {
		t.Fatalf("jwt expiry not decoded: %d", hours)
	}

	// Browser export shape (localStorage keys) must be accepted too.
	export := []byte(`{"authToken":"` + cred.AuthToken + `","refreshToken":"def50200ff","miora__user_info":{"id":"741996331787679","email":"user@example.test","plan":"free"}}`)
	browser, errBrowser := credentialFromStorage(export)
	if errBrowser != nil || browser == nil || !browser.valid() {
		t.Fatalf("browser export decode failed: %v", errBrowser)
	}
	if browser.Email != "user@example.test" || browser.UserID != "741996331787679" {
		t.Fatalf("browser identity not filled: %+v", browser)
	}

	// A bare auth file with type + auth_token is the third accepted shape.
	fileJSON := []byte(`{"type":"miora-provider","auth_token":"` + cred.AuthToken + `"}`)
	parsed, errFile := credentialFromStorage(fileJSON)
	if errFile != nil || parsed == nil || parsed.AuthToken != cred.AuthToken {
		t.Fatalf("auth file decode failed: %v", errFile)
	}
}

func TestBrowserExtraction(t *testing.T) {
	dir := t.TempDir()
	token := fakeJWT(t, time.Now().Add(48*time.Hour))
	refresh := "def50200" + strings.Repeat("ab", 200)
	blob := "\x00\x01_https://miora.design\x00\x01authToken\x00" + token +
		"\x00\x01_https://miora.design\x00\x01refreshToken\x00" + refresh +
		"\x00\x01_https://miora.design\x00\x01miora__user_info\x00{\"id\":\"741996331787679\",\"email\":\"user@example.test\",\"plan\":\"free\",\"cr\":100}"
	noise := "\x00\x01_https://example.com\x00\x01authToken\x00" + token + "unrelated"
	if errWrite := os.WriteFile(filepath.Join(dir, "000123.ldb"), []byte(blob+noise), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	sessions := extractMioraSessions(dir)
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	session := sessions[0]
	if session.AuthToken != token {
		t.Fatal("auth token not extracted")
	}
	if session.RefreshToken != refresh {
		t.Fatalf("refresh token not extracted (got %q)", session.RefreshToken)
	}
	if session.Email != "user@example.test" || session.UserID != "741996331787679" {
		t.Fatalf("identity not extracted: %+v", session)
	}
}

func TestBrowserExtractionIgnoresForeignOrigins(t *testing.T) {
	dir := t.TempDir()
	token := fakeJWT(t, time.Now().Add(time.Hour))
	if errWrite := os.WriteFile(filepath.Join(dir, "000124.log"), []byte("_https://other.example\x00\x01authToken\x00"+token), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	if sessions := extractMioraSessions(dir); len(sessions) != 0 {
		t.Fatalf("foreign origin leaked a session: %+v", sessions)
	}
}

func TestParseChatPayload(t *testing.T) {
	body := []byte(`{
      "model": "miora/gem-3.1",
      "messages": [
        {"role": "system", "content": "ignored"},
        {"role": "user", "content": [
          {"type": "text", "text": "画一只在霓虹街头的猫 16:9"},
          {"type": "image_url", "image_url": {"url": "https://example.test/cat.png"}}
        ]}
      ],
      "miora": {"resolution": "2K", "n": 2}
    }`)
	parsed, errParse := parseChatPayload(body)
	if errParse != nil {
		t.Fatalf("parseChatPayload: %v", errParse)
	}
	if !strings.Contains(parsed.Prompt, "霓虹街头的猫") {
		t.Fatalf("prompt not extracted: %q", parsed.Prompt)
	}
	if strings.Contains(parsed.Prompt, "ignored") {
		t.Fatalf("system prompt leaked into prompt: %q", parsed.Prompt)
	}
	if len(parsed.Images) != 1 || parsed.Images[0] != "https://example.test/cat.png" {
		t.Fatalf("image parts not extracted: %+v", parsed.Images)
	}
	if parsed.Resolution != "2K" || parsed.Count != 2 {
		t.Fatalf("miora extension not applied: %+v", parsed)
	}
}

func TestParseChatPayloadRequiresPrompt(t *testing.T) {
	if _, errParse := parseChatPayload([]byte(`{"model":"gem-3.1","messages":[]}`)); errParse == nil {
		t.Fatal("expected an error for an empty prompt")
	}
}

func TestPlanExecutionPrefersLastUserTurn(t *testing.T) {
	withCatalog(t, []catalogModel{{
		ModelID:     "hy-image-3.5-f",
		ContentType: mediaImage,
		Label:       "Hy Image3.5",
		Available:   true,
		Capabilities: modelCapabilities{
			AspectRatios:       []string{"1:1", "16:9", "9:16"},
			AllowedResolutions: []string{"1K", "1.5K", "2K"},
			DefaultAspectRatio: "1:1",
			DefaultResolution:  "1.5K",
			ReferenceRange: &struct {
				Max int `json:"max"`
				Min int `json:"min"`
			}{Max: 14, Min: 1},
		},
	}})
	cfg := defaultConfig()
	parsed := &parsedChat{Prompt: "一只猫 16:9", Size: "4096x2304"}
	model, media, errResolve := resolveModel(cfg, parsed)
	if errResolve != nil {
		t.Fatalf("resolveModel: %v", errResolve)
	}
	if media != mediaImage || model != "hy-image-3.5-f" {
		t.Fatalf("auto model should fall back to the first available catalogue model: %q %q", model, media)
	}
	cfg.DefaultImageModel = "hy-image-3.5-f"
	if model, _, errResolve = resolveModel(cfg, &parsedChat{Prompt: "x"}); errResolve != nil || model != "hy-image-3.5-f" {
		t.Fatalf("configured default not honoured: %v %q", errResolve, model)
	}
	model, media, errResolve = resolveModel(cfg, &parsedChat{Model: "hy-image-3.5-f", Prompt: "x"})
	if errResolve != nil || model != "hy-image-3.5-f" || media != mediaImage {
		t.Fatalf("explicit model resolution failed: %v %q %q", errResolve, model, media)
	}

	cred := &credential{AuthToken: "token"}
	generation, errBuild := buildGeneration(context.Background(), cfg, cred, parsed, "hy-image-3.5-f", mediaImage)
	if errBuild != nil {
		t.Fatalf("buildGeneration: %v", errBuild)
	}
	if generation.TaskType != "text_to_image" {
		t.Fatalf("task type: %q", generation.TaskType)
	}
	if generation.AspectRatio != "16:9" {
		t.Fatalf("aspect ratio: %q", generation.AspectRatio)
	}
	if generation.Resolution != "2K" {
		t.Fatalf("resolution: %q", generation.Resolution)
	}
	if generation.Footnote != "0" {
		t.Fatalf("watermark footnote: %q", generation.Footnote)
	}
	body := generation.buildBody()
	extra, _ := body["extraParameters"].(map[string]any)
	if extra["aspectRatio"] != "16:9" || extra["resolution"] != "2K" {
		t.Fatalf("body extras: %+v", extra)
	}
}

func TestBuildGenerationClampsVideoDuration(t *testing.T) {
	withCatalog(t, []catalogModel{{
		ModelID:     "vidu-q2-pro-video",
		ContentType: mediaVideo,
		Available:   true,
		Capabilities: modelCapabilities{
			Resolutions: []string{"480P", "768P"},
			SecondsRange: &struct {
				Max int `json:"max"`
				Min int `json:"min"`
			}{Min: 5, Max: 15},
		},
	}})
	cfg := defaultConfig()
	generation, errBuild := buildGeneration(context.Background(), cfg, &credential{AuthToken: "t"}, &parsedChat{Model: "vidu-q2-pro-video", Prompt: "waves", Duration: 99}, "vidu-q2-pro-video", mediaVideo)
	if errBuild != nil {
		t.Fatalf("buildGeneration: %v", errBuild)
	}
	if generation.Duration != 15 {
		t.Fatalf("duration not clamped: %d", generation.Duration)
	}
	if generation.TaskType != "text_to_video" {
		t.Fatalf("task type: %q", generation.TaskType)
	}
}

func TestMediaKindHeuristics(t *testing.T) {
	cfg := defaultConfig()
	cases := map[string]string{
		"vidu-q2-pro-video":   mediaVideo,
		"kling-o1-video":      mediaVideo,
		"tripo-3d-3.1":        mediaThreeD,
		"hy-image-3.5-f":      mediaImage,
		"gpt-image-2.5-flare": mediaImage,
	}
	for model, want := range cases {
		if got := mediaKindForModel(cfg, model); got != want {
			t.Fatalf("mediaKindForModel(%s) = %s, want %s", model, got, want)
		}
	}
}

func TestRenderResultMarkdown(t *testing.T) {
	cfg := defaultConfig()
	result := &generationResult{
		TaskID: "task-1", Media: mediaImage, DisplayName: "Hy Image3.5",
		Files:  []mediaFile{{FileKey: "file/a.png", URL: "https://cdn.test/a.png?sig=1", Type: "image"}},
		Credit: 6.7, ElapsedMS: 13200, Size: "1024x1024", AspectRatio: "1:1", Resolution: "1K",
	}
	text := renderResult(cfg, result, "一只猫")
	if !strings.Contains(text, "![一只猫](https://cdn.test/a.png?sig=1)") {
		t.Fatalf("markdown image missing: %s", text)
	}
	if !strings.Contains(text, "6.70 credits") {
		t.Fatalf("credit footer missing: %s", text)
	}
	if !strings.Contains(text, "Hy Image3.5") {
		t.Fatalf("model name missing: %s", text)
	}

	cfg.ResponseFooter = false
	if strings.Contains(renderResult(cfg, result, "x"), "credits") {
		t.Fatal("footer should be suppressed when response_footer=false")
	}
}

func TestManagementRouteSuffix(t *testing.T) {
	cases := map[string]string{
		"/v0/management/miora-provider/accounts":       "/accounts",
		"/v0/management/miora-provider/models/refresh": "/models/refresh",
		"/v0/resource/plugins/miora-provider/panel":    "/panel",
		"/v0/resource/plugins/miora-provider/status":   "/status",
		"/v0/management/miora-provider":                "/",
		"/v0/management/miora-provider/accounts/scan/": "/accounts/scan",
	}
	for path, want := range cases {
		if got := managementRouteSuffix(path); got != want {
			t.Fatalf("managementRouteSuffix(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestStatusPayloadHasNoSecrets(t *testing.T) {
	payload := statusPayload()
	raw, _ := json.Marshal(payload)
	text := string(raw)
	for _, forbidden := range []string{"auth_token", "refresh_token", "eyJ"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("status payload leaks %q: %s", forbidden, text)
		}
	}
	if payload["provider"] != providerID {
		t.Fatalf("provider mismatch: %v", payload["provider"])
	}
}

func TestPanelHTML(t *testing.T) {
	page := panelHTML()
	for _, needle := range []string{
		"<title>Miora", "cli-proxy-auth", `var P = "miora-provider"`, "/v0/management/",
		"btnScan", "btnGenerate", "modelSelect", "gallery", "managementKey",
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("panel is missing %q", needle)
		}
	}
	if strings.Contains(page, "__PROVIDER__") {
		t.Fatal("panel template placeholder was not replaced")
	}
	if strings.Contains(page, "http://") && strings.Contains(page, "cdn") {
		t.Fatal("panel must not load third-party scripts")
	}
	if strings.Contains(page, "<script src") {
		t.Fatal("panel must inline its scripts")
	}
}

func TestLoginPageHTML(t *testing.T) {
	page := loginPageHTML("state-123", providerID)
	if !strings.Contains(page, "state-123") || !strings.Contains(page, providerID) {
		t.Fatal("login page placeholders not replaced")
	}
	if strings.Contains(page, "__STATE__") {
		t.Fatal("login page state placeholder leaked")
	}
}

// fakeUpstream simulates the Miora media API: submit, poll, sign.
func fakeUpstream(t *testing.T, statuses []string) *httptest.Server {
	t.Helper()
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ai/media-generate/image", func(w http.ResponseWriter, r *http.Request) {
		writeUpstream(t, w, map[string]any{"code": 0, "data": map[string]any{"taskId": "task-1", "status": "pending"}})
	})
	mux.HandleFunc("/api/ai/async-task/batch-query", func(w http.ResponseWriter, r *http.Request) {
		status := "processing"
		if calls < len(statuses) {
			status = statuses[calls]
		}
		calls++
		writeUpstream(t, w, map[string]any{"code": 0, "data": map[string]any{
			"task-1": map[string]any{
				"taskId":        "task-1",
				"status":        status,
				"progress":      100,
				"resultFileKey": "file/u/1/a.png",
				"assets":        []map[string]any{{"fileKey": "file/u/1/a.png", "fileType": "image"}},
				"metadata":      map[string]any{"size": "1024x1024", "resolution": "1K", "aspectRatio": "1:1", "usage": map[string]any{"credit": 6.7}},
			},
		}})
	})
	mux.HandleFunc("/api/ai/media-generate/progress", func(w http.ResponseWriter, r *http.Request) {
		writeUpstream(t, w, map[string]any{"code": 0, "data": map[string]any{"tasks": map[string]any{}}})
	})
	mux.HandleFunc("/api/ai/cos/cdn-sign-url", func(w http.ResponseWriter, r *http.Request) {
		writeUpstream(t, w, map[string]any{"code": 0, "data": map[string]any{
			"signedUrls": "https://cdn.test/a.png?sig=abc",
			"filePaths":  []string{"file/u/1/a.png"},
		}})
	})
	return httptest.NewServer(mux)
}

func writeUpstream(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if errEncode := json.NewEncoder(w).Encode(payload); errEncode != nil {
		t.Fatalf("encode upstream response: %v", errEncode)
	}
}

func TestRunGenerationPipeline(t *testing.T) {
	server := fakeUpstream(t, []string{"processing", "completed"})
	defer server.Close()

	cfg := defaultConfig()
	cfg.BaseURL = server.URL
	cfg.PollIntervalSeconds = 1
	cfg.TaskTimeoutSeconds = 20
	cfg.ResponseFooter = true

	cred := &credential{AuthToken: "token", Email: "user@example.test"}
	generation := generateRequest{Model: "hy-image-3.5-f", Media: mediaImage, Prompt: "a cat", TaskType: "text_to_image"}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	result, errRun := runGeneration(ctx, cfg, cred, generation, nil)
	if errRun != nil {
		t.Fatalf("runGeneration: %v", errRun)
	}
	if len(result.Files) != 1 || result.Files[0].URL != "https://cdn.test/a.png?sig=abc" {
		t.Fatalf("unexpected files: %+v", result.Files)
	}
	if result.Credit != 6.7 {
		t.Fatalf("credit not parsed: %v", result.Credit)
	}
	if result.Resolution != "1K" || result.AspectRatio != "1:1" {
		t.Fatalf("metadata not parsed: %+v", result)
	}
	tasks := listTasks(10)
	if len(tasks) == 0 || tasks[0].Status != "completed" {
		t.Fatalf("task not recorded: %+v", tasks)
	}
}

func TestRunGenerationReportsUpstreamFailure(t *testing.T) {
	server := fakeUpstream(t, []string{"failed"})
	defer server.Close()
	cfg := defaultConfig()
	cfg.BaseURL = server.URL
	cfg.PollIntervalSeconds = 1
	cfg.TaskTimeoutSeconds = 15
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, errRun := runGeneration(ctx, cfg, &credential{AuthToken: "token"}, generateRequest{
		Model: "hy-image-3.5-f", Media: mediaImage, Prompt: "x", TaskType: "text_to_image",
	}, nil)
	if errRun == nil {
		t.Fatal("expected a failure for a failed upstream task")
	}
	if !strings.Contains(errRun.Error(), "失败") {
		t.Fatalf("unexpected error text: %v", errRun)
	}
}

func TestExecutionFailureMapsStatus(t *testing.T) {
	body, errBuild := executionFailure(&upstreamError{Status: http.StatusUnauthorized, Message: "401"})
	if errBuild != nil {
		t.Fatalf("executionFailure: %v", errBuild)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK || env.Error.Code != "credential_invalid" || env.Error.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("unexpected envelope: %s", string(body))
	}
	if !strings.Contains(string(body), "重新导入") {
		t.Fatalf("error should tell the operator how to recover: %s", string(body))
	}
}

func TestAuthParseRejectsForeignProvider(t *testing.T) {
	request, _ := json.Marshal(map[string]any{"Provider": "other-provider", "RawJSON": base64.StdEncoding.EncodeToString([]byte(`{"auth_token":"x"}`))})
	raw, errParse := authParse(request)
	if errParse != nil {
		t.Fatalf("authParse: %v", errParse)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool `json:"Handled"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if !env.OK || env.Result.Handled {
		t.Fatalf("foreign provider must be unhandled: %s", string(raw))
	}
}

func TestAuthParseAcceptsOwnFile(t *testing.T) {
	token := fakeJWT(t, time.Now().Add(48*time.Hour))
	fileJSON := []byte(`{"type":"miora-provider","auth_token":"` + token + `","email":"user@example.test"}`)
	request, _ := json.Marshal(map[string]any{
		"Provider": providerID,
		"FileName": "miora-user.json",
		"RawJSON":  base64.StdEncoding.EncodeToString(fileJSON),
	})
	raw, errParse := authParse(request)
	if errParse != nil {
		t.Fatalf("authParse: %v", errParse)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool `json:"Handled"`
			Auth    struct {
				Provider    string `json:"Provider"`
				FileName    string `json:"FileName"`
				StorageJSON []byte `json:"StorageJSON"`
			} `json:"Auth"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if !env.Result.Handled || env.Result.Auth.Provider != providerID {
		t.Fatalf("own auth file must be handled: %s", string(raw))
	}
	decoded, errDecode := credentialFromStorage(env.Result.Auth.StorageJSON)
	if errDecode != nil || decoded == nil || decoded.AuthToken != token {
		t.Fatalf("storage blob unusable: %v", errDecode)
	}
}

func TestLoginSessionLoopbackFlow(t *testing.T) {
	// A fake upstream so the login page can validate a pasted token.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ai/user/profile":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"userId":"741996331787679","email":"user@example.test","nickname":"Tester"}}`))
		case "/api/ai/quota/credit":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"total_amount":"100","used_amount":"6.7"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := defaultConfig()
	cfg.BaseURL = server.URL
	previous := currentConfig.Load()
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })

	raw, errStart := authLoginStart(nil)
	if errStart != nil {
		t.Fatalf("authLoginStart: %v", errStart)
	}
	var started struct {
		OK     bool `json:"ok"`
		Result struct {
			URL   string `json:"URL"`
			State string `json:"State"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &started); errUnmarshal != nil {
		t.Fatalf("decode login start: %v", errUnmarshal)
	}
	if !started.OK || started.Result.URL == "" || started.Result.State == "" {
		t.Fatalf("login start response incomplete: %s", string(raw))
	}
	t.Cleanup(stopLoginSessions)
	if !strings.HasPrefix(started.Result.URL, "http://127.0.0.1:") {
		t.Fatalf("login URL should be loopback: %s", started.Result.URL)
	}

	page, errPage := http.Get(started.Result.URL)
	if errPage != nil {
		t.Fatalf("fetch login page: %v", errPage)
	}
	body, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body), "连接 Miora") {
		t.Fatalf("login page looks wrong: %d", page.StatusCode)
	}

	// Pending before any submission.
	pollRaw, errPoll := authLoginPoll([]byte(`{"State":"` + started.Result.State + `"}`))
	if errPoll != nil {
		t.Fatalf("authLoginPoll: %v", errPoll)
	}
	var pending struct {
		Result struct {
			Status string `json:"Status"`
		} `json:"result"`
	}
	_ = json.Unmarshal(pollRaw, &pending)
	if pending.Result.Status != "pending" {
		t.Fatalf("expected pending, got %s", pending.Result.Status)
	}

	// Submit the token through the loopback page, exactly like a browser would.
	token := fakeJWT(t, time.Now().Add(48*time.Hour))
	submitBody, _ := json.Marshal(map[string]string{"auth_token": token})
	submitURL := strings.TrimSuffix(started.Result.URL, "/?state="+started.Result.State) + "/api/submit"
	submitRequest, _ := http.NewRequest(http.MethodPost, submitURL, bytes.NewReader(submitBody))
	submitRequest.Header.Set("Content-Type", "application/json")
	submitRequest.Header.Set("X-Login-State", started.Result.State)
	submit, errSubmit := http.DefaultClient.Do(submitRequest)
	if errSubmit != nil {
		t.Fatalf("submit: %v", errSubmit)
	}
	submitJSON, _ := io.ReadAll(submit.Body)
	_ = submit.Body.Close()
	if submit.StatusCode != http.StatusOK {
		t.Fatalf("submit failed: %d %s", submit.StatusCode, string(submitJSON))
	}

	pollRaw, errPoll = authLoginPoll([]byte(`{"State":"` + started.Result.State + `"}`))
	if errPoll != nil {
		t.Fatalf("authLoginPoll after submit: %v", errPoll)
	}
	var done struct {
		Result struct {
			Status string `json:"Status"`
			Auth   struct {
				Label       string `json:"Label"`
				StorageJSON []byte `json:"StorageJSON"`
			} `json:"Auth"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(pollRaw, &done); errUnmarshal != nil {
		t.Fatalf("decode poll: %v", errUnmarshal)
	}
	if done.Result.Status != "success" {
		t.Fatalf("expected success, got %s (%s)", done.Result.Status, string(pollRaw))
	}
	cred, errCred := credentialFromStorage(done.Result.Auth.StorageJSON)
	if errCred != nil || cred == nil || cred.AuthToken != token {
		t.Fatalf("login flow did not return the credential: %v", errCred)
	}
	if cred.Email != "user@example.test" {
		t.Fatalf("identity not enriched: %+v", cred)
	}
}

// withCatalog installs a catalogue snapshot for the duration of a test.
func withCatalog(t *testing.T, models []catalogModel) {
	t.Helper()
	catalogCache.mu.Lock()
	previous := catalogCache.snapshot
	catalogCache.snapshot = catalogSnapshot{Models: models, FetchedAt: time.Now(), Tier: "free"}
	catalogCache.mu.Unlock()
	t.Cleanup(func() {
		catalogCache.mu.Lock()
		catalogCache.snapshot = previous
		catalogCache.mu.Unlock()
	})
}
