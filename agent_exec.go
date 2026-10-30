package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	defer func() { go runtime.closeAgentSession(cfg, session.SessionID) }()

	modelID := strings.TrimSpace(plan.model)
	if modelID == "" {
		modelID = cfg.AgentDefaultModel
	}
	prompt := buildAgentPrompt(plan.parsed)

	var builder strings.Builder
	usage, _, errPrompt := runtime.prompt(ctx, cfg, session, modelID, prompt, func(delta string) error {
		builder.WriteString(delta)
		if onDelta != nil {
			return onDelta(delta)
		}
		return nil
	})
	if errPrompt != nil {
		if authErr := asAuthError(errPrompt); authErr != nil {
			return "", usage, authErr
		}
		return builder.String(), usage, errPrompt
	}
	text := builder.String()
	if strings.TrimSpace(text) == "" {
		return "", usage, fmt.Errorf("Miora Agent 没有返回文本内容（stopReason 可能是工具调用）")
	}
	return text, usage, nil
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

	// Host callbacks are synchronous, so emitting inside the SSE read loop would
	// throttle the agent stream: hand deltas to a pump goroutine that coalesces
	// them and keeps the read loop free.
	emitter := newDeltaEmitter(streamID, stream)
	text, usage, errAgent := runAgentTurn(ctx, cfg, plan, func(delta string) error {
		emitter.push(delta)
		return nil
	})
	emitter.finish(stream.chunkFinalUsage("stop", usage))
	if errAgent != nil {
		emitStreamError(streamID, errAgent.Error())
		closeStream(streamID)
		return okEnvelope(map[string]any{"headers": streamHeaders()})
	}
	closeStream(streamID)
	logInfo("agent stream served", map[string]any{"model": plan.model, "chars": len(text)})
	return okEnvelope(map[string]any{"headers": streamHeaders()})
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
	agentRuntimes.mu.Lock()
	defer agentRuntimes.mu.Unlock()
	accounts := make([]map[string]any, 0, len(agentRuntimes.byAccount))
	for key, runtime := range agentRuntimes.byAccount {
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
