package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutput_BuildFFmpegArgs(t *testing.T) {
	// 1. H.265 Passthrough Output config
	cfgH265 := OutputConfig{
		ID:             "out-h265",
		Name:           "H265 Output",
		URL:            "rtmp://localhost/live",
		StreamKey:      "stream1",
		Codec:          CodecH265Passthrough,
		Enabled:        true,
		OverlayEnabled: false,
	}

	outH265 := NewOutput(cfgH265, "/tmp")
	argsH265 := outH265.buildFFmpegArgs()

	// Verify H.265 uses stream copy (-c:v copy)
	hasCopy := false
	for _, arg := range argsH265 {
		if arg == "copy" {
			hasCopy = true
			break
		}
	}
	if !hasCopy {
		t.Error("Expected H.265 passthrough to have '-c copy' or 'copy' codec argument")
	}

	// Verify RTMP URL contains stream key
	lastArg := argsH265[len(argsH265)-1]
	if lastArg != "rtmp://localhost/live/stream1" {
		t.Errorf("Expected RTMP URL to combine URL and StreamKey, got %s", lastArg)
	}

	// 2. H.264 Transcoded Output config
	cfgH264 := OutputConfig{
		ID:             "out-h264",
		Name:           "H264 Output",
		URL:            "rtmp://localhost/live",
		StreamKey:      "stream2",
		Codec:          CodecH264Transcode,
		Bitrate:        2500,
		Preset:         "veryfast",
		Enabled:        true,
		OverlayEnabled: true,
		OverlayX:       50,
		OverlayY:       100,
	}

	outH264 := NewOutput(cfgH264, "/tmp")
	argsH264 := outH264.buildFFmpegArgs()

	// Verify H.264 uses libx264 transcoder
	hasLibx264 := false
	hasPreset := false
	hasBitrate := false
	for i, arg := range argsH264 {
		if arg == "libx264" {
			hasLibx264 = true
		}
		if arg == "-preset" && i+1 < len(argsH264) && argsH264[i+1] == "veryfast" {
			hasPreset = true
		}
		if arg == "-b:v" && i+1 < len(argsH264) && argsH264[i+1] == "2500k" {
			hasBitrate = true
		}
	}

	if !hasLibx264 {
		t.Error("Expected H.264 transcoder config to use libx264")
	}
	if !hasPreset {
		t.Error("Expected H.264 transcoder config to set preset 'veryfast'")
	}
	if !hasBitrate {
		t.Error("Expected H.264 transcoder config to set bitrate '2500k'")
	}
}

func TestOutput_MoveOverlay(t *testing.T) {
	cfg := OutputConfig{
		ID:             "test-overlay",
		OverlayEnabled: true,
		OverlayX:       10,
		OverlayY:       20,
	}
	out := NewOutput(cfg, "/tmp")

	// Move while not active/running -> should update local config only and return nil without ZMQ error
	err := out.MoveOverlay(150, 250)
	if err != nil {
		t.Fatalf("MoveOverlay failed: %v", err)
	}

	out.mu.Lock()
	x := out.config.OverlayX
	y := out.config.OverlayY
	out.mu.Unlock()

	if x != 150 || y != 250 {
		t.Errorf("Expected config coordinates 150, 250, got %d, %d", x, y)
	}
}

func TestOutputManager_CRUD(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "outputs_test.json")

	broadcaster := NewBroadcaster()
	manager := NewOutputManager(broadcaster, configPath, tmpDir)

	// Verify initially empty
	if len(manager.outputs) != 0 {
		t.Error("Expected output manager to be empty initially")
	}

	cfg := OutputConfig{
		ID:        "out1",
		Name:      "Test Destination",
		URL:       "rtmp://localhost/live",
		StreamKey: "key1",
		Codec:     CodecH265Passthrough,
		Enabled:   true,
	}

	// Add
	err := manager.AddOutput(cfg)
	if err != nil {
		t.Fatalf("AddOutput failed: %v", err)
	}

	if len(manager.outputs) != 1 {
		t.Errorf("Expected 1 output, got %d", len(manager.outputs))
	}

	// Verify saved to file
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Config file not created: %v", err)
	}

	var saved []OutputConfig
	err = json.Unmarshal(data, &saved)
	if err != nil {
		t.Fatalf("Failed to parse config file: %v", err)
	}
	if len(saved) != 1 || saved[0].ID != "out1" {
		t.Errorf("Saved config is incorrect: %v", saved)
	}

	// Update
	cfg.Name = "Updated Name"
	err = manager.UpdateOutput("out1", cfg)
	if err != nil {
		t.Fatalf("UpdateOutput failed: %v", err)
	}

	manager.mu.RLock()
	name := manager.outputs["out1"].config.Name
	manager.mu.RUnlock()

	if name != "Updated Name" {
		t.Errorf("Expected name 'Updated Name', got '%s'", name)
	}

	// Remove
	err = manager.RemoveOutput("out1")
	if err != nil {
		t.Fatalf("RemoveOutput failed: %v", err)
	}

	if len(manager.outputs) != 0 {
		t.Errorf("Expected 0 outputs, got %d", len(manager.outputs))
	}
}

func TestOutputManager_Overlay(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "outputs_overlay.json")
	broadcaster := NewBroadcaster()
	manager := NewOutputManager(broadcaster, configPath, tmpDir)

	cfg := OutputConfig{
		ID:             "out-overlay",
		OverlayEnabled: true,
		OverlayX:       5,
		OverlayY:       5,
	}
	_ = manager.AddOutput(cfg)

	// Move
	err := manager.MoveOverlay("out-overlay", 80, 90)
	if err != nil {
		t.Fatalf("MoveOverlay failed: %v", err)
	}

	manager.mu.RLock()
	out := manager.outputs["out-overlay"]
	manager.mu.RUnlock()

	out.mu.Lock()
	ox := out.config.OverlayX
	oy := out.config.OverlayY
	out.mu.Unlock()

	if ox != 80 || oy != 90 {
		t.Errorf("Expected updated coordinates 80, 90, got %d, %d", ox, oy)
	}

	// Try moving invalid ID
	err = manager.MoveOverlay("invalid", 10, 10)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Expected 'not found' error for invalid ID, got: %v", err)
	}
}
