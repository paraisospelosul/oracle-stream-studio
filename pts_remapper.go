package main

import (
	"log"
	"sync"
)

const (
	ptsWrapMask = (int64(1) << 33) - 1
	pcrWrapMod  = (int64(1) << 33) * 300
	defaultStep = int64(3000) // 33.333ms (1 frame @ 30fps in 90kHz ticks)
)

// PTSRemapper maintains an unbroken, strictly monotonic MPEG-TS timeline (PTS, DTS, PCR)
// and Continuity Counter (0..15 per PID) across source switches and RAM slate loops.
type PTSRemapper struct {
	offset    int64
	offsetSet bool
	needSync  bool
	hasOutput bool

	// Highest remapped output timestamps emitted so far (unwrapped 90kHz ticks)
	maxAnyPTS int64
	maxAnyDTS int64
	maxAnyPCR int64

	// Per-PID tracking for strict DTS monotonicity and loop detection
	maxOutDTS  map[uint16]int64
	lastRawDTS map[uint16]int64

	// Continuity Counter per PID (ISO/IEC 13818-1)
	cc     [8192]uint8
	ccInit [8192]bool

	videoPID uint16
	audioPID uint16
	pmtPID   uint16

	mu sync.Mutex
}

// NewPTSRemapper creates a remapper pre-configured with standard normalized PIDs
func NewPTSRemapper() *PTSRemapper {
	return &PTSRemapper{
		videoPID:   defaultVideoPID,
		audioPID:   defaultAudioPID,
		pmtPID:     defaultPMTPID,
		maxOutDTS:  make(map[uint16]int64, 4),
		lastRawDTS: make(map[uint16]int64, 4),
	}
}

// SetPIDs updates the active elementary stream PIDs
func (pr *PTSRemapper) SetPIDs(videoPID, audioPID, pmtPID uint16) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if videoPID != 0 {
		pr.videoPID = videoPID
	}
	if audioPID != 0 {
		pr.audioPID = audioPID
	}
	if pmtPID != 0 {
		pr.pmtPID = pmtPID
	}
}

// NotifySourceSwitch marks that the next Remap() call comes from a new source or slate loop
// so the timestamp offset will be seamlessly recalculated without resetting Continuity Counters.
func (pr *PTSRemapper) NotifySourceSwitch() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.needSync = true
	clear(pr.lastRawDTS)
}

// PrepareSwitch immediately computes the new offset from the first batch of a new source
func (pr *PTSRemapper) PrepareSwitch(newSourceData []byte) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.needSync = true
	clear(pr.lastRawDTS)
	if pr.hasOutput {
		pr.syncOffsetLocked(newSourceData)
	}
}

// TrackOutgoing is retained as a safe no-op because Remap() now tracks all outgoing timestamps directly
func (pr *PTSRemapper) TrackOutgoing(_ []byte) {}

func (pr *PTSRemapper) isMediaPIDLocked(pid uint16) bool {
	if pid == 0 {
		return false
	}
	return pid == pr.videoPID || pid == pr.audioPID || pid == defaultVideoPID || pid == defaultAudioPID
}

// syncOffsetLocked scans up to the first 32 TS packets to find the minimum incoming DTS/PTS
// across both video and audio, setting pr.offset so both DTS and PTS advance monotonically
// even when switching between B-frame streams (iPhone) and non-B-frame slates.
func (pr *PTSRemapper) syncOffsetLocked(data []byte) bool {
	var foundPES bool
	var minDTS, minPTS int64
	var firstPCR int64

	limit := len(data)
	if limit > tsPacketSize*32 {
		limit = tsPacketSize * 32
	}

	for off := 0; off+tsPacketSize <= limit; off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}

		if firstPCR == 0 && info.HasAdaptationField && info.AdaptationLength >= 6 {
			if pkt[5]&0x10 != 0 {
				firstPCR = extractPCR(pkt[6:])
			}
		}

		if info.PUSI && info.HasPayload && pr.isMediaPIDLocked(info.PID) && info.PayloadOffset+14 <= tsPacketSize {
			pts, dts, hasPTS := extractPTSDTS(pkt[info.PayloadOffset:])
			if hasPTS {
				if dts == 0 {
					dts = pts
				}
				if !foundPES {
					minDTS = dts
					minPTS = pts
					foundPES = true
				} else {
					if dts < minDTS {
						minDTS = dts
					}
					if pts < minPTS {
						minPTS = pts
					}
				}
			}
		}
	}

	if foundPES {
		offDTS := pr.maxAnyDTS + defaultStep - minDTS
		offPTS := pr.maxAnyPTS + defaultStep - minPTS
		if offPTS > offDTS {
			pr.offset = offPTS
		} else {
			pr.offset = offDTS
		}
		pr.offsetSet = true
		pr.needSync = false
		return true
	}

	if firstPCR > 0 && pr.maxAnyPCR > 0 {
		pr.offset = (pr.maxAnyPCR/300 + defaultStep) - (firstPCR / 300)
		pr.offsetSet = true
		pr.needSync = false
		return true
	}

	return false
}

// Remap rewrites Continuity Counters and PTS/DTS/PCR timestamps IN PLACE.
// Callers passing shared buffers (such as RAM slate slices) must pass a copy.
func (pr *PTSRemapper) Remap(data []byte) []byte {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	if pr.needSync && pr.hasOutput {
		pr.syncOffsetLocked(data)
	}

	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}

		// 1. Rewrite Continuity Counter per PID (ISO/IEC 13818-1)
		if info.PID != nullPID && info.PID < 8192 {
			if info.HasPayload {
				if !pr.ccInit[info.PID] {
					pr.ccInit[info.PID] = true
					pr.cc[info.PID] = 0
				} else {
					pr.cc[info.PID] = (pr.cc[info.PID] + 1) & 0x0F
				}
			}
			pkt[3] = (pkt[3] & 0xF0) | (pr.cc[info.PID] & 0x0F)
		}

		// 2. Detect unexpected backward jump (>1s) in raw incoming stream if not already synced
		if info.PUSI && info.HasPayload && pr.isMediaPIDLocked(info.PID) && info.PayloadOffset+14 <= tsPacketSize {
			pts, dts, hasPTS := extractPTSDTS(pkt[info.PayloadOffset:])
			if hasPTS {
				rawOrd := dts
				if rawOrd == 0 {
					rawOrd = pts
				}
				if prevRaw, seen := pr.lastRawDTS[info.PID]; seen && pr.hasOutput && rawOrd < prevRaw-90000 {
					pr.syncOffsetLocked(data[off:])
					log.Printf("[remapper] Backward timestamp jump detected on PID %d (%d -> %d), adjusted offset=%d", info.PID, prevRaw, rawOrd, pr.offset)
				}
				pr.lastRawDTS[info.PID] = rawOrd
			}
		}

		// 3. Rewrite PCR in adaptation field
		if info.HasAdaptationField && info.AdaptationLength >= 6 && (pkt[5]&0x10) != 0 {
			rawPCR := extractPCR(pkt[6:])
			newPCR := rawPCR + pr.offset*300
			if newPCR < 0 {
				newPCR = 0
			}
			if pr.offsetSet {
				writePCR(pkt[6:], newPCR)
			}
			if newPCR > pr.maxAnyPCR {
				pr.maxAnyPCR = newPCR
			}
		}

		// 4. Rewrite PTS/DTS in PES header and enforce per-PID strict DTS monotonicity
		if info.PUSI && info.HasPayload && pr.isMediaPIDLocked(info.PID) && info.PayloadOffset+14 <= tsPacketSize {
			pesData := pkt[info.PayloadOffset:]
			pts, dts, hasPTS := extractPTSDTS(pesData)
			if hasPTS {
				hasDTS := dts > 0 || ((pesData[7]>>6)&0x03) == 3
				newPTS := pts + pr.offset
				newDTS := newPTS
				if hasDTS {
					newDTS = dts + pr.offset
				}

				if prevMax, ok := pr.maxOutDTS[info.PID]; ok && newDTS <= prevMax {
					delta := prevMax + 1920 - newDTS // +21.3ms (1 AAC frame) guard
					newDTS += delta
					newPTS += delta
				}
				if newPTS < 0 {
					newPTS = 0
				}
				if newDTS < 0 {
					newDTS = 0
				}

				if pr.offsetSet || newPTS != pts || (hasDTS && newDTS != dts) {
					marker := byte(0x20)
					if hasDTS {
						marker = 0x30
					}
					writeTimestamp(pesData[9:14], newPTS, marker)
					if hasDTS && len(pesData) >= 19 {
						writeTimestamp(pesData[14:19], newDTS, 0x10)
					}
				}

				pr.maxOutDTS[info.PID] = newDTS
				if newDTS > pr.maxAnyDTS {
					pr.maxAnyDTS = newDTS
				}
				if newPTS > pr.maxAnyPTS {
					pr.maxAnyPTS = newPTS
				}
				pr.hasOutput = true
			}
		}
	}

	return data
}

// Reset clears timestamp state completely (used only in tests or full restart)
func (pr *PTSRemapper) Reset() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.offset = 0
	pr.offsetSet = false
	pr.needSync = false
	pr.hasOutput = false
	pr.maxAnyPTS = 0
	pr.maxAnyDTS = 0
	pr.maxAnyPCR = 0
	clear(pr.maxOutDTS)
	clear(pr.lastRawDTS)
	pr.cc = [8192]uint8{}
	pr.ccInit = [8192]bool{}
}

// IsActive returns true if a timestamp offset is currently active
func (pr *PTSRemapper) IsActive() bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.offsetSet
}

// --- Low-level MPEG-TS timestamp helpers ---

func extractPCR(data []byte) int64 {
	if len(data) < 6 {
		return 0
	}
	base := int64(data[0])<<25 | int64(data[1])<<17 | int64(data[2])<<9 |
		int64(data[3])<<1 | int64(data[4]>>7)
	ext := int64(data[4]&0x01)<<8 | int64(data[5])
	return base*300 + ext
}

func writePCR(data []byte, pcr int64) {
	if len(data) < 6 {
		return
	}
	wrapped := ((pcr % pcrWrapMod) + pcrWrapMod) % pcrWrapMod
	base := wrapped / 300
	ext := wrapped % 300

	data[0] = byte(base >> 25)
	data[1] = byte(base >> 17)
	data[2] = byte(base >> 9)
	data[3] = byte(base >> 1)
	data[4] = byte(base<<7) | 0x7E | byte((ext>>8)&0x01)
	data[5] = byte(ext)
}

func extractPTSDTS(pesData []byte) (pts, dts int64, ok bool) {
	if len(pesData) < 14 {
		return 0, 0, false
	}
	if pesData[0] != 0x00 || pesData[1] != 0x00 || pesData[2] != 0x01 {
		return 0, 0, false
	}

	ptsDTSFlags := (pesData[7] >> 6) & 0x03
	if ptsDTSFlags < 2 {
		return 0, 0, false
	}

	pts = readTimestamp(pesData[9:14])
	if ptsDTSFlags == 3 && len(pesData) >= 19 {
		dts = readTimestamp(pesData[14:19])
	}
	return pts, dts, true
}

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

func writeTimestamp(data []byte, ts int64, marker byte) {
	if len(data) < 5 {
		return
	}
	wrapped := ts & ptsWrapMask
	data[0] = marker | byte((wrapped>>29)&0x0E) | 0x01
	data[1] = byte(wrapped >> 22)
	data[2] = byte((wrapped>>14)&0xFE) | 0x01
	data[3] = byte(wrapped >> 7)
	data[4] = byte((wrapped<<1)&0xFE) | 0x01
}
