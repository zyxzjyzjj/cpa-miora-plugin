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
	// RetiredProjects are projects whose memory grew past the budget. They stay
	// listed so the candidate walk never re-adopts them.
	RetiredProjects []string `json:"retired_projects,omitempty"`
}

var agentState = struct {
	mu     sync.Mutex
	loaded bool
	path   string
	file   agentStateFile
}{file: agentStateFile{Version: 1, Entries: map[string]agentStateEntry{}}}

const retiredProjectLimit = 20

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

// retiredProjects returns the projects that must not be reused.
func retiredProjects() map[string]struct{} {
	loadAgentState()
	agentState.mu.Lock()
	defer agentState.mu.Unlock()
	out := make(map[string]struct{}, len(agentState.file.RetiredProjects))
	for _, id := range agentState.file.RetiredProjects {
		out[id] = struct{}{}
	}
	return out
}

// retireProject forgets the account's current project and blacklists it so the next
// turn provisions a fresh one (memory resets to the ~49k baseline).
func retireProject(key string) {
	loadAgentState()
	agentState.mu.Lock()
	entry, ok := agentState.file.Entries[key]
	delete(agentState.file.Entries, key)
	if ok && entry.ProjectID != "" {
		already := false
		for _, id := range agentState.file.RetiredProjects {
			if id == entry.ProjectID {
				already = true
				break
			}
		}
		if !already {
			agentState.file.RetiredProjects = append(agentState.file.RetiredProjects, entry.ProjectID)
		}
		if len(agentState.file.RetiredProjects) > retiredProjectLimit {
			agentState.file.RetiredProjects = agentState.file.RetiredProjects[len(agentState.file.RetiredProjects)-retiredProjectLimit:]
		}
	}
	snapshot := agentState.file
	path := agentState.path
	agentState.mu.Unlock()

	rememberedProjects.mu.Lock()
	delete(rememberedProjects.byKey, key)
	rememberedProjects.mu.Unlock()

	if raw, errMarshal := json.MarshalIndent(snapshot, "", "  "); errMarshal == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
}
