package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestMetricsPerRequestIsolation guards against the concurrency bug where
// per-request state lived in shared struct fields: starting a second request
// before the first completed used to silently corrupt the first request's
// reported model/endpoint. Interleaving two in-flight requests here would
// have failed under the old implementation.
func TestMetricsPerRequestIsolation(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	m := NewMetricsHook(logger)

	m.OnRequestStart("req-a", "model-a", "/api/chat", "")
	m.OnRequestStart("req-b", "model-b", "/api/generate", "")

	m.OnToken("req-a", "hello", 1, time.Millisecond)
	m.OnToken("req-b", "world", 1, time.Millisecond)

	m.OnRequestComplete("req-a", 10*time.Millisecond, 1)
	m.OnRequestComplete("req-b", 20*time.Millisecond, 1)

	got := parseCompletionFields(t, buf.String())
	if len(got) != 2 {
		t.Fatalf("expected 2 completion log lines, got %d", len(got))
	}

	if got[0]["model"] != "model-a" || got[0]["endpoint"] != "/api/chat" {
		t.Errorf("req-a completion attributed to wrong request: %+v", got[0])
	}
	if got[1]["model"] != "model-b" || got[1]["endpoint"] != "/api/generate" {
		t.Errorf("req-b completion attributed to wrong request: %+v", got[1])
	}
}

// TestMetricsCleansUpOnError ensures in-flight state doesn't leak when a
// request errors out instead of completing normally.
func TestMetricsCleansUpOnError(t *testing.T) {
	logger := NewLogger("error", "text")
	m := NewMetricsHook(logger)

	m.OnRequestStart("req-x", "model-x", "/api/chat", "")
	m.OnError("req-x", errors.New("boom"))

	m.mu.Lock()
	_, stillPresent := m.inFlight["req-x"]
	m.mu.Unlock()

	if stillPresent {
		t.Errorf("expected in-flight state for req-x to be cleaned up after OnError")
	}
}

func parseCompletionFields(t *testing.T, logOutput string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(logOutput), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("failed to parse log line %q: %v", line, err)
		}
		if entry["msg"] != "metrics: request complete" {
			continue
		}
		out = append(out, map[string]string{
			"model":    entry["model"].(string),
			"endpoint": entry["endpoint"].(string),
		})
	}
	return out
}
