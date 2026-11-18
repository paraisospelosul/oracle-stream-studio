package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type PreviewStatus struct {
	FPS     int  `json:"fps"`
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	Running bool `json:"running"`
}

// PreviewManager generates an on-demand JPEG preview from the broadcaster stream.
// Defaults to OFF (0% CPU) on boot and uses keyframe-only decoding (-skip_frame nokey)
// at low FPS for minimal CPU footprint on 2-OCPU ARM64 instances.
type PreviewManager struct {
	broadcaster *Broadcaster

	fps     int
	width   int
	height  int
	quality int
	mu      sync.RWMutex

	latestFrame []byte
	frameMu     sync.RWMutex

	cmd       *exec.Cmd
	procMu    sync.Mutex
	running   atomic.Bool
	stopCh    chan struct{}
	stopMutex sync.Mutex
}

func NewPreviewManager(broadcaster *Broadcaster) *PreviewManager {
	return &PreviewManager{
		broadcaster: broadcaster,
		fps:         0, // OFF by default — zero CPU until requested in Web UI
		width:       640,
		height:      360,
		quality:     6,
	}
}

func (p *PreviewManager) GetSettings() (fps, width, height int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fps, p.width, p.height
}

func (p *PreviewManager) GetStatus() PreviewStatus {
	p.mu.RLock()
	fps, w, h := p.fps, p.width, p.height
	p.mu.RUnlock()
	return PreviewStatus{
		FPS:     fps,
		Width:   w,
		Height:  h,
		Running: p.running.Load(),
	}
}

func (p *PreviewManager) IsRunning() bool {
	return p.running.Load()
}

// DisableAuto turns off preview automatically when no WebSocket clients remain open
func (p *PreviewManager) DisableAuto() {
	p.mu.RLock()
	w, h := p.width, p.height
	p.mu.RUnlock()
	log.Println("[preview] Auto-disabling preview (no open Web UI clients for 30s)")
	p.UpdateSettings(0, w, h)
}

func (p *PreviewManager) UpdateSettings(fps, width, height int) {
	if fps < 0 {
		fps = 0
	}
	if fps > 15 {
		fps = 15
	}
	if width <= 0 {
		width = 640
	}
	if height <= 0 {
		height = 360
	}

	p.mu.Lock()
	p.fps = fps
	p.width = width
	p.height = height
	p.mu.Unlock()

	if fps == 0 {
		p.Stop()
		p.frameMu.Lock()
		p.latestFrame = nil
		p.frameMu.Unlock()
		log.Printf("[preview] Preview disabled (0%% CPU)")
		return
	}

	p.Stop()
	go p.Start()
	log.Printf("[preview] Settings updated: %dfps %dx%d", fps, width, height)
}

func (p *PreviewManager) GetFrame() []byte {
	p.frameMu.RLock()
	defer p.frameMu.RUnlock()
	if p.latestFrame == nil {
		return nil
	}
	frame := make([]byte, len(p.latestFrame))
	copy(frame, p.latestFrame)
	return frame
}

func (p *PreviewManager) Start() {
	p.mu.RLock()
	fps := p.fps
	width := p.width
	height := p.height
	quality := p.quality
	p.mu.RUnlock()

	if fps <= 0 {
		return
	}
	if !p.running.CompareAndSwap(false, true) {
		return
	}

	p.stopMutex.Lock()
	stopCh := make(chan struct{})
	p.stopCh = stopCh
	p.stopMutex.Unlock()

	log.Printf("[preview] Starting on-demand preview: %dfps %dx%d q%d", fps, width, height, quality)

	for p.running.Load() {
		p.runPreviewProcess(fps, width, height, quality, stopCh)

		if !p.running.Load() {
			break
		}

		select {
		case <-time.After(2 * time.Second):
		case <-stopCh:
			return
		}
	}
}

func (p *PreviewManager) Stop() {
	if !p.running.CompareAndSwap(true, false) {
		return
	}

	p.stopMutex.Lock()
	if p.stopCh != nil {
		select {
		case <-p.stopCh:
		default:
			close(p.stopCh)
		}
	}
	p.stopMutex.Unlock()

	p.procMu.Lock()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.procMu.Unlock()

	time.Sleep(100 * time.Millisecond)
}

func (p *PreviewManager) runPreviewProcess(fps, width, height, quality int, stopCh chan struct{}) {
	subID := "preview_internal"
	dataCh := p.broadcaster.Subscribe(subID)
	defer p.broadcaster.UnsubscribeChannel(subID, dataCh)

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-fflags", "+discardcorrupt",
		"-analyzeduration", "500000",
		"-probesize", "500000",
		"-threads", "1",
	}

	// Ultra-light mode: at <=2 fps, decode ONLY IDR keyframes (~85% lower CPU on ARM!)
	if fps <= 2 {
		args = append(args, "-skip_frame", "nokey")
	}

	vf := fmt.Sprintf("scale=%d:%d:flags=fast_bilinear", width, height)
	if fps > 2 {
		vf = fmt.Sprintf("fps=%d,%s", fps, vf)
	}

	args = append(args,
		"-f", "mpegts",
		"-i", "pipe:0",
		"-vf", vf,
		"-q:v", fmt.Sprintf("%d", quality),
		"-an",
		"-threads", "1",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"pipe:1",
	)

	cmd := exec.Command("ffmpeg", args...)
	setProcessDeathSignal(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[preview] stdin pipe error: %v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[preview] stdout pipe error: %v", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("[preview] stderr pipe error: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[preview] start error: %v", err)
		return
	}

	p.procMu.Lock()
	p.cmd = cmd
	p.procMu.Unlock()

	log.Printf("[preview] FFmpeg started (PID %d, skip_nokey=%v)", cmd.Process.Pid, fps <= 2)

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[preview] FFmpeg: %s", scanner.Text())
		}
	}()

	go func() {
		defer stdin.Close()
		for {
			select {
			case data, ok := <-dataCh:
				if !ok {
					return
				}
				if _, err := stdin.Write(data); err != nil {
					return
				}
			case <-stopCh:
				return
			}
		}
	}()

	p.readFrames(stdout)

	_ = cmd.Wait()
	log.Printf("[preview] FFmpeg exited")
}

func (p *PreviewManager) readFrames(reader io.Reader) {
	soiMarker := []byte{0xFF, 0xD8}
	eoiMarker := []byte{0xFF, 0xD9}

	buf := make([]byte, 64*1024)
	var frameBuf bytes.Buffer
	inFrame := false

	for {
		n, err := reader.Read(buf)
		if err != nil {
			return
		}
		chunk := buf[:n]

		for len(chunk) > 0 {
			if !inFrame {
				idx := bytes.Index(chunk, soiMarker)
				if idx < 0 {
					break
				}
				frameBuf.Reset()
				frameBuf.Write(soiMarker)
				chunk = chunk[idx+2:]
				inFrame = true
			} else {
				idx := bytes.Index(chunk, eoiMarker)
				if idx < 0 {
					frameBuf.Write(chunk)
					break
				}
				frameBuf.Write(chunk[:idx+2])
				chunk = chunk[idx+2:]

				frame := make([]byte, frameBuf.Len())
				copy(frame, frameBuf.Bytes())
				p.frameMu.Lock()
				p.latestFrame = frame
				p.frameMu.Unlock()
				inFrame = false
			}
		}
	}
}
