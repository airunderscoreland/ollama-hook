package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the top-level ollama-hook configuration (ollama-hook.yaml).
type Config struct {
	Upstream  string        `yaml:"upstream"`
	Listen    string        `yaml:"listen"`
	LogLevel  string        `yaml:"log_level"`
	LogFormat string        `yaml:"log_format"`
	Plugins   PluginsConfig `yaml:"plugins"`
}

// PluginsConfig holds the per-plugin config blocks. Each plugin is opt-in
// via its own "enabled" flag; see plugin.go for how these are consumed.
type PluginsConfig struct {
	DB      DBPluginConfig      `yaml:"db"`
	RGB     RGBPluginConfig     `yaml:"rgb"`
	Webhook WebhookPluginConfig `yaml:"webhook"`
}

// DBPluginConfig configures the "db" plugin (PostgreSQL conversation logging).
type DBPluginConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`      // DATABASE_URL
	URLFile string `yaml:"url_file"` // DATABASE_URL_FILE (systemd-creds path)
}

// RGBPluginConfig configures the "rgb" plugin (OpenRGB lighting effects).
type RGBPluginConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ConfigFile string `yaml:"config_file"` // path to rgb.yaml; searches default dirs if empty
}

// WebhookPluginConfig configures the "webhook" plugin (POST event payloads to a URL).
type WebhookPluginConfig struct {
	Enabled bool     `yaml:"enabled"`
	URL     string   `yaml:"url"`
	Events  []string `yaml:"events"`
}

// defaultConfig returns the built-in defaults, matching DEVELOPMENT-PLAN.md.
func defaultConfig() *Config {
	return &Config{
		Upstream:  "http://127.0.0.1:11434",
		Listen:    ":11435",
		LogLevel:  "info",
		LogFormat: "text",
		Plugins: PluginsConfig{
			Webhook: WebhookPluginConfig{
				Events: []string{"request_start", "request_complete", "error"},
			},
		},
	}
}

// LoadConfig loads the generic proxy config. Search order: explicit path →
// OLLAMA_HOOK_CONFIG env → ./ollama-hook.yaml → ~/.config/ollama-hook/ollama-hook.yaml.
// Falls back to built-in defaults if no file is found. Select env vars
// (OLLAMA_UPSTREAM, PROXY_LISTEN, LOG_LEVEL, LOG_FORMAT, DATABASE_URL,
// DATABASE_URL_FILE) override the loaded config, matching the precedence
// documented in DEVELOPMENT-PLAN.md.
func LoadConfig(path string) (*Config, error) {
	cfg := defaultConfig()

	data, found := findHookConfigFile(path)
	if found {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	cfg.applyEnvOverrides()

	return cfg, nil
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("OLLAMA_UPSTREAM"); v != "" {
		c.Upstream = v
	}
	if v := os.Getenv("PROXY_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		c.LogFormat = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		c.Plugins.DB.URL = v
	}
	if v := os.Getenv("DATABASE_URL_FILE"); v != "" {
		c.Plugins.DB.URLFile = v
	}
}

// findHookConfigFile returns the ollama-hook.yaml contents and true if found.
func findHookConfigFile(path string) ([]byte, bool) {
	if path != "" {
		data, err := os.ReadFile(path)
		return data, err == nil
	}

	if envPath := os.Getenv("OLLAMA_HOOK_CONFIG"); envPath != "" {
		data, err := os.ReadFile(envPath)
		if err == nil {
			return data, true
		}
	}

	searchPaths := []string{"./ollama-hook.yaml"}
	if home, err := os.UserHomeDir(); err == nil {
		searchPaths = append(searchPaths, filepath.Join(home, ".config", "ollama-hook", "ollama-hook.yaml"))
	}

	for _, p := range searchPaths {
		data, err := os.ReadFile(p)
		if err == nil {
			return data, true
		}
	}

	return nil, false
}
