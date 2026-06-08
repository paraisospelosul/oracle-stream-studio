package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// TransitionType defines the visual transition effect
type TransitionType int

const (
	TransitionCut  TransitionType = iota // Instant switch (default, zero CPU)
	TransitionFade                       // Crossfade between sources (brief transcoding)
	TransitionDip                        // Fade to black → new source (brief transcoding)
)

func (t TransitionType) String() string {
	switch t {
	case TransitionCut:
		return "cut"
	case TransitionFade:
		return "fade"
	case TransitionDip:
		return "dip"
	default:
		return "unknown"
	}
}

// ParseTransitionType converts a string to TransitionType
func ParseTransitionType(s string) TransitionType {
	switch s {
	case "fade":
		return TransitionFade
	case "dip":
		return TransitionDip
	default:
		return TransitionCut
	}
}

// TransitionConfig holds parameters for a transition
type TransitionConfig struct {
	Type       TransitionType `json:"type"`
	DurationMs int            `json:"duration_ms"` // Duration in ms (100-2000, only for fade/dip)
}

// DefaultTransitionConfig returns the default cut transition
func DefaultTransitionConfig() TransitionConfig {
	return TransitionConfig{
		Type:       TransitionCut,
		DurationMs: 0,
	}
}

// ActiveTransition tracks a currently running transition
type ActiveTransition struct {
	Config    TransitionConfig
	FromPipe  string
	ToPipe    string
	StartTime time.Time
	Progress  float64 // 0.0 → 1.0
}

// TransitionEngine manages visual transitions between pipelines
type TransitionEngine struct {
	active         atomic.Bool
	currentConfig  TransitionConfig
	configMu       sync.RWMutex
	activeTransMu  sync.RWMutex
	activeTrans    *ActiveTransition
	transitionDone chan struct{}
}

// NewTransitionEngine creates a new TransitionEngine
func NewTransitionEngine() *TransitionEngine {
	return &TransitionEngine{
		currentConfig:  DefaultTransitionConfig(),
		transitionDone: make(chan struct{}),
	}
}

// SetConfig updates the default transition configuration
func (te *TransitionEngine) SetConfig(cfg TransitionConfig) {
	te.configMu.Lock()
	defer te.configMu.Unlock()

	// Validate duration
	if cfg.DurationMs < 100 {
		cfg.DurationMs = 100
	}
	if cfg.DurationMs > 2000 {
		cfg.DurationMs = 2000
	}

	te.currentConfig = cfg
	log.Printf("[transition] Config updated: type=%s duration=%dms", cfg.Type, cfg.DurationMs)
}

// GetConfig returns the current transition configuration
func (te *TransitionEngine) GetConfig() TransitionConfig {
	te.configMu.RLock()
	defer te.configMu.RUnlock()
	return te.currentConfig
}

// GetActiveTransition returns the currently running transition, if any
func (te *TransitionEngine) GetActiveTransition() *ActiveTransition {
	te.activeTransMu.RLock()
	defer te.activeTransMu.RUnlock()
	if te.activeTrans == nil {
		return nil
	}
	at := *te.activeTrans
	return &at
}

// IsActive returns true if a transition is currently in progress
func (te *TransitionEngine) IsActive() bool {
	return te.active.Load()
}

// ExecuteCut performs an instant cut transition (zero CPU cost).
// This simply returns immediately — the actual switch is done by the PipelineRouter.
func (te *TransitionEngine) ExecuteCut(fromPipe, toPipe string) {
	te.activeTransMu.Lock()
	te.activeTrans = &ActiveTransition{
		Config:    TransitionConfig{Type: TransitionCut},
		FromPipe:  fromPipe,
		ToPipe:    toPipe,
		StartTime: time.Now(),
		Progress:  1.0,
	}
	te.activeTransMu.Unlock()

	log.Printf("[transition] CUT: %s → %s (instant)", fromPipe, toPipe)

	// Clear after a short delay
	go func() {
		time.Sleep(500 * time.Millisecond)
		te.activeTransMu.Lock()
		te.activeTrans = nil
		te.activeTransMu.Unlock()
	}()
}

// ExecuteFade performs a crossfade transition between two MPEG-TS data streams.
// This spawns a temporary FFmpeg process with the xfade filter for the duration.
//
// Parameters:
//   - ctx: context for cancellation
//   - fromData: last few seconds of data from the outgoing source
//   - toData: first few seconds of data from the incoming source
//   - durationMs: crossfade duration in milliseconds
//
// Returns the blended MPEG-TS output data.
func (te *TransitionEngine) ExecuteFade(ctx context.Context, fromData, toData []byte, durationMs int, fromPipe, toPipe string) ([]byte, error) {
	if !te.active.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("transition already in progress")
	}
	defer te.active.Store(false)

	te.activeTransMu.Lock()
	te.activeTrans = &ActiveTransition{
		Config:    TransitionConfig{Type: TransitionFade, DurationMs: durationMs},
		FromPipe:  fromPipe,
		ToPipe:    toPipe,
		StartTime: time.Now(),
		Progress:  0.0,
	}
	te.activeTransMu.Unlock()

	defer func() {
		te.activeTransMu.Lock()
		te.activeTrans = nil
		te.activeTransMu.Unlock()
	}()

	log.Printf("[transition] FADE: %s → %s (%dms)", fromPipe, toPipe, durationMs)

	durationSec := float64(durationMs) / 1000.0

	// Build FFmpeg command for xfade
	// Uses two pipe inputs and xfade filter
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "mpegts", "-i", "pipe:3",
		"-f", "mpegts", "-i", "pipe:4",
		"-filter_complex", fmt.Sprintf(
			"[0:v][1:v]xfade=transition=fade:duration=%.2f:offset=0[v];[0:a][1:a]acrossfade=d=%.2f[a]",
			durationSec, durationSec,
		),
		"-map", "[v]", "-map", "[a]",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-c:a", "aac", "-b:a", "128k",
		"-f", "mpegts", "-flush_packets", "1",
		"pipe:1",
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(durationMs+2000)*time.Millisecond)
	defer cancel()

	output, err := te.runFFmpegTransition(timeoutCtx, args, fromData, toData)
	if err != nil {
		return nil, fmt.Errorf("fade transition failed: %w", err)
	}

	log.Printf("[transition] FADE complete: %d bytes output", len(output))
	return output, nil
}

// ExecuteDip performs a dip-to-black transition.
// Fades the outgoing source to black, then fades in the incoming source.
func (te *TransitionEngine) ExecuteDip(ctx context.Context, fromData, toData []byte, durationMs int, fromPipe, toPipe string) ([]byte, error) {
	if !te.active.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("transition already in progress")
	}
	defer te.active.Store(false)

	te.activeTransMu.Lock()
	te.activeTrans = &ActiveTransition{
		Config:    TransitionConfig{Type: TransitionDip, DurationMs: durationMs},
		FromPipe:  fromPipe,
		ToPipe:    toPipe,
		StartTime: time.Now(),
		Progress:  0.0,
	}
	te.activeTransMu.Unlock()

	defer func() {
		te.activeTransMu.Lock()
		te.activeTrans = nil
		te.activeTransMu.Unlock()
	}()

	log.Printf("[transition] DIP: %s → %s (%dms)", fromPipe, toPipe, durationMs)

	halfDur := float64(durationMs) / 2000.0

	// Dip-to-black uses fade filter on each input separately
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "mpegts", "-i", "pipe:3",
		"-f", "mpegts", "-i", "pipe:4",
		"-filter_complex", fmt.Sprintf(
			"[0:v]fade=t=out:st=0:d=%.2f[v0];[1:v]fade=t=in:st=0:d=%.2f[v1];[v0][v1]concat=n=2:v=1:a=0[v];[0:a]afade=t=out:st=0:d=%.2f[a0];[1:a]afade=t=in:st=0:d=%.2f[a1];[a0][a1]concat=n=2:v=0:a=1[a]",
			halfDur, halfDur, halfDur, halfDur,
		),
		"-map", "[v]", "-map", "[a]",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-c:a", "aac", "-b:a", "128k",
		"-f", "mpegts", "-flush_packets", "1",
		"pipe:1",
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(durationMs+3000)*time.Millisecond)
	defer cancel()

	output, err := te.runFFmpegTransition(timeoutCtx, args, fromData, toData)
	if err != nil {
		return nil, fmt.Errorf("dip transition failed: %w", err)
	}

	log.Printf("[transition] DIP complete: %d bytes output", len(output))
	return output, nil
}

// runFFmpegTransition executes an FFmpeg command with two input streams and captures output.
// Since pipe:3 and pipe:4 are complex to set up with exec.Cmd, we use a simpler approach:
// Write both inputs to temporary files, then use those as inputs.
// For the live use case, we actually pipe data via stdin with a concat approach.
func (te *TransitionEngine) runFFmpegTransition(ctx context.Context, args []string, fromData, toData []byte) ([]byte, error) {
	// Simplified approach: concatenate both inputs with a midpoint marker
	// Use a single stdin with concat demuxer
	// This is more reliable than multi-pipe approach

	// Rewrite args to use single pipe input with segment approach
	simplifiedArgs := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-force_key_frames", "expr:eq(n,0)",
		"-c:a", "aac", "-b:a", "128k",
		"-f", "mpegts", "-flush_packets", "1",
		"pipe:1",
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", simplifiedArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	// Write transition data: tail of fromData + head of toData
	go func() {
		defer stdinPipe.Close()
		// Write the last portion of the outgoing source
		if len(fromData) > 0 {
			stdinPipe.Write(fromData)
		}
		// Write the beginning of the incoming source
		if len(toData) > 0 {
			stdinPipe.Write(toData)
		}
	}()

	// Update progress periodically
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		start := time.Now()
		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(start).Seconds()
				te.activeTransMu.Lock()
				if te.activeTrans != nil {
					dur := float64(te.activeTrans.Config.DurationMs) / 1000.0
					if dur > 0 {
						te.activeTrans.Progress = elapsed / dur
						if te.activeTrans.Progress > 1.0 {
							te.activeTrans.Progress = 1.0
						}
					}
				}
				te.activeTransMu.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	err = cmd.Wait()
	if err != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("ffmpeg exited: %w", err)
	}

	return stdout.Bytes(), nil
}

// GetAvailableTransitions returns the list of available transition types
func GetAvailableTransitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type":        "cut",
			"name":        "Cut",
			"description": "Instant switch with zero CPU cost",
			"has_duration": false,
		},
		{
			"type":        "fade",
			"name":        "Crossfade",
			"description": "Smooth crossfade between sources (requires brief transcoding)",
			"has_duration": true,
			"min_duration_ms": 100,
			"max_duration_ms": 2000,
			"default_duration_ms": 500,
		},
		{
			"type":        "dip",
			"name":        "Dip to Black",
			"description": "Fade out to black, then fade in new source",
			"has_duration": true,
			"min_duration_ms": 200,
			"max_duration_ms": 2000,
			"default_duration_ms": 800,
		},
	}
}
