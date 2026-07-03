package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// requestMetrics holds mutable per-request state while a request is in flight.
type requestMetrics struct {
	startTime time.Time
	firstTok  time.Time
	model     string
	endpoint  string
}

// MetricsHook records per-request timings and aggregate counters.
type MetricsHook struct {
	logger *slog.Logger

	// In-flight requests indexed by reqID (protected by mu).
	mu       sync.Mutex
	inFlight map[string]*requestMetrics

	// Aggregate counters (atomic for lock-free reads).
	requestsTotal atomic.Int64
	tokensTotal   atomic.Int64
	errorsTotal   atomic.Int64
	rgbDropsTotal atomic.Int64
}

func NewMetricsHook(logger *slog.Logger) *MetricsHook {
	return &MetricsHook{logger: logger, inFlight: make(map[string]*requestMetrics)}
}

func (m *MetricsHook) OnRequestStart(reqID, model, endpoint, _ /*userPrompt*/ string) {
	m.mu.Lock()
	m.inFlight[reqID] = &requestMetrics{
		startTime: time.Now(),
		model:     model,
		endpoint:  endpoint,
	}
	m.mu.Unlock()

	m.requestsTotal.Add(1)
}

func (m *MetricsHook) OnToken(reqID, _ /*token*/ string, _ /*tokenCount*/ int, _ /*elapsed*/ time.Duration) {
	m.mu.Lock()
	if req, ok := m.inFlight[reqID]; ok && req.firstTok.IsZero() {
		req.firstTok = time.Now()
	}
	m.mu.Unlock()

	m.tokensTotal.Add(1)
}

func (m *MetricsHook) OnThinking(_ /*reqID*/, _ /*content*/ string) {}

func (m *MetricsHook) OnToolCalls(_ /*reqID*/ string, _ /*calls*/ json.RawMessage) {}

func (m *MetricsHook) OnRequestComplete(reqID string, duration time.Duration, totalTokens int) {
	m.mu.Lock()
	req, ok := m.inFlight[reqID]
	delete(m.inFlight, reqID)
	m.mu.Unlock()

	if !ok {
		m.logger.Warn("metrics: no in-flight state for completed request", "req_id", reqID)
		return
	}

	startTime := req.startTime
	firstTok := req.firstTok
	model := req.model
	endpoint := req.endpoint

	now := time.Now()
	totalMs := float64(now.Sub(startTime).Milliseconds())
	upstreamMs := float64(duration.Milliseconds())
	overheadMs := totalMs - upstreamMs
	if overheadMs < 0 {
		overheadMs = 0
	}

	var ttftMs float64
	if !firstTok.IsZero() {
		ttftMs = float64(firstTok.Sub(startTime).Milliseconds())
	}

	var tokPerSec float64
	if totalMs > 0 {
		tokPerSec = float64(totalTokens) / (totalMs / 1000.0)
	}

	m.logger.Info("metrics: request complete",
		"model", model,
		"endpoint", endpoint,
		"ttft_ms", ttftMs,
		"total_ms", totalMs,
		"upstream_total_duration_ms", upstreamMs,
		"proxy_overhead_ms", overheadMs,
		"tokens", totalTokens,
		"tokens_per_sec", fmt.Sprintf("%.1f", tokPerSec),
	)
}

func (m *MetricsHook) OnError(reqID string, _ /*err*/ error) {
	m.errorsTotal.Add(1)

	m.mu.Lock()
	delete(m.inFlight, reqID)
	m.mu.Unlock()
}

// IncrementRGBDrops records a dropped RGB command due to rate limiting.
func (m *MetricsHook) IncrementRGBDrops() {
	m.rgbDropsTotal.Add(1)
}

// ServeMetrics handles GET /_proxy/metrics with a plain text dump.
func (m *MetricsHook) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	var sb strings.Builder
	fmt.Fprintf(&sb, "requests_total %d\n", m.requestsTotal.Load())
	fmt.Fprintf(&sb, "tokens_total %d\n", m.tokensTotal.Load())
	fmt.Fprintf(&sb, "errors_total %d\n", m.errorsTotal.Load())
	fmt.Fprintf(&sb, "rgb_queue_drops_total %d\n", m.rgbDropsTotal.Load())

	w.Write([]byte(sb.String()))
}
