package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildAgentPromptSingleTurn(t *testing.T) {
	parsed := &parsedChat{
		Messages: []chatMessage{{Role: "user", Content: json.RawMessage(`"帮我写个 slogan"`)}},
		Prompt:   "帮我写个 slogan",
	}
	if got := buildAgentPrompt(parsed); got != "帮我写个 slogan" {
		t.Fatalf("single turn should pass through verbatim, got %q", got)
	}
}

func TestBuildAgentPromptRendersHistory(t *testing.T) {
	parsed := &parsedChat{
		Messages: []chatMessage{
			{Role: "system", Content: json.RawMessage(`"你是文案助手"`)},
			{Role: "user", Content: json.RawMessage(`"第一问"`)},
			{Role: "assistant", Content: json.RawMessage(`"第一答"`)},
			{Role: "user", Content: json.RawMessage(`"第二问"`)},
		},
		Prompt: "第二问",
	}
	got := buildAgentPrompt(parsed)
	for _, needle := range []string{"<系统设定>", "你是文案助手", "用户：第一问", "助手：第一答", "用户：第二问", "请直接回答最后一条用户消息"} {
		if !strings.Contains(got, needle) {
			t.Fatalf("transcript missing %q:\n%s", needle, got)
		}
	}
}

func TestIsAgentModelID(t *testing.T) {
	cfg := defaultConfig()
	cfg.AgentModels = []string{"my-custom-agent"}
	withAgentCatalogue(t, []agentModel{
		{ID: "deepseek-v4.1-flash", Name: "DeepSeek"},
		{ID: "glm-5.3", Name: "GLM"},
	})
	cases := map[string]bool{
		"deepseek-v4.1-flash":  true,
		"glm-5.3":              true,
		"my-custom-agent":      true,
		"custom-local:gpt-5.2": true,
		"hy-image-3.5-f":       false,
		"vidu-q2-pro-video":    false,
		"miora-auto":           false,
		"":                     false,
	}
	for model, want := range cases {
		if got := isAgentModelID(cfg, model); got != want {
			t.Fatalf("isAgentModelID(%q) = %v, want %v", model, got, want)
		}
	}
}

// fakeACP serves the bootstrap SSE protocol: initialize, session/new, then the
// prompt batch with a couple of text deltas and a final result.
func fakeACP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/acp/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer acp-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeFrame := func(payload string) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeFrame(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
		writeFrame(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1","models":{"_meta":{"codebuddy.ai":{"availableModels":[]}}}}}`)
		writeFrame(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"你好"}}}}`)
		writeFrame(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"，世界"}}}}`)
		writeFrame(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"usage_update","inputTokens":123,"outputTokens":7,"model":"deepseek-v4.1-flash"}}}`)
		writeFrame(`{"jsonrpc":"2.0","method":"_codebuddy.ai/artifact","params":{"event":"created","artifact":{"uri":"https://sign.miorausercontent.com/a.png?sign=x","type":"image"}}}`)
		writeFrame(`{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn","usage":{"inputTokens":123,"outputTokens":9,"model":"deepseek-v4.1-flash"}}}`)
	})
	return httptest.NewServer(mux)
}

func TestAgentRuntimeSessionAndPrompt(t *testing.T) {
	server := fakeACP(t)
	defer server.Close()

	cfg := defaultConfig()
	runtime := &agentRuntime{
		ProjectID: "proj-1",
		RuntimeID: "rt-1",
		ACPURL:    server.URL + "/acp",
		ACPToken:  "acp-token",
		Created:   time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	session, errSession := runtime.startAgentSession(ctx, cfg)
	if errSession != nil {
		t.Fatalf("startAgentSession: %v", errSession)
	}
	if session.SessionID != "sess-1" {
		t.Fatalf("unexpected session id: %q", session.SessionID)
	}

	var deltas []string
	usage, stopReason, artifacts, errPrompt := runtime.prompt(ctx, cfg, session, "deepseek-v4.1-flash", "你好", func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if errPrompt != nil {
		t.Fatalf("prompt: %v", errPrompt)
	}
	if strings.Join(deltas, "") != "你好，世界" {
		t.Fatalf("deltas not captured: %v", deltas)
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop reason: %q", stopReason)
	}
	if usage == nil || usage.InputTokens != 123 || usage.OutputTokens != 9 {
		t.Fatalf("usage not parsed from the result frame: %+v", usage)
	}
	if len(artifacts) != 1 || !strings.HasPrefix(artifacts[0], "https://sign.miorausercontent.com/") {
		t.Fatalf("artifact notification not surfaced: %v", artifacts)
	}
}

func TestAgentBootstrapRejectsBadToken(t *testing.T) {
	server := fakeACP(t)
	defer server.Close()
	runtime := &agentRuntime{ACPURL: server.URL + "/acp", ACPToken: "wrong", Created: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, errSession := runtime.startAgentSession(ctx, defaultConfig()); errSession == nil {
		t.Fatal("expected an auth failure for a bad ACP token")
	}
}

func TestUsagePayload(t *testing.T) {
	payload := usagePayload(&agentUsage{InputTokens: 100, OutputTokens: 20})
	if payload["prompt_tokens"] != int64(100) || payload["completion_tokens"] != int64(20) || payload["total_tokens"] != int64(120) {
		t.Fatalf("unexpected usage payload: %+v", payload)
	}
	if empty := usagePayload(nil); empty["total_tokens"] != int64(0) {
		t.Fatalf("nil usage should be zeroed: %+v", empty)
	}
}

// withAgentCatalogue installs a cached agent catalogue for the duration of a test.
func withAgentCatalogue(t *testing.T, models []agentModel) {
	t.Helper()
	agentModelCache.mu.Lock()
	previousModels := agentModelCache.models
	previousFetched := agentModelCache.fetchedAt
	agentModelCache.models = models
	agentModelCache.fetchedAt = time.Now()
	agentModelCache.mu.Unlock()
	t.Cleanup(func() {
		agentModelCache.mu.Lock()
		agentModelCache.models = previousModels
		agentModelCache.fetchedAt = previousFetched
		agentModelCache.mu.Unlock()
	})
}

func TestMediaURLsFromToolOutput(t *testing.T) {
	// Shape captured from a real mcp__image_tools__image_gen completion.
	raw := json.RawMessage(`{"text":"[{\"type\":\"text\",\"text\":\"{\\n  \\\"success\\\": true,\\n  \\\"images\\\": [\\n    {\\n      \\\"index\\\": 1,\\n      \\\"file_key\\\": \\\"file/741996331787679/2026/10/30/x/async-task/y.png\\\",\\n      \\\"access_url\\\": \\\"https://sign.miorausercontent.com/file/741996331787679/2026/10/30/x/async-task/y.png/wm_zhcn?sign=abc&t=1\\\"\\n    }\\n  ]\\n}\"}]","type":"text"}`)
	urls := mediaURLsFromToolOutput(raw)
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "https://sign.miorausercontent.com/") {
		t.Fatalf("artifact url not extracted: %v", urls)
	}
	if keys := toolFileKeys(raw); len(keys) != 1 || keys[0] != "file/741996331787679/2026/10/30/x/async-task/y.png" {
		t.Fatalf("file key not extracted: %v", keys)
	}
	if got := artifactMarkdown(urls); !strings.Contains(got, "![Miora 生成](") {
		t.Fatalf("markdown not rendered: %q", got)
	}
	if extra := mediaURLsFromToolOutput(json.RawMessage(`{"status":"completed"}`)); len(extra) != 0 {
		t.Fatalf("non-media tool output should yield nothing: %v", extra)
	}
}
