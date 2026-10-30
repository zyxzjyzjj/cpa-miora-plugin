package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// schedulerPick rotates across this provider's accounts. Miora rate-limits per
// account, and a rotating token per account is cheap, so a simple round-robin is
// enough; CPA's own cooldown logic still sees upstream 401/429 via the executor.
func schedulerPick(request []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode scheduler pick request: %w", errUnmarshal)
		}
	}
	if len(req.Candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin})
	}
	count := len(req.Candidates)
	start := int(schedulerCursor.Add(1)-1) % count
	for offset := 0; offset < count; offset++ {
		candidate := req.Candidates[(start+offset)%count]
		if !candidateUsable(candidate) {
			continue
		}
		return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true})
	}
	// Everything looked busy: let the host apply its own fill-first policy instead
	// of pinning every request to one account here.
	return okEnvelope(pluginapi.SchedulerPickResponse{DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst})
}

var schedulerCursor atomic.Uint64

func candidateUsable(candidate pluginapi.SchedulerAuthCandidate) bool {
	status := strings.ToLower(strings.TrimSpace(candidate.Status))
	switch status {
	case "", "ready", "ok", "active", "available":
	default:
		return false
	}
	if disabled, ok := candidate.Metadata["disabled"].(bool); ok && disabled {
		return false
	}
	if cooling, ok := candidate.Metadata["cooling"].(bool); ok && cooling {
		return false
	}
	return true
}
