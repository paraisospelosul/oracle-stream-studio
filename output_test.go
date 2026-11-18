package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOutput_BuildFFmpegArgs(t *testing.T) {
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

	hasVideoCopy := false
	hasAudioCopy := false
	for i := 0; i < len(argsH265)-1; i++ {
		if argsH265[i] == "-c:v" && argsH265[i+1] == "copy" {
			hasVideoCopy = true
		}
		if argsH265[i] == "-c:a" && argsH265[i+1] == "copy" {
			hasAudioCopy = true
		}
	}
	if !hasVideoCopy || !hasAudioCopy {
		t.Errorf("Expected H.265 passthrough to copy both video and audio, got: %v", argsH265)
	}

	lastArg := argsH265[len(argsH265)-1]
	if lastArg != "rtmp://localhost/live/stream1" {
		t.Errorf("Expected RTMP URL to combine URL and StreamKey, got %s", lastArg)
	}

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
	if !hasLibx264 || !hasPreset || !hasBitrate {
		t.Errorf("Expected H.264 transcoder args, got: %v", argsH264)
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

	if err := out.MoveOverlay(150, 250); err != nil {
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

	if err := manager.AddOutput(cfg); err != nil {
		t.Fatalf("AddOutput failed: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Config file not created: %v", err)
	}

	var saved savedConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("Failed to parse config file: %v", err)
	}
	if len(saved.Outputs) != 1 || saved.Outputs[0].ID != "out1" {
		t.Errorf("Saved config is incorrect: %+v", saved)
	}

	cfg.Name = "Updated Name"
	if err := manager.UpdateOutput("out1", cfg); err != nil {
		t.Fatalf("UpdateOutput failed: %v", err)
	}

	if err := manager.RemoveOutput("out1"); err != nil {
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

	if err := manager.MoveOverlay("out-overlay", 80, 90); err != nil {
		t.Fatalf("MoveOverlay failed: %v", err)
	}

	err := manager.MoveOverlay("invalid", 10, 10)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Expected 'not found' error for invalid ID, got: %v", err)
	}
}

func TestOutput_ConcurrentStartStopAndRestartAll(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "outputs_race.json")
	broadcaster := NewBroadcaster()
	manager := NewOutputManager(broadcaster, configPath, tmpDir)

	for _, id := range []string{"out-race-1", "out-race-2"} {
		cfg := OutputConfig{
			ID:             id,
			Name:           id,
			URL:            "rtmp://127.0.0.1:19999/live",
			StreamKey:      "testkey",
			Codec:          CodecH265Passthrough,
			Enabled:        true,
			OverlayEnabled: false,
		}
		if err := manager.AddOutput(cfg); err != nil {
			t.Fatalf("AddOutput(%s) failed: %v", id, err)
		}
	}

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := "out-race-1"
			if idx%2 == 1 {
				id = "out-race-2"
			}
			for i := 0; i < 4; i++ {
				_ = manager.StartOutput(id)
				_ = manager.GetStats()
				_ = manager.MoveOverlay(id, 20+i, 30+i)
				_ = manager.StopOutput(id)
			}
		}(g)
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 3; i++ {
			manager.RestartAll()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 3; i++ {
			manager.StopAll()
			_ = manager.GetOutputs()
		}
	}()

	wg.Wait()
	manager.StopAll()

	for _, st := range manager.GetStats() {
		if st.Running {
			t.Errorf("Expected output %s to be stopped after StopAll()", st.ID)
		}
	}
}
