package main

import (
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

// Recorder records the broadcaster stream to a .ts file, then remuxes to .mp4 on stop
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
	if r.recording.Load() {
		return fmt.Errorf("already recording")
	}

	stamp := time.Now().Format("2006-01-02_15-04-05")
	tsPath := filepath.Join(r.recordDir, fmt.Sprintf("rec_%s.ts", stamp))
	mp4Path := filepath.Join(r.recordDir, fmt.Sprintf("rec_%s.mp4", stamp))

	f, err := os.Create(tsPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	r.mu.Lock()
	r.tsPath = tsPath
	r.mp4Path = mp4Path
	r.file = f
	r.startTime = time.Now()
	r.fileSize.Store(0)
	r.stopCh = make(chan struct{})
	r.mu.Unlock()

	r.recording.Store(true)
	go r.recordLoop()

	log.Printf("[recorder] Started recording to %s", tsPath)
	return nil
}

func (r *Recorder) Stop() error {
	if !r.recording.Load() {
		return fmt.Errorf("not recording")
	}

	r.recording.Store(false)
	select {
	case <-r.stopCh:
	default:
		close(r.stopCh)
	}

	// Wait for write loop to finish
	time.Sleep(300 * time.Millisecond)

	r.mu.Lock()
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
	tsPath := r.tsPath
	mp4Path := r.mp4Path
	r.mu.Unlock()

	// Remux .ts to .mp4 in background
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
		// Delete .ts after successful remux
		os.Remove(tsPath)
		log.Printf("[recorder] Remux complete: %s", mp4Path)
	}()

	log.Println("[recorder] Stopped recording")
	return nil
}

func (r *Recorder) recordLoop() {
	subID := "recorder_internal"
	dataCh := r.broadcaster.Subscribe(subID)
	defer r.broadcaster.Unsubscribe(subID)

	const (
		maxSizeMB    = 10240            // 10 GB limit (Stage 6)
		maxSizeBytes = maxSizeMB * 1024 * 1024
		maxDuration  = 12 * time.Hour   // 12 hours limit (Stage 6)
	)

	for {
		select {
		case data, ok := <-dataCh:
			if !ok {
				return
			}
			r.mu.RLock()
			f := r.file
			startTime := r.startTime
			r.mu.RUnlock()

			// Check limits (Stage 6)
			if r.fileSize.Load() > maxSizeBytes {
				log.Println("[recorder] Maximum file size limit reached (10GB), stopping recording...")
				go r.Stop()
				return
			}
			if !startTime.IsZero() && time.Since(startTime) > maxDuration {
				log.Println("[recorder] Maximum duration limit reached (12h), stopping recording...")
				go r.Stop()
				return
			}

			if f != nil {
				n, err := f.Write(data)
				if err != nil {
					log.Printf("[recorder] Write error: %v", err)
					return
				}
				r.fileSize.Add(uint64(n))
			}
		case <-r.stopCh:
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
		// Skip temp .ts files if there's a matching .mp4
		name := e.Name()
		if filepath.Ext(name) == ".ts" {
			mp4Name := name[:len(name)-3] + ".mp4"
			if _, err := os.Stat(filepath.Join(r.recordDir, mp4Name)); err == nil {
				continue // mp4 exists, skip ts
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
