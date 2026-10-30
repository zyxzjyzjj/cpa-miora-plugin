package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// mediaFile is one produced artifact.
type mediaFile struct {
	FileKey string `json:"file_key"`
	URL     string `json:"url"`
	Type    string `json:"type"`
}

// generationResult is the outcome of one media generation.
type generationResult struct {
	TaskID       string      `json:"task_id"`
	Model        string      `json:"model"`
	Media        string      `json:"media"`
	Files        []mediaFile `json:"files"`
	Credit       float64     `json:"credit"`
	ElapsedMS    int64       `json:"elapsed_ms"`
	Resolution   string      `json:"resolution"`
	AspectRatio  string      `json:"aspect_ratio"`
	Size         string      `json:"size"`
	DisplayName  string      `json:"display_name"`
	URLExpiresAt time.Time   `json:"url_expires_at"`
}

// taskRecord is one row of the panel's task history.
type taskRecord struct {
	ID       string      `json:"id"`
	TaskID   string      `json:"task_id"`
	Account  string      `json:"account"`
	Model    string      `json:"model"`
	Media    string      `json:"media"`
	Prompt   string      `json:"prompt"`
	Status   string      `json:"status"`
	Progress float64     `json:"progress"`
	Credit   float64     `json:"credit"`
	Error    string      `json:"error,omitempty"`
	Files    []mediaFile `json:"files"`
	// Times are RFC3339 strings, empty until known: a zero time.Time would
	// serialise as "0001-01-01T00:00:00Z" and the panel rendered it as
	// "739918 天前" for every task that had not finished yet.
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms"`
}

// eventRecord is one request-log row.
type eventRecord struct {
	Time      time.Time `json:"time"`
	Account   string    `json:"account"`
	Model     string    `json:"model"`
	Media     string    `json:"media"`
	Prompt    string    `json:"prompt"`
	Status    string    `json:"status"`
	TaskID    string    `json:"task_id"`
	Credit    float64   `json:"credit"`
	ElapsedMS int64     `json:"elapsed_ms"`
	Error     string    `json:"error,omitempty"`
}

var taskStore = struct {
	mu     sync.Mutex
	tasks  []taskRecord
	events []eventRecord
}{}

// recordTask upserts one task row. A generation writes several rows as it moves
// through pending -> processing -> completed, and they must all land on the same
// entry so the panel never shows a duplicate without its files.
func recordTask(record taskRecord) {
	upsertTask(record)
}

// upsertTask inserts or updates one task row. Progress updates from a running
// generation must not create a second row, so records are keyed by task ID.
func upsertTask(record taskRecord) {
	if strings.TrimSpace(record.ID) == "" {
		record.ID = record.TaskID
	}
	taskStore.mu.Lock()
	defer taskStore.mu.Unlock()
	for index := range taskStore.tasks {
		if taskStore.tasks[index].ID == record.ID {
			merged := taskStore.tasks[index]
			if record.Status != "" {
				merged.Status = record.Status
			}
			if record.Progress > merged.Progress {
				merged.Progress = record.Progress
			}
			if record.Credit > 0 {
				merged.Credit = record.Credit
			}
			if len(record.Files) > 0 {
				merged.Files = record.Files
			}
			if record.Error != "" {
				merged.Error = record.Error
			}
			if record.TaskID != "" {
				merged.TaskID = record.TaskID
			}
			if record.FinishedAt != "" {
				merged.FinishedAt = record.FinishedAt
			}
			if merged.StartedAt == "" && record.StartedAt != "" {
				merged.StartedAt = record.StartedAt
			}
			if record.ElapsedMS > 0 {
				merged.ElapsedMS = record.ElapsedMS
			}
			taskStore.tasks[index] = merged
			return
		}
	}
	taskStore.tasks = append([]taskRecord{record}, taskStore.tasks...)
	limit := config().EventLogSize
	if limit <= 0 {
		limit = 200
	}
	if len(taskStore.tasks) > limit {
		taskStore.tasks = taskStore.tasks[:limit]
	}
}

func recordEvent(event eventRecord) {
	taskStore.mu.Lock()
	defer taskStore.mu.Unlock()
	taskStore.events = append([]eventRecord{event}, taskStore.events...)
	limit := config().EventLogSize
	if limit <= 0 {
		limit = 200
	}
	if len(taskStore.events) > limit {
		taskStore.events = taskStore.events[:limit]
	}
}

func listTasks(limit int) []taskRecord {
	taskStore.mu.Lock()
	defer taskStore.mu.Unlock()
	if limit <= 0 || limit > len(taskStore.tasks) {
		limit = len(taskStore.tasks)
	}
	out := make([]taskRecord, limit)
	copy(out, taskStore.tasks[:limit])
	return out
}

func listEvents(limit int) []eventRecord {
	taskStore.mu.Lock()
	defer taskStore.mu.Unlock()
	if limit <= 0 || limit > len(taskStore.events) {
		limit = len(taskStore.events)
	}
	out := make([]eventRecord, limit)
	copy(out, taskStore.events[:limit])
	return out
}

// acquirePermit takes one concurrency slot, honouring the caller's context. A
// generation can take minutes, so an unbounded executor would hold a CPA worker
// (and an upstream slot) per request.
func acquirePermit(ctx context.Context, cfg *Config) (func(), error) {
	limit := 4
	if cfg != nil && cfg.MaxConcurrentTasks > 0 {
		limit = cfg.MaxConcurrentTasks
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 64 {
		limit = 64
	}
	semaphore := permitSemaphore(limit)
	select {
	case semaphore <- struct{}{}:
		return func() { <-semaphore }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var permitRegistry = struct {
	mu    sync.Mutex
	semis map[int]chan struct{}
}{semis: map[int]chan struct{}{}}

func permitSemaphore(limit int) chan struct{} {
	permitRegistry.mu.Lock()
	defer permitRegistry.mu.Unlock()
	if semaphore, ok := permitRegistry.semis[limit]; ok {
		return semaphore
	}
	semaphore := make(chan struct{}, limit)
	permitRegistry.semis[limit] = semaphore
	return semaphore
}

// progressFunc receives intermediate task progress for streamed responses.
type progressFunc func(status string, progress float64, note string)

// runGeneration submits a media generation, waits for the terminal state, signs
// the produced artifacts and records the task.
func runGeneration(ctx context.Context, cfg *Config, cred *credential, req generateRequest, onProgress progressFunc) (*generationResult, error) {
	started := time.Now()
	client := newAPIClient(cfg, cred)

	if onProgress != nil {
		onProgress("submitting", 0, "已提交到 Miora")
	}
	taskID, errSubmit := client.submitTask(ctx, req)
	if errSubmit != nil {
		recordEvent(eventRecord{
			Time: time.Now(), Account: cred.displayLabel(), Model: req.Model,
			Media: req.Media, Prompt: truncateRunes(req.Prompt, 160), Status: "submit_failed",
			Error: errSubmit.Error(), ElapsedMS: time.Since(started).Milliseconds(),
		})
		return nil, errSubmit
	}

	deadline := started.Add(cfg.taskTimeout())
	interval := cfg.pollInterval()
	lastStatus := ""
	lastProgress := 0.0
	states := map[string]taskState{}

	for {
		select {
		case <-ctx.Done():
			recordTask(taskRecord{
				ID: taskID, TaskID: taskID, Account: cred.displayLabel(), Model: req.Model,
				Media: req.Media, Prompt: truncateRunes(req.Prompt, 400), Status: "cancelled",
				StartedAt: formatTime(started), FinishedAt: formatTime(time.Now()),
				ElapsedMS: time.Since(started).Milliseconds(),
			})
			return nil, ctx.Err()
		default:
		}

		polled, errPoll := client.pollTasks(ctx, req.Media, []string{taskID})
		if errPoll == nil {
			states = polled
		}
		state, ok := states[taskID]
		if ok {
			if state.Status != lastStatus || state.Progress != lastProgress {
				lastStatus = state.Status
				lastProgress = state.Progress
				upsertTask(taskRecord{
					ID: taskID, TaskID: taskID, Account: cred.displayLabel(), Model: req.Model,
					Media: req.Media, Prompt: truncateRunes(req.Prompt, 400), Status: state.Status,
					Progress: state.Progress, StartedAt: formatTime(started),
				})
				if onProgress != nil {
					onProgress(state.Status, state.Progress, "")
				}
			}
			if state.terminal() {
				if state.failed() {
					errTask := fmt.Errorf("Miora 任务失败：%s", firstNonEmpty(state.ErrorMessage, state.Status))
					recordTask(taskRecord{
						ID: taskID, TaskID: taskID, Account: cred.displayLabel(), Model: req.Model,
						Media: req.Media, Prompt: truncateRunes(req.Prompt, 400), Status: "failed",
						Error: errTask.Error(), StartedAt: formatTime(started), FinishedAt: formatTime(time.Now()),
						ElapsedMS: time.Since(started).Milliseconds(),
					})
					recordEvent(eventRecord{
						Time: time.Now(), Account: cred.displayLabel(), Model: req.Model, Media: req.Media,
						Prompt: truncateRunes(req.Prompt, 160), Status: "failed", TaskID: taskID,
						Error: errTask.Error(), ElapsedMS: time.Since(started).Milliseconds(),
					})
					return nil, errTask
				}
				return finalizeGeneration(ctx, cfg, client, cred, req, state, started)
			}
		}
		if time.Now().After(deadline) {
			errTimeout := fmt.Errorf("等待 Miora 任务超时（%s），任务 %s 仍在上游排队，可在面板产物里继续查看", cfg.taskTimeout(), taskID)
			recordTask(taskRecord{
				ID: taskID, TaskID: taskID, Account: cred.displayLabel(), Model: req.Model,
				Media: req.Media, Prompt: truncateRunes(req.Prompt, 400), Status: "timeout",
				Error: errTimeout.Error(), StartedAt: formatTime(started), FinishedAt: formatTime(time.Now()),
				ElapsedMS: time.Since(started).Milliseconds(),
			})
			return nil, errTimeout
		}
		if errWait := sleepCtx(ctx, interval); errWait != nil {
			return nil, errWait
		}
	}
}

// finalizeGeneration signs artifacts and records the successful task.
func finalizeGeneration(ctx context.Context, cfg *Config, client *apiClient, cred *credential, req generateRequest, state taskState, started time.Time) (*generationResult, error) {
	keys := state.fileKeys()
	signed, errSign := client.signURLs(ctx, keys)
	if errSign != nil {
		logWarn("result signing failed", map[string]any{"task": state.TaskID, "error": errSign.Error()})
	}

	files := make([]mediaFile, 0, len(keys))
	for _, key := range keys {
		fileType := req.Media
		for _, asset := range state.Assets {
			if asset.FileKey == key && asset.FileType != "" {
				fileType = asset.FileType
			}
		}
		url := signed[key]
		if url == "" && state.ResultSignURL != "" && key == state.ResultFileKey {
			url = state.ResultSignURL
		}
		files = append(files, mediaFile{FileKey: key, URL: url, Type: fileType})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("Miora 任务完成但没有返回产物（task %s）", state.TaskID)
	}

	result := &generationResult{
		TaskID:       state.TaskID,
		Model:        req.Model,
		Media:        req.Media,
		Files:        files,
		Credit:       state.Metadata.Usage.Credit.value(),
		ElapsedMS:    time.Since(started).Milliseconds(),
		Resolution:   firstNonEmpty(state.Metadata.Resolution, req.Resolution),
		AspectRatio:  firstNonEmpty(state.Metadata.AspectRatio, req.AspectRatio),
		Size:         state.Metadata.Size,
		DisplayName:  firstNonEmpty(state.Metadata.ModelDisplayName, req.Model),
		URLExpiresAt: time.Now().Add(time.Hour),
	}

	recordTask(taskRecord{
		ID: state.TaskID, TaskID: state.TaskID, Account: cred.displayLabel(), Model: req.Model,
		Media: req.Media, Prompt: truncateRunes(req.Prompt, 400), Status: "completed",
		Progress: 100, Credit: result.Credit, Files: files,
		StartedAt: formatTime(started), FinishedAt: formatTime(time.Now()), ElapsedMS: result.ElapsedMS,
	})
	recordEvent(eventRecord{
		Time: time.Now(), Account: cred.displayLabel(), Model: req.Model, Media: req.Media,
		Prompt: truncateRunes(req.Prompt, 160), Status: "completed", TaskID: state.TaskID,
		Credit: result.Credit, ElapsedMS: result.ElapsedMS,
	})
	logInfo("media generated", map[string]any{
		"task": state.TaskID, "model": req.Model, "media": req.Media,
		"files": len(files), "credits": result.Credit,
	})
	return result, nil
}

func sleepCtx(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// renderResult turns a generation into the assistant message text. Media clients
// parse the Markdown URL; the structured copy lives in the `miora` response
// extension and in the panel.
func renderResult(cfg *Config, result *generationResult, prompt string) string {
	var builder strings.Builder
	switch result.Media {
	case mediaVideo:
		for index, file := range result.Files {
			if index > 0 {
				builder.WriteString("\n")
			}
			builder.WriteString("[▶ Miora 视频")
			if index > 0 {
				builder.WriteString(fmt.Sprintf(" #%d", index+1))
			}
			builder.WriteString("](" + file.URL + ")")
		}
	case mediaThreeD:
		for index, file := range result.Files {
			if index > 0 {
				builder.WriteString("\n")
			}
			builder.WriteString("[Miora 3D 模型")
			if index > 0 {
				builder.WriteString(fmt.Sprintf(" #%d", index+1))
			}
			builder.WriteString("](" + file.URL + ")")
		}
	default:
		for index, file := range result.Files {
			if index > 0 {
				builder.WriteString("\n\n")
			}
			alt := strings.ReplaceAll(strings.TrimSpace(prompt), "]", ")")
			if len(alt) > 90 {
				alt = truncateRunes(alt, 90)
			}
			if alt == "" {
				alt = "Miora image"
			}
			builder.WriteString("![" + alt + "](" + file.URL + ")")
		}
	}

	if cfg == nil || !cfg.ResponseFooter {
		return builder.String()
	}

	details := []string{result.DisplayName}
	if result.Size != "" {
		details = append(details, result.Size)
	}
	if result.AspectRatio != "" {
		details = append(details, result.AspectRatio)
	}
	if result.Resolution != "" {
		details = append(details, result.Resolution)
	}
	if result.Credit > 0 {
		details = append(details, fmt.Sprintf("%.2f credits", result.Credit))
	}
	details = append(details, fmt.Sprintf("%.1fs", float64(result.ElapsedMS)/1000))
	builder.WriteString("\n\n> " + strings.Join(details, " · "))
	builder.WriteString("\n> 链接 1 小时内有效，可用面板按 fileKey 重新签名（不会重复扣费）。")
	return builder.String()
}
