package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// TranscodingBridge provides temporary transcoding to force-generate IDR frames
// when switching between sources without waiting for natural keyframes.
//
// In this version, the bridge runs a continuous transcoding session during the transition.
// It accepts raw packets, writes them to FFmpeg, and emits transcoded packets (guaranteed to
// start with an IDR frame) to the output channel. Once a natural keyframe is detected on the
// source stream, the bridge is stopped.
type TranscodingBridge struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	active  atomic.Bool
	outCh   chan []byte
	stopCh  chan struct{}
	mu      sync.Mutex
	lastUse time.Time
}

// NewTranscodingBridge creates a new bridge instance
// By default, this tool will error if TargetFile already exists. To overwrite an existing file, set Overwrite to true.
func NewTranscodingBridge() *TranscodingBridge {
	return &TranscodingBridge{}
}

// IsAvailable returns true if the bridge is not currently in use
func (tb *TranscodingBridge) IsAvailable() bool {
	return !tb.active.Load()
}

// DetectCodec examines MPEG-TS data to determine if it contains H.264 or H.265 video.
// Returns "h265", "h264", or "" if unable to detect.
func DetectCodec(data []byte) string {
	var pmtPID uint16
	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		if len(pkt) < tsPacketSize || pkt[0] != tsSyncByte {
			continue
		}
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}
		// Extract PMT PID from PAT
		if info.PID == patPID && info.PUSI && info.HasPayload {
			if pid := ParsePAT(pkt[info.PayloadOffset:]); pid != 0 {
				pmtPID = pid
			}
		}
		// Parse PMT for stream type
		if pmtPID != 0 && info.PID == pmtPID && info.PUSI && info.HasPayload {
			payload := pkt[info.PayloadOffset:]
			if len(payload) < 12 {
				continue
			}
			poff := 0
			if len(payload) > 0 {
				poff = 1 + int(payload[0])
			}
			if poff+12 > len(payload) || payload[poff] != 0x02 {
				continue
			}
			sectionLength := int(payload[poff+1]&0x0F)<<8 | int(payload[poff+2])
			programInfoLength := int(payload[poff+10]&0x0F)<<8 | int(payload[poff+11])
			streamOffset := poff + 12 + programInfoLength
			endOffset := poff + 3 + sectionLength - 4
			for streamOffset+5 <= endOffset && streamOffset+5 <= len(payload) {
				streamType := payload[streamOffset]
				esInfoLen := int(payload[streamOffset+3]&0x0F)<<8 | int(payload[streamOffset+4])
				switch streamType {
				case 0x24:
					return "h265"
				case 0x1B:
					return "h264"
				}
				streamOffset += 5 + esInfoLen
			}
		}
	}
	return ""
}

// Start starts a continuous transcoding session using a transient FFmpeg process.
// It immediately writes the cached PAT and PMT (if available) to stdin so FFmpeg has the stream layout,
// then reads output packets and writes them to outCh.
func (tb *TranscodingBridge) Start(ctx context.Context, codec string, pat, pmt []byte, outCh chan []byte) error {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	if !tb.active.CompareAndSwap(false, true) {
		return fmt.Errorf("bridge already active")
	}

	tb.lastUse = time.Now()

	// Default to H.264 if undetected
	if codec == "" {
		codec = "h264"
	}

	var args []string
	switch codec {
	case "h265":
		args = []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "mpegts", "-i", "pipe:0",
			"-c:v", "libx265", "-preset", "ultrafast",
			"-x265-params", "keyint=30:min-keyint=30:pools=1:frame-threads=1:numa-pools=1",
			"-c:a", "copy",
			"-f", "mpegts", "-flush_packets", "1",
			"pipe:1",
		}
	case "h264":
		args = []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "mpegts", "-i", "pipe:0",
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-x264opts", "keyint=30:min-keyint=30",
			"-threads", "1",
			"-c:a", "copy",
			"-f", "mpegts", "-flush_packets", "1",
			"pipe:1",
		}
	default:
		args = []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "mpegts", "-i", "pipe:0",
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-x264opts", "keyint=30:min-keyint=30",
			"-threads", "1",
			"-c:a", "copy",
			"-f", "mpegts", "-flush_packets", "1",
			"pipe:1",
		}
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		tb.active.Store(false)
		return fmt.Errorf("bridge stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		tb.active.Store(false)
		return fmt.Errorf("bridge stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		tb.active.Store(false)
		return fmt.Errorf("bridge start: %w", err)
	}

	tb.cmd = cmd
	tb.stdin = stdinPipe
	tb.stdout = stdoutPipe
	tb.stopCh = make(chan struct{})
	tb.outCh = outCh

	log.Printf("[bridge] Transcoding bridge started (codec=%s)", codec)

	// Inject cached PAT/PMT if available
	if len(pat) > 0 {
		stdinPipe.Write(pat)
	}
	if len(pmt) > 0 {
		stdinPipe.Write(pmt)
	}

	// Read loop to pipe output back to switcher
	go func() {
		defer tb.Stop()
		buf := make([]byte, 1316*7) // aligned to MPEG-TS packet groups
		for {
			n, err := stdoutPipe.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				select {
				case outCh <- append([]byte(nil), buf[:n]...):
				case <-tb.stopCh:
					return
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return nil
}

// Write writes raw video packets to the running transcoding bridge
func (tb *TranscodingBridge) Write(data []byte) error {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	if !tb.active.Load() || tb.stdin == nil {
		return fmt.Errorf("bridge not active")
	}

	_, err := tb.stdin.Write(data)
	return err
}

// Stop stops the transcoding session and kills the FFmpeg process
func (tb *TranscodingBridge) Stop() {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	if !tb.active.Load() {
		return
	}

	if tb.stopCh != nil {
		select {
		case <-tb.stopCh:
		default:
			close(tb.stopCh)
		}
	}

	if tb.stdin != nil {
		tb.stdin.Close()
	}

	if tb.cmd != nil {
		if tb.cmd.Process != nil {
			tb.cmd.Process.Kill()
		}
		tb.cmd.Wait()
	}

	tb.active.Store(false)
	log.Println("[bridge] Transcoding bridge stopped")
}
