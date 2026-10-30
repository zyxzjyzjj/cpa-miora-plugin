package main

import (
	"fmt"
	"sync"
	"time"
)

// upstreamTrace keeps the last few upstream round trips so the panel can explain
// which transport served a request and why one failed. It never stores headers
// or bodies: only method, path, status and transport.
type upstreamTrace struct {
	At        time.Time `json:"at"`
	Transport string    `json:"transport"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Error     string    `json:"error,omitempty"`
}

var upstreamTraces struct {
	mu      sync.Mutex
	entries []upstreamTrace
}

func recordUpstreamTrace(transport, method, rawURL string, status int, err error) {
	entry := upstreamTrace{
		At:        time.Now(),
		Transport: transport,
		Method:    method,
		Path:      redactURL(rawURL),
		Status:    status,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	upstreamTraces.mu.Lock()
	defer upstreamTraces.mu.Unlock()
	upstreamTraces.entries = append([]upstreamTrace{entry}, upstreamTraces.entries...)
	if len(upstreamTraces.entries) > 40 {
		upstreamTraces.entries = upstreamTraces.entries[:40]
	}
}

func listUpstreamTraces() []upstreamTrace {
	upstreamTraces.mu.Lock()
	defer upstreamTraces.mu.Unlock()
	out := make([]upstreamTrace, len(upstreamTraces.entries))
	copy(out, upstreamTraces.entries)
	return out
}

func traceSummary() string {
	traces := listUpstreamTraces()
	if len(traces) == 0 {
		return "无上游调用记录"
	}
	latest := traces[0]
	return fmt.Sprintf("%s %s → %d（%s）", latest.Method, latest.Path, latest.Status, latest.Transport)
}
