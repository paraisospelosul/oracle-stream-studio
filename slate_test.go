package main

import (
	"testing"
)

func TestParseFallbackResolution(t *testing.T) {
	w, h := ParseFallbackResolution("1080p")
	if w != 1920 || h != 1080 {
		t.Errorf("Expected 1920x1080, got %dx%d", w, h)
	}

	w720, h720 := ParseFallbackResolution("720p")
	if w720 != 1280 || h720 != 720 {
		t.Errorf("Expected 1280x720, got %dx%d", w720, h720)
	}
}

func TestSliceTSIntoFrames(t *testing.T) {
	makePkt := func(pid uint16, pusi bool) []byte {
		p := make([]byte, tsPacketSize)
		p[0] = tsSyncByte
		p[1] = byte((pid >> 8) & 0x1F)
		if pusi {
			p[1] |= 0x40
		}
		p[2] = byte(pid & 0xFF)
		p[3] = 0x10
		return p
	}

	// Build synthetic stream: PAT + PMT + Frame0(2 pkts) + PAT + PMT + Frame1(2 pkts)
	var raw []byte
	raw = append(raw, makePkt(patPID, true)...)
	raw = append(raw, makePkt(defaultPMTPID, true)...)
	raw = append(raw, makePkt(defaultVideoPID, true)...)
	raw = append(raw, makePkt(defaultVideoPID, false)...)

	raw = append(raw, makePkt(patPID, true)...)
	raw = append(raw, makePkt(defaultPMTPID, true)...)
	raw = append(raw, makePkt(defaultVideoPID, true)...)
	raw = append(raw, makePkt(defaultAudioPID, true)...)

	frames := SliceTSIntoFrames(raw)
	if len(frames) != 2 {
		t.Fatalf("Expected 2 sliced frames, got %d", len(frames))
	}
	// Each frame should contain 4 packets (PAT + PMT + 2 media packets)
	if len(frames[0]) != 4*tsPacketSize || len(frames[1]) != 4*tsPacketSize {
		t.Errorf("Expected 752 bytes per frame, got %d and %d", len(frames[0]), len(frames[1]))
	}
}
