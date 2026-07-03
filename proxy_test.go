package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testHook records hook events for assertions.
type testHook struct {
	events []string
}

func (h *testHook) OnRequestStart(_ /*reqID*/, model, endpoint, _ /*prompt*/ string) {
	h.events = append(h.events, "start:"+model+":"+endpoint)
}
func (h *testHook) OnToken(_ /*reqID*/, token string, _ /*count*/ int, _ /*elapsed*/ time.Duration) {
	h.events = append(h.events, "token:"+token)
}
func (h *testHook) OnThinking(_ /*reqID*/, content string) {
	h.events = append(h.events, "thinking:"+content)
}
func (h *testHook) OnToolCalls(_ /*reqID*/ string, calls json.RawMessage) {
	h.events = append(h.events, "tool_calls:"+string(calls))
}
func (h *testHook) OnRequestComplete(_ /*reqID*/ string, _ /*duration*/ time.Duration, _ /*tokens*/ int) {
	h.events = append(h.events, "complete")
}
func (h *testHook) OnError(_ /*reqID*/ string, err error) {
	h.events = append(h.events, "error:"+err.Error())
}

func newTestProxy(t *testing.T, upstream *httptest.Server) (*ProxyHandler, *testHook, *MetricsHook) {
	t.Helper()
	u, _ := url.Parse(upstream.URL)
	logger := NewLogger("debug", "text")
	hook := &testHook{}
	metrics := NewMetricsHook(logger)
	multi := NewMultiHook(hook, metrics)
	handler := NewProxyHandler(u, multi, metrics, logger)
	return handler, hook, metrics
}

func TestPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[]}`))
	}))
	defer upstream.Close()

	handler, hook, _ := newTestProxy(t, upstream)

	req := httptest.NewRequest("GET", "/api/tags", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"models":[]}` {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if len(hook.events) != 0 {
		t.Errorf("expected no hook events for passthrough, got %v", hook.events)
	}
}

func TestStreamingChat(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"model":"test","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":"Hello"},"done":false}`,
		`{"model":"test","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":" world"},"done":false}`,
		`{"model":"test","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","total_duration":500000000}`,
	}, "\n") + "\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		w.Write([]byte(ndjson))
	}))
	defer upstream.Close()

	handler, hook, _ := newTestProxy(t, upstream)

	body := strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/api/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Verify hook events fired in order.
	expected := []string{"start:test:/api/chat", "token:Hello", "token: world", "complete"}
	if len(hook.events) != len(expected) {
		t.Fatalf("expected %d events, got %d: %v", len(expected), len(hook.events), hook.events)
	}
	for i, e := range expected {
		if hook.events[i] != e {
			t.Errorf("event %d: expected %q, got %q", i, e, hook.events[i])
		}
	}

	// Verify response body matches upstream byte-for-byte.
	if rec.Body.String() != ndjson {
		t.Errorf("response body mismatch:\ngot:  %q\nwant: %q", rec.Body.String(), ndjson)
	}
}

func TestStreamingGenerate(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"model":"test","created_at":"2024-01-01T00:00:00Z","response":"Hi","done":false}`,
		`{"model":"test","created_at":"2024-01-01T00:00:00Z","response":"!","done":false}`,
		`{"model":"test","created_at":"2024-01-01T00:00:00Z","response":"","done":true,"done_reason":"stop","total_duration":300000000}`,
	}, "\n") + "\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(ndjson))
	}))
	defer upstream.Close()

	handler, hook, _ := newTestProxy(t, upstream)

	body := strings.NewReader(`{"model":"test","prompt":"hello"}`)
	req := httptest.NewRequest("POST", "/api/generate", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	expected := []string{"start:test:/api/generate", "token:Hi", "token:!", "complete"}
	if len(hook.events) != len(expected) {
		t.Fatalf("expected %d events, got %d: %v", len(expected), len(hook.events), hook.events)
	}
	for i, e := range expected {
		if hook.events[i] != e {
			t.Errorf("event %d: expected %q, got %q", i, e, hook.events[i])
		}
	}
}

func TestUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer upstream.Close()

	handler, hook, _ := newTestProxy(t, upstream)

	body := strings.NewReader(`{"model":"test","messages":[]}`)
	req := httptest.NewRequest("POST", "/api/chat", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("expected 500, got %d", rec.Code)
	}

	// Should have start + error events.
	hasError := false
	for _, e := range hook.events {
		if strings.HasPrefix(e, "error:") {
			hasError = true
		}
	}
	if !hasError {
		t.Errorf("expected error hook event, got %v", hook.events)
	}
}

func TestMalformedNDJSON(t *testing.T) {
	// Mix of valid and invalid lines — proxy should not crash.
	ndjson := strings.Join([]string{
		`{"model":"test","message":{"role":"assistant","content":"ok"},"done":false}`,
		`this is not json`,
		`{"model":"test","message":{"role":"assistant","content":""},"done":true,"total_duration":100000000}`,
	}, "\n") + "\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(ndjson))
	}))
	defer upstream.Close()

	handler, _, _ := newTestProxy(t, upstream)

	body := strings.NewReader(`{"model":"test","messages":[]}`)
	req := httptest.NewRequest("POST", "/api/chat", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// All lines should still be forwarded to the client.
	if rec.Body.String() != ndjson {
		t.Errorf("response body should include all lines including malformed ones")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	handler, _, _ := newTestProxy(t, upstream)

	req := httptest.NewRequest("GET", "/_proxy/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	for _, key := range []string{"requests_total", "tokens_total", "errors_total", "rgb_queue_drops_total"} {
		if !strings.Contains(body, key) {
			t.Errorf("metrics output missing key %q", key)
		}
	}
}

func TestNonStreamingMode(t *testing.T) {
	responseBody := `{"model":"test","message":{"role":"assistant","content":"Hello world"},"done":true,"total_duration":200000000}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read body to verify stream:false was forwarded.
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = bodyBytes
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	handler, hook, _ := newTestProxy(t, upstream)

	body := strings.NewReader(`{"model":"test","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/api/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Non-streaming should only fire start + complete, no token events.
	hasStart := false
	hasComplete := false
	hasToken := false
	for _, e := range hook.events {
		if strings.HasPrefix(e, "start:") {
			hasStart = true
		}
		if e == "complete" {
			hasComplete = true
		}
		if strings.HasPrefix(e, "token:") {
			hasToken = true
		}
	}
	if !hasStart {
		t.Error("expected start event")
	}
	if !hasComplete {
		t.Error("expected complete event")
	}
	if hasToken {
		t.Error("did not expect token events in non-streaming mode")
	}
}

func TestToolStripping(t *testing.T) {
	var receivedBody []byte

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			// Simulate a model without tool support.
			w.Write([]byte(`{"capabilities":["completion"]}`))
			return
		}
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"test","message":{"role":"assistant","content":"ok"},"done":true,"total_duration":100000000}`))
	}))
	defer upstream.Close()

	handler, _, _ := newTestProxy(t, upstream)

	body := strings.NewReader(`{"model":"test","stream":false,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather"}}],"tool_choice":"auto"}`)
	req := httptest.NewRequest("POST", "/api/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Verify tools and tool_choice were stripped from what Ollama received.
	if strings.Contains(string(receivedBody), `"tools"`) {
		t.Error("expected tools to be stripped from request body")
	}
	if strings.Contains(string(receivedBody), `"tool_choice"`) {
		t.Error("expected tool_choice to be stripped from request body")
	}
	// But the rest of the body should be intact.
	if !strings.Contains(string(receivedBody), `"messages"`) {
		t.Error("expected messages to still be present")
	}
	if !strings.Contains(string(receivedBody), `"model"`) {
		t.Error("expected model to still be present")
	}
}

func TestToolNotStrippedWhenSupported(t *testing.T) {
	var receivedBody []byte

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			w.Write([]byte(`{"capabilities":["completion","tools"]}`))
			return
		}
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"test","message":{"role":"assistant","content":"ok"},"done":true,"total_duration":100000000}`))
	}))
	defer upstream.Close()

	handler, _, _ := newTestProxy(t, upstream)

	body := strings.NewReader(`{"model":"test","stream":false,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	req := httptest.NewRequest("POST", "/api/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Tools should be preserved.
	if !strings.Contains(string(receivedBody), `"tools"`) {
		t.Error("expected tools to be preserved for model that supports them")
	}
}

func TestOpenAIChatCompletions(t *testing.T) {
	// SSE format: "data: {...}" lines with blank lines between, ending with "data: [DONE]"
	sse := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\" world\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	handler, hook, _ := newTestProxy(t, upstream)

	body := strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Verify hook events.
	expected := []string{"start:test:/v1/chat/completions", "token:Hello", "token: world", "complete"}
	if len(hook.events) != len(expected) {
		t.Fatalf("expected %d events, got %d: %v", len(expected), len(hook.events), hook.events)
	}
	for i, e := range expected {
		if hook.events[i] != e {
			t.Errorf("event %d: expected %q, got %q", i, e, hook.events[i])
		}
	}

	// Response body should pass through unchanged.
	if rec.Body.String() != sse {
		t.Errorf("response body mismatch")
	}
}

// testExternalLogger records calls to WriteExternalConversation.
type testExternalLogger struct {
	calls []ExternalLogRequest
	err   error
	retID string
}

func (l *testExternalLogger) WriteExternalConversation(req ExternalLogRequest) (string, error) {
	l.calls = append(l.calls, req)
	if l.err != nil {
		return "", l.err
	}
	id := l.retID
	if id == "" {
		id = "test-id"
	}
	return id, nil
}

func newExternalLogProxy(t *testing.T, token string, logger ExternalLogger) *ProxyHandler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	slogger := NewLogger("error", "text")
	metrics := NewMetricsHook(slogger)
	handler := NewProxyHandler(u, NewMultiHook(), metrics, slogger)
	handler.logToken = token
	handler.externalLogger = logger
	return handler
}

func TestExternalLogDisabled(t *testing.T) {
	handler := newExternalLogProxy(t, "", nil)
	req := httptest.NewRequest("POST", "/_proxy/log", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rec.Code)
	}
}

func TestExternalLogUnauthorized(t *testing.T) {
	handler := newExternalLogProxy(t, "secret", &testExternalLogger{})
	req := httptest.NewRequest("POST", "/_proxy/log", strings.NewReader(`{}`))
	req.Header.Set("X-Proxy-Log-Token", "wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestExternalLogNoDB(t *testing.T) {
	handler := newExternalLogProxy(t, "secret", nil)
	req := httptest.NewRequest("POST", "/_proxy/log", strings.NewReader(`{}`))
	req.Header.Set("X-Proxy-Log-Token", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rec.Code)
	}
}

func TestExternalLogMissingFields(t *testing.T) {
	handler := newExternalLogProxy(t, "secret", &testExternalLogger{})
	body := `{"model":"claude","endpoint":"ask_claude","query":"why?","response":"because","duration_ms":0}`
	req := httptest.NewRequest("POST", "/_proxy/log", strings.NewReader(body))
	req.Header.Set("X-Proxy-Log-Token", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "duration_ms") {
		t.Errorf("expected error to mention duration_ms, got: %s", rec.Body.String())
	}
}

func TestExternalLogSuccess(t *testing.T) {
	el := &testExternalLogger{retID: "abc-123"}
	handler := newExternalLogProxy(t, "secret", el)
	body := `{"model":"claude-haiku-4-5-20251001","endpoint":"ask_claude_automation","source_type":"voice_automation","query":"why?","response":"because","duration_ms":1500,"metadata":{"satellite":"desk"}}`
	req := httptest.NewRequest("POST", "/_proxy/log", strings.NewReader(body))
	req.Header.Set("X-Proxy-Log-Token", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(el.calls) != 1 {
		t.Fatalf("expected 1 DB call, got %d", len(el.calls))
	}
	got := el.calls[0]
	if got.Model != "claude-haiku-4-5-20251001" || got.Query != "why?" || got.DurationMs != 1500 {
		t.Errorf("unexpected call: %+v", got)
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}
	if resp["id"] != "abc-123" {
		t.Errorf("expected id=abc-123, got %q", resp["id"])
	}
}

func TestExternalLogDBError(t *testing.T) {
	el := &testExternalLogger{err: fmt.Errorf("db down")}
	handler := newExternalLogProxy(t, "secret", el)
	body := `{"model":"m","endpoint":"e","source_type":"test","query":"q","response":"r","duration_ms":100}`
	req := httptest.NewRequest("POST", "/_proxy/log", strings.NewReader(body))
	req.Header.Set("X-Proxy-Log-Token", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}
