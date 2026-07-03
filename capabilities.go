package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ModelCapabilities caches model capabilities from Ollama's /api/show endpoint.
type ModelCapabilities struct {
	upstream *url.URL
	client   *http.Client
	logger   *slog.Logger

	mu    sync.RWMutex
	cache map[string][]string // model name → capabilities list
}

func NewModelCapabilities(upstream *url.URL, client *http.Client, logger *slog.Logger) *ModelCapabilities {
	return &ModelCapabilities{
		upstream: upstream,
		client:   client,
		logger:   logger,
		cache:    make(map[string][]string),
	}
}

// SupportsTools returns true if the model has "tools" in its capabilities.
func (mc *ModelCapabilities) SupportsTools(model string) bool {
	caps := mc.getCapabilities(model)
	for _, c := range caps {
		if c == "tools" {
			return true
		}
	}
	return false
}

func (mc *ModelCapabilities) getCapabilities(model string) []string {
	// Check cache first.
	mc.mu.RLock()
	caps, ok := mc.cache[model]
	mc.mu.RUnlock()
	if ok {
		return caps
	}

	// Fetch from Ollama.
	caps = mc.fetchCapabilities(model)

	mc.mu.Lock()
	mc.cache[model] = caps
	mc.mu.Unlock()

	return caps
}

type showResponse struct {
	Capabilities []string `json:"capabilities"`
}

func (mc *ModelCapabilities) fetchCapabilities(model string) []string {
	u := *mc.upstream
	u.Path = "/api/show"

	body := fmt.Sprintf(`{"model":%q}`, model)
	resp, err := mc.client.Post(u.String(), "application/json", strings.NewReader(body))
	if err != nil {
		mc.logger.Warn("failed to fetch model capabilities", "model", model, "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		mc.logger.Warn("unexpected status from /api/show", "model", model, "status", resp.StatusCode)
		return nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		mc.logger.Warn("failed to read /api/show response", "model", model, "error", err)
		return nil
	}

	var show showResponse
	if err := json.Unmarshal(data, &show); err != nil {
		mc.logger.Warn("failed to parse /api/show response", "model", model, "error", err)
		return nil
	}

	mc.logger.Info("fetched model capabilities", "model", model, "capabilities", show.Capabilities)
	return show.Capabilities
}
