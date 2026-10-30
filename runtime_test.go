package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func useTestConfig(t *testing.T, cfg *Config) {
	t.Helper()
	previous := currentConfig.Swap(cfg)
	t.Cleanup(func() { currentConfig.Store(previous) })
}

func TestDirectTransportReusesConnections(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	cfg := defaultConfig()
	cfg.ProxyURL = "direct://"
	for range 3 {
		if _, err := doUpstreamDirect(context.Background(), cfg, http.MethodGet, server.URL, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("three sequential requests opened %d connections", got)
	}
}

func TestHostFailureDoesNotReplayBillableRequest(t *testing.T) {
	var directCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
	}))
	defer server.Close()
	hostCfg := defaultConfig()
	hostCfg.UseHostHTTP = true
	for _, hostErr := range []error{context.DeadlineExceeded, errors.New("callback failed after submission")} {
		restore := setHostCall(func(string, any) (json.RawMessage, error) { return nil, hostErr })
		_, err := doUpstream(context.Background(), hostCfg, http.MethodPost, server.URL, nil, []byte(`{}`))
		restore()
		if !errors.Is(err, hostErr) {
			t.Fatalf("host error lost: %v", err)
		}
	}
	if directCalls.Load() != 0 {
		t.Fatal("host failure replayed the POST directly")
	}
	for _, status := range []int{401, 403, 429, 502, 504} {
		if gatewayRejected(&httpResult{StatusCode: status, Headers: http.Header{"Content-Type": {"text/html"}}, Body: []byte("<html>error</html>")}) {
			t.Fatalf("HTTP %d must not trigger a direct replay", status)
		}
	}
}

func TestCancelledRequestDoesNotCallHost(t *testing.T) {
	var calls atomic.Int32
	restore := setHostCall(func(string, any) (json.RawMessage, error) {
		calls.Add(1)
		return nil, errors.New("should not be called")
	})
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := doUpstream(ctx, defaultConfig(), http.MethodPost, "https://example.test/", nil, nil)
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("cancelled submission reached the host: calls=%d error=%v", calls.Load(), err)
	}
}

func TestPlanUploadsReferenceOnceForMultipleResults(t *testing.T) {
	withCatalog(t, nil)
	var uploads, stsCalls atomic.Int32
	restore := setHostCall(func(method string, request any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
		req := request.(pluginapi.HTTPRequest)
		body := []byte(`{}`)
		if strings.HasSuffix(req.URL, pathSTSTokens) {
			stsCalls.Add(1)
			body = []byte(`{"code":0,"data":{"secret_id":"test-id","secret_key":"test-secret","bucket":"test-bucket","region":"test-region","prefix":"file/test"}}`)
		} else if req.Method == http.MethodPut && strings.Contains(req.URL, "myqcloud.com") {
			uploads.Add(1)
			if req.Headers.Get("Authorization") == "" || string(req.Body) != "image" {
				return nil, errors.New("upload lost authorization or image")
			}
		} else {
			return nil, fmt.Errorf("unexpected upstream request %s", req.URL)
		}
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body})
	})
	defer restore()
	cfg := defaultConfig()
	cfg.UseHostHTTP = true
	cfg.UploadReferenceImages = true
	plan, err := planExecution(context.Background(), cfg, &executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		StorageJSON: []byte(`{"auth_token":"test-token"}`),
		Payload:     []byte(`{"model":"image-model","prompt":"a cat","n":4,"images":["data:image/png;base64,aW1hZ2U="]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.generations) != 4 || uploads.Load() != 1 || stsCalls.Load() != 1 {
		t.Fatalf("generations=%d uploads=%d STS=%d", len(plan.generations), uploads.Load(), stsCalls.Load())
	}
	for _, generation := range plan.generations {
		if len(generation.ReferenceKeys) != 1 || generation.TaskType != "image_to_image" {
			t.Fatalf("missing uploaded reference: %+v", generation)
		}
	}
}

func TestGenerationPollingStopsOnClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var polls, progress atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case pathGeneratePrefix + "image":
					writeUpstream(t, w, map[string]any{"code": 0, "data": map[string]any{"taskId": fmt.Sprint(status)}})
				case pathImageQuery:
					polls.Add(1)
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"code":1,"message":"rejected"}`)
				case pathMediaProgress:
					progress.Add(1)
				}
			}))
			defer server.Close()
			cfg := defaultConfig()
			cfg.BaseURL, cfg.UseHostHTTP = server.URL, false
			_, err := runGeneration(context.Background(), cfg, &credential{AuthToken: "test"}, generateRequest{Media: mediaImage}, nil)
			var upstream *upstreamError
			if !errors.As(err, &upstream) || upstream.Status != status {
				t.Fatalf("expected upstream %d, got %v", status, err)
			}
			if polls.Load() != 1 || progress.Load() != 0 {
				t.Fatalf("unnecessary polling: batch=%d progress=%d", polls.Load(), progress.Load())
			}
		})
	}
}

func TestGenerationDeadlineInterruptsPollingAndSigning(t *testing.T) {
	for _, phase := range []string{"polling", "signing"} {
		t.Run(phase, func(t *testing.T) {
			taskID := "timeout-" + phase
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == pathImageQuery && phase == "polling" || r.URL.Path == pathSignURLs {
					_, _ = io.Copy(io.Discard, r.Body)
					<-r.Context().Done()
					return
				}
				if r.URL.Path == pathImageQuery {
					writeUpstream(t, w, map[string]any{"code": 0, "data": map[string]any{taskID: map[string]any{"status": "completed", "resultFileKey": "file/test.png"}}})
					return
				}
				writeUpstream(t, w, map[string]any{"code": 0, "data": map[string]any{"taskId": taskID}})
			}))
			defer server.Close()
			cfg := defaultConfig()
			cfg.BaseURL, cfg.UseHostHTTP, cfg.TaskTimeoutSeconds = server.URL, false, 1
			started := time.Now()
			_, err := runGeneration(context.Background(), cfg, &credential{AuthToken: "test"}, generateRequest{Media: mediaImage}, nil)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
				t.Fatalf("task deadline did not interrupt HTTP request: %v", err)
			}
			for _, task := range listTasks(0) {
				if task.ID == taskID {
					if task.Status != "timeout" || task.FinishedAt == "" {
						t.Fatalf("timeout not recorded: %+v", task)
					}
					return
				}
			}
			t.Fatal("missing timeout task")
		})
	}
}

func TestImagePollingSkipsRedundantProgressEndpoint(t *testing.T) {
	var progress atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathMediaProgress {
			progress.Add(1)
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"task-1":{"status":"pending"}}}`)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.BaseURL, cfg.UseHostHTTP = server.URL, false
	states, err := newAPIClient(cfg, &credential{AuthToken: "test"}).pollTasks(context.Background(), mediaImage, []string{"task-1"})
	if err != nil || len(states) != 1 || progress.Load() != 0 {
		t.Fatalf("states=%v error=%v redundant calls=%d", states, err, progress.Load())
	}
}

func TestCatalogSharesConcurrentFailuresAndCachesBriefly(t *testing.T) {
	withCatalog(t, nil)
	invalidateCatalog()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"code":1,"message":"temporarily unavailable"}`)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.BaseURL, cfg.UseHostHTTP = server.URL, false
	useTestConfig(t, cfg)
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() { _ = loadCatalog(context.Background()) })
	}
	workers.Wait()
	_ = loadCatalog(context.Background())
	if requests.Load() != 1 {
		t.Fatalf("catalog outage triggered %d requests", requests.Load())
	}
	if peekCatalog().Err == "" {
		t.Fatal("catalog failure not exposed")
	}
}

func TestCatalogInvalidationDiscardsInflightResult(t *testing.T) {
	withCatalog(t, nil)
	invalidateCatalog()
	entered, resume := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			<-resume
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"data":[{"modelId":"image-model","available":true}]}}`)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.BaseURL, cfg.UseHostHTTP = server.URL, false
	useTestConfig(t, cfg)
	done := make(chan struct{})
	go func() { _ = loadCatalog(context.Background()); close(done) }()
	<-entered
	invalidateCatalog()
	close(resume)
	<-done
	if !peekCatalog().FetchedAt.IsZero() {
		t.Fatal("invalidated request repopulated the cache")
	}
	if snapshot := loadCatalog(context.Background()); len(snapshot.Models) != 1 || requests.Load() != 2 {
		t.Fatalf("catalog was not refreshed: models=%d requests=%d", len(snapshot.Models), requests.Load())
	}
}

func TestPermitLimitChangesShareActiveCount(t *testing.T) {
	limiter := generationLimiter{changed: make(chan struct{})}
	first, _ := limiter.acquire(2)
	second, _ := limiter.acquire(2)
	defer second()
	if release, ok := limiter.acquire(1); ok {
		release()
		t.Fatal("lowered limit created an independent pool")
	}
	first()
	if release, ok := limiter.acquire(1); ok {
		release()
		t.Fatal("active task must finish before admitting another")
	}
}

func TestLoginRequiresStateAndKeepsBrowserTokensServerSide(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"userId":"test-user","email":"user@example.test"}}`)
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.BaseURL, cfg.UseHostHTTP = server.URL, false
	useTestConfig(t, cfg)
	s := &loginSession{State: "test-state", expires: time.Now().Add(time.Minute), candidates: []browserCandidate{{AuthToken: "browser-token", RefreshToken: "test-refresh"}}}
	if err := s.start(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	base := "http://" + s.listener.Addr().String()
	for _, path := range []string{"/", "/?state=wrong", "/api/state", "/api/browser-scan", "/api/submit"} {
		response, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden || strings.Contains(string(body), s.State) {
			t.Fatalf("unauthorized %s returned %d: %s", path, response.StatusCode, body)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/submit", strings.NewReader(`{"candidate_index":0}`))
	req.Header.Set("X-Login-State", s.State)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "browser-token") || s.cred.AuthToken != "browser-token" {
		t.Fatalf("candidate import failed or exposed token: %s", body)
	}
}

func TestExpiredLoginDoesNotReturnCredential(t *testing.T) {
	s := &loginSession{State: "expired-test", expires: time.Now().Add(-time.Second), cred: &credential{AuthToken: "test-token"}}
	loginMu.Lock()
	loginSessions[s.State] = s
	loginMu.Unlock()
	defer s.close()
	raw, err := authLoginPoll([]byte(`{"State":"expired-test"}`))
	var response struct {
		Result pluginapi.AuthLoginPollResponse `json:"result"`
	}
	decodeErr := json.Unmarshal(raw, &response)
	if err != nil || decodeErr != nil || response.Result.Status != pluginapi.AuthLoginStatusError || len(response.Result.Auth.StorageJSON) != 0 {
		t.Fatalf("expired login returned auth: %s (%v)", raw, err)
	}
}

func TestCompletionStreamKeepsIdentity(t *testing.T) {
	stream := completionStream{id: "chatcmpl-test", created: 123, model: "test-model"}
	for _, raw := range [][]byte{stream.chunkRole(), stream.chunkContent("hello"), stream.chunkFinal("stop")} {
		var chunk struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil || chunk.ID != stream.id || chunk.Created != stream.created {
			t.Fatalf("stream identity changed: %s", raw)
		}
	}
}

func TestBackgroundReconfigureAndStop(t *testing.T) {
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 10 {
				startBackground(defaultConfig())
				stopBackground()
			}
		})
	}
	workers.Wait()
	stopBackground()
	background.mu.Lock()
	defer background.mu.Unlock()
	if background.cancel != nil || background.done != nil {
		t.Fatal("background worker survived shutdown")
	}
}

func TestOfflineAutoModelRespectsFilters(t *testing.T) {
	withCatalog(t, nil)
	cfg := defaultConfig()
	cfg.Models = []ModelConfig{
		{ID: "blocked", Upstream: "blocked", Media: mediaImage},
		{ID: "offline", Upstream: "offline", Media: mediaImage},
	}
	cfg.ExcludeModels = []string{"blocked"}
	model, _, err := resolveModel(cfg, &parsedChat{Model: autoModelID})
	if err != nil || model != "offline" {
		t.Fatalf("offline auto model=%q, error=%v", model, err)
	}
	if models := catalogueModels(cfg); len(models) != 1 || models[0].ModelID != "offline" {
		t.Fatalf("model filter ignored: %+v", models)
	}
}

func TestTraceErrorDoesNotExposeSignedURL(t *testing.T) {
	target := "https://cdn.example.test/image.png?signature=test-private-value"
	recordUpstreamTrace("direct", http.MethodGet, target, 0, &url.Error{Op: "Get", URL: target, Err: context.DeadlineExceeded})
	entry := listUpstreamTraces()[0]
	if strings.Contains(entry.Path+entry.Error, "signature") || entry.Error == "" {
		t.Fatalf("trace exposed a signed URL: %+v", entry)
	}
}
