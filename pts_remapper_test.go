package main

import (
	"bytes"
	"testing"
)

func TestPCR_ReadWrite(t *testing.T) {
	// 42-bit PCR: base * 300 + ext
	originalPCR := int64(123456789 * 300) // Extension = 0
	buf := make([]byte, 6)

	writePCR(buf, originalPCR)
	readPCR := extractPCR(buf)

	if readPCR != originalPCR {
		t.Errorf("PCR read/write mismatch: wrote %d, read %d", originalPCR, readPCR)
	}

	// Test with extension
	originalPCRWithExt := int64(987654321*300 + 123)
	writePCR(buf, originalPCRWithExt)
	readPCRWithExt := extractPCR(buf)

	if readPCRWithExt != originalPCRWithExt {
		t.Errorf("PCR read/write mismatch with extension: wrote %d, read %d", originalPCRWithExt, readPCRWithExt)
	}
}

func TestPTSDTS_ReadWrite(t *testing.T) {
	originalTimestamp := int64(8589934591) // 33-bit max is 8589934591 (2^33 - 1)
	buf := make([]byte, 5)

	writeTimestamp(buf, originalTimestamp, 0x20)
	readTS := readTimestamp(buf)

	if readTS != originalTimestamp {
		t.Errorf("PTS/DTS read/write mismatch: wrote %d, read %d", originalTimestamp, readTS)
	}
}

func TestPTSRemapper_BasicRemap(t *testing.T) {
	remapper := NewPTSRemapper()
	remapper.SetPIDs(256, 257, 4096) // video PID 256, audio 257

	// Create a dummy video PES packet with a PTS flag and timestamp
	pesPacket := make([]byte, 20)
	pesPacket[0] = 0x00
	pesPacket[1] = 0x00
	pesPacket[2] = 0x01 // Start code prefix
	pesPacket[3] = 0xE0 // Video stream ID
	pesPacket[7] = 0x80 // PTS flag (binary 10000000 in bits 7-6)

	originalPTS := int64(90000) // 1 second in 90kHz
	writeTimestamp(pesPacket[9:14], originalPTS, 0x20)

	// Verify reading it back works
	pts, dts := extractPTSDTS(pesPacket)
	if pts != originalPTS {
		t.Errorf("Failed to read test PTS: got %d, want %d", pts, originalPTS)
	}
	if dts != 0 {
		t.Errorf("DTS should be 0, got %d", dts)
	}

	// Prepare switch and check offset calculation
	remapper.lastPTS = int64(180000) // outgoing ended at 2 seconds
	remapper.PrepareSwitch(pesPacket) // first new packet PTS is 1 second

	// Offset = lastPTS - firstPTS + frameDuration = 180000 - 90000 + 3003 = 93003
	expectedOffset := int64(93003)
	if remapper.offset != expectedOffset {
		t.Errorf("Expected offset %d, got %d", expectedOffset, remapper.offset)
	}

	// Run remap on the PES packet bytes
	remapPES(pesPacket, remapper.offset)

	// Read again and verify it is shifted by the offset
	newPTS, _ := extractPTSDTS(pesPacket)
	expectedPTS := originalPTS + expectedOffset
	if newPTS != expectedPTS {
		t.Errorf("Expected new PTS %d, got %d", expectedPTS, newPTS)
	}
}

func TestPTSRemapper_RemapLoopJump(t *testing.T) {
	remapper := NewPTSRemapper()
	remapper.SetPIDs(256, 257, 4096)
	remapper.lastPTS = int64(180000)
	remapper.offset = 1000
	remapper.offsetSet = true
	remapper.lastIncomingPTS = 200000

	// Loop jump: next PTS is much lower than lastIncomingPTS (e.g. 50000 < 200000 - 90000)
	pesPacket := make([]byte, 20)
	pesPacket[0] = 0x00
	pesPacket[1] = 0x00
	pesPacket[2] = 0x01
	pesPacket[3] = 0xE0
	pesPacket[7] = 0x80 // PTS only
	writeTimestamp(pesPacket[9:14], 50000, 0x20)

	// Wrap in a TS packet structure to use full Remap function
	tsPkt := make([]byte, 188)
	tsPkt[0] = 0x47
	// PID = 256 (0x0100)
	tsPkt[1] = 0x01
	tsPkt[2] = 0x00
	// PUSI = true (payload unit start indicator) -> bit 6 of header byte 1
	tsPkt[1] |= 0x40

	// Payload offset starts after header (4 bytes)
	copy(tsPkt[4:], pesPacket)

	remapper.Remap(tsPkt)

	// The offset should have adjusted to: lastPTS - pts + 3003 = 180000 - 50000 + 3003 = 133003
	expectedOffset := int64(133003)
	if remapper.offset != expectedOffset {
		t.Errorf("Expected offset to auto-adjust to %d, got %d", expectedOffset, remapper.offset)
	}
}

func TestPTSRemapper_Reset(t *testing.T) {
	remapper := NewPTSRemapper()
	remapper.lastPCR = 100
	remapper.lastPTS = 200
	remapper.offset = 300
	remapper.offsetSet = true

	remapper.Reset()

	if remapper.lastPCR != 0 || remapper.lastPTS != 0 || remapper.offset != 0 || remapper.offsetSet {
		t.Error("Reset did not clear all remapper fields")
	}
}

func TestDummyPTS(t *testing.T) {
	// Keep compiler happy about imports
	_ = bytes.Compare([]byte{}, []byte{})
}
