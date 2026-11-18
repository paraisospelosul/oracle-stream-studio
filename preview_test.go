package main

import (
	"bytes"
	"testing"
)

func TestPreviewManager_DefaultsAndDisable(t *testing.T) {
	b := NewBroadcaster()
	pm := NewPreviewManager(b)

	status := pm.GetStatus()
	if status.FPS != 0 || status.Running {
		t.Errorf("Expected preview to boot OFF (0%% CPU), got: %+v", status)
	}
	if pm.GetFrame() != nil {
		t.Error("Expected nil frame when preview is disabled")
	}

	pm.frameMu.Lock()
	pm.latestFrame = []byte{0xFF, 0xD8, 0x01, 0xFF, 0xD9}
	pm.frameMu.Unlock()

	if got := pm.GetFrame(); len(got) != 5 {
		t.Fatalf("Expected 5-byte frame, got %d", len(got))
	}

	pm.DisableAuto()
	if pm.GetFrame() != nil {
		t.Error("Expected latestFrame to be cleared after DisableAuto()")
	}
}

func TestPreviewManager_ReadFramesMJPEGStream(t *testing.T) {
	b := NewBroadcaster()
	pm := NewPreviewManager(b)

	frame1 := []byte{0xFF, 0xD8, 0x10, 0x20, 0xFF, 0xD9}
	frame2 := []byte{0xFF, 0xD8, 0xAA, 0xBB, 0xCC, 0xFF, 0xD9}

	var stream []byte
	stream = append(stream, 0x00, 0x11, 0x22) // Leading noise
	stream = append(stream, frame1...)
	stream = append(stream, 0x55) // Inter-frame byte
	stream = append(stream, frame2...)
	stream = append(stream, 0xFF, 0xD8, 0x99) // Trailing incomplete frame (no EOI)

	pm.readFrames(bytes.NewReader(stream))

	got := pm.GetFrame()
	if !bytes.Equal(got, frame2) {
		t.Fatalf("Expected latest complete JPEG frame %x, got %x", frame2, got)
	}

	// Mutating caller slice must not mutate internal frame buffer
	got[2] = 0x00
	if !bytes.Equal(pm.GetFrame(), frame2) {
		t.Error("Expected GetFrame() to return an isolated copy")
	}
}
