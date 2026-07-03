package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// stubHook is a minimal Hook for exercising the plugin registry without
// touching real plugin implementations (db/rgb need live services).
type stubHook struct{ name string }

func (s *stubHook) OnRequestStart(string, string, string, string) {}
func (s *stubHook) OnToken(string, string, int, time.Duration)    {}
func (s *stubHook) OnThinking(string, string)                     {}
func (s *stubHook) OnToolCalls(string, json.RawMessage)           {}
func (s *stubHook) OnRequestComplete(string, time.Duration, int)  {}
func (s *stubHook) OnError(string, error)                         {}

// withCleanRegistry snapshots and restores pluginRegistry so tests can
// register stub plugins without leaking into other tests or the real
// db/rgb registrations done via init().
func withCleanRegistry(t *testing.T) {
	t.Helper()
	saved := pluginRegistry
	pluginRegistry = map[string]PluginFactory{}
	t.Cleanup(func() { pluginRegistry = saved })
}

func TestBuildPluginsSkipsDisabled(t *testing.T) {
	withCleanRegistry(t)
	RegisterPlugin("stub", func(cfg *Config, logger *slog.Logger) (Hook, error) {
		return nil, nil // disabled
	})

	hooks, err := BuildPlugins(defaultConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("BuildPlugins failed: %v", err)
	}
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks from a disabled plugin, got %d", len(hooks))
	}
}

func TestBuildPluginsIncludesEnabled(t *testing.T) {
	withCleanRegistry(t)
	RegisterPlugin("stub", func(cfg *Config, logger *slog.Logger) (Hook, error) {
		return &stubHook{name: "stub"}, nil
	})

	hooks, err := BuildPlugins(defaultConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("BuildPlugins failed: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}
	if hooks[0].(*stubHook).name != "stub" {
		t.Errorf("unexpected hook: %+v", hooks[0])
	}
}

func TestBuildPluginsPropagatesError(t *testing.T) {
	withCleanRegistry(t)
	RegisterPlugin("broken", func(cfg *Config, logger *slog.Logger) (Hook, error) {
		return nil, errors.New("boom")
	})

	_, err := BuildPlugins(defaultConfig(), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected BuildPlugins to propagate the plugin's error")
	}
}

func TestRegisterPluginPanicsOnDuplicate(t *testing.T) {
	withCleanRegistry(t)
	RegisterPlugin("dup", func(cfg *Config, logger *slog.Logger) (Hook, error) { return nil, nil })

	defer func() {
		if recover() == nil {
			t.Error("expected RegisterPlugin to panic on duplicate name")
		}
	}()
	RegisterPlugin("dup", func(cfg *Config, logger *slog.Logger) (Hook, error) { return nil, nil })
}

func TestDBAndRGBPluginsRegistered(t *testing.T) {
	// Guards against accidentally dropping the init()-based registration
	// in database_hook.go / openrgb.go during future refactors.
	for _, name := range []string{"db", "rgb"} {
		if _, ok := pluginRegistry[name]; !ok {
			t.Errorf("expected plugin %q to be registered", name)
		}
	}
}
