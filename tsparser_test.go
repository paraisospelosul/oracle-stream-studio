package main

import (
	"testing"
)

func TestParseTSPacket(t *testing.T) {
	// A valid TS packet starts with 0x47 (sync byte) and is 188 bytes long
	pkt := make([]byte, 188)
	pkt[0] = 0x47 // Sync byte

	// Set PID = 256 (0x0100) in header bytes 1 & 2
	pkt[1] = 0x01
	pkt[2] = 0x00

	info, ok := ParseTSPacket(pkt)
	if !ok {
		t.Fatal("Failed to parse valid TS packet")
	}

	if info.PID != 256 {
		t.Errorf("Expected PID 256, got %d", info.PID)
	}
}

func TestPacketAligner(t *testing.T) {
	// Aligner expects a stream of bytes and extracts valid 188-byte TS packets aligned to the sync byte 0x47
	aligner := NewPacketAligner()

	// Feed dirty data before sync byte
	dirtyData := []byte{0x00, 0x11, 0x22, 0x33, 0x47} // Ends with sync byte
	validPkt := make([]byte, 187)                     // rest of the 188 bytes
	validPkt[0] = 0x01                                // Set PID high byte
	validPkt[1] = 0x00                                // Set PID low byte

	// Combine to feed to aligner
	stream := append(dirtyData, validPkt...)

	packets := aligner.Feed(stream)
	if len(packets) != 1 {
		t.Fatalf("Expected 1 aligned packet, got %d", len(packets))
	}

	alignedPkt := packets[0]
	if alignedPkt[0] != 0x47 {
		t.Errorf("Expected sync byte 0x47, got 0x%02X", alignedPkt[0])
	}
}

func TestSwitcherStateMachine(t *testing.T) {
	// A simple test for the state transitions
	state := StateFallback
	if state.String() != "fallback" {
		t.Errorf("Expected StateFallback to stringify to 'fallback', got '%s'", state.String())
	}

	state = StateLive
	if state.String() != "live" {
		t.Errorf("Expected StateLive to stringify to 'live', got '%s'", state.String())
	}
}
