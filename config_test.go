package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadHookConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig with defaults failed: %v", err)
	}

	if cfg.Upstream != "http://127.0.0.1:11434" {
		t.Errorf("expected default upstream, got %q", cfg.Upstream)
	}
	if cfg.Listen != ":11435" {
		t.Errorf("expected default listen :11435, got %q", cfg.Listen)
	}
	if cfg.Plugins.DB.Enabled || cfg.Plugins.RGB.Enabled || cfg.Plugins.Webhook.Enabled {
		t.Errorf("expected all plugins disabled by default, got %+v", cfg.Plugins)
	}
}

func TestLoadHookConfigOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ollama-hook.yaml")
	os.WriteFile(path, []byte(`
listen: ":9999"
plugins:
  db:
    enabled: true
    url: "postgres://example/db"
  rgb:
    enabled: true
    config_file: "/etc/ollama-hook/rgb.yaml"
`), 0644)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with override failed: %v", err)
	}

	if cfg.Listen != ":9999" {
		t.Errorf("expected overridden listen :9999, got %q", cfg.Listen)
	}
	if !cfg.Plugins.DB.Enabled || cfg.Plugins.DB.URL != "postgres://example/db" {
		t.Errorf("expected db plugin enabled with url, got %+v", cfg.Plugins.DB)
	}
	if !cfg.Plugins.RGB.Enabled || cfg.Plugins.RGB.ConfigFile != "/etc/ollama-hook/rgb.yaml" {
		t.Errorf("expected rgb plugin enabled with config_file, got %+v", cfg.Plugins.RGB)
	}
	// Upstream wasn't set in the override, so the default should survive.
	if cfg.Upstream != "http://127.0.0.1:11434" {
		t.Errorf("expected default upstream to survive partial override, got %q", cfg.Upstream)
	}
}

func TestLoadHookConfigEnvOverrides(t *testing.T) {
	t.Setenv("OLLAMA_UPSTREAM", "http://example:1234")
	t.Setenv("PROXY_LISTEN", ":8080")
	t.Setenv("DATABASE_URL", "postgres://env/db")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Upstream != "http://example:1234" {
		t.Errorf("expected env-overridden upstream, got %q", cfg.Upstream)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("expected env-overridden listen, got %q", cfg.Listen)
	}
	if cfg.Plugins.DB.URL != "postgres://env/db" {
		t.Errorf("expected env-overridden db url, got %q", cfg.Plugins.DB.URL)
	}
}

func TestLoadHookConfigEnvOverridesFileConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ollama-hook.yaml")
	os.WriteFile(path, []byte(`upstream: "http://from-file:1111"`), 0644)

	t.Setenv("OLLAMA_UPSTREAM", "http://from-env:2222")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Upstream != "http://from-env:2222" {
		t.Errorf("expected env var to win over file config, got %q", cfg.Upstream)
	}
}
