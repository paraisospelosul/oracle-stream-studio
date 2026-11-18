package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const recorderSubID = "recorder_internal"

// Recorder records the broadcaster stream to a .ts file using buffered I/O, then remuxes to .mp4 on stop
type Recorder struct {
	broadcaster *Broadcaster
	recordDir   string

	recording atomic.Bool
	startTime time.Time
	tsPath    string
	mp4Path   string
	file      *os.File
	fileSize  atomic.Uint64
	mu        sync.RWMutex
	stopCh    chan struct{}
	doneCh    chan struct{}
}

type RecordingStatus struct {
	Active   bool   `json:"active"`
	Duration string `json:"duration,omitempty"`
	SizeMB   int64  `json:"size_mb,omitempty"`
	FilePath string `json:"file_path,omitempty"`
}

type RecordingEntry struct {
	Name    string `json:"name"`
	SizeMB  int64  `json:"size_mb"`
	Path    string `json:"path"`
	ModTime string `json:"mod_time"`
}

func NewRecorder(broadcaster *Broadcaster, dataDir string) *Recorder {
	recordDir := filepath.Join(dataDir, "recordings")
	os.MkdirAll(recordDir, 0755)
	return &Recorder{
		broadcaster: broadcaster,
		recordDir:   recordDir,
	}
}

func (r *Recorder) GetStatus() RecordingStatus {
	if !r.recording.Load() {
		return RecordingStatus{Active: false}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	duration := ""
	if !r.startTime.IsZero() {
		duration = time.Since(r.startTime).Truncate(time.Second).String()
	}
	return RecordingStatus{
		Active:   true,
		Duration: duration,
		SizeMB:   int64(r.fileSize.Load() / (1024 * 1024)),
	}
}

func (r *Recorder) Start() error {
	if !r.recording.CompareAndSwap(false, true) {
		return fmt.Errorf("already recording")
	}

	stamp := time.Now().Format("2006-01-02_15-04-05")
	tsPath := filepath.Join(r.recordDir, fmt.Sprintf("rec_%s.ts", stamp))
	mp4Path := filepath.Join(r.recordDir, fmt.Sprintf("rec_%s.mp4", stamp))

	f, err := os.Create(tsPath)
	if err != nil {
		r.recording.Store(false)
		return fmt.Errorf("create file: %w", err)
	}

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	// Subscribe synchronously before returning so immediate Broadcast() calls never miss packets
	dataCh := r.broadcaster.Subscribe(recorderSubID)

	r.mu.Lock()
	r.tsPath = tsPath
	r.mp4Path = mp4Path
	r.file = f
	r.startTime = time.Now()
	r.fileSize.Store(0)
	r.stopCh = stopCh
	r.doneCh = doneCh
	r.mu.Unlock()

	go r.recordLoop(f, dataCh, stopCh, doneCh)

	log.Printf("[recorder] Started recording to %s", tsPath)
	return nil
}

func (r *Recorder) Stop() error {
	if !r.recording.CompareAndSwap(true, false) {
		return fmt.Errorf("not recording")
	}

	r.mu.RLock()
	stopCh := r.stopCh
	doneCh := r.doneCh
	r.mu.RUnlock()

	if stopCh != nil {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	}

	if doneCh != nil {
		select {
		case <-doneCh:
		case <-time.After(1 * time.Second):
		}
	}

	r.mu.Lock()
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	tsPath := r.tsPath
	mp4Path := r.mp4Path
	r.mu.Unlock()

	if info, err := os.Stat(tsPath); err == nil && info.Size() >= tsPacketSize {
		go func() {
			log.Printf("[recorder] Remuxing %s -> %s", tsPath, mp4Path)
			cmd := exec.Command("ffmpeg",
				"-hide_banner", "-loglevel", "error",
				"-i", tsPath,
				"-c", "copy",
				"-movflags", "+faststart",
				mp4Path,
			)
			if err := cmd.Run(); err != nil {
				log.Printf("[recorder] Remux error: %v (keeping .ts)", err)
				return
			}
			_ = os.Remove(tsPath)
			log.Printf("[recorder] Remux complete: %s", mp4Path)
		}()
	}

	log.Println("[recorder] Stopped recording")
	return nil
}

func (r *Recorder) recordLoop(f *os.File, dataCh chan []byte, stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[recorder] PANIC RECOVERED in recordLoop: %v", rec)
		}
	}()
	defer r.broadcaster.Unsubscribe(recorderSubID)

	bufWriter := bufio.NewWriterSize(f, 256*1024)
	defer bufWriter.Flush()

	const (
		maxSizeMB    = 10240
		maxSizeBytes = maxSizeMB * 1024 * 1024
		maxDuration  = 12 * time.Hour
	)

	r.mu.RLock()
	startTime := r.startTime
	r.mu.RUnlock()

	for {
		select {
		case data, ok := <-dataCh:
			if !ok {
				return
			}
			if r.fileSize.Load() > maxSizeBytes {
				log.Println("[recorder] Maximum file size limit reached (10GB), stopping recording...")
				go func() { _ = r.Stop() }()
				return
			}
			if !startTime.IsZero() && time.Since(startTime) > maxDuration {
				log.Println("[recorder] Maximum duration limit reached (12h), stopping recording...")
				go func() { _ = r.Stop() }()
				return
			}

			n, err := bufWriter.Write(data)
			if err != nil {
				log.Printf("[recorder] Write error: %v", err)
				return
			}
			// Coalesce queued bursts in RAM; flush once burst drains so disk stays up-to-date
			if len(dataCh) == 0 {
				_ = bufWriter.Flush()
			}
			r.fileSize.Add(uint64(n))

		case <-stopCh:
			return
		}
	}
}

func (r *Recorder) ListRecordings() []RecordingEntry {
	entries, err := os.ReadDir(r.recordDir)
	if err != nil {
		return nil
	}
	var recordings []RecordingEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) == ".ts" {
			mp4Name := name[:len(name)-3] + ".mp4"
			if _, err := os.Stat(filepath.Join(r.recordDir, mp4Name)); err == nil {
				continue
			}
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		recordings = append(recordings, RecordingEntry{
			Name:    name,
			SizeMB:  int64(info.Size() / (1024 * 1024)),
			Path:    filepath.Join(r.recordDir, name),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
	}
	return recordings
}

func (r *Recorder) ToJSON() ([]byte, error) {
	return json.Marshal(r.GetStatus())
}
