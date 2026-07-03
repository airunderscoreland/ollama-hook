package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadRGBConfig("")
	if err != nil {
		t.Fatalf("LoadConfig with defaults failed: %v", err)
	}

	if len(cfg.Devices) == 0 {
		t.Fatal("expected at least one device")
	}
	if cfg.Devices[0].Name != "fans" {
		t.Errorf("expected device name 'fans', got %q", cfg.Devices[0].Name)
	}
	if cfg.OpenRGB.ServerHost != "localhost" {
		t.Errorf("expected server_host 'localhost', got %q", cfg.OpenRGB.ServerHost)
	}
	if cfg.OpenRGB.ServerPort != 6742 {
		t.Errorf("expected server_port 6742, got %d", cfg.OpenRGB.ServerPort)
	}
	if cfg.OpenRGB.RateLimitHz != 30 {
		t.Errorf("expected rate_limit_hz 30, got %d", cfg.OpenRGB.RateLimitHz)
	}
	if cfg.Events.Idle != "idle" {
		t.Errorf("expected idle event mapped to 'idle', got %q", cfg.Events.Idle)
	}

	// Check an effect.
	idle, ok := cfg.Effects["idle"]
	if !ok {
		t.Fatal("expected 'idle' effect")
	}
	if idle.Color != "#808080" {
		t.Errorf("expected idle color '#808080', got %q", idle.Color)
	}
	if idle.ColorHex() != "808080" {
		t.Errorf("expected ColorHex '808080', got %q", idle.ColorHex())
	}
}

func TestLoadConfigOverride(t *testing.T) {
	// Write a partial override that changes the idle color.
	dir := t.TempDir()
	overridePath := filepath.Join(dir, "custom.yaml")
	os.WriteFile(overridePath, []byte(`
effects:
  idle:
    mode: static
    color: "#FF0000"
`), 0644)

	cfg, err := LoadRGBConfig(overridePath)
	if err != nil {
		t.Fatalf("LoadConfig with override failed: %v", err)
	}

	idle, ok := cfg.Effects["idle"]
	if !ok {
		t.Fatal("expected 'idle' effect after override")
	}
	if idle.Color != "#FF0000" {
		t.Errorf("expected overridden idle color '#FF0000', got %q", idle.Color)
	}
}

func TestValidationBadColor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	os.WriteFile(path, []byte(`
effects:
  idle:
    mode: static
    color: "not-a-color"
`), 0644)

	_, err := LoadRGBConfig(path)
	if err == nil {
		t.Fatal("expected validation error for bad color")
	}
}

func TestValidationBadEventRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	os.WriteFile(path, []byte(`
events:
  idle: nonexistent_effect
`), 0644)

	_, err := LoadRGBConfig(path)
	if err == nil {
		t.Fatal("expected validation error for bad event reference")
	}
}

func TestHoldDuration(t *testing.T) {
	// String sentinel
	e := Effect{HoldSeconds: "response_duration"}
	_, isResponseDuration := e.HoldDuration()
	if !isResponseDuration {
		t.Error("expected response_duration sentinel")
	}

	// Fixed duration
	e = Effect{HoldSeconds: 5.0}
	dur, isRD := e.HoldDuration()
	if isRD {
		t.Error("expected fixed duration, not response_duration")
	}
	if dur != 5.0 {
		t.Errorf("expected 5.0, got %f", dur)
	}

	// Integer (YAML may parse as int)
	e = Effect{HoldSeconds: 3}
	dur, _ = e.HoldDuration()
	if dur != 3.0 {
		t.Errorf("expected 3.0, got %f", dur)
	}
}
