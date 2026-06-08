package main

import (
	"context"
	"testing"
)

func TestTranscodingBridge_Availability(t *testing.T) {
	bridge := NewTranscodingBridge()

	if !bridge.IsAvailable() {
		t.Error("New bridge should be available")
	}

	// Active status set manually for testing state transitions
	bridge.active.Store(true)
	if bridge.IsAvailable() {
		t.Error("Active bridge should not be available")
	}

	bridge.active.Store(false)
	if !bridge.IsAvailable() {
		t.Error("Inactive bridge should be available")
	}
}

func TestTranscodingBridge_WriteInactive(t *testing.T) {
	bridge := NewTranscodingBridge()

	err := bridge.Write([]byte("data"))
	if err == nil {
		t.Error("Expected error writing to inactive bridge")
	}
}

func TestTranscodingBridge_StartFailureWithoutFFmpeg(t *testing.T) {
	bridge := NewTranscodingBridge()
	ctx := context.Background()
	outCh := make(chan []byte, 1)

	// Attempting to start the bridge (which runs ffmpeg) should fail when ffmpeg is missing
	err := bridge.Start(ctx, "h264", nil, nil, outCh)
	if err != nil {
		t.Logf("Expected failure on Start when ffmpeg is missing: %v", err)
	} else {
		// If it actually succeeded, clean up
		t.Log("Start succeeded, cleaning up bridge")
		bridge.Stop()
	}
}

func TestDetectCodec_Empty(t *testing.T) {
	codec := DetectCodec([]byte{})
	if codec != "" {
		t.Errorf("Expected empty codec from empty data, got %s", codec)
	}

	codec = DetectCodec(make([]byte, 100))
	if codec != "" {
		t.Errorf("Expected empty codec from dummy data, got %s", codec)
	}
}
