package main

import (
	"testing"
)

func buildTestPESPacket(pid uint16, pts, dts int64, hasDTS bool, cc uint8) []byte {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = tsSyncByte
	pkt[1] = 0x40 | byte((pid>>8)&0x1F) // PUSI=1
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 | (cc & 0x0F) // Payload only

	pes := pkt[4:]
	pes[0] = 0x00
	pes[1] = 0x00
	pes[2] = 0x01
	pes[3] = 0xE0
	if hasDTS {
		pes[7] = 0xC0
		pes[8] = 10
		writeTimestamp(pes[9:14], pts, 0x30)
		writeTimestamp(pes[14:19], dts, 0x10)
	} else {
		pes[7] = 0x80
		pes[8] = 5
		writeTimestamp(pes[9:14], pts, 0x20)
	}
	return pkt
}

func TestPCR_ReadWrite(t *testing.T) {
	originalPCR := int64(123456789*300 + 123)
	buf := make([]byte, 6)
	writePCR(buf, originalPCR)
	if got := extractPCR(buf); got != originalPCR {
		t.Errorf("PCR read/write mismatch: wrote %d, read %d", originalPCR, got)
	}
}

func TestPTSDTS_ReadWrite(t *testing.T) {
	originalTimestamp := int64(8589934591) // 2^33 - 1
	buf := make([]byte, 5)
	writeTimestamp(buf, originalTimestamp, 0x20)
	if got := readTimestamp(buf); got != originalTimestamp {
		t.Errorf("PTS/DTS read/write mismatch: wrote %d, read %d", originalTimestamp, got)
	}
}

func TestPTSRemapper_MonotonicAndContinuityCounter(t *testing.T) {
	remapper := NewPTSRemapper()
	remapper.SetPIDs(256, 257, 4096)

	// Source A (Live with B-frame offset: DTS=90000, PTS=99000, incoming CC=7)
	pkt1 := buildTestPESPacket(256, 99000, 90000, true, 7)
	remapper.Remap(pkt1)
	if gotCC := pkt1[3] & 0x0F; gotCC != 0 {
		t.Errorf("Expected first CC to be normalized to 0, got %d", gotCC)
	}

	// Source switch to Slate starting at PTS=0, DTS=0 with random incoming CC=12
	remapper.NotifySourceSwitch()
	pkt2 := buildTestPESPacket(256, 0, 0, false, 12)
	remapper.Remap(pkt2)

	if gotCC := pkt2[3] & 0x0F; gotCC != 1 {
		t.Errorf("Expected Continuity Counter to continue smoothly to 1, got %d", gotCC)
	}

	newPTS, _, ok := extractPTSDTS(pkt2[4:])
	if !ok {
		t.Fatal("Failed to extract remapped PTS")
	}
	// Must exceed previous maxPTS (99000) by defaultStep (3000) -> 102000
	if newPTS != 102000 {
		t.Errorf("Expected monotonic remapped PTS 102000, got %d", newPTS)
	}
}

func TestPTSRemapper_Reset(t *testing.T) {
	remapper := NewPTSRemapper()
	pkt := buildTestPESPacket(256, 90000, 90000, false, 0)
	remapper.Remap(pkt)
	remapper.NotifySourceSwitch()
	pkt2 := buildTestPESPacket(256, 0, 0, false, 0)
	remapper.Remap(pkt2)

	if !remapper.IsActive() {
		t.Error("Expected remapper to be active after source switch")
	}

	remapper.Reset()
	if remapper.IsActive() {
		t.Error("Expected remapper to be inactive after Reset()")
	}
}
