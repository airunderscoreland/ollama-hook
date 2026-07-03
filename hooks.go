package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Hook is the interface for observing Ollama request lifecycle events.
// reqID is a per-request identifier that correlates all events for the
// same request, allowing hooks to handle concurrent requests correctly.
type Hook interface {
	OnRequestStart(reqID, model, endpoint, userPrompt string)
	OnToken(reqID, token string, tokenCount int, elapsed time.Duration)
	OnThinking(reqID, content string)
	OnToolCalls(reqID string, calls json.RawMessage)
	OnRequestComplete(reqID string, duration time.Duration, totalTokens int)
	OnError(reqID string, err error)
}

// MultiHook fans out events to multiple hooks.
type MultiHook struct {
	hooks []Hook
}

func NewMultiHook(hooks ...Hook) *MultiHook {
	return &MultiHook{hooks: hooks}
}

func (m *MultiHook) OnRequestStart(reqID, model, endpoint, userPrompt string) {
	for _, h := range m.hooks {
		h.OnRequestStart(reqID, model, endpoint, userPrompt)
	}
}

func (m *MultiHook) OnToken(reqID, token string, tokenCount int, elapsed time.Duration) {
	for _, h := range m.hooks {
		h.OnToken(reqID, token, tokenCount, elapsed)
	}
}

func (m *MultiHook) OnThinking(reqID, content string) {
	for _, h := range m.hooks {
		h.OnThinking(reqID, content)
	}
}

func (m *MultiHook) OnToolCalls(reqID string, calls json.RawMessage) {
	for _, h := range m.hooks {
		h.OnToolCalls(reqID, calls)
	}
}

func (m *MultiHook) OnRequestComplete(reqID string, duration time.Duration, totalTokens int) {
	for _, h := range m.hooks {
		h.OnRequestComplete(reqID, duration, totalTokens)
	}
}

func (m *MultiHook) OnError(reqID string, err error) {
	for _, h := range m.hooks {
		h.OnError(reqID, err)
	}
}

// LogHook logs all events to the provided logger.
type LogHook struct {
	logger *slog.Logger
}

func NewLogHook(logger *slog.Logger) *LogHook {
	return &LogHook{logger: logger}
}

func (l *LogHook) OnRequestStart(reqID, model, endpoint, userPrompt string) {
	l.logger.Info("hook: request start", "req_id", reqID, "model", model, "endpoint", endpoint)
}

func (l *LogHook) OnToken(reqID, token string, tokenCount int, elapsed time.Duration) {
	display := token
	if len(display) > 32 {
		display = display[:32] + "..."
	}
	l.logger.Debug("hook: token",
		"req_id", reqID,
		"token", display,
		"count", tokenCount,
		"elapsed_ms", elapsed.Milliseconds(),
	)
}

func (l *LogHook) OnThinking(reqID, content string) {
	display := content
	if len(display) > 32 {
		display = display[:32] + "..."
	}
	l.logger.Debug("hook: thinking", "req_id", reqID, "content", display)
}

func (l *LogHook) OnToolCalls(reqID string, calls json.RawMessage) {
	l.logger.Info("hook: tool calls", "req_id", reqID, "calls", string(calls))
}

func (l *LogHook) OnRequestComplete(reqID string, duration time.Duration, totalTokens int) {
	l.logger.Info("hook: request complete",
		"req_id", reqID,
		"duration", fmt.Sprintf("%.2fs", duration.Seconds()),
		"tokens", totalTokens,
	)
}

func (l *LogHook) OnError(reqID string, err error) {
	l.logger.Error("hook: error", "req_id", reqID, "error", err)
}
