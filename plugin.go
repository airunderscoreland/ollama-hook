package main

import (
	"fmt"
	"log/slog"
	"sort"
)

// PluginFactory builds a Hook from the loaded config. It returns (nil, nil)
// if the plugin is disabled or not configured — BuildPlugins skips nil
// hooks. A non-nil error aborts startup.
//
// Third-party plugins register a factory via RegisterPlugin, typically from
// an init() function in their own file:
//
//	func init() {
//	    RegisterPlugin("webhook", newWebhookPlugin)
//	}
//
//	func newWebhookPlugin(cfg *Config, logger *slog.Logger) (Hook, error) {
//	    if !cfg.Plugins.Webhook.Enabled {
//	        return nil, nil
//	    }
//	    return NewWebhookHook(cfg.Plugins.Webhook, logger), nil
//	}
//
// A hook that also implements ExternalLogger or io.Closer is picked up
// automatically by main.go — no separate wiring needed.
type PluginFactory func(cfg *Config, logger *slog.Logger) (Hook, error)

var pluginRegistry = map[string]PluginFactory{}

// RegisterPlugin adds a plugin factory under the given name. Call it from
// init() in the plugin's own file. Panics on a duplicate name — that's a
// programming error caught at startup, not a runtime condition to recover from.
func RegisterPlugin(name string, factory PluginFactory) {
	if _, exists := pluginRegistry[name]; exists {
		panic(fmt.Sprintf("plugin %q already registered", name))
	}
	pluginRegistry[name] = factory
}

// BuildPlugins runs every registered plugin factory in a stable (sorted by
// name) order and returns the hooks that opted in. Log-only and metrics
// hooks are always-on and are not part of this registry; see main.go.
func BuildPlugins(cfg *Config, logger *slog.Logger) ([]Hook, error) {
	names := make([]string, 0, len(pluginRegistry))
	for name := range pluginRegistry {
		names = append(names, name)
	}
	sort.Strings(names)

	var hooks []Hook
	for _, name := range names {
		hook, err := pluginRegistry[name](cfg, logger)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: %w", name, err)
		}
		if hook == nil {
			continue
		}
		logger.Info("plugin enabled", "plugin", name)
		hooks = append(hooks, hook)
	}
	return hooks, nil
}
