package main

import (
	"log"
	"sync"
)

// PTSRemapper adjusts PTS/DTS/PCR timestamps in MPEG-TS packets to maintain
// continuity when switching between sources. Without remapping, timestamp
// discontinuities cause decoder glitches (frozen frames, audio pops).
//
// How it works:
//  1. Tracks the last PCR/PTS/DTS values from the outgoing source
//  2. When a switch occurs, calculates the offset needed to make the new
//     source's timestamps continue seamlessly from the old source's timeline
//  3. Applies that offset to every PCR/PTS/DTS in the new source's packets
type PTSRemapper struct {
	// Last known timestamps from the outgoing source
	lastPCR int64
	lastPTS int64
	lastDTS int64

	// Last known raw incoming timestamps (before remapping)
	lastIncomingPTS int64
	lastIncomingPCR int64

	// Offset to apply to the new source's timestamps
	offset    int64
	offsetSet bool

	// Video PID for PES timestamp adjustment
	videoPID uint16
	audioPID uint16
	pmtPID   uint16

	mu sync.Mutex
}

// NewPTSRemapper creates a new remapper
func NewPTSRemapper() *PTSRemapper {
	return &PTSRemapper{}
}

// SetPIDs configures the video/audio/PMT PIDs for the remapper
func (pr *PTSRemapper) SetPIDs(videoPID, audioPID, pmtPID uint16) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.videoPID = videoPID
	pr.audioPID = audioPID
	pr.pmtPID = pmtPID
}

// TrackOutgoing records timestamps from the outgoing source's packets.
// Call this on every packet BEFORE the switch.
func (pr *PTSRemapper) TrackOutgoing(data []byte) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		if pkt[0] != tsSyncByte {
			continue
		}
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}

		// Extract PCR from adaptation field
		if info.HasAdaptationField && info.AdaptationLength >= 6 {
			flags := pkt[5]
			if flags&0x10 != 0 { // PCR flag
				pcr := extractPCR(pkt[6:])
				if pcr > 0 {
					pr.lastPCR = pcr
				}
			}
		}

		// Extract PTS/DTS from PES headers on video/audio PIDs
		if info.PUSI && info.HasPayload &&
			(info.PID == pr.videoPID || info.PID == pr.audioPID) &&
			info.PID != 0 {
			pts, dts := extractPTSDTS(pkt[info.PayloadOffset:])
			if pts > 0 {
				pr.lastPTS = pts
			}
			if dts > 0 {
				pr.lastDTS = dts
			}
		}
	}
}

// PrepareSwitch calculates the timestamp offset for the new source.
// Call this ONCE when the switch occurs, with the first data from the new source.
func (pr *PTSRemapper) PrepareSwitch(newSourceData []byte) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	// Find the first PTS in the new source
	var firstPTS int64
	var firstPCR int64
	for offset := 0; offset+tsPacketSize <= len(newSourceData); offset += tsPacketSize {
		pkt := newSourceData[offset : offset+tsPacketSize]
		if pkt[0] != tsSyncByte {
			continue
		}
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}

		// Find first PCR
		if firstPCR == 0 && info.HasAdaptationField && info.AdaptationLength >= 6 {
			flags := pkt[5]
			if flags&0x10 != 0 {
				firstPCR = extractPCR(pkt[6:])
			}
		}

		// Find first PTS
		if firstPTS == 0 && info.PUSI && info.HasPayload &&
			(info.PID == pr.videoPID || info.PID == pr.audioPID) &&
			info.PID != 0 {
			pts, _ := extractPTSDTS(pkt[info.PayloadOffset:])
			if pts > 0 {
				firstPTS = pts
			}
		}

		if firstPTS > 0 && firstPCR > 0 {
			break
		}
	}

	if pr.lastPTS > 0 && firstPTS > 0 {
		// Offset = where old source ended - where new source starts + 1 frame (~3003 ticks for 30fps)
		frameDuration := int64(3003) // 90kHz / 30fps
		pr.offset = pr.lastPTS - firstPTS + frameDuration
		pr.offsetSet = true
	} else if pr.lastPCR > 0 && firstPCR > 0 {
		// Fallback: use PCR-based offset
		pr.offset = pr.lastPCR - firstPCR + 27000000/30 // 27MHz / 30fps
		pr.offsetSet = true
	}
}

// Remap adjusts PTS/DTS/PCR timestamps in the given MPEG-TS data.
// The data is modified IN PLACE for performance (avoids allocation).
// Returns the same slice for chaining.
func (pr *PTSRemapper) Remap(data []byte) []byte {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		if pkt[0] != tsSyncByte {
			continue
		}
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}

		// Track/detect loop jumps in incoming video/audio timestamps
		if info.PUSI && info.HasPayload &&
			(info.PID == pr.videoPID || info.PID == pr.audioPID) &&
			info.PID != 0 {
			pts, _ := extractPTSDTS(pkt[info.PayloadOffset:])
			if pts > 0 {
				// Detect loop jump (incoming PTS jumps backward by > 1 second)
				if pr.lastIncomingPTS > 0 && pts < pr.lastIncomingPTS-90000 {
					frameDuration := int64(3003) // 30fps
					if pr.lastPTS > 0 {
						pr.offset = pr.lastPTS - pts + frameDuration
						pr.offsetSet = true
						log.Printf("[remapper] Loop/jump detected (raw PTS: %d -> %d). Adjusted offset to %d", pr.lastIncomingPTS, pts, pr.offset)
					}
				}
				pr.lastIncomingPTS = pts
			}
		}

		// Track/detect loop jumps in incoming PCR
		if info.HasAdaptationField && info.AdaptationLength >= 6 {
			flags := pkt[5]
			if flags&0x10 != 0 { // PCR flag
				pcr := extractPCR(pkt[6:])
				if pcr > 0 {
					if pr.lastIncomingPCR > 0 && pcr < pr.lastIncomingPCR-27000000 { // PCR jumps backward by > 1 second
						// We let the PTS jump detector handle the offset calculation to avoid conflicts,
						// but we update the tracking timestamp here.
					}
					pr.lastIncomingPCR = pcr
				}
			}
		}

		// Apply remapping if offset is set
		if pr.offsetSet && pr.offset != 0 {
			// Remap PCR in adaptation field
			if info.HasAdaptationField && info.AdaptationLength >= 6 {
				flags := pkt[5]
				if flags&0x10 != 0 {
					pcr := extractPCR(pkt[6:])
					if pcr > 0 {
						newPCR := pcr + pr.offset*300
						if newPCR > 0 {
							writePCR(pkt[6:], newPCR)
							pr.lastPCR = newPCR
						}
					}
				}
			}

			// Remap PTS/DTS in PES headers
			if info.PUSI && info.HasPayload &&
				(info.PID == pr.videoPID || info.PID == pr.audioPID) &&
				info.PID != 0 {
				remapPES(pkt[info.PayloadOffset:], pr.offset)
			}
		}
	}

	return data
}

// Reset clears the remapper state for a fresh start
func (pr *PTSRemapper) Reset() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.lastPCR = 0
	pr.lastPTS = 0
	pr.lastDTS = 0
	pr.lastIncomingPTS = 0
	pr.lastIncomingPCR = 0
	pr.offset = 0
	pr.offsetSet = false
}

// IsActive returns true if the remapper has an active offset
func (pr *PTSRemapper) IsActive() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.offsetSet
}

// --- Low-level MPEG-TS timestamp helpers ---

// extractPCR reads a 42-bit PCR value from the adaptation field
// PCR = base(33 bits) * 300 + extension(9 bits)
// Stored in 6 bytes
func extractPCR(data []byte) int64 {
	if len(data) < 6 {
		return 0
	}
	base := int64(data[0])<<25 | int64(data[1])<<17 | int64(data[2])<<9 |
		int64(data[3])<<1 | int64(data[4]>>7)
	ext := int64(data[4]&0x01)<<8 | int64(data[5])
	return base*300 + ext
}

// writePCR writes a PCR value back into the adaptation field
func writePCR(data []byte, pcr int64) {
	if len(data) < 6 {
		return
	}
	base := pcr / 300
	ext := pcr % 300

	data[0] = byte(base >> 25)
	data[1] = byte(base >> 17)
	data[2] = byte(base >> 9)
	data[3] = byte(base >> 1)
	data[4] = byte(base<<7) | 0x7E | byte(ext>>8)
	data[5] = byte(ext)
}

// extractPTSDTS reads PTS and DTS from a PES header
func extractPTSDTS(pesData []byte) (pts, dts int64) {
	if len(pesData) < 14 {
		return 0, 0
	}
	// PES start code: 00 00 01
	if pesData[0] != 0x00 || pesData[1] != 0x00 || pesData[2] != 0x01 {
		return 0, 0
	}

	flags := pesData[7]
	ptsDTSFlags := (flags >> 6) & 0x03

	if ptsDTSFlags >= 2 && len(pesData) >= 14 {
		// PTS present (5 bytes starting at offset 9)
		pts = readTimestamp(pesData[9:14])
	}

	if ptsDTSFlags == 3 && len(pesData) >= 19 {
		// DTS present (5 bytes starting at offset 14)
		dts = readTimestamp(pesData[14:19])
	}

	return pts, dts
}

// readTimestamp reads a 33-bit PTS/DTS timestamp from 5 bytes
func readTimestamp(data []byte) int64 {
	if len(data) < 5 {
		return 0
	}
	ts := (int64(data[0]>>1) & 0x07) << 30
	ts |= int64(data[1]) << 22
	ts |= (int64(data[2] >> 1)) << 15
	ts |= int64(data[3]) << 7
	ts |= int64(data[4] >> 1)
	return ts
}

// writeTimestamp writes a 33-bit PTS/DTS timestamp into 5 bytes
func writeTimestamp(data []byte, ts int64, marker byte) {
	if len(data) < 5 {
		return
	}
	data[0] = marker | byte((ts>>29)&0x0E) | 0x01
	data[1] = byte(ts >> 22)
	data[2] = byte((ts>>14)&0xFE) | 0x01
	data[3] = byte(ts >> 7)
	data[4] = byte((ts<<1)&0xFE) | 0x01
}

// remapPES adjusts PTS/DTS in a PES header by the given offset
func remapPES(pesData []byte, offset int64) {
	if len(pesData) < 14 {
		return
	}
	if pesData[0] != 0x00 || pesData[1] != 0x00 || pesData[2] != 0x01 {
		return
	}

	flags := pesData[7]
	ptsDTSFlags := (flags >> 6) & 0x03

	if ptsDTSFlags >= 2 && len(pesData) >= 14 {
		pts := readTimestamp(pesData[9:14])
		if pts > 0 {
			newPTS := pts + offset
			if newPTS > 0 {
				marker := byte(0x20) // PTS only marker
				if ptsDTSFlags == 3 {
					marker = 0x30 // PTS+DTS marker
				}
				writeTimestamp(pesData[9:14], newPTS, marker)
			}
		}
	}

	if ptsDTSFlags == 3 && len(pesData) >= 19 {
		dts := readTimestamp(pesData[14:19])
		if dts > 0 {
			newDTS := dts + offset
			if newDTS > 0 {
				writeTimestamp(pesData[14:19], newDTS, 0x10)
			}
		}
	}
}
