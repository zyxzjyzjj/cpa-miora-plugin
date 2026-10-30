package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The agent executor is a separate branch from media generation: same
// /v1/chat/completions surface, completely different upstream (ACP over HTTP).
//
// Unlike media (one prompt -> one artifact), an agent turn is conversational, so
// the whole client transcript is rendered into a single ACP prompt. Each request
// opens a fresh ACP session on the account's shared sandbox, which keeps clients
// from leaking context into each other.

// runAgentTurn sends one turn and returns the assistant text plus token usage. When
// onDelta is set the text is also streamed incrementally.
func runAgentTurn(ctx context.Context, cfg *Config, plan *executionPlan, onDelta func(string) error) (string, *agentUsage, error) {
	if plan == nil || plan.parsed == nil {
		return "", nil, fmt.Errorf("请求为空")
	}
	// Serialise per account: the sandbox serves one ACP session at a time.
	lock := agentTurnLock(agentAccountKey(cfg, plan.cred))
	lock.Lock()
	defer lock.Unlock()

	runtime, session, errSession := openAgentSession(ctx, cfg, plan.cred)
	if errSession != nil {
		return "", nil, errSession
	}

	modelID := strings.TrimSpace(plan.model)
	if modelID == "" {
		modelID = cfg.AgentDefaultModel
	}
	prompt := buildAgentPrompt(plan.parsed)

	var builder strings.Builder
	streamDeltas := func(delta string) error {
		builder.WriteString(delta)
		if onDelta != nil {
			return onDelta(delta)
		}
		return nil
	}
	usage, _, artifacts, errPrompt := runtime.prompt(ctx, cfg, session, modelID, prompt, streamDeltas)
	if errPrompt != nil && strings.TrimSpace(runtime.SessionID) == "" {
		// The sandbox reclaimed the session; open a fresh one and retry once.
		if retrySession, errRetry := runtime.startAgentSession(ctx, cfg); errRetry == nil {
			builder.Reset()
			usage, _, artifacts, errPrompt = runtime.prompt(ctx, cfg, retrySession, modelID, prompt, streamDeltas)
		}
	}
	if errPrompt != nil {
		if authErr := asAuthError(errPrompt); authErr != nil {
			return "", usage, authErr
		}
		return builder.String(), usage, errPrompt
	}
	text := builder.String()
	logInfo("agent turn artifacts: "+strconv.Itoa(len(artifacts)), map[string]any{"model": plan.model})
	// Design tools (image / video / 3D) return artifacts out of band; surface them
	// so a chat client actually sees what the agent produced.
	if markdown := artifactMarkdown(artifacts); markdown != "" {
		if onDelta != nil {
			_ = onDelta(markdown)
		}
		text += markdown
	}
	if strings.TrimSpace(text) == "" {
		return "", usage, fmt.Errorf("Miora Agent 没有返回文本内容（stopReason 可能是工具调用）")
	}
	return text, usage, nil
}

// artifactMarkdown renders tool artifacts as Markdown so chat clients can show them.
func artifactMarkdown(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("\n\n")
	for index, url := range urls {
		if index > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString("![Miora 生成](" + url + ")\n")
		builder.WriteString("> " + url)
	}
	return builder.String()
}

// openAgentSession provisions/reuses a sandbox and opens one session. A cold or
// wedged sandbox surfaces as a timeout, so the runtime is dropped and retried once
// with a freshly provisioned one.
func openAgentSession(ctx context.Context, cfg *Config, cred *credential) (*agentRuntime, *agentSession, error) {
	runtime, errRuntime := ensureAgentRuntime(ctx, cfg, cred)
	if errRuntime == nil {
		if session, errSession := runtime.startAgentSession(ctx, cfg); errSession == nil {
			return runtime, session, nil
		} else {
			logWarn("agent session failed, provisioning a fresh runtime", map[string]any{"error": errSession.Error()})
			// A runtime that cannot hand out a session is dead for good: drop it from
			// both the memory cache and the state file, otherwise the next attempt
			// restores the same wedged sandbox.
			key := agentAccountKey(cfg, cred)
			forgetState(key)
			if client := newAPIClient(cfg, cred); client != nil && runtime.RuntimeID != "" {
				_ = deleteRuntime(ctx, client, runtime.RuntimeID)
			}
		}
	}
	forgetAgentRuntime(cfg, cred)
	runtime, errRuntime = ensureAgentRuntime(ctx, cfg, cred)
	if errRuntime != nil {
		return nil, nil, errRuntime
	}
	session, errSession := runtime.startAgentSession(ctx, cfg)
	if errSession != nil {
		forgetAgentRuntime(cfg, cred)
		return nil, nil, errSession
	}
	return runtime, session, nil
}

// executorAgentStream serves one agent turn over the host stream pump.
func executorAgentStream(ctx context.Context, cfg *Config, req *executorRequest, plan *executionPlan) ([]byte, error) {
	streamID := strings.TrimSpace(req.StreamID)
	stream := completionStream{id: completionID(), model: req.Model, created: time.Now().Unix()}
	if streamID == "" {
		// No host stream: buffer the whole turn and return SSE-shaped chunks inline.
		text, usage, errAgent := runAgentTurn(ctx, cfg, plan, nil)
		if errAgent != nil {
			return executionFailure(errAgent)
		}
		chunks := make([]pluginapi.ExecutorStreamChunk, 0, 3)
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: stream.chunkRole()})
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: stream.chunkContent(text)})
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: stream.chunkFinalUsage("stop", usage)})
		return okEnvelope(map[string]any{"headers": streamHeaders(), "chunks": chunks})
	}

	if errEmit := hostStreamEmit(streamID, stream.chunkRole()); errEmit != nil {
		closeStream(streamID)
		return nil, fmt.Errorf("emit role chunk: %w", errEmit)
	}

	// The host only drains the stream bridge after executor.execute_stream returns
	// (see pluginhost/rpc_client_stream.go: async plugins answer first, then keep
	// emitting). A three-minute sandbox boot must therefore happen in the
	// background, otherwise the client sits in silence until the turn is over.
	_ = hostStreamEmit(streamID, stream.chunkContent("Miora Agent 已接单，正在准备沙箱…\n"))

	turnCtx, cancel := context.WithTimeout(context.Background(), agentTurnTimeout(cfg))
	go func() {
		defer cancel()
		defer closeStream(streamID)

		emitter := newDeltaEmitter(streamID, stream)
		firstDelta := make(chan struct{})
		var once sync.Once
		markFirst := func() { once.Do(func() { close(firstDelta) }) }
		go agentWaitHeartbeat(turnCtx, streamID, stream, firstDelta)

		text, usage, errAgent := runAgentTurn(turnCtx, cfg, plan, func(delta string) error {
			markFirst()
			emitter.push(delta)
			return nil
		})
		markFirst()
		if errAgent != nil {
			emitter.finish(nil)
			emitStreamError(streamID, errAgent.Error())
			logWarn("agent stream failed", map[string]any{"model": plan.model, "error": errAgent.Error()})
			return
		}
		emitter.finish(stream.chunkFinalUsage("stop", usage))
		logInfo("agent stream served", map[string]any{"model": plan.model, "chars": len(text)})
	}()
	return okEnvelope(map[string]any{"headers": streamHeaders()})
}

// agentTurnTimeout bounds one detached agent turn so a wedged sandbox releases the
// per-account lock instead of blocking every later request.
func agentTurnTimeout(cfg *Config) time.Duration {
	if cfg != nil && cfg.AgentTimeoutSeconds > 0 {
		return time.Duration(cfg.AgentTimeoutSeconds) * time.Second
	}
	return 300 * time.Second
}

// agentWaitHeartbeat keeps the client informed while the sandbox boots: no token
// can arrive before the ACP session exists, and some clients time out in silence.
func agentWaitHeartbeat(ctx context.Context, streamID string, stream completionStream, firstDelta <-chan struct{}) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-firstDelta:
			return
		case <-ticker.C:
			if errEmit := hostStreamEmit(streamID, stream.chunkContent("…仍在等待 Miora Agent（沙箱准备较慢）\n")); errEmit != nil {
				return
			}
		}
	}
}

// buildAgentPrompt renders a client transcript into one ACP prompt string.
func buildAgentPrompt(parsed *parsedChat) string {
	if parsed == nil {
		return ""
	}
	messages := parsed.Messages
	if len(messages) == 0 {
		return parsed.Prompt
	}
	systemParts := make([]string, 0, 2)
	turns := make([]string, 0, len(messages))
	userCount := 0
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		text := strings.TrimSpace(textFromContent(message.Content))
		if text == "" {
			continue
		}
		switch role {
		case "system", "developer":
			systemParts = append(systemParts, text)
		case "assistant":
			turns = append(turns, "助手："+text)
		default:
			userCount++
			turns = append(turns, "用户："+text)
		}
	}
	if len(systemParts) == 0 && len(turns) == 1 && userCount == 1 {
		return strings.TrimPrefix(turns[0], "用户：")
	}
	var builder strings.Builder
	if len(systemParts) > 0 {
		builder.WriteString("<系统设定>\n")
		builder.WriteString(strings.Join(systemParts, "\n\n"))
		builder.WriteString("\n</系统设定>\n\n")
	}
	builder.WriteString("<对话历史>\n")
	builder.WriteString(strings.Join(turns, "\n\n"))
	builder.WriteString("\n</对话历史>\n\n请直接回答最后一条用户消息。")
	return builder.String()
}

// agentCompletionBody renders a non-streaming chat completion with real usage.
func agentCompletionBody(model, content string, usage *agentUsage) []byte {
	body := map[string]any{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": usagePayload(usage),
	}
	raw, _ := json.Marshal(body)
	return raw
}

// chunkFinalUsage closes a stream with the OpenAI usage block.
func (s completionStream) chunkFinalUsage(reason string, usage *agentUsage) []byte {
	frame := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	if usage != nil && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
		frame["usage"] = usagePayload(usage)
	}
	raw, _ := json.Marshal(frame)
	return raw
}

func usagePayload(usage *agentUsage) map[string]any {
	prompt := int64(0)
	completion := int64(0)
	if usage != nil {
		prompt = usage.InputTokens
		completion = usage.OutputTokens
	}
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
}

// forgetAgentRuntime drops a cached sandbox so the next call provisions a new one.
func forgetAgentRuntime(cfg *Config, cred *credential) {
	agentRuntimes.mu.Lock()
	defer agentRuntimes.mu.Unlock()
	delete(agentRuntimes.byAccount, agentAccountKey(cfg, cred))
}

// agentStatusView is what the panel shows about the ACP bridge.
func agentStatusView(cfg *Config) map[string]any {
	// Snapshot under the lock, then look the catalogue up outside it: knownAgentModels
	// takes the same mutex and sync.Mutex is not reentrant, so calling it while
	// holding the lock deadlocked every /models request (empty studio selects).
	agentRuntimes.mu.Lock()
	runtimes := make([]agentRuntime, 0, len(agentRuntimes.byAccount))
	keys := make([]string, 0, len(agentRuntimes.byAccount))
	for key, runtime := range agentRuntimes.byAccount {
		keys = append(keys, key)
		runtimes = append(runtimes, *runtime)
	}
	agentRuntimes.mu.Unlock()

	accounts := make([]map[string]any, 0, len(keys))
	for index, key := range keys {
		runtime := runtimes[index]
		accounts = append(accounts, map[string]any{
			"account":       key,
			"project_id":    runtime.ProjectID,
			"runtime_id":    runtime.RuntimeID,
			"models":        len(runtime.Models),
			"created_at":    formatTime(runtime.Created),
			"token_expires": formatTime(runtime.TokenExpires),
			"usable":        runtime.usable(cfg),
		})
	}
	return map[string]any{
		"enabled":       cfg.AgentEnabled,
		"default_model": cfg.AgentDefaultModel,
		"mode":          cfg.AgentMode,
		"project_title": cfg.AgentProjectTitle,
		"catalogue":     len(knownAgentModels(cfg)),
		"runtimes":      accounts,
	}
}

// deltaEmitter decouples the ACP read loop from synchronous host callbacks. It
// coalesces deltas for a short window so a 150-frame turn becomes a few dozen
// chunks instead of 150 blocking round trips.
type deltaEmitter struct {
	streamID string
	stream   completionStream
	queue    chan string
	done     chan struct{}
}

func newDeltaEmitter(streamID string, stream completionStream) *deltaEmitter {
	emitter := &deltaEmitter{
		streamID: streamID,
		stream:   stream,
		queue:    make(chan string, 256),
		done:     make(chan struct{}),
	}
	go emitter.pump()
	return emitter
}

func (e *deltaEmitter) push(delta string) {
	if delta == "" {
		return
	}
	select {
	case e.queue <- delta:
	case <-e.done:
	}
}

func (e *deltaEmitter) finish(final []byte) {
	close(e.queue)
	<-e.done
	if len(final) > 0 {
		if errEmit := hostStreamEmit(e.streamID, final); errEmit != nil {
			logWarn("failed to emit final agent chunk", map[string]any{"error": errEmit.Error()})
		}
	}
}

func (e *deltaEmitter) pump() {
	defer close(e.done)
	buffer := strings.Builder{}
	flush := func() {
		if buffer.Len() == 0 {
			return
		}
		if errEmit := hostStreamEmit(e.streamID, e.stream.chunkContent(buffer.String())); errEmit != nil {
			logWarn("failed to emit agent delta", map[string]any{"error": errEmit.Error()})
		}
		buffer.Reset()
	}
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case delta, ok := <-e.queue:
			if !ok {
				flush()
				return
			}
			buffer.WriteString(delta)
			if buffer.Len() >= 240 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
