package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	ctx, cancel := context.WithTimeout(ctx, cfg.taskTimeout()+30*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		generationPermits.mu.Lock()
		changed := generationPermits.changed
		generationPermits.mu.Unlock()
		limit := cfg.MaxConcurrentTasks
		if active := currentConfig.Load(); active != nil {
			limit = active.MaxConcurrentTasks
		}
		if release, ok := generationPermits.acquire(limit); ok {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// A single active count spans configuration changes; lowering the limit waits
// for existing tasks to finish instead of creating another independent pool.
type generationLimiter struct {
	mu      sync.Mutex
	active  int
	changed chan struct{}
}

var generationPermits = generationLimiter{changed: make(chan struct{})}

func (l *generationLimiter) acquire(limit int) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= limit {
		return nil, false
	}
	l.active++
	return func() {
		l.mu.Lock()
		l.active--
		close(l.changed)
		l.changed = make(chan struct{})
		l.mu.Unlock()
	}, true
}

// progressFunc receives intermediate task progress for streamed responses.
type progressFunc func(status string, progress float64, note string)

// runGeneration submits a media generation, waits for the terminal state, signs
// the produced artifacts and records the task.
func runGeneration(ctx context.Context, cfg *Config, cred *credential, req generateRequest, onProgress progressFunc) (_ *generationResult, runErr error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.taskTimeout())
	defer cancel()
	started := time.Now()
	if onProgress != nil {
		onProgress("submitting", 0, "已提交到 Miora")
	}
	taskID, errSubmit := submitGeneration(ctx, cfg, cred, req, started)
	if errSubmit != nil {
		return nil, errSubmit
	}
	return awaitGeneration(ctx, cfg, cred, req, taskID, started, onProgress)
}

// submitGeneration posts one media task and records a visible row immediately, so
// the panel shows the request even while it is still running and gets a real error
// when Miora rejects it (for example the free tier's one-concurrent-3D rule).
func submitGeneration(ctx context.Context, cfg *Config, cred *credential, req generateRequest, started time.Time) (string, error) {
	client := newAPIClient(cfg, cred)
	taskID, errSubmit := client.submitTask(ctx, req)
	if errSubmit != nil {
		recordTask(taskRecord{
			ID: taskKey(req, started), Account: cred.displayLabel(), Model: req.Model,
			Media: req.Media, Prompt: truncateRunes(req.Prompt, 400), Status: "submit_failed",
			Error: errSubmit.Error(), StartedAt: formatTime(started), FinishedAt: formatTime(time.Now()),
			ElapsedMS: time.Since(started).Milliseconds(),
		})
		recordEvent(eventRecord{
			Time: time.Now(), Account: cred.displayLabel(), Model: req.Model,
			Media: req.Media, Prompt: truncateRunes(req.Prompt, 160), Status: "submit_failed",
			Error: errSubmit.Error(), ElapsedMS: time.Since(started).Milliseconds(),
		})
		return "", errSubmit
	}
	recordTask(taskRecord{
		ID: taskID, TaskID: taskID, Account: cred.displayLabel(), Model: req.Model,
		Media: req.Media, Prompt: truncateRunes(req.Prompt, 400), Status: "submitted",
		StartedAt: formatTime(started),
	})
	return taskID, nil
}

// taskKey builds a stable row id for requests that never reached Miora.
func taskKey(req generateRequest, started time.Time) string {
	return "local-" + strconv.FormatInt(started.UnixNano(), 36) + "-" + strings.ReplaceAll(req.Media, "_", "")
}

// awaitGeneration polls one submitted task until it finishes.
func awaitGeneration(ctx context.Context, cfg *Config, cred *credential, req generateRequest, taskID string, started time.Time, onProgress progressFunc) (_ *generationResult, runErr error) {
	client := newAPIClient(cfg, cred)
	// Every exit after submission leaves a terminal row, including cancellation
	// during a request, a polling sleep, or result signing.
	defer func() {
		if runErr == nil {
			return
		}
		status := "failed"
		if errors.Is(runErr, context.DeadlineExceeded) {
			status = "timeout"
			runErr = fmt.Errorf("等待 Miora 任务 %s 超时，上游任务可能仍在运行：%w", taskID, runErr)
		} else if errors.Is(runErr, context.Canceled) {
			status = "cancelled"
		}
		recordTask(taskRecord{
			ID: taskID, TaskID: taskID, Account: cred.displayLabel(), Model: req.Model,
			Media: req.Media, Prompt: truncateRunes(req.Prompt, 400), Status: status,
			Error: runErr.Error(), StartedAt: formatTime(started), FinishedAt: formatTime(time.Now()),
			ElapsedMS: time.Since(started).Milliseconds(),
		})
		recordEvent(eventRecord{
			Time: time.Now(), Account: cred.displayLabel(), Model: req.Model, Media: req.Media,
			Prompt: truncateRunes(req.Prompt, 160), Status: status, TaskID: taskID,
			Error: runErr.Error(), ElapsedMS: time.Since(started).Milliseconds(),
		})
	}()
	lastStatus := ""
	lastProgress := 0.0
	pollFailures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		states, errPoll := client.pollTasks(ctx, req.Media, []string{taskID})
		if errPoll != nil {
			var upstream *upstreamError
			if errors.As(errPoll, &upstream) && (upstream.isAuthError() || upstream.Status >= 400 && upstream.Status < 500) {
				return nil, errPoll
			}
			// Transient polling failures are retried, but they must be visible: a
			// silent loop looks like "the task never progresses" in the panel.
			pollFailures++
			if pollFailures <= 3 || pollFailures%10 == 0 {
				logWarn("task poll failed", map[string]any{
					"task": taskID, "media": req.Media, "attempt": pollFailures, "error": errPoll.Error(),
				})
			}
		}
		if state, ok := states[taskID]; ok {
			if state.Status != lastStatus || state.Progress != lastProgress {
				lastStatus, lastProgress = state.Status, state.Progress
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
					return nil, fmt.Errorf("Miora 任务失败：%s", firstNonEmpty(state.ErrorMessage, state.Status))
				}
				return finalizeGeneration(ctx, cfg, client, cred, req, state, started)
			}
		}
		if errWait := sleepCtx(ctx, cfg.pollInterval()); errWait != nil {
			return nil, errWait
		}
	}
}

// finalizeGeneration signs artifacts and records the successful task.
func finalizeGeneration(ctx context.Context, cfg *Config, client *apiClient, cred *credential, req generateRequest, state taskState, started time.Time) (*generationResult, error) {
	keys := state.fileKeys()
	signed, errSign := client.signURLs(ctx, keys)
	if errSign != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
