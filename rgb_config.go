package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed rgb.yaml
var defaultRGBConfigYAML []byte

// RGBConfig is the top-level RGB proxy configuration.
type RGBConfig struct {
	Devices []Device          `yaml:"devices"`
	OpenRGB OpenRGBConfig     `yaml:"openrgb"`
	Effects map[string]Effect `yaml:"effects"`
	Events  EventMap          `yaml:"events"`
}

// Device describes an OpenRGB hardware target.
type Device struct {
	Name  string `yaml:"name"`
	Index int    `yaml:"index"`
	Zone  *int   `yaml:"zone,omitempty"`
	Size  *int   `yaml:"size,omitempty"`
}

// OpenRGBConfig controls how the OpenRGB server is reached.
type OpenRGBConfig struct {
	ServerHost  string `yaml:"server_host"`
	ServerPort  int    `yaml:"server_port"`
	RateLimitHz int    `yaml:"rate_limit_hz"`
}

// Effect describes an RGB lighting effect.
type Effect struct {
	Mode           string  `yaml:"mode"`
	Color          string  `yaml:"color"`
	BrightnessFrom string  `yaml:"brightness_from,omitempty"`
	BrightnessMin  float64 `yaml:"brightness_min,omitempty"`
	BrightnessMax  float64 `yaml:"brightness_max,omitempty"`
	HoldSeconds    any     `yaml:"hold_seconds,omitempty"` // float64 or "response_duration"
	HoldMinSeconds float64 `yaml:"hold_min_seconds,omitempty"`
}

// EventMap maps proxy lifecycle events to effect names.
type EventMap struct {
	RequestStart    string `yaml:"request_start"`
	Token           string `yaml:"token"`
	ThinkingContent string `yaml:"thinking_content"`
	RequestComplete string `yaml:"request_complete"`
	Error           string `yaml:"error"`
	Idle            string `yaml:"idle"`
}

// LoadRGBConfig loads RGB configuration from the given path. If path is
// empty, it searches the default locations. Falls back to embedded defaults.
func LoadRGBConfig(path string) (*RGBConfig, error) {
	// Start with embedded defaults.
	var cfg RGBConfig
	if err := yaml.Unmarshal(defaultRGBConfigYAML, &cfg); err != nil {
		return nil, fmt.Errorf("parsing embedded defaults: %w", err)
	}

	// Find the config file.
	data, found := findRGBConfigFile(path)
	if found {
		// Overlay user config on top of defaults.
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return &cfg, nil
}

// findRGBConfigFile returns the RGB config file contents and true if found.
func findRGBConfigFile(path string) ([]byte, bool) {
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, true
		}
		return nil, false
	}

	// Check env var.
	if envPath := os.Getenv("OLLAMA_RGB_CONFIG"); envPath != "" {
		data, err := os.ReadFile(envPath)
		if err == nil {
			return data, true
		}
	}

	// Search default locations.
	searchPaths := []string{
		"./rgb.yaml",
	}
	if home, err := os.UserHomeDir(); err == nil {
		searchPaths = append(searchPaths, filepath.Join(home, ".config", "ollama-hook", "rgb.yaml"))
	}

	for _, p := range searchPaths {
		data, err := os.ReadFile(p)
		if err == nil {
			return data, true
		}
	}

	return nil, false
}

var hexColorRegex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Validate checks the config for consistency.
func (c *RGBConfig) Validate() error {
	if len(c.Devices) == 0 {
		return fmt.Errorf("no devices configured")
	}

	for name, effect := range c.Effects {
		if effect.Mode == "" {
			return fmt.Errorf("effect %q: mode is required", name)
		}
		if effect.Color == "" {
			return fmt.Errorf("effect %q: color is required", name)
		}
		if !hexColorRegex.MatchString(effect.Color) {
			return fmt.Errorf("effect %q: color %q must be #RRGGBB format", name, effect.Color)
		}
	}

	// Check that all event mappings reference existing effects.
	eventRefs := map[string]string{
		"request_start":    c.Events.RequestStart,
		"token":            c.Events.Token,
		"thinking_content": c.Events.ThinkingContent,
		"request_complete": c.Events.RequestComplete,
		"error":            c.Events.Error,
		"idle":             c.Events.Idle,
	}
	for event, effectName := range eventRefs {
		if effectName == "" {
			continue // unmapped events are no-ops
		}
		if _, ok := c.Effects[effectName]; !ok {
			return fmt.Errorf("event %q references unknown effect %q", event, effectName)
		}
	}

	return nil
}

// ColorHex returns the color without the # prefix, suitable for openrgb CLI.
func (e *Effect) ColorHex() string {
	return strings.TrimPrefix(e.Color, "#")
}

// HoldDuration returns the hold duration for the effect. If hold_seconds is
// "response_duration", it returns 0 and true (meaning use response duration).
// Otherwise returns the fixed duration and false.
func (e *Effect) HoldDuration() (float64, bool) {
	switch v := e.HoldSeconds.(type) {
	case string:
		if v == "response_duration" {
			return 0, true
		}
		return 0, false
	case float64:
		return v, false
	case int:
		return float64(v), false
	default:
		return 0, false
	}
}
