package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSwitcher_Config(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_config_test.json")

	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "http://localhost/stats", tmpDir, configPath)

	cfg := switcher.GetConfig()
	if cfg.SRTAddr != "127.0.0.1:1234" {
		t.Errorf("Expected SRTAddr '127.0.0.1:1234', got %s", cfg.SRTAddr)
	}

	cfg.SRTAddr = "192.168.1.100:9999"
	cfg.MinBitrateKbps = 1500
	switcher.UpdateConfig(cfg)

	updated := switcher.GetConfig()
	if updated.SRTAddr != "192.168.1.100:9999" || updated.MinBitrateKbps != 1500 {
		t.Errorf("Config was not updated correctly: %+v", updated)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}
	var saved SwitcherConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("Failed to parse saved config: %v", err)
	}
	if saved.SRTAddr != "192.168.1.100:9999" {
		t.Errorf("Saved config has incorrect address: %s", saved.SRTAddr)
	}

	switcher.UpdateFallbackPath("/opt/custom/queda.jpg")
	if switcher.GetConfig().FallbackPath != "/opt/custom/queda.jpg" {
		t.Errorf("Fallback path not updated: %s", switcher.GetConfig().FallbackPath)
	}
}

func TestSwitcher_StateTransitionsAndBRB(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_state_test.json")
	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "", tmpDir, configPath)

	if switcher.GetState() != StateFallback {
		t.Errorf("Expected initial state StateFallback, got %s", switcher.GetState())
	}

	switcher.setStateWithReason(StateLive, "SRT connected successfully")
	if switcher.GetState() != StateLive {
		t.Errorf("Expected state StateLive, got %s", switcher.GetState())
	}

	switcher.SetManualFallback(true)
	if !switcher.IsManualFallback() {
		t.Error("Expected manual fallback (BRB) to be true")
	}
	switcher.SetManualFallback(false)
	if switcher.IsManualFallback() {
		t.Error("Expected manual fallback (BRB) to be false")
	}

	stats := switcher.GetStats()
	if stats.SwitchCount != 1 {
		t.Errorf("Expected switch count 1, got %d", stats.SwitchCount)
	}

	history := switcher.GetSwitchHistory()
	if len(history) != 1 {
		t.Fatalf("Expected history length 1, got %d", len(history))
	}
	event := history[0]
	if event.From != "fallback" || event.To != "live" || event.Reason != "SRT connected successfully" {
		t.Errorf("History event is incorrect: %+v", event)
	}
}

func TestSwitcher_BitrateHistory(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_bitrate_test.json")
	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "", tmpDir, configPath)

	switcher.bitrateHistoryMu.Lock()
	for i := 0; i < 60; i++ {
		switcher.bitrateHistory[i] = float64(1000 + i*10)
	}
	switcher.bitrateHistoryIdx = 60
	switcher.bitrateHistoryLen = 60
	switcher.bitrateHistoryMu.Unlock()

	hist1m := switcher.getBitrateHistoryForWindow(60)
	if len(hist1m) == 0 {
		t.Fatal("Bitrate history for 1m should not be empty")
	}
	if hist1m[0] < 1000 || hist1m[0] > 1600 {
		t.Errorf("Bitrate history average out of range: %f", hist1m[0])
	}
}

func TestSwitcher_PacketInspection(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_inspect_test.json")
	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "", tmpDir, configPath)

	pktNonKey := make([]byte, tsPacketSize)
	pktNonKey[0] = tsSyncByte

	if switcher.containsKeyframe(pktNonKey) {
		t.Error("Expected empty/PAT packet not to contain keyframe")
	}
	if switcher.containsVideo(pktNonKey) {
		t.Error("Expected PID 0 packet not to be marked as containing video")
	}
}

func TestSwitcher_BuildArgs(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "switcher_args_test.json")
	switcher := NewSwitcher("127.0.0.1:1234", "caller", "/tmp/fallback.ts", 3000, "", tmpDir, configPath)

	srtArgs := switcher.buildSRTArgs()
	urlFound := false
	for _, arg := range srtArgs {
		if strings.HasPrefix(arg, "srt://") {
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
