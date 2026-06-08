package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// AudioMeter monitors audio levels from the broadcaster stream using ebur128
type AudioMeter struct {
	broadcaster *Broadcaster
	peakL       float64
	peakR       float64
	momentary   float64
	mu          sync.RWMutex
	cmd         *exec.Cmd
	running     bool
	stopCh      chan struct{}
}

type AudioLevels struct {
	PeakL     float64 `json:"peak_left_db"`
	PeakR     float64 `json:"peak_right_db"`
	Momentary float64 `json:"momentary_lufs"`
}

func NewAudioMeter(broadcaster *Broadcaster) *AudioMeter {
	return &AudioMeter{
		broadcaster: broadcaster,
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
	am.running = true
	log.Println("[audio] Starting audio meter (ebur128)")
	for am.running {
		am.runMeterProcess()
		if !am.running {
			break
		}
		select {
		case <-time.After(3 * time.Second):
		case <-am.stopCh:
			return
		}
	}
}

func (am *AudioMeter) Stop() {
	am.running = false
	select {
	case <-am.stopCh:
	default:
		close(am.stopCh)
	}
	if am.cmd != nil && am.cmd.Process != nil {
		am.cmd.Process.Kill()
	}
	time.Sleep(100 * time.Millisecond)
}

// ebur128 output line example:
// [Parsed_ebur128_0 @ 0x...] t: 1.0   M: -23.0 S: -23.0   I: -70.0 ...   FTPK: -12.3 -12.3 dBFS  TPK: -12.3 -12.3 dBFS
var ebur128Re = regexp.MustCompile(`M:\s*([-\d.]+)\s+S:.*?FTPK:\s*([-\d.]+)\s+([-\d.]+)`)

func (am *AudioMeter) runMeterProcess() {
	subID := "audiometer_internal"
	dataCh := am.broadcaster.Subscribe(subID)
	defer am.broadcaster.Unsubscribe(subID)

	args := []string{
		"-hide_banner",
		"-fflags", "+genpts+discardcorrupt",
		"-analyzeduration", "2000000",
		"-probesize", "1000000",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-vn",
		"-filter_complex", "ebur128=peak=true",
		"-f", "null", "-",
	}

	cmd := exec.Command("ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[audio] stdin pipe error: %v", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("[audio] stderr pipe error: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[audio] start error: %v", err)
		return
	}
	am.cmd = cmd
	log.Printf("[audio] FFmpeg ebur128 started (PID %d)", cmd.Process.Pid)

	// Feed data to FFmpeg
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
			case <-am.stopCh:
				return
			}
		}
	}()

	// Parse ebur128 stderr output
	am.parseEbur128(stderr)
	cmd.Wait()
	log.Println("[audio] FFmpeg ebur128 exited")
}

func (am *AudioMeter) parseEbur128(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4096)
	for scanner.Scan() {
		line := scanner.Text()
		matches := ebur128Re.FindStringSubmatch(line)
		if len(matches) >= 4 {
			m, _ := strconv.ParseFloat(matches[1], 64)
			pl, _ := strconv.ParseFloat(matches[2], 64)
			pr, _ := strconv.ParseFloat(matches[3], 64)
			if m < -70 { m = -70 }
			if pl < -70 { pl = -70 }
			if pr < -70 { pr = -70 }
			am.mu.Lock()
			am.momentary = m
			am.peakL = pl
			am.peakR = pr
			am.mu.Unlock()
		}
	}
}

// FormatLevel returns a human-readable level string
func FormatLevel(db float64) string {
	if db <= -70 {
		return "-∞"
	}
	return fmt.Sprintf("%.1f dB", db)
}
