package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	openrgb "github.com/csutorasa/go-openrgb-sdk"
)

// RGBHook implements Hook by talking to the OpenRGB server over TCP.
// A persistent connection is held for the lifetime of the hook.
type RGBHook struct {
	cfg    *RGBConfig
	logger *slog.Logger
	sdk    *rgbSDK

	// Rate-limited command channel for OnToken updates.
	// Token updates are dropped if the channel is full.
	cmdCh chan rgbCmd

	// Cancel pending return-to-idle goroutines.
	mu         sync.Mutex
	idleCancel context.CancelFunc

	// Track tokens/sec for brightness scaling.
	tokenStart time.Time
	tokenCount int
}

// rgbSDK holds the persistent client and cached device data.
type rgbSDK struct {
	client  *openrgb.Client
	devices map[int]*openrgb.ControllerData // config device index → data
}

type rgbCmd struct {
	mode  string // effect mode name ("static", "breathing", …)
	color string // RRGGBB hex, pre-scaled
}

// NewRGBHook connects to the OpenRGB server and returns a ready hook.
// Returns an error if the server is unreachable or a configured device
// index is out of range.
func NewRGBHook(cfg *RGBConfig, logger *slog.Logger) (*RGBHook, error) {
	sdk, err := dialOpenRGB(cfg, logger)
	if err != nil {
		return nil, err
	}

	h := &RGBHook{
		cfg:    cfg,
		logger: logger,
		sdk:    sdk,
		cmdCh:  make(chan rgbCmd, 1),
	}
	go h.rateLimitedExecutor()
	return h, nil
}

func dialOpenRGB(cfg *RGBConfig, logger *slog.Logger) (*rgbSDK, error) {
	client, err := openrgb.NewClientHostPort(cfg.OpenRGB.ServerHost, cfg.OpenRGB.ServerPort)
	if err != nil {
		return nil, fmt.Errorf("connect to OpenRGB server %s:%d: %w",
			cfg.OpenRGB.ServerHost, cfg.OpenRGB.ServerPort, err)
	}

	countResp, err := client.RequestControllerCount()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("request controller count: %w", err)
	}

	devices := make(map[int]*openrgb.ControllerData)
	for _, dev := range cfg.Devices {
		if uint32(dev.Index) >= countResp.Count {
			logger.Warn("device index out of range, skipping",
				"device", dev.Name, "index", dev.Index, "count", countResp.Count)
			continue
		}
		resp, err := client.RequestControllerData(uint32(dev.Index))
		if err != nil {
			client.Close()
			return nil, fmt.Errorf("request controller data for device %d: %w", dev.Index, err)
		}
		devices[dev.Index] = resp.Controller
		modeNames := make([]string, len(resp.Controller.Modes))
		for i, m := range resp.Controller.Modes {
			modeNames[i] = m.ModeName
		}
		logger.Info("openrgb device ready",
			"config_name", dev.Name,
			"device_name", resp.Controller.Name,
			"modes", modeNames,
			"zones", len(resp.Controller.Zones),
		)
	}

	return &rgbSDK{client: client, devices: devices}, nil
}

func (h *RGBHook) Close() error {
	return h.sdk.client.Close()
}

// Hook interface.

func (h *RGBHook) OnRequestStart(_ /*reqID*/, model, endpoint, _ /*userPrompt*/ string) {
	h.cancelIdle()
	select {
	case <-h.cmdCh:
	default:
	}
	h.mu.Lock()
	h.tokenStart = time.Now()
	h.tokenCount = 0
	h.mu.Unlock()

	if effect, ok := h.cfg.Effects[h.cfg.Events.RequestStart]; ok {
		h.execEffect(effect)
	}
}

func (h *RGBHook) OnToken(_ /*reqID*/, token string, tokenCount int, elapsed time.Duration) {
	h.mu.Lock()
	h.tokenCount = tokenCount
	h.mu.Unlock()

	effect, ok := h.cfg.Effects[h.cfg.Events.Token]
	if !ok {
		return
	}

	color := effect.ColorHex()
	if effect.BrightnessFrom == "tokens_per_sec" && elapsed > 0 {
		tps := float64(tokenCount) / elapsed.Seconds()
		color = scaleColor(color, scaleBrightness(tps, effect.BrightnessMin, effect.BrightnessMax))
	}

	select {
	case h.cmdCh <- rgbCmd{mode: effect.Mode, color: color}:
	default:
		h.logger.Debug("rgb: dropping token update (rate limited)")
	}
}

func (h *RGBHook) OnThinking(_ /*reqID*/, content string) {
	if effect, ok := h.cfg.Effects[h.cfg.Events.ThinkingContent]; ok {
		h.execEffect(effect)
	}
}

func (h *RGBHook) OnToolCalls(_ /*reqID*/ string, _ /*calls*/ json.RawMessage) {}

func (h *RGBHook) OnRequestComplete(_ /*reqID*/ string, duration time.Duration, totalTokens int) {
	select {
	case <-h.cmdCh:
	default:
	}

	effect, ok := h.cfg.Effects[h.cfg.Events.RequestComplete]
	if !ok {
		return
	}
	h.execEffect(effect)

	holdSec, isResponseDuration := effect.HoldDuration()
	if isResponseDuration {
		holdSec = duration.Seconds()
	}
	if holdSec < float64(effect.HoldMinSeconds) {
		holdSec = float64(effect.HoldMinSeconds)
	}
	if holdSec < 2.0 {
		holdSec = 2.0
	}
	h.scheduleIdle(time.Duration(holdSec * float64(time.Second)))
}

func (h *RGBHook) OnError(_ /*reqID*/ string, err error) {
	select {
	case <-h.cmdCh:
	default:
	}

	effect, ok := h.cfg.Effects[h.cfg.Events.Error]
	if !ok {
		return
	}
	h.execEffect(effect)

	holdSec, _ := effect.HoldDuration()
	if holdSec < 2.0 {
		holdSec = 5.0
	}
	h.scheduleIdle(time.Duration(holdSec * float64(time.Second)))
}

// execEffect applies an effect to all configured devices.
func (h *RGBHook) execEffect(effect Effect) {
	for _, dev := range h.cfg.Devices {
		h.applyEffect(dev, effect.Mode, effect.ColorHex())
	}
}

// applyEffect sends the appropriate SDK commands for one device.
//
// "breathing" → hardware Breathing mode via UpdateMode (device firmware
// handles the animation; color set in ModeColors).
// everything else → SetCustomMode + UpdateZoneLeds (direct per-LED control).
func (h *RGBHook) applyEffect(dev Device, modeName, colorHex string) {
	data, ok := h.sdk.devices[dev.Index]
	if !ok {
		return
	}
	color := hexToSDKColor(colorHex)

	switch modeName {
	case "breathing":
		h.applyBreathing(dev, data, color)
	default:
		h.applyDirect(dev, data, color)
	}
}

// applyBreathing activates the hardware Breathing mode with the given color.
func (h *RGBHook) applyBreathing(dev Device, data *openrgb.ControllerData, color openrgb.Color) {
	modeIdx, mode := findModeByName(data.Modes, "Breathing")
	if mode == nil {
		h.logger.Warn("Breathing mode not found on device, falling back to direct",
			"device", dev.Name)
		h.applyDirect(dev, data, color)
		return
	}
	h.logger.Debug("applying breathing mode",
		"device", dev.Name,
		"mode_idx", modeIdx,
		"mode_colors", len(mode.ModeColors),
		"color_mode", mode.ModeColorMode,
		"colors_min", mode.ModeColorsMin,
		"colors_max", mode.ModeColorsMax,
	)
	m := cloneMode(mode)
	if len(m.ModeColors) == 0 {
		m.ModeColors = []openrgb.Color{color}
	} else {
		for i := range m.ModeColors {
			m.ModeColors[i] = color
		}
	}
	err := h.sdk.client.RGBControllerUpdateMode(uint32(dev.Index),
		&openrgb.RGBControllerUpdateModeRequest{ModeIdx: int32(modeIdx), Mode: m})
	if err != nil {
		h.logger.Warn("UpdateMode (breathing) failed", "device", dev.Name, "error", err)
	}
}

// applyDirect uses custom/direct mode to set a solid color on the zone.
func (h *RGBHook) applyDirect(dev Device, data *openrgb.ControllerData, color openrgb.Color) {
	if err := h.sdk.client.RGBControllerSetCustomMode(uint32(dev.Index)); err != nil {
		h.logger.Warn("SetCustomMode failed", "device", dev.Name, "error", err)
		return
	}

	if dev.Zone != nil {
		zone := data.Zones[*dev.Zone]
		colors := openrgb.NewColors(int(zone.ZoneLedsCount), color)
		err := h.sdk.client.RGBControllerUpdateZoneLeds(uint32(dev.Index),
			&openrgb.RGBControllerUpdateZoneLedsRequest{
				ZoneIdx:  uint32(*dev.Zone),
				LedColor: colors,
			})
		if err != nil {
			h.logger.Warn("UpdateZoneLeds failed", "device", dev.Name, "error", err)
		}
	} else {
		colors := openrgb.NewColors(len(data.Colors), color)
		err := h.sdk.client.RGBControllerUpdateLeds(uint32(dev.Index),
			&openrgb.RGBControllerUpdateLedsRequest{LedColor: colors})
		if err != nil {
			h.logger.Warn("UpdateLeds failed", "device", dev.Name, "error", err)
		}
	}
}

// rateLimitedExecutor processes token-update commands at the configured rate.
func (h *RGBHook) rateLimitedExecutor() {
	hz := h.cfg.OpenRGB.RateLimitHz
	if hz <= 0 {
		hz = 30
	}
	interval := time.Second / time.Duration(hz)

	for cmd := range h.cmdCh {
		for _, dev := range h.cfg.Devices {
			h.applyEffect(dev, cmd.mode, cmd.color)
		}
		time.Sleep(interval)
	}
}

func (h *RGBHook) cancelIdle() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.idleCancel != nil {
		h.idleCancel()
		h.idleCancel = nil
	}
}

func (h *RGBHook) scheduleIdle(after time.Duration) {
	h.cancelIdle()
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.idleCancel = cancel
	h.mu.Unlock()

	go func() {
		select {
		case <-time.After(after):
			if effect, ok := h.cfg.Effects[h.cfg.Events.Idle]; ok {
				h.execEffect(effect)
			}
		case <-ctx.Done():
		}
	}()
}

// findModeByName is a null-byte-tolerant alternative to Modes.FindByName.
// The OpenRGB server includes a trailing \x00 in mode name strings; the SDK
// exposes them verbatim, so exact matching against a clean string fails.
func findModeByName(modes openrgb.Modes, name string) (uint32, *openrgb.Mode) {
	for i, m := range modes {
		if strings.TrimRight(m.ModeName, "\x00") == name {
			return uint32(i), m
		}
	}
	return 0, nil
}

// cloneMode returns a deep copy of a Mode so we can modify colors without
// mutating the cached ControllerData.
func cloneMode(m *openrgb.Mode) *openrgb.Mode {
	c := *m
	c.ModeColors = make([]openrgb.Color, len(m.ModeColors))
	copy(c.ModeColors, m.ModeColors)
	return &c
}

// hexToSDKColor converts a 6-char RRGGBB hex string to an openrgb.Color.
func hexToSDKColor(hex string) openrgb.Color {
	if len(hex) != 6 {
		return openrgb.ColorBlack
	}
	r, _ := strconv.ParseUint(hex[0:2], 16, 8)
	g, _ := strconv.ParseUint(hex[2:4], 16, 8)
	b, _ := strconv.ParseUint(hex[4:6], 16, 8)
	return openrgb.Color{R: uint8(r), G: uint8(g), B: uint8(b)}
}

// scaleBrightness maps tokens/sec to a brightness in [min, max].
// Below 5 tps → min; above 50 tps → max; linear between.
func scaleBrightness(tps, min, max float64) float64 {
	const lowTPS = 5.0
	const highTPS = 50.0

	if min == 0 && max == 0 {
		return 1.0
	}
	t := (tps - lowTPS) / (highTPS - lowTPS)
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return min + t*(max-min)
}

// scaleColor multiplies a hex color by a brightness factor.
func scaleColor(hex string, brightness float64) string {
	if len(hex) != 6 {
		return hex
	}
	r, _ := strconv.ParseInt(hex[0:2], 16, 64)
	g, _ := strconv.ParseInt(hex[2:4], 16, 64)
	b, _ := strconv.ParseInt(hex[4:6], 16, 64)

	clamp := func(v int64) int64 {
		if v > 255 {
			return 255
		}
		if v < 0 {
			return 0
		}
		return v
	}
	r = clamp(int64(math.Round(float64(r) * brightness)))
	g = clamp(int64(math.Round(float64(g) * brightness)))
	b = clamp(int64(math.Round(float64(b) * brightness)))
	return fmt.Sprintf("%02X%02X%02X", r, g, b)
}

func init() {
	RegisterPlugin("rgb", newRGBPlugin)
}

// newRGBPlugin builds the "rgb" plugin from config. Returns (nil, nil) if
// disabled. A configured-but-unreachable OpenRGB server is a startup error,
// not a silent no-op — the user asked for RGB, so a connection failure
// should be visible rather than swallowed.
func newRGBPlugin(cfg *Config, logger *slog.Logger) (Hook, error) {
	pc := cfg.Plugins.RGB
	if !pc.Enabled {
		return nil, nil
	}

	rgbCfg, err := LoadRGBConfig(pc.ConfigFile)
	if err != nil {
		return nil, fmt.Errorf("loading rgb config: %w", err)
	}

	hook, err := NewRGBHook(rgbCfg, logger)
	if err != nil {
		return nil, fmt.Errorf("connecting to openrgb: %w", err)
	}

	logger.Info("rgb hook ready",
		"devices", len(rgbCfg.Devices),
		"server", fmt.Sprintf("%s:%d", rgbCfg.OpenRGB.ServerHost, rgbCfg.OpenRGB.ServerPort),
	)

	return hook, nil
}
