package main

import (
	"testing"
)

func TestParseTSPacket(t *testing.T) {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = 0x41 // PUSI=1, PID high=0x01
	pkt[2] = 0x00 // PID=256
	pkt[3] = 0x15 // Payload only, CC=5

	info, ok := ParseTSPacket(pkt)
	if !ok {
		t.Fatal("Failed to parse valid TS packet")
	}
	if info.PID != 256 {
		t.Errorf("Expected PID 256, got %d", info.PID)
	}
	if !info.PUSI || !info.HasPayload || info.ContinuityCounter != 5 {
		t.Errorf("Unexpected packet flags: %+v", info)
	}
}

func TestPacketAligner_NextAndBatch(t *testing.T) {
	aligner := NewPacketAligner()

	dirtyPrefix := []byte{0x00, 0x11, 0x22, 0x33}
	pkt1 := make([]byte, tsPacketSize)
	pkt1[0] = tsSyncByte
	pkt1[1] = 0x01
	pkt1[2] = 0x00

	pkt2 := make([]byte, tsPacketSize)
	pkt2[0] = tsSyncByte
	pkt2[1] = 0x01
	pkt2[2] = 0x01

	stream := append(dirtyPrefix, pkt1...)
	stream = append(stream, pkt2...)
	stream = append(stream, 0x47, 0x01) // trailing partial packet

	aligner.Feed(stream)
	batch := aligner.NextBatch()
	if len(batch) != tsPacketSize*2 {
		t.Fatalf("Expected batch of %d bytes, got %d", tsPacketSize*2, len(batch))
	}
	if batch[0] != tsSyncByte || batch[tsPacketSize] != tsSyncByte {
		t.Errorf("Expected sync bytes at packet boundaries")
	}

	// Feed remaining 186 bytes to complete the 3rd packet
	aligner.Feed(make([]byte, tsPacketSize-2))
	single := aligner.Next()
	if len(single) != tsPacketSize || single[0] != tsSyncByte {
		t.Fatalf("Expected final 188-byte packet from trailing bytes")
	}
}

func TestSwitcherStateMachine(t *testing.T) {
	state := StateFallback
	if state.String() != "fallback" {
		t.Errorf("Expected StateFallback to stringify to 'fallback', got '%s'", state.String())
	}

	state = StateLive
	if state.String() != "live" {
		t.Errorf("Expected StateLive to stringify to 'live', got '%s'", state.String())
	}
}
