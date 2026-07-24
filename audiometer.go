package main

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// AudioMeter monitors audio presence from the broadcaster stream by counting
// audio TS packet bytes — zero FFmpeg, zero CPU decoding.
// Replaces the old ebur128-based meter that spawned a permanent FFmpeg process.
type AudioMeter struct {
	broadcaster *Broadcaster
	switcher    *Switcher

	// Estimated levels based on audio byte throughput
	peakL     float64
	peakR     float64
	momentary float64
	mu        sync.RWMutex

	// Byte counting
	audioBytes atomic.Uint64
	running    atomic.Bool
	stopCh     chan struct{}
}

type AudioLevels struct {
	PeakL     float64 `json:"peak_left_db"`
	PeakR     float64 `json:"peak_right_db"`
	Momentary float64 `json:"momentary_lufs"`
}

func NewAudioMeter(broadcaster *Broadcaster, switcher *Switcher) *AudioMeter {
	return &AudioMeter{
		broadcaster: broadcaster,
		switcher:    switcher,
		peakL:       -70,
		peakR:       -70,
		momentary:   -70,
	}
}

func (am *AudioMeter) GetLevels() AudioLevels {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return AudioLevels{PeakL: am.peakL, PeakR: am.peakR, Momentary: am.momentary}
}

func (am *AudioMeter) Start() {
	am.stopCh = make(chan struct{})
	am.running.Store(true)
	log.Println("[audio] Starting lightweight audio meter (TS byte counter)")

	// Goroutine 1: count audio bytes from broadcaster packets
	go am.countAudioBytes()

	// Goroutine 2: estimate levels every second from byte count
	am.estimateLoop()
}

func (am *AudioMeter) Stop() {
	if !am.running.CompareAndSwap(true, false) {
		return
	}
	select {
	case <-am.stopCh:
	default:
		close(am.stopCh)
	}
}

// countAudioBytes subscribes to the broadcaster and counts bytes belonging
// to audio PID packets. This runs entirely in memory with zero FFmpeg.
func (am *AudioMeter) countAudioBytes() {
	subID := "audiometer_bytecounter"
	dataCh := am.broadcaster.Subscribe(subID)
	defer am.broadcaster.Unsubscribe(subID)

	for {
		select {
		case data, ok := <-dataCh:
			if !ok {
				return
			}
			audioPID := am.switcher.audioPID
			if audioPID == 0 {
				continue
			}
			// Count bytes in TS packets that match the audio PID
			audioBytes := countPIDBytes(data, audioPID)
			if audioBytes > 0 {
				am.audioBytes.Add(uint64(audioBytes))
			}
		case <-am.stopCh:
			return
		}
	}
}

// countPIDBytes counts total payload bytes in TS packets matching the given PID
func countPIDBytes(data []byte, targetPID uint16) int {
	total := 0
	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		if pkt[0] != tsSyncByte {
			continue
		}
		pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		if pid != targetPID {
			continue
		}
		// Count payload bytes (after header + adaptation field)
		adaptationControl := (pkt[3] >> 4) & 0x03
		hasPayload := (adaptationControl & 0x01) != 0
		if !hasPayload {
			continue
		}
		payloadOffset := 4
		if (adaptationControl & 0x02) != 0 { // has adaptation field
			if payloadOffset < tsPacketSize {
				payloadOffset += 1 + int(pkt[4])
			}
		}
		if payloadOffset < tsPacketSize {
			total += tsPacketSize - payloadOffset
		}
	}
	return total
}

// estimateLoop runs every second and converts byte throughput to estimated dB levels.
// This provides a "good enough" indicator for the dashboard without any audio decoding.
//
// Mapping logic:
//   - 0 bytes/s          → silence (-70 dB)
//   - >0 but <2KB/s      → very quiet (-50 dB)
//   - 2-8KB/s            → quiet (-40 to -30 dB)
//   - 8-16KB/s           → normal (-24 to -12 dB)
//   - >16KB/s            → loud (-12 to -3 dB)
//
// These thresholds are tuned for AAC 128kbps stereo (~16KB/s at full volume).
func (am *AudioMeter) estimateLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-am.stopCh:
			return
		case <-ticker.C:
			bytesPerSec := am.audioBytes.Swap(0)
			level := bytesToEstimatedDB(bytesPerSec)

			am.mu.Lock()
			am.peakL = level
			am.peakR = level
			am.momentary = level
			am.mu.Unlock()
		}
	}
}

// bytesToEstimatedDB converts audio bytes/second to an estimated dB level.
// This is NOT a real audio measurement — it's a presence/activity indicator.
func bytesToEstimatedDB(bytesPerSec uint64) float64 {
	if bytesPerSec == 0 {
		return -70.0 // Silence
	}

	// AAC 128kbps stereo = ~16000 bytes/sec of audio payload
	// Map linearly in log scale:
	//   100 bytes/s  → -50 dB (minimal data, likely padding)
	//   16000 bytes/s → -6 dB (full bitrate = normal audio)
	kbps := float64(bytesPerSec) * 8.0 / 1000.0 // Convert to kbps

	switch {
	case kbps < 1:
		return -60.0
	case kbps < 16:
		return -50.0
	case kbps < 48:
		return -40.0
	case kbps < 96:
		return -24.0
	case kbps < 128:
		return -12.0
	default:
		return -6.0
	}
}

// FormatLevel returns a human-readable level string
func FormatLevel(db float64) string {
	if db <= -70 {
		return "-∞"
	}
	return fmt.Sprintf("%.1f dB", db)
}
