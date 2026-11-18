package main

// MPEGTS packet parser - low-level MPEG Transport Stream utilities
// Handles PAT/PMT parsing, PID detection, H.265/H.264 keyframe detection, and batch alignment

const (
	tsPacketSize    = 188
	tsSyncByte      = 0x47
	patPID          = 0x0000
	sdtPID          = 0x0011
	nullPID         = 0x1FFF
	defaultPMTPID   = 0x1000 // 4096
	defaultVideoPID = 0x0100 // 256
	defaultAudioPID = 0x0101 // 257
)

// H.265 NAL unit types that indicate keyframes (Random Access Points)
const (
	nalBlaWLP   = 16 // BLA_W_LP
	nalBlaWRADL = 17 // BLA_W_RADL
	nalBlaNLP   = 18 // BLA_N_LP
	nalIdrWRADL = 19 // IDR_W_RADL
	nalIdrNLP   = 20 // IDR_N_LP
	nalCraNUT   = 21 // CRA_NUT
)

// TSPacketInfo holds parsed information from a single 188-byte MPEGTS packet
type TSPacketInfo struct {
	PID                uint16
	PUSI               bool // Payload Unit Start Indicator
	HasAdaptationField bool
	HasPayload         bool
	AdaptationLength   int
	PayloadOffset      int
	ContinuityCounter  uint8
}

// ParseTSPacket extracts basic info from a 188-byte MPEGTS packet
func ParseTSPacket(pkt []byte) (TSPacketInfo, bool) {
	if len(pkt) < tsPacketSize || pkt[0] != tsSyncByte {
		return TSPacketInfo{}, false
	}

	info := TSPacketInfo{}
	info.PID = uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	info.PUSI = (pkt[1] & 0x40) != 0

	adaptationControl := (pkt[3] >> 4) & 0x03
	info.ContinuityCounter = pkt[3] & 0x0F
	info.HasAdaptationField = (adaptationControl & 0x02) != 0
	info.HasPayload = (adaptationControl & 0x01) != 0

	offset := 4
	if info.HasAdaptationField {
		if offset >= tsPacketSize {
			return info, false
		}
		info.AdaptationLength = int(pkt[offset])
		offset += 1 + info.AdaptationLength
	}

	info.PayloadOffset = offset
	return info, true
}

// ParsePAT extracts the PMT PID from a Program Association Table
func ParsePAT(payload []byte) uint16 {
	if len(payload) < 8 {
		return 0
	}

	offset := 1 + int(payload[0])
	if offset+8 > len(payload) {
		return 0
	}

	// Table ID should be 0x00 for PAT
	if payload[offset] != 0x00 {
		return 0
	}

	sectionLength := int(payload[offset+1]&0x0F)<<8 | int(payload[offset+2])
	if sectionLength < 9 {
		return 0
	}

	progOffset := offset + 8
	endOffset := offset + 3 + sectionLength - 4 // -4 for CRC

	for progOffset+4 <= endOffset && progOffset+4 <= len(payload) {
		programNum := uint16(payload[progOffset])<<8 | uint16(payload[progOffset+1])
		pmtPID := uint16(payload[progOffset+2]&0x1F)<<8 | uint16(payload[progOffset+3])

		if programNum != 0 { // Skip NIT (program 0)
			return pmtPID
		}
		progOffset += 4
	}

	return 0
}

// ParsePMT extracts video and audio PIDs from a Program Map Table
func ParsePMT(payload []byte) (videoPID, audioPID uint16) {
	if len(payload) < 12 {
		return 0, 0
	}

	offset := 1 + int(payload[0])
	if offset+12 > len(payload) {
		return 0, 0
	}

	// Table ID should be 0x02 for PMT
	if payload[offset] != 0x02 {
		return 0, 0
	}

	sectionLength := int(payload[offset+1]&0x0F)<<8 | int(payload[offset+2])
	if offset+10 >= len(payload) {
		return 0, 0
	}
	programInfoLength := int(payload[offset+10]&0x0F)<<8 | int(payload[offset+11])

	streamOffset := offset + 12 + programInfoLength
	endOffset := offset + 3 + sectionLength - 4 // -4 for CRC

	for streamOffset+5 <= endOffset && streamOffset+5 <= len(payload) {
		streamType := payload[streamOffset]
		elementaryPID := uint16(payload[streamOffset+1]&0x1F)<<8 | uint16(payload[streamOffset+2])
		esInfoLength := int(payload[streamOffset+3]&0x0F)<<8 | int(payload[streamOffset+4])

		switch streamType {
		case 0x24: // H.265/HEVC
			if videoPID == 0 {
				videoPID = elementaryPID
			}
		case 0x1B: // H.264/AVC
			if videoPID == 0 {
				videoPID = elementaryPID
			}
		case 0x0F, 0x11: // AAC (ADTS / LATM)
			if audioPID == 0 {
				audioPID = elementaryPID
			}
		case 0x03, 0x04: // MPEG Audio
			if audioPID == 0 {
				audioPID = elementaryPID
			}
		}

		streamOffset += 5 + esInfoLength
	}

	return videoPID, audioPID
}

// IsKeyframe checks if the PES payload contains an H.264 or H.265 keyframe NAL unit
func IsKeyframe(pesPayload []byte) bool {
	i := 0
	for i < len(pesPayload)-4 {
		if pesPayload[i] == 0x00 && pesPayload[i+1] == 0x00 {
			nalStart := -1
			if pesPayload[i+2] == 0x01 {
				nalStart = i + 3
			} else if pesPayload[i+2] == 0x00 && i+3 < len(pesPayload) && pesPayload[i+3] == 0x01 {
				nalStart = i + 4
			}

			if nalStart != -1 && nalStart < len(pesPayload) {
				// H.264 (IDR=5, SPS=7, PPS=8)
				nalTypeH264 := pesPayload[nalStart] & 0x1F
				if nalTypeH264 == 5 || nalTypeH264 == 7 || nalTypeH264 == 8 {
					return true
				}

				// H.265 (IDR/CRA/BLA=16..21, VPS/SPS/PPS=32..34)
				nalTypeH265 := (pesPayload[nalStart] >> 1) & 0x3F
				if (nalTypeH265 >= nalBlaWLP && nalTypeH265 <= nalCraNUT) || (nalTypeH265 >= 32 && nalTypeH265 <= 34) {
					return true
				}

				i = nalStart
				continue
			}
		}
		i++
	}

	return false
}

// ExtractPESPayload extracts the elementary stream payload from a PES packet
// that starts in this MPEGTS packet (PUSI must be set)
func ExtractPESPayload(pkt []byte, payloadOffset int) []byte {
	if payloadOffset >= tsPacketSize {
		return nil
	}

	payload := pkt[payloadOffset:]
	if len(payload) < 9 {
		return nil
	}

	if payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
		return nil
	}

	pesHeaderDataLength := int(payload[8])
	dataStart := 9 + pesHeaderDataLength
	if dataStart >= len(payload) {
		return nil
	}

	return payload[dataStart:]
}

// PacketAligner aligns an arbitrary byte stream to 188-byte MPEGTS packet boundaries
type PacketAligner struct {
	buf    []byte
	synced bool
}

// NewPacketAligner creates a PacketAligner with a reusable backing buffer
func NewPacketAligner() *PacketAligner {
	return &PacketAligner{
		buf: make([]byte, 0, tsPacketSize*64),
	}
}

// Feed appends raw bytes to the aligner buffer
func (pa *PacketAligner) Feed(data []byte) {
	pa.buf = append(pa.buf, data...)
}

// syncBuffer locates the MPEGTS sync boundary inside pa.buf
func (pa *PacketAligner) syncBuffer() bool {
	if len(pa.buf) < tsPacketSize {
		return false
	}
	if pa.synced && pa.buf[0] == tsSyncByte {
		return true
	}

	pa.synced = false
	syncIdx := -1
	for i := 0; i <= len(pa.buf)-tsPacketSize; i++ {
		if pa.buf[i] == tsSyncByte {
			if i+tsPacketSize < len(pa.buf) {
				if pa.buf[i+tsPacketSize] == tsSyncByte {
					syncIdx = i
					break
				}
			} else {
				syncIdx = i
				break
			}
		}
	}

	if syncIdx < 0 {
		// Keep last (tsPacketSize - 1) bytes in case a sync byte sits at the tail
		keep := tsPacketSize - 1
		if len(pa.buf) > keep {
			copy(pa.buf, pa.buf[len(pa.buf)-keep:])
			pa.buf = pa.buf[:keep]
		}
		return false
	}

	if syncIdx > 0 {
		rem := len(pa.buf) - syncIdx
		copy(pa.buf, pa.buf[syncIdx:])
		pa.buf = pa.buf[:rem]
	}
	pa.synced = true
	return len(pa.buf) >= tsPacketSize
}

// Next returns a single aligned 188-byte MPEGTS packet, or nil if not enough data
func (pa *PacketAligner) Next() []byte {
	for {
		if !pa.syncBuffer() {
			return nil
		}
		if pa.buf[0] != tsSyncByte {
			pa.synced = false
			continue
		}

		pkt := make([]byte, tsPacketSize)
		copy(pkt, pa.buf[:tsPacketSize])
		rem := len(pa.buf) - tsPacketSize
		if rem > 0 {
			copy(pa.buf, pa.buf[tsPacketSize:])
		}
		pa.buf = pa.buf[:rem]
		return pkt
	}
}

// NextBatch returns all contiguous aligned 188-byte MPEGTS packets currently in the buffer
// in a single allocation, drastically reducing GC pressure, channel operations, and syscalls.
func (pa *PacketAligner) NextBatch() []byte {
	for {
		if !pa.syncBuffer() {
			return nil
		}
		if pa.buf[0] != tsSyncByte {
			pa.synced = false
			continue
		}

		validLen := 0
		for validLen+tsPacketSize <= len(pa.buf) {
			if pa.buf[validLen] != tsSyncByte {
				break
			}
			validLen += tsPacketSize
		}

		if validLen == 0 {
			pa.synced = false
			continue
		}

		batch := make([]byte, validLen)
		copy(batch, pa.buf[:validLen])

		rem := len(pa.buf) - validLen
		if rem > 0 {
			copy(pa.buf, pa.buf[validLen:])
		}
		pa.buf = pa.buf[:rem]
		return batch
	}
}
