package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// ExternalLogger is satisfied by *DatabaseHook; extracted as an interface for testing.
type ExternalLogger interface {
	WriteExternalConversation(ExternalLogRequest) (string, error)
}

// ProxyHandler handles all incoming requests. Streaming endpoints
// (/api/chat, /api/generate) are intercepted for NDJSON parsing.
// Everything else passes through via reverse proxy.
type ProxyHandler struct {
	upstream       *url.URL
	reverseProxy   *httputil.ReverseProxy
	client         *http.Client
	hook           Hook
	metrics        *MetricsHook
	capabilities   *ModelCapabilities
	logger         *slog.Logger
	logToken       string
	externalLogger ExternalLogger
}

// NewProxyHandler creates a new handler that proxies to the given upstream URL.
func NewProxyHandler(upstream *url.URL, hook Hook, metrics *MetricsHook, logger *slog.Logger) *ProxyHandler {
	rp := httputil.NewSingleHostReverseProxy(upstream)

	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = upstream.Host
	}

	client := &http.Client{
		// No timeout — streaming responses can last minutes.
		Transport: &http.Transport{
			DisableCompression: true,
		},
	}

	return &ProxyHandler{
		upstream:     upstream,
		reverseProxy: rp,
		client:       client,
		hook:         hook,
		metrics:      metrics,
		capabilities: NewModelCapabilities(upstream, client, logger),
		logger:       logger,
	}
}

// endpointType classifies the request for interception.
type endpointType int

const (
	endpointPassthrough endpointType = iota
	endpointOllamaChat
	endpointOllamaGenerate
	endpointOpenAIChat
)

func classifyEndpoint(method, path string) endpointType {
	if method != http.MethodPost {
		return endpointPassthrough
	}
	switch path {
	case "/api/chat":
		return endpointOllamaChat
	case "/api/generate":
		return endpointOllamaGenerate
	case "/v1/chat/completions":
		return endpointOpenAIChat
	default:
		return endpointPassthrough
	}
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Intercept /_proxy/* paths before they hit the reverse proxy.
	if strings.HasPrefix(r.URL.Path, "/_proxy/") {
		if r.URL.Path == "/_proxy/metrics" && r.Method == http.MethodGet {
			p.metrics.ServeMetrics(w, r)
			return
		}
		if r.URL.Path == "/_proxy/log" && r.Method == http.MethodPost {
			p.handleExternalLog(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	id := reqID()
	reqLogger := p.logger.With("req_id", id, "method", r.Method, "path", r.URL.Path)

	epType := classifyEndpoint(r.Method, r.URL.Path)
	if epType != endpointPassthrough {
		reqLogger.Info("intercepting streaming request")
		p.handleStreaming(w, r, id, reqLogger, epType)
		return
	}

	reqLogger.Info("passthrough request")
	p.reverseProxy.ServeHTTP(w, r)
}

// NDJSON line structs — minimal, only the fields we care about.

type messageContent struct {
	str string
}

func (m *messageContent) UnmarshalJSON(b []byte) error {
	// content can be a plain string or an array of content parts
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &m.str)
	}
	// array of {type, text} parts — concatenate text parts
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return nil // ignore unknown shapes
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	m.str = sb.String()
	return nil
}

type requestBody struct {
	Model    string `json:"model"`
	Stream   *bool  `json:"stream"`
	Prompt   string `json:"prompt"` // /api/generate
	Messages []struct {
		Role    string         `json:"role"`
		Content messageContent `json:"content"`
	} `json:"messages"` // /api/chat, /v1/chat/completions
}

// extractUserPrompt pulls the user-visible prompt text out of a parsed request.
// For chat endpoints it returns the last user-role message; for generate it
// returns the prompt string directly.
func extractUserPrompt(body requestBody, epType endpointType) string {
	switch epType {
	case endpointOllamaGenerate:
		return body.Prompt
	case endpointOllamaChat, endpointOpenAIChat:
		for i := len(body.Messages) - 1; i >= 0; i-- {
			if body.Messages[i].Role == "user" {
				return body.Messages[i].Content.str
			}
		}
	}
	return ""
}

type chatStreamLine struct {
	Message struct {
		Content   string          `json:"content"`
		Thinking  string          `json:"thinking"`
		ToolCalls json.RawMessage `json:"tool_calls"`
	} `json:"message"`
	Done          bool  `json:"done"`
	TotalDuration int64 `json:"total_duration"` // nanoseconds
}

type generateStreamLine struct {
	Response      string `json:"response"`
	Thinking      string `json:"thinking"`
	Done          bool   `json:"done"`
	TotalDuration int64  `json:"total_duration"` // nanoseconds
}

// OpenAI-compatible SSE chunk format.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// maybeStripTools removes tools/tool_choice from the request body if the
// target model doesn't support tool use. Returns the (possibly modified) body.
func (p *ProxyHandler) maybeStripTools(body []byte, model string, logger *slog.Logger) []byte {
	// Quick check: does the body even contain "tools"?
	if !bytes.Contains(body, []byte(`"tools"`)) {
		return body
	}

	// Check if model supports tools.
	if p.capabilities.SupportsTools(model) {
		return body
	}

	// Strip tools and tool_choice by unmarshalling to a generic map.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		logger.Warn("failed to parse request body for tool stripping", "error", err)
		return body
	}

	stripped := false
	for _, key := range []string{"tools", "tool_choice"} {
		if _, ok := raw[key]; ok {
			delete(raw, key)
			stripped = true
		}
	}

	if !stripped {
		return body
	}

	newBody, err := json.Marshal(raw)
	if err != nil {
		logger.Warn("failed to re-marshal request body after tool stripping", "error", err)
		return body
	}

	logger.Info("stripped tools from request (model does not support tools)", "model", model)
	return newBody
}

func (p *ProxyHandler) handleStreaming(w http.ResponseWriter, r *http.Request, id string, logger *slog.Logger, epType endpointType) {
	startTime := time.Now()

	// Read the request body to peek at model and stream fields.
	bodyBytes, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		logger.Error("failed to read request body", "error", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		p.hook.OnError(id, err)
		return
	}

	var reqBody requestBody
	json.Unmarshal(bodyBytes, &reqBody) // best-effort parse

	// Strip tools from request if the model doesn't support them.
	bodyBytes = p.maybeStripTools(bodyBytes, reqBody.Model, logger)

	// Check if streaming is explicitly disabled.
	isStreaming := true
	if reqBody.Stream != nil && !*reqBody.Stream {
		isStreaming = false
	}

	logger = logger.With("model", reqBody.Model, "streaming", isStreaming)

	p.hook.OnRequestStart(id, reqBody.Model, r.URL.Path, extractUserPrompt(reqBody, epType))

	// Build the upstream request.
	upstreamURL := *p.upstream
	upstreamURL.Path = r.URL.Path
	upstreamURL.RawQuery = r.URL.RawQuery

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		logger.Error("failed to create upstream request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		p.hook.OnError(id, err)
		return
	}

	// Copy headers from client to upstream.
	for k, vv := range r.Header {
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}

	// Forward request to Ollama.
	resp, err := p.client.Do(upReq)
	if err != nil {
		logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		p.hook.OnError(id, err)
		return
	}
	defer resp.Body.Close()

	// Copy response headers to client.
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		logger.Error("upstream returned error", "status", resp.StatusCode)
		io.Copy(w, resp.Body)
		p.hook.OnError(id, fmt.Errorf("upstream status %d", resp.StatusCode))
		return
	}

	if !isStreaming {
		// Non-streaming: read full body, forward, fire completion.
		body, _ := io.ReadAll(resp.Body)
		w.Write(body)
		elapsed := time.Since(startTime)
		logger.Info("non-streaming response complete", "duration", elapsed)
		p.hook.OnRequestComplete(id, elapsed, 0)
		return
	}

	// Streaming: scan lines from upstream.
	flusher, ok := w.(http.Flusher)
	if !ok {
		logger.Warn("ResponseWriter does not support flushing")
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	tokenCount := 0

	for scanner.Scan() {
		line := scanner.Bytes()

		// Write to client first — zero added latency.
		w.Write(line)
		w.Write([]byte("\n"))
		if ok {
			flusher.Flush()
		}

		// Then parse and fire hooks based on endpoint type.
		switch epType {
		case endpointOllamaChat:
			var chunk chatStreamLine
			if err := json.Unmarshal(line, &chunk); err != nil {
				logger.Warn("malformed NDJSON line", "error", err)
				continue
			}
			if chunk.Message.Thinking != "" {
				p.hook.OnThinking(id, chunk.Message.Thinking)
			}
			if chunk.Message.Content != "" {
				tokenCount++
				p.hook.OnToken(id, chunk.Message.Content, tokenCount, time.Since(startTime))
			}
			if len(chunk.Message.ToolCalls) > 0 && string(chunk.Message.ToolCalls) != "null" {
				p.hook.OnToolCalls(id, chunk.Message.ToolCalls)
			}
			if chunk.Done {
				duration := time.Duration(chunk.TotalDuration)
				logger.Info("stream complete", "tokens", tokenCount, "total_duration", duration)
				p.hook.OnRequestComplete(id, duration, tokenCount)
			}

		case endpointOllamaGenerate:
			var chunk generateStreamLine
			if err := json.Unmarshal(line, &chunk); err != nil {
				logger.Warn("malformed NDJSON line", "error", err)
				continue
			}
			if chunk.Thinking != "" {
				p.hook.OnThinking(id, chunk.Thinking)
			}
			if chunk.Response != "" {
				tokenCount++
				p.hook.OnToken(id, chunk.Response, tokenCount, time.Since(startTime))
			}
			if chunk.Done {
				duration := time.Duration(chunk.TotalDuration)
				logger.Info("stream complete", "tokens", tokenCount, "total_duration", duration)
				p.hook.OnRequestComplete(id, duration, tokenCount)
			}

		case endpointOpenAIChat:
			// SSE format: "data: {...}" lines with blank lines between.
			// Also "data: [DONE]" as the final sentinel.
			lineStr := string(line)
			if lineStr == "" || lineStr == "\r" {
				continue // blank separator line
			}
			if !strings.HasPrefix(lineStr, "data: ") {
				continue
			}
			data := strings.TrimPrefix(lineStr, "data: ")
			if data == "[DONE]" {
				elapsed := time.Since(startTime)
				logger.Info("stream complete", "tokens", tokenCount, "total_duration", elapsed)
				p.hook.OnRequestComplete(id, elapsed, tokenCount)
				continue
			}
			var chunk openAIChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				logger.Warn("malformed SSE data", "error", err)
				continue
			}
			if len(chunk.Choices) > 0 {
				delta := chunk.Choices[0].Delta
				if delta.ReasoningContent != "" {
					p.hook.OnThinking(id, delta.ReasoningContent)
				}
				if delta.Content != "" {
					tokenCount++
					p.hook.OnToken(id, delta.Content, tokenCount, time.Since(startTime))
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Error("stream read error", "error", err)
		p.hook.OnError(id, err)
	}
}

func (p *ProxyHandler) handleExternalLog(w http.ResponseWriter, r *http.Request) {
	if p.logToken == "" {
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	if r.Header.Get("X-Proxy-Log-Token") != p.logToken {
		p.logger.Warn("external log: unauthorized attempt", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if p.externalLogger == nil {
		http.Error(w, "database not enabled", http.StatusNotImplemented)
		return
	}

	var req ExternalLogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	var missing []string
	if req.Model == "" {
		missing = append(missing, "model")
	}
	if req.Endpoint == "" {
		missing = append(missing, "endpoint")
	}
	if req.SourceType == "" {
		missing = append(missing, "source_type")
	}
	if req.Query == "" {
		missing = append(missing, "query")
	}
	if req.Response == "" {
		missing = append(missing, "response")
	}
	if req.DurationMs <= 0 {
		missing = append(missing, "duration_ms")
	}
	if len(missing) > 0 {
		http.Error(w, "missing required fields: "+strings.Join(missing, ", "), http.StatusBadRequest)
		return
	}

	id, err := p.externalLogger.WriteExternalConversation(req)
	if err != nil {
		p.logger.Error("external log: write failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}
