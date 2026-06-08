package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// SwitcherState represents the current state of the switcher
type SwitcherState int

const (
	StateLive      SwitcherState = iota
	StateFallback
)

func (s SwitcherState) String() string {
	switch s {
	case StateLive:
		return "live"
	case StateFallback:
		return "backup"
	default:
		return "unknown"
	}
}

// SwitcherConfig holds dynamic configuration
type SwitcherConfig struct {
	SRTAddr                  string `json:"srt_addr"`
	SRTMode                  string `json:"srt_mode"`
	SRTTimeout               int    `json:"srt_timeout"`
	StatsURL                 string `json:"stats_url"`
	FallbackPath             string `json:"fallback_path"`
	MinBitrateKbps           int    `json:"min_bitrate_kbps"`
	BitrateHysteresisSeconds int    `json:"bitrate_hysteresis_seconds"`
}

// SwitchEvent records a state transition
type SwitchEvent struct {
	Time   string `json:"time"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// SwitcherStats holds real-time statistics
type SwitcherStats struct {
	State             string               `json:"state"`
	SRTConnected      bool                 `json:"srt_connected"`
	InputBitrateKbps  float64              `json:"input_bitrate_kbps"`
	PacketsReceived   uint64               `json:"packets_received"`
	PacketsForwarded  uint64               `json:"packets_forwarded"`
	BytesReceived     uint64               `json:"bytes_received"`
	KeyframesDetected uint64               `json:"keyframes_detected"`
	SwitchCount       uint64               `json:"switch_count"`
	LastSwitchTime    string               `json:"last_switch_time"`
	Uptime            string               `json:"uptime"`
	VideoPID          uint16               `json:"video_pid"`
	AudioPID          uint16               `json:"audio_pid"`
	BitrateHistory    map[string][]float64 `json:"bitrate_history"`
	RecentEvents      []SwitchEvent        `json:"recent_events"`
	Pipelines         []PipelineStats      `json:"pipelines"`
	BridgeAvailable   bool                 `json:"bridge_available"`
	PTSRemapActive    bool                 `json:"pts_remap_active"`
}

// Broadcaster distributes MPEGTS packets to multiple subscribers
type Broadcaster struct {
	subscribers map[string]chan []byte
	mu          sync.RWMutex
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subscribers: make(map[string]chan []byte)}
}

func (b *Broadcaster) Subscribe(id string) chan []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subscribers[id]; ok {
		close(ch)
		delete(b.subscribers, id)
	}
	ch := make(chan []byte, 5000)
	b.subscribers[id] = ch
	return ch
}

func (b *Broadcaster) Unsubscribe(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subscribers[id]; ok {
		close(ch)
		delete(b.subscribers, id)
	}
}

func (b *Broadcaster) Broadcast(data []byte) {
	buf := make([]byte, len(data))
	copy(buf, data)
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers {
		select {
		case ch <- buf:
		default:
			// Drop packet if subscriber is slow — prevent blocking
		}
	}
}

func (b *Broadcaster) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// InputProcess manages an FFmpeg input process
type InputProcess struct {
	name    string
	args    []string
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	running atomic.Bool
	mu      sync.Mutex
}

func NewInputProcess(name string, args []string) *InputProcess {
	return &InputProcess{name: name, args: args}
}

func (ip *InputProcess) Start() error {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	ip.cmd = exec.Command("ffmpeg", ip.args...)
	ip.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	var err error
	ip.stdout, err = ip.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	ip.stderr, err = ip.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := ip.cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	ip.running.Store(true)
	// Drain stderr and log it (since loglevel is error, this won't spam)
	go func() {
		scanner := bufio.NewScanner(ip.stderr)
		for scanner.Scan() {
			log.Printf("[%s] FFmpeg: %s", ip.name, scanner.Text())
		}
	}()
	log.Printf("[%s] FFmpeg started (PID %d)", ip.name, ip.cmd.Process.Pid)
	return nil
}

func (ip *InputProcess) Stop() {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	if ip.cmd != nil && ip.cmd.Process != nil {
		ip.cmd.Process.Kill()
		ip.cmd.Wait()
	}
	ip.running.Store(false)
	log.Printf("[%s] FFmpeg stopped", ip.name)
}

func (ip *InputProcess) IsRunning() bool { return ip.running.Load() }

// Switcher is the core engine
type Switcher struct {
	config     SwitcherConfig
	configMu   sync.RWMutex
	configPath string
	srtEnabled atomic.Bool

	state   SwitcherState
	stateMu sync.RWMutex
	startTime time.Time

	// MPEGTS PIDs
	videoPID uint16
	audioPID uint16
	pmtPID   uint16

	// Stats
	packetsReceived   atomic.Uint64
	packetsForwarded  atomic.Uint64
	bytesReceived     atomic.Uint64
	keyframesDetected atomic.Uint64
	switchCount       atomic.Uint64
	lastSwitchTime    time.Time
	lastSwitchTimeMu  sync.RWMutex

	// Bitrate
	bitrateBytes       atomic.Uint64
	currentBitrateKbps atomic.Uint64

	// Bitrate threshold
	lowBitrateStart time.Time
	inLowBitrate    bool

	// Bitrate history ring buffer (36000 = 10 hours at 1/sec)
	bitrateHistory    [36000]float64
	bitrateHistoryIdx int
	bitrateHistoryLen int
	bitrateHistoryMu  sync.RWMutex

	// Switch event history (last 100)
	switchEvents   []SwitchEvent
	switchEventsMu sync.RWMutex

	// Restart channels (quick action buttons)
	restartSRTCh      chan struct{}
	restartFallbackCh chan struct{}

	// Output distribution
	broadcaster *Broadcaster
	dataDir     string

	// Advanced switching (Stages 1-4)
	router     *PipelineRouter
	bridge     *TranscodingBridge
	remapper   *PTSRemapper
	transition *TransitionEngine
}

func NewSwitcher(srtAddr, srtMode, fallbackPath string, timeoutMs int, statsURL string, dataDir string, configPath string) *Switcher {
	sw := &Switcher{
		config: SwitcherConfig{
			SRTAddr:                  srtAddr,
			SRTMode:                  srtMode,
			FallbackPath:             fallbackPath,
			SRTTimeout:               timeoutMs,
			StatsURL:                 statsURL,
			MinBitrateKbps:           0,
			BitrateHysteresisSeconds: 5,
		},
		configPath:        configPath,
		dataDir:           dataDir,
		state:             StateFallback,
		broadcaster:       NewBroadcaster(),
		switchEvents:      make([]SwitchEvent, 0, 100),
		restartSRTCh:      make(chan struct{}, 1),
		restartFallbackCh: make(chan struct{}, 1),
		router:            NewPipelineRouter(3),
		bridge:            NewTranscodingBridge(),
		remapper:          NewPTSRemapper(),
		transition:        NewTransitionEngine(),
	}
	sw.srtEnabled.Store(true)
	sw.loadConfig()
	return sw
}

// RestartSRT signals the SRT manager to kill and reconnect
func (s *Switcher) RestartSRT() {
	select {
	case s.restartSRTCh <- struct{}{}:
		log.Println("[switcher] SRT restart requested")
	default:
	}
}

// RestartFallback signals the fallback manager to restart
func (s *Switcher) RestartFallback() {
	select {
	case s.restartFallbackCh <- struct{}{}:
		log.Println("[switcher] Fallback restart requested")
	default:
	}
}

func (s *Switcher) GetConfig() SwitcherConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

func (s *Switcher) UpdateConfig(cfg SwitcherConfig) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = cfg
	s.saveConfig()
}

func (s *Switcher) UpdateFallbackPath(path string) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config.FallbackPath = path
	s.saveConfig()
	log.Printf("[switcher] Fallback path updated to: %s", path)
}

func (s *Switcher) saveConfig() {
	data, _ := json.MarshalIndent(s.config, "", "  ")
	os.WriteFile(s.configPath, data, 0644)
}

func (s *Switcher) loadConfig() {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return
	}
	var cfg SwitcherConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}
	s.config = cfg
	log.Printf("[switcher] Loaded config from %s", s.configPath)
}

func (s *Switcher) GetState() SwitcherState {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.state
}

func (s *Switcher) setStateWithReason(state SwitcherState, reason string) {
	s.stateMu.Lock()
	oldState := s.state
	if oldState == state {
		s.stateMu.Unlock()
		return
	}
	s.state = state
	s.stateMu.Unlock()

	log.Printf("[switcher] %s -> %s (%s)", oldState, state, reason)
	s.switchCount.Add(1)
	s.lastSwitchTimeMu.Lock()
	s.lastSwitchTime = time.Now()
	s.lastSwitchTimeMu.Unlock()

	event := SwitchEvent{
		Time:   time.Now().Format(time.RFC3339),
		From:   oldState.String(),
		To:     state.String(),
		Reason: reason,
	}
	s.switchEventsMu.Lock()
	s.switchEvents = append(s.switchEvents, event)
	if len(s.switchEvents) > 100 {
		s.switchEvents = s.switchEvents[len(s.switchEvents)-100:]
	}
	s.switchEventsMu.Unlock()
}

func (s *Switcher) GetSwitchHistory() []SwitchEvent {
	s.switchEventsMu.RLock()
	defer s.switchEventsMu.RUnlock()
	events := make([]SwitchEvent, len(s.switchEvents))
	copy(events, s.switchEvents)
	return events
}

func (s *Switcher) getBitrateHistory() map[string][]float64 {
	return map[string][]float64{
		"1m":  s.getBitrateHistoryForWindow(60),
		"5m":  s.getBitrateHistoryForWindow(300),
		"30m": s.getBitrateHistoryForWindow(1800),
		"1h":  s.getBitrateHistoryForWindow(3600),
		"5h":  s.getBitrateHistoryForWindow(18000),
		"10h": s.getBitrateHistoryForWindow(36000),
	}
}

func (s *Switcher) getBitrateHistoryForWindow(seconds int) []float64 {
	s.bitrateHistoryMu.RLock()
	defer s.bitrateHistoryMu.RUnlock()
	if s.bitrateHistoryLen == 0 {
		return []float64{}
	}

	lookback := seconds
	if lookback > s.bitrateHistoryLen {
		lookback = s.bitrateHistoryLen
	}

	targetPoints := 300
	if lookback < targetPoints {
		targetPoints = lookback
	}

	result := make([]float64, targetPoints)
	start := (s.bitrateHistoryIdx - lookback + 36000) % 36000
	pointsPerBucket := float64(lookback) / float64(targetPoints)

	for i := 0; i < targetPoints; i++ {
		bucketStart := int(float64(i) * pointsPerBucket)
		bucketEnd := int(float64(i+1) * pointsPerBucket)
		if bucketEnd > lookback {
			bucketEnd = lookback
		}

		var sum float64
		for j := bucketStart; j < bucketEnd; j++ {
			sum += s.bitrateHistory[(start+j)%36000]
		}
		count := bucketEnd - bucketStart
		if count > 0 {
			result[i] = sum / float64(count)
		}
	}
	return result
}

func (s *Switcher) GetStats() SwitcherStats {
	s.lastSwitchTimeMu.RLock()
	lastSwitch := s.lastSwitchTime
	s.lastSwitchTimeMu.RUnlock()

	lastSwitchStr := ""
	if !lastSwitch.IsZero() {
		lastSwitchStr = lastSwitch.Format(time.RFC3339)
	}
	uptimeStr := ""
	if !s.startTime.IsZero() {
		uptimeStr = time.Since(s.startTime).Truncate(time.Second).String()
	}

	s.switchEventsMu.RLock()
	recentCount := len(s.switchEvents)
	if recentCount > 10 {
		recentCount = 10
	}
	recentEvents := make([]SwitchEvent, recentCount)
	if recentCount > 0 {
		copy(recentEvents, s.switchEvents[len(s.switchEvents)-recentCount:])
	}
	s.switchEventsMu.RUnlock()

	return SwitcherStats{
		State:             s.GetState().String(),
		SRTConnected:      s.GetState() == StateLive,
		InputBitrateKbps:  float64(s.currentBitrateKbps.Load()),
		PacketsReceived:   s.packetsReceived.Load(),
		PacketsForwarded:  s.packetsForwarded.Load(),
		BytesReceived:     s.bytesReceived.Load(),
		KeyframesDetected: s.keyframesDetected.Load(),
		SwitchCount:       s.switchCount.Load(),
		LastSwitchTime:    lastSwitchStr,
		Uptime:            uptimeStr,
		VideoPID:          s.videoPID,
		AudioPID:          s.audioPID,
		BitrateHistory:    s.getBitrateHistory(),
		RecentEvents:      recentEvents,
		Pipelines:         s.router.GetPipelineStats(),
		BridgeAvailable:   s.bridge.IsAvailable(),
		PTSRemapActive:    s.remapper.IsActive(),
	}
}

// SwitchScene performs a scene switch using the specified transition.
// This is the high-level API called by the server and auto-switch engine.
func (s *Switcher) SwitchScene(sceneID, transType string, transDurMs int) {
	log.Printf("[switcher] SwitchScene: target=%s transition=%s duration=%dms", sceneID, transType, transDurMs)
	// For now, scene switching updates the fallback path and restarts
	// The PipelineRouter handles instant switching once scenes have their own pipelines
	s.transition.ExecuteCut(s.router.GetActive().ID, sceneID)
}

func (s *Switcher) buildSRTArgs() []string {
	s.configMu.RLock()
	srtAddr := s.config.SRTAddr
	srtMode := s.config.SRTMode
	s.configMu.RUnlock()

	separator := "?"
	if strings.Contains(srtAddr, "?") {
		separator = "&"
	}
	srtURL := fmt.Sprintf("srt://%s%smode=%s&latency=500000&timeout=5000000", srtAddr, separator, srtMode)
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-i", srtURL,
		"-c", "copy",
		"-f", "mpegts",
		"-flush_packets", "1",
		"pipe:1",
	}
}

func (s *Switcher) buildFallbackArgs() []string {
	s.configMu.RLock()
	fallbackPath := s.config.FallbackPath
	s.configMu.RUnlock()

	ext := strings.ToLower(filepath.Ext(fallbackPath))
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
		return []string{
			"-hide_banner", "-loglevel", "error",
			"-loop", "1", "-re", "-framerate", "30",
			"-i", fallbackPath,
			"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
			"-c:v", "libx265", "-preset", "ultrafast",
			"-x265-params", "keyint=60:min-keyint=60:pools=1:frame-threads=1:numa-pools=1",
			"-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "128k",
			"-f", "mpegts", "-flush_packets", "1",
			"pipe:1",
		}
	}
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-re", "-stream_loop", "-1",
		"-i", fallbackPath,
		"-c", "copy",
		"-f", "mpegts", "-flush_packets", "1",
		"pipe:1",
	}
}

func (s *Switcher) statsPoller(ctx context.Context, forceFallbackCh chan struct{}) {
	s.configMu.RLock()
	statsURL := s.config.StatsURL
	s.configMu.RUnlock()
	if statsURL == "" {
		return
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.configMu.RLock()
			url := s.config.StatsURL
			s.configMu.RUnlock()
			if url == "" {
				continue
			}
			resp, err := client.Get(url)
			if err != nil {
				continue
			}
			var stats struct {
				Publishers map[string]interface{} `json:"publishers"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&stats); err == nil {
				if len(stats.Publishers) == 0 {
					s.srtEnabled.Store(false)
					select {
					case forceFallbackCh <- struct{}{}:
					default:
					}
				} else {
					s.srtEnabled.Store(true)
				}
			}
			resp.Body.Close()
		}
	}
}


func (s *Switcher) checkBitrateThreshold() bool {
	s.configMu.RLock()
	minBitrate := s.config.MinBitrateKbps
	hysteresis := s.config.BitrateHysteresisSeconds
	s.configMu.RUnlock()
	if minBitrate <= 0 {
		return false
	}
	currentBitrate := s.currentBitrateKbps.Load()
	if currentBitrate > 0 && int(currentBitrate) < minBitrate {
		if !s.inLowBitrate {
			s.inLowBitrate = true
			s.lowBitrateStart = time.Now()
		} else if time.Since(s.lowBitrateStart) >= time.Duration(hysteresis)*time.Second {
			s.inLowBitrate = false
			return true
		}
	} else {
		s.inLowBitrate = false
	}
	return false
}

func (s *Switcher) Run(ctx context.Context) error {
	s.startTime = time.Now()
	log.Println("[switcher] Starting with advanced pipeline routing...")

	go s.bitrateLoop(ctx)

	// Create data channels for pipelines
	fallbackDataCh := make(chan []byte, 5000)
	srtDataCh := make(chan []byte, 5000)
	bridgeDataCh := make(chan []byte, 5000)
	srtDiedCh := make(chan struct{}, 1)
	forceFallbackCh := make(chan struct{}, 1)

	// Register pipelines with the router
	s.router.AddPipeline("srt", "srt", srtDataCh)
	s.router.AddPipeline("fallback", "fallback", fallbackDataCh)
	s.router.AddPipeline("bridge", "bridge", bridgeDataCh)

	// Set fallback as initial active pipeline
	s.router.SwitchTo("fallback")

	// Start the drain goroutine for inactive pipelines
	go s.router.DrainInactive(ctx)

	// Start input managers
	go s.fallbackManagerLoop(ctx, fallbackDataCh)
	go s.statsPoller(ctx, forceFallbackCh)
	go s.srtManagerLoop(ctx, srtDataCh, srtDiedCh)

	// Configure PTS remapper with PIDs once detected
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.videoPID != 0 {
					s.remapper.SetPIDs(s.videoPID, s.audioPID, s.pmtPID)
				}
			}
		}
	}()

	s.setStateWithReason(StateFallback, "startup")

	s.configMu.RLock()
	timeoutDuration := time.Duration(s.config.SRTTimeout) * time.Millisecond
	s.configMu.RUnlock()

	timeout := time.NewTimer(timeoutDuration)
	defer timeout.Stop()

	bitrateCheckTicker := time.NewTicker(1 * time.Second)
	defer bitrateCheckTicker.Stop()

	for {
		currentState := s.GetState()
		switch currentState {
		case StateLive:
			select {
			case data, ok := <-srtDataCh:
				if !ok {
					return nil
				}
				
				// Update pipeline stats
				if srtPipe := s.router.GetPipeline("srt"); srtPipe != nil {
					srtPipe.mu.Lock()
					srtPipe.Ready = true
					srtPipe.LastData = time.Now()
					srtPipe.mu.Unlock()
				}
				
				if !s.bridge.IsAvailable() {
					// Feed incoming raw data to bridge
					s.bridge.Write(data)
					
					// Watch for natural keyframe in raw stream to transition back to direct passthrough
					if s.containsKeyframe(data) {
						log.Println("[switcher] Natural keyframe arrived during live transcoding, stopping bridge")
						s.bridge.Stop()
						s.router.SwitchTo("srt")
					}
				} else {
					// Direct passthrough path
					s.processPackets(data)
					if s.containsVideo(data) {
						s.configMu.RLock()
						td := time.Duration(s.config.SRTTimeout) * time.Millisecond
						s.configMu.RUnlock()
						if !timeout.Stop() {
							select {
							case <-timeout.C:
							default:
							}
						}
						timeout.Reset(td)
					}
					// Correct timestamps and broadcast
					remapped := s.remapper.Remap(data)
					s.remapper.TrackOutgoing(remapped)
					s.broadcaster.Broadcast(remapped)
					s.packetsForwarded.Add(1)
				}
				
			case bridgeData := <-bridgeDataCh:
				// Broadcast transcoded packets
				s.processPackets(bridgeData)
				if s.containsVideo(bridgeData) {
					s.configMu.RLock()
					td := time.Duration(s.config.SRTTimeout) * time.Millisecond
					s.configMu.RUnlock()
					if !timeout.Stop() {
						select {
						case <-timeout.C:
						default:
						}
					}
					timeout.Reset(td)
				}
				// Correct timestamps and broadcast
				remapped := s.remapper.Remap(bridgeData)
				s.remapper.TrackOutgoing(remapped)
				s.broadcaster.Broadcast(remapped)
				s.packetsForwarded.Add(1)

			case _, ok := <-fallbackDataCh:
				if ok {
					// Update pipeline stats
					if fbPipe := s.router.GetPipeline("fallback"); fbPipe != nil {
						fbPipe.mu.Lock()
						fbPipe.Ready = true
						fbPipe.LastData = time.Now()
						fbPipe.mu.Unlock()
					}
				}
			case <-timeout.C:
				log.Println("[switcher] SRT timeout in StateLive, returning to fallback")
				s.bridge.Stop()
				s.setStateWithReason(StateFallback, "SRT timeout")
				s.router.SwitchTo("fallback")
				s.remapper.Reset()
				s.RestartSRT()
			case <-bitrateCheckTicker.C:
				if s.checkBitrateThreshold() {
					log.Println("[switcher] Bitrate below threshold in StateLive, returning to fallback")
					s.bridge.Stop()
					s.setStateWithReason(StateFallback, "bitrate below minimum")
					s.router.SwitchTo("fallback")
					s.remapper.Reset()
					s.RestartSRT()
				}
			case <-forceFallbackCh:
				s.bridge.Stop()
				s.setStateWithReason(StateFallback, "no publishers (API)")
				s.router.SwitchTo("fallback")
				s.remapper.Reset()
				s.RestartSRT()
			case <-srtDiedCh:
				s.bridge.Stop()
				s.setStateWithReason(StateFallback, "SRT process died")
				s.router.SwitchTo("fallback")
				s.remapper.Reset()
			case <-ctx.Done():
				s.bridge.Stop()
				return nil
			}

		case StateFallback:
			select {
			case data := <-fallbackDataCh:
				// Update pipeline stats
				if fbPipe := s.router.GetPipeline("fallback"); fbPipe != nil {
					fbPipe.mu.Lock()
					fbPipe.Ready = true
					fbPipe.LastData = time.Now()
					fbPipe.mu.Unlock()
				}
				// Correct timestamps (no-op since offset is reset, but safe) and broadcast
				remapped := s.remapper.Remap(data)
				s.remapper.TrackOutgoing(remapped)
				s.broadcaster.Broadcast(remapped)
				s.packetsForwarded.Add(1)
			case data, ok := <-srtDataCh:
				if ok {
					// Update pipeline stats
					if srtPipe := s.router.GetPipeline("srt"); srtPipe != nil {
						srtPipe.mu.Lock()
						srtPipe.Ready = true
						srtPipe.LastData = time.Now()
						srtPipe.mu.Unlock()
					}
					if s.containsKeyframe(data) {
						// Fast path: keyframe available → instant switch
						s.setStateWithReason(StateLive, "SRT keyframe detected")
						s.router.SwitchTo("srt")
						s.remapper.PrepareSwitch(data)
						s.configMu.RLock()
						td := time.Duration(s.config.SRTTimeout) * time.Millisecond
						s.configMu.RUnlock()
						if !timeout.Stop() {
							select {
							case <-timeout.C:
							default:
							}
						}
						timeout.Reset(td)
						s.processPackets(data)
						
						remapped := s.remapper.Remap(data)
						s.broadcaster.Broadcast(remapped)
						s.packetsForwarded.Add(1)
					} else if s.containsVideo(data) && s.bridge.IsAvailable() {
						// Bridge path: video without keyframe → start transcoding bridge
						log.Printf("[switcher] SRT data without keyframe — starting transcoding bridge")
						
						var pat, pmt []byte
						if srtPipe := s.router.GetPipeline("srt"); srtPipe != nil {
							srtPipe.mu.Lock()
							pat = srtPipe.CachedPAT
							pmt = srtPipe.CachedPMT
							srtPipe.mu.Unlock()
						}
						
						codec := DetectCodec(data)
						if err := s.bridge.Start(ctx, codec, pat, pmt, bridgeDataCh); err != nil {
							log.Printf("[switcher] Bridge start failed: %v (will wait for natural keyframe)", err)
						} else {
							s.bridge.Write(data)
							s.setStateWithReason(StateLive, "bridge start")
							s.router.SwitchTo("bridge")
							s.remapper.PrepareSwitch(data)
							s.configMu.RLock()
							td := time.Duration(s.config.SRTTimeout) * time.Millisecond
							s.configMu.RUnlock()
							if !timeout.Stop() {
								select {
								case <-timeout.C:
								default:
								}
							}
							timeout.Reset(td)
						}
					}
				}
			case <-forceFallbackCh:
				// Stay in fallback
			case <-srtDiedCh:
				// SRT still down
			case <-ctx.Done():
				return nil
			}
		}
	}
}


func (s *Switcher) fallbackManagerLoop(ctx context.Context, dataCh chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		args := s.buildFallbackArgs()
		fallbackProc := NewInputProcess("fallback", args)
		if err := fallbackProc.Start(); err != nil {
			log.Printf("[switcher] Fallback start failed: %v, retrying in 3s", err)
			select {
			case <-time.After(3 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}

		procDiedCh := make(chan struct{}, 1)
		go s.readInputLoop(ctx, fallbackProc, dataCh, procDiedCh, true)

		select {
		case <-procDiedCh:
			fallbackProc.Stop()
			log.Println("[switcher] Fallback died, restarting in 2s...")
		case <-s.restartFallbackCh:
			fallbackProc.Stop()
			log.Println("[switcher] Fallback restart requested")
		case <-ctx.Done():
			fallbackProc.Stop()
			return
		}

		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func (s *Switcher) srtManagerLoop(ctx context.Context, dataCh chan []byte, diedCh chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		for !s.srtEnabled.Load() {
			select {
			case <-time.After(1 * time.Second):
			case <-ctx.Done():
				return
			}
		}

		newProc := NewInputProcess("srt", s.buildSRTArgs())
		if err := newProc.Start(); err != nil {
			log.Printf("[switcher] SRT start failed: %v, retrying in 3s", err)
			select {
			case <-time.After(3 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}

		procDiedCh := make(chan struct{}, 1)
		go s.readInputLoop(ctx, newProc, dataCh, procDiedCh, false)

		select {
		case <-procDiedCh:
		case <-s.restartSRTCh:
		case <-ctx.Done():
			newProc.Stop()
			return
		}

		newProc.Stop()
		select {
		case diedCh <- struct{}{}:
		default:
		}

		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func (s *Switcher) readInputLoop(ctx context.Context, proc *InputProcess, dataCh chan []byte, diedCh chan struct{}, throttle bool) {
	aligner := NewPacketAligner()
	readBuf := make([]byte, 1316*7)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		startTime := time.Now()
		n, err := proc.stdout.Read(readBuf)
		if err != nil {
			if diedCh != nil {
				select {
				case diedCh <- struct{}{}:
				default:
				}
			}
			return
		}
		if throttle {
			elapsed := time.Since(startTime)
			if elapsed < 10*time.Millisecond {
				time.Sleep(10*time.Millisecond - elapsed)
			}
		}
		if n > 0 {
			if !throttle {
				s.packetsReceived.Add(1)
				s.bytesReceived.Add(uint64(n))
				s.bitrateBytes.Add(uint64(n))
			}
			aligner.Feed(readBuf[:n])
			for {
				pkt := aligner.Next()
				if pkt == nil {
					break
				}
				select {
				case dataCh <- pkt:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (s *Switcher) processPackets(data []byte) {
	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}
		if info.PID == patPID && info.PUSI && info.HasPayload {
			if pmtPID := ParsePAT(pkt[info.PayloadOffset:]); pmtPID != 0 {
				s.pmtPID = pmtPID
			}
		}
		if info.PID == s.pmtPID && s.pmtPID != 0 && info.PUSI && info.HasPayload {
			vPID, aPID := ParsePMT(pkt[info.PayloadOffset:])
			if vPID != 0 {
				if s.videoPID != vPID {
					log.Printf("[switcher] Video PID: %d", vPID)
				}
				s.videoPID = vPID
			}
			if aPID != 0 {
				if s.audioPID != aPID {
					log.Printf("[switcher] Audio PID: %d", aPID)
				}
				s.audioPID = aPID
			}
		}
	}
}

func (s *Switcher) containsKeyframe(data []byte) bool {
	// Single-pass: extract PIDs AND detect keyframes in one loop
	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}
		// PAT → extract PMT PID
		if info.PID == patPID && info.PUSI && info.HasPayload {
			if pmtPID := ParsePAT(pkt[info.PayloadOffset:]); pmtPID != 0 {
				s.pmtPID = pmtPID
			}
		}
		// PMT → extract video/audio PIDs
		if info.PID == s.pmtPID && s.pmtPID != 0 && info.PUSI && info.HasPayload {
			vPID, aPID := ParsePMT(pkt[info.PayloadOffset:])
			if vPID != 0 {
				if s.videoPID != vPID {
					log.Printf("[switcher] Video PID: %d", vPID)
				}
				s.videoPID = vPID
			}
			if aPID != 0 {
				if s.audioPID != aPID {
					log.Printf("[switcher] Audio PID: %d", aPID)
				}
				s.audioPID = aPID
			}
		}
		// Keyframe detection on video PID
		if info.PID == s.videoPID && s.videoPID != 0 && info.PUSI && info.HasPayload {
			pesPayload := ExtractPESPayload(pkt, info.PayloadOffset)
			if pesPayload != nil && IsKeyframe(pesPayload) {
				s.keyframesDetected.Add(1)
				return true
			}
		}
	}
	return false
}

func (s *Switcher) bitrateLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			bytes := s.bitrateBytes.Swap(0)
			kbps := (bytes * 8) / 1000
			s.currentBitrateKbps.Store(kbps)
			s.bitrateHistoryMu.Lock()
			s.bitrateHistory[s.bitrateHistoryIdx] = float64(kbps)
			s.bitrateHistoryIdx = (s.bitrateHistoryIdx + 1) % 36000
			if s.bitrateHistoryLen < 36000 {
				s.bitrateHistoryLen++
			}
			s.bitrateHistoryMu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

func (s *Switcher) containsVideo(data []byte) bool {
	if s.videoPID == 0 {
		return true
	}
	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if ok && info.PID == s.videoPID {
			return true
		}
	}
	return false
}

// IsSRTInputActive checks if the SRT stream is currently connected and actively transmitting data.
func (s *Switcher) IsSRTInputActive() bool {
	if s.GetState() == StateLive {
		return true
	}
	p := s.router.GetPipeline("srt")
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Ready && time.Since(p.LastData) < 3*time.Second
}
