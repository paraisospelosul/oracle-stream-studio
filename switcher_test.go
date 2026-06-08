package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSwitcher_Config(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_config_test.json")

	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "http://localhost/stats", tmpDir, configPath)

	// Verify defaults
	cfg := switcher.GetConfig()
	if cfg.SRTAddr != "127.0.0.1:1234" {
		t.Errorf("Expected SRTAddr '127.0.0.1:1234', got %s", cfg.SRTAddr)
	}

	// Update config
	cfg.SRTAddr = "192.168.1.100:9999"
	cfg.MinBitrateKbps = 1500
	switcher.UpdateConfig(cfg)

	// Verify update
	updated := switcher.GetConfig()
	if updated.SRTAddr != "192.168.1.100:9999" || updated.MinBitrateKbps != 1500 {
		t.Errorf("Config was not updated correctly: %+v", updated)
	}

	// Verify saved to file
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}
	var saved SwitcherConfig
	err = json.Unmarshal(data, &saved)
	if err != nil {
		t.Fatalf("Failed to parse saved config: %v", err)
	}
	if saved.SRTAddr != "192.168.1.100:9999" {
		t.Errorf("Saved config has incorrect address: %s", saved.SRTAddr)
	}

	// Update fallback path
	switcher.UpdateFallbackPath("/opt/custom/queda.mp4")
	if switcher.GetConfig().FallbackPath != "/opt/custom/queda.mp4" {
		t.Errorf("Fallback path not updated: %s", switcher.GetConfig().FallbackPath)
	}
}

func TestSwitcher_StateTransitions(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_state_test.json")
	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "", tmpDir, configPath)

	// Initial state is fallback
	if switcher.GetState() != StateFallback {
		t.Errorf("Expected initial state StateFallback, got %s", switcher.GetState())
	}

	// Move to StateLive
	switcher.setStateWithReason(StateLive, "SRT connected successfully")
	if switcher.GetState() != StateLive {
		t.Errorf("Expected state StateLive, got %s", switcher.GetState())
	}

	// Check switch count and history
	stats := switcher.GetStats()
	if stats.SwitchCount != 1 {
		t.Errorf("Expected switch count 1, got %d", stats.SwitchCount)
	}

	history := switcher.GetSwitchHistory()
	if len(history) != 1 {
		t.Fatalf("Expected history length 1, got %d", len(history))
	}

	event := history[0]
	if event.From != "backup" || event.To != "live" || event.Reason != "SRT connected successfully" {
		t.Errorf("History event is incorrect: %+v", event)
	}
}

func TestSwitcher_BitrateHistory(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_bitrate_test.json")
	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "", tmpDir, configPath)

	// Populate dummy bitrate history entries
	switcher.bitrateHistoryMu.Lock()
	for i := 0; i < 60; i++ {
		switcher.bitrateHistory[i] = float64(1000 + i*10)
	}
	switcher.bitrateHistoryIdx = 60
	switcher.bitrateHistoryLen = 60
	switcher.bitrateHistoryMu.Unlock()

	// Get history for 1m (60 seconds)
	hist1m := switcher.getBitrateHistoryForWindow(60)
	if len(hist1m) == 0 {
		t.Fatal("Bitrate history for 1m should not be empty")
	}

	// Check average is within range
	val := hist1m[0]
	if val < 1000 || val > 1600 {
		t.Errorf("Bitrate history average out of range: %f", val)
	}
}

func TestSwitcher_PacketInspection(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_inspect_test.json")
	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "", tmpDir, configPath)

	// Create dummy packet with H.264 keyframe
	// Let's verify containsKeyframe returns false on empty/non-keyframe and true on a valid keyframe if we can mock it
	pktNonKey := make([]byte, 188)
	pktNonKey[0] = 0x47

	if switcher.containsKeyframe(pktNonKey) {
		t.Error("Expected empty/dirty packet not to contain keyframe")
	}

	if switcher.containsVideo(pktNonKey) {
		t.Error("Expected empty/dirty packet not to be marked as containing video")
	}
}

func TestSwitcher_BuildArgs(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_args_test.json")
	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "", tmpDir, configPath)

	srtArgs := switcher.buildSRTArgs()
	if len(srtArgs) < 5 {
		t.Errorf("Expected built srt args to be populated, got: %v", srtArgs)
	}

	// Validate srt URL contains mode=caller
	hasMode := false
	for _, arg := range srtArgs {
		if idx := len(arg); idx > 0 && (arg == "srt://127.0.0.1:1234?mode=caller&latency=500000&timeout=5000000" || 
			arg == "srt://127.0.0.1:1234?mode=caller") {
			hasMode = true
		}
	}
	// The full URL string format
	urlFound := false
	for _, arg := range srtArgs {
		if len(arg) > 6 && arg[:6] == "srt://" {
			urlFound = true
			if !strings.Contains(arg, "mode=caller") {
				t.Errorf("SRT input URL does not contain mode=caller: %s", arg)
			}
		}
	}
	if !urlFound {
		t.Error("Did not find srt:// url in buildSRTArgs output")
	}
}

// Keep compiler happy
type dummyStruct struct{}
