package main

// MPEGTS packet parser - low-level MPEG Transport Stream utilities
// Handles PAT/PMT parsing, PID detection, and H.265 keyframe detection

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47
	patPID       = 0x0000
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

	// Skip pointer field if present
	offset := 0
	if len(payload) > 0 {
		pointerField := int(payload[0])
		offset = 1 + pointerField
	}

	if offset+8 > len(payload) {
		return 0
	}

	// Table ID should be 0x00 for PAT
	if payload[offset] != 0x00 {
		return 0
	}

	// Section length
	sectionLength := int(payload[offset+1]&0x0F)<<8 | int(payload[offset+2])
	if sectionLength < 9 {
		return 0
	}

	// Skip to program entries (offset + 8 = past header)
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

	// Skip pointer field
	offset := 0
	if len(payload) > 0 {
		pointerField := int(payload[0])
		offset = 1 + pointerField
	}

	if offset+12 > len(payload) {
		return 0, 0
	}

	// Table ID should be 0x02 for PMT
	if payload[offset] != 0x02 {
		return 0, 0
	}

	sectionLength := int(payload[offset+1]&0x0F)<<8 | int(payload[offset+2])

	// Program info length
	if offset+10 >= len(payload) {
		return 0, 0
	}
	programInfoLength := int(payload[offset+10]&0x0F)<<8 | int(payload[offset+11])

	// Skip to stream entries
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
		case 0x1B: // H.264/AVC (fallback detection)
			if videoPID == 0 {
				videoPID = elementaryPID
			}
		case 0x0F, 0x11: // AAC
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

			if nalStart != -1 {
				// Check H.264 (IDR is 5, SPS is 7, PPS is 8)
				nalTypeH264 := pesPayload[nalStart] & 0x1F
				if nalTypeH264 == 5 || nalTypeH264 == 7 || nalTypeH264 == 8 {
					return true
				}

				// Check H.265 (IDR/CRA/BLA are 16-21, VPS/SPS/PPS are 32-34)
				nalTypeH265 := (pesPayload[nalStart] >> 1) & 0x3F
				if (nalTypeH265 >= nalBlaWLP && nalTypeH265 <= nalCraNUT) || (nalTypeH265 >= 32 && nalTypeH265 <= 34) {
					return true
				}

				// Move index to nalStart to continue searching from the data of this NAL unit
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

	// Check PES start code: 0x00 0x00 0x01
	if payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
		return nil
	}

	// PES header data length
	if len(payload) < 9 {
		return nil
	}
	pesHeaderDataLength := int(payload[8])

	dataStart := 9 + pesHeaderDataLength
	if dataStart >= len(payload) {
		return nil
	}

	return payload[dataStart:]
}

// PacketAligner helps align a byte stream to 188-byte MPEGTS packet boundaries
type PacketAligner struct {
	buf    []byte
	synced bool
}

// NewPacketAligner creates a new PacketAligner
func NewPacketAligner() *PacketAligner {
	return &PacketAligner{
		buf: make([]byte, 0, tsPacketSize*20),
	}
}

// Feed adds raw bytes to the aligner buffer
func (pa *PacketAligner) Feed(data []byte) {
	pa.buf = append(pa.buf, data...)
}

// Next returns the next aligned 188-byte MPEGTS packet, or nil if not enough data
func (pa *PacketAligner) Next() []byte {
	for {
		if len(pa.buf) < tsPacketSize {
			return nil
		}

		if !pa.synced {
			// Find sync byte
			syncIdx := -1
			for i := 0; i < len(pa.buf)-tsPacketSize; i++ {
				if pa.buf[i] == tsSyncByte {
					// Verify next packet also starts with sync byte
					if i+tsPacketSize < len(pa.buf) && pa.buf[i+tsPacketSize] == tsSyncByte {
						syncIdx = i
						break
					}
					// If we can't verify, try anyway if it's the last possible position
					if i+tsPacketSize >= len(pa.buf) {
						syncIdx = i
						break
					}
				}
			}
			if syncIdx < 0 {
				// Discard all but last tsPacketSize bytes
				if len(pa.buf) > tsPacketSize {
					pa.buf = pa.buf[len(pa.buf)-tsPacketSize:]
				}
				return nil
			}
			pa.buf = pa.buf[syncIdx:]
			pa.synced = true
		}

		if len(pa.buf) < tsPacketSize {
			return nil
		}

		if pa.buf[0] != tsSyncByte {
			pa.synced = false
			continue
		}

		pkt := make([]byte, tsPacketSize)
		copy(pkt, pa.buf[:tsPacketSize])
		pa.buf = pa.buf[tsPacketSize:]
		return pkt
	}
}
