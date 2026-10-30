package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Agent provisioning is expensive (a project plus a sandbox and, on the first
// turn, a minute of boot time), so the chosen project/runtime is persisted next
// to the user cache instead of living only in memory. A CPA restart then reuses
// the same sandbox instead of stacking up a new project on every launch.

type agentStateEntry struct {
	ProjectID string `json:"project_id"`
	RuntimeID string `json:"runtime_id"`
	SessionID string `json:"session_id,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type agentStateFile struct {
	Version int                        `json:"version"`
	Entries map[string]agentStateEntry `json:"entries"`
}

var agentState = struct {
	mu     sync.Mutex
	loaded bool
	path   string
	file   agentStateFile
}{file: agentStateFile{Version: 1, Entries: map[string]agentStateEntry{}}}

func agentStatePath() string {
	agentState.mu.Lock()
	defer agentState.mu.Unlock()
	if agentState.path != "" {
		return agentState.path
	}
	base, errBase := os.UserCacheDir()
	if errBase != nil || base == "" {
		base = os.TempDir()
	}
	agentState.path = filepath.Join(base, "cpa-miora-plugin", "agent-state.json")
	return agentState.path
}

func loadAgentState() {
	agentState.mu.Lock()
	defer agentState.mu.Unlock()
	if agentState.loaded {
		return
	}
	agentState.loaded = true
	raw, errRead := os.ReadFile(agentState.path)
	if errRead != nil || len(raw) == 0 {
		return
	}
	parsed := agentStateFile{}
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil || parsed.Entries == nil {
		return
	}
	agentState.file = parsed
}

func stateEntry(key string) (agentStateEntry, bool) {
	loadAgentState()
	agentState.mu.Lock()
	defer agentState.mu.Unlock()
	entry, ok := agentState.file.Entries[key]
	return entry, ok
}

func rememberState(key, projectID, runtimeID, sessionID string) {
	loadAgentState()
	agentState.mu.Lock()
	if agentState.file.Entries == nil {
		agentState.file.Entries = map[string]agentStateEntry{}
	}
	agentState.file.Entries[key] = agentStateEntry{
		ProjectID: projectID,
		RuntimeID: runtimeID,
		SessionID: sessionID,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	snapshot := agentState.file
	path := agentState.path
	agentState.mu.Unlock()

	raw, errMarshal := json.MarshalIndent(snapshot, "", "  ")
	if errMarshal != nil {
		return
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o644)
}

func forgetState(key string) {
	loadAgentState()
	agentState.mu.Lock()
	delete(agentState.file.Entries, key)
	snapshot := agentState.file
	path := agentState.path
	agentState.mu.Unlock()
	raw, errMarshal := json.MarshalIndent(snapshot, "", "  ")
	if errMarshal == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
}
