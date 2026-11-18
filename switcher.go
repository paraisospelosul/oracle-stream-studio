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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SwitcherState represents the current state of the switcher
type SwitcherState int

const (
	StateLive SwitcherState = iota
	StateFallback
)

func (s SwitcherState) String() string {
	switch s {
	case StateLive:
		return "live"
	case StateFallback:
		return "fallback"
	default:
		return "unknown"
	}
}

// SwitcherConfig holds dynamic configuration persisted to switcher.json
type SwitcherConfig struct {
	SRTAddr                  string `json:"srt_addr"`
	SRTMode                  string `json:"srt_mode"`
	SRTTimeout               int    `json:"srt_timeout"`
	StatsURL                 string `json:"stats_url"`
	FallbackPath             string `json:"fallback_path"`
	FallbackResolution       string `json:"fallback_resolution"`
	WaitForKeyframe          bool   `json:"wait_for_keyframe"`
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

// AudioLevels provides zero-CPU audio stream presence and bitrate telemetry
// derived from MPEG-TS AAC packet headers (no audio decoding / LUFS measurement).
type AudioLevels struct {
	Active      bool   `json:"active"`
	BitrateKbps uint64 `json:"bitrate_kbps"`
	Source      string `json:"source"`        // "live", "fallback", or "none"
	BarLevel    int    `json:"bar_level_pct"` // 0..100 visual bar percentage derived from AAC stream bitrate
}

// SwitcherStats holds real-time statistics for the Web UI
type SwitcherStats struct {
	State              string               `json:"state"`
	SRTConnected       bool                 `json:"srt_connected"`
	SRTStandby         bool                 `json:"srt_standby"`
	ManualFallback     bool                 `json:"manual_fallback"`
	ActiveSceneID      string               `json:"active_scene_id"`
	ActiveSceneName    string               `json:"active_scene_name"`
	FallbackResolution string               `json:"fallback_resolution"`
	WaitForKeyframe    bool                 `json:"wait_for_keyframe"`
	InputBitrateKbps   float64              `json:"input_bitrate_kbps"`
	PacketsReceived    uint64               `json:"packets_received"`
	PacketsForwarded   uint64               `json:"packets_forwarded"`
	BytesReceived      uint64               `json:"bytes_received"`
	KeyframesDetected  uint64               `json:"keyframes_detected"`
	SwitchCount        uint64               `json:"switch_count"`
	LastSwitchTime     string               `json:"last_switch_time"`
	Uptime             string               `json:"uptime"`
	VideoPID           uint16               `json:"video_pid"`
	AudioPID           uint16               `json:"audio_pid"`
	BitrateHistory     map[string][]float64 `json:"bitrate_history"`
	RecentEvents       []SwitchEvent        `json:"recent_events"`
	PTSRemapActive     bool                 `json:"pts_remap_active"`
}

// Broadcaster distributes aligned MPEG-TS batches to active subscribers
type Broadcaster struct {
	subscribers map[string]chan []byte
	subCount    atomic.Int32
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
	ch := make(chan []byte, 512)
	b.subscribers[id] = ch
	b.subCount.Store(int32(len(b.subscribers)))
	return ch
}

func (b *Broadcaster) Unsubscribe(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subscribers[id]; ok {
		close(ch)
		delete(b.subscribers, id)
	}
	b.subCount.Store(int32(len(b.subscribers)))
}

// UnsubscribeChannel removes the subscriber only if its active channel matches target,
// preventing an exiting generation from unsubscribing a newly restarted generation.
func (b *Broadcaster) UnsubscribeChannel(id string, target chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subscribers[id]; ok && ch == target {
		close(ch)
		delete(b.subscribers, id)
	}
	b.subCount.Store(int32(len(b.subscribers)))
}

func (b *Broadcaster) Broadcast(data []byte) {
	if b.subCount.Load() == 0 {
		return
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers {
		select {
		case ch <- buf:
		default:
			// Drop batch if subscriber is slow — prevent blocking live pipeline
		}
	}
}

func (b *Broadcaster) SubscriberCount() int {
	return int(b.subCount.Load())
}

// InputProcess manages the lightweight FFmpeg SRT demux process
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
	setProcessDeathSignal(ip.cmd)
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
		_ = ip.cmd.Process.Kill()
		_ = ip.cmd.Wait()
	}
	ip.running.Store(false)
	log.Printf("[%s] FFmpeg stopped", ip.name)
}

func (ip *InputProcess) IsRunning() bool { return ip.running.Load() }

// Switcher is the ultra-light failover core engine
type Switcher struct {
	config     SwitcherConfig
	configMu   sync.RWMutex
	saveMu     sync.Mutex
	configPath string
	srtEnabled atomic.Bool

	// Manual Pause / BRB mode controlled from Web UI
	manualFallback    atomic.Bool
	lastSRTDataUnixMs atomic.Int64

	state     SwitcherState
	stateMu   sync.RWMutex
	startTime time.Time

	// MPEG-TS PIDs
	videoPID atomic.Uint32
	audioPID atomic.Uint32
	pmtPID   atomic.Uint32

	// Stats
	packetsReceived   atomic.Uint64
	packetsForwarded  atomic.Uint64
	bytesReceived     atomic.Uint64
	keyframesDetected atomic.Uint64
	switchCount       atomic.Uint64
	lastSwitchTime    time.Time
	lastSwitchTimeMu  sync.RWMutex

	// Video + Audio Bitrate (zero-CPU atomic counters)
	bitrateBytes       atomic.Uint64
	currentBitrateKbps atomic.Uint64
	audioBytes         atomic.Uint64
	currentAudioKbps   atomic.Uint64

	// Bitrate threshold hysteresis
	lowBitrateStart time.Time
	inLowBitrate    bool

	// Bitrate history ring buffer (7200 = 2 hours at 1/sec)
	bitrateHistory    [7200]float64
	bitrateHistoryIdx int
	bitrateHistoryLen int
	bitrateHistoryMu  sync.RWMutex

	// Switch event history (last 100)
	switchEvents   []SwitchEvent
	switchEventsMu sync.RWMutex

	// Control channels
	restartSRTCh      chan struct{}
	restartFallbackCh chan struct{}
	modeChangeCh      chan struct{}

	// Output distribution & RAM Slate SceneManager
	broadcaster  *Broadcaster
	sceneManager *SceneManager
	remapper     *PTSRemapper
	dataDir      string
}

func NewSwitcher(srtAddr, srtMode, fallbackPath string, timeoutMs int, statsURL string, dataDir string, configPath string) *Switcher {
	sw := &Switcher{
		config: SwitcherConfig{
			SRTAddr:                  srtAddr,
			SRTMode:                  srtMode,
			FallbackPath:             fallbackPath,
			FallbackResolution:       "1080p",
			WaitForKeyframe:          false,
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
		modeChangeCh:      make(chan struct{}, 1),
		remapper:          NewPTSRemapper(),
	}
	sw.videoPID.Store(defaultVideoPID)
	sw.audioPID.Store(defaultAudioPID)
	sw.pmtPID.Store(defaultPMTPID)
	sw.srtEnabled.Store(true)
	sw.loadConfig()
	if sw.config.FallbackResolution == "" {
		sw.config.FallbackResolution = "1080p"
	}
	return sw
}

// AttachSceneManager links the RAM Slate SceneManager to the Switcher
func (s *Switcher) AttachSceneManager(sm *SceneManager) {
	s.sceneManager = sm
}

// RestartSRT signals the SRT manager to reconnect immediately
func (s *Switcher) RestartSRT() {
	select {
	case s.restartSRTCh <- struct{}{}:
		log.Println("[switcher] SRT restart requested")
	default:
	}
}

// RestartFallback resets the RAM slate cursor to Frame 0 (IDR Keyframe) seamlessly
func (s *Switcher) RestartFallback() {
	select {
	case s.restartFallbackCh <- struct{}{}:
		log.Println("[switcher] RAM Slate reset to keyframe requested")
	default:
	}
}

// SetManualFallback toggles "Modo Pausa / BRB" (forces Fallback Slate without disconnecting SRT)
func (s *Switcher) SetManualFallback(enabled bool) {
	prev := s.manualFallback.Swap(enabled)
	if prev == enabled {
		return
	}
	if enabled {
		log.Println("[switcher] Manual Fallback (BRB Mode) ENABLED by user")
	} else {
		log.Println("[switcher] Manual Fallback (BRB Mode) DISABLED — resuming live when SRT ready")
	}
	select {
	case s.modeChangeCh <- struct{}{}:
	default:
	}
}

func (s *Switcher) IsManualFallback() bool {
	return s.manualFallback.Load()
}

func (s *Switcher) GetConfig() SwitcherConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

func (s *Switcher) UpdateConfig(cfg SwitcherConfig) {
	s.configMu.Lock()
	oldAddr := s.config.SRTAddr
	oldMode := s.config.SRTMode
	if cfg.FallbackResolution == "" {
		cfg.FallbackResolution = s.config.FallbackResolution
	}
	if cfg.FallbackPath == "" {
		cfg.FallbackPath = s.config.FallbackPath
	}
	if cfg.SRTTimeout < 500 {
		cfg.SRTTimeout = 2000
	}
	s.config = cfg
	s.configMu.Unlock()

	s.persistConfig()

	if cfg.SRTAddr != oldAddr || cfg.SRTMode != oldMode {
		s.RestartSRT()
	}
}

func (s *Switcher) UpdateFallbackSettings(res string, waitForKeyframe bool) {
	if res != "720p" {
		res = "1080p"
	}
	s.configMu.Lock()
	resChanged := s.config.FallbackResolution != res
	s.config.FallbackResolution = res
	s.config.WaitForKeyframe = waitForKeyframe
	s.configMu.Unlock()

	s.persistConfig()

	if resChanged && s.sceneManager != nil {
		s.sceneManager.SetResolution(res)
	}
}

func (s *Switcher) UpdateFallbackPath(path string) {
	s.configMu.Lock()
	s.config.FallbackPath = path
	s.configMu.Unlock()

	s.persistConfig()
	log.Printf("[switcher] Fallback path updated to: %s", path)
}

func (s *Switcher) persistConfig() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.configMu.RLock()
	cfg := s.config
	s.configMu.RUnlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("[switcher] Save error: %v", err)
		return
	}
	_ = os.WriteFile(s.configPath, data, 0644)
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
	if cfg.FallbackResolution == "" {
		cfg.FallbackResolution = "1080p"
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

// GetAudioLevels returns zero-CPU audio status calculated from live TS packet headers
func (s *Switcher) GetAudioLevels() AudioLevels {
	state := s.GetState()
	aKbps := s.currentAudioKbps.Load()

	if state == StateFallback {
		return AudioLevels{
			Active:      false,
			BitrateKbps: 128,
			Source:      "fallback",
			BarLevel:    35,
		}
	}

	if aKbps == 0 {
		return AudioLevels{
			Active:      false,
			BitrateKbps: 0,
			Source:      "none",
			BarLevel:    0,
		}
	}

	// Map healthy AAC bitrate (e.g. 64-160 kbps) to a 15..100% visual bar level
	pct := int((aKbps * 100) / 160)
	if pct < 15 {
		pct = 15
	}
	if pct > 100 {
		pct = 100
	}
	return AudioLevels{
		Active:      true,
		BitrateKbps: aKbps,
		Source:      "live",
		BarLevel:    pct,
	}
}

func (s *Switcher) getBitrateHistory() map[string][]float64 {
	return map[string][]float64{
		"1m":  s.getBitrateHistoryForWindow(60),
		"5m":  s.getBitrateHistoryForWindow(300),
		"30m": s.getBitrateHistoryForWindow(1800),
		"1h":  s.getBitrateHistoryForWindow(3600),
		"2h":  s.getBitrateHistoryForWindow(7200),
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
	start := (s.bitrateHistoryIdx - lookback + 7200) % 7200
	pointsPerBucket := float64(lookback) / float64(targetPoints)

	for i := 0; i < targetPoints; i++ {
		bucketStart := int(float64(i) * pointsPerBucket)
		bucketEnd := int(float64(i+1) * pointsPerBucket)
		if bucketEnd > lookback {
			bucketEnd = lookback
		}

		var sum float64
		for j := bucketStart; j < bucketEnd; j++ {
			sum += s.bitrateHistory[(start+j)%7200]
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

	s.configMu.RLock()
	fbRes := s.config.FallbackResolution
	waitKF := s.config.WaitForKeyframe
	s.configMu.RUnlock()

	activeSceneID := ""
	activeSceneName := ""
	if s.sceneManager != nil {
		if sc := s.sceneManager.GetActiveScene(); sc != nil {
			activeSceneID = sc.ID
			activeSceneName = sc.Name
		}
	}

	state := s.GetState()
	lastSRT := s.lastSRTDataUnixMs.Load()
	srtReceiving := lastSRT > 0 && (time.Now().UnixMilli()-lastSRT) < 2500

	return SwitcherStats{
		State:              state.String(),
		SRTConnected:       state == StateLive,
		SRTStandby:         state == StateFallback && srtReceiving,
		ManualFallback:     s.manualFallback.Load(),
		ActiveSceneID:      activeSceneID,
		ActiveSceneName:    activeSceneName,
		FallbackResolution: fbRes,
		WaitForKeyframe:    waitKF,
		InputBitrateKbps:   float64(s.currentBitrateKbps.Load()),
		PacketsReceived:    s.packetsReceived.Load(),
		PacketsForwarded:   s.packetsForwarded.Load(),
		BytesReceived:      s.bytesReceived.Load(),
		KeyframesDetected:  s.keyframesDetected.Load(),
		SwitchCount:        s.switchCount.Load(),
		LastSwitchTime:     lastSwitchStr,
		Uptime:             uptimeStr,
		VideoPID:           uint16(s.videoPID.Load()),
		AudioPID:           uint16(s.audioPID.Load()),
		BitrateHistory:     s.getBitrateHistory(),
		RecentEvents:       recentEvents,
		PTSRemapActive:     s.remapper.IsActive(),
	}
}

// buildSRTArgs normalizes incoming SRT streams to fixed PIDs (PMT=0x1000, Video=256, Audio=257)
// and uncoalesced PES headers (-pes_payload_size 0) for glitch-free switching with RAM Slates.
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
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-c", "copy",
		"-mpegts_pmt_start_pid", "0x1000",
		"-mpegts_start_pid", "0x0100",
		"-pes_payload_size", "0",
		"-f", "mpegts",
		"-flush_packets", "1",
		"pipe:1",
	}
}

func (s *Switcher) statsPoller(ctx context.Context, forceFallbackCh chan struct{}) {
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
				s.srtEnabled.Store(true)
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

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func (s *Switcher) Run(ctx context.Context) error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[switcher] PANIC RECOVERED in Run: %v", r)
		}
	}()
	s.startTime = time.Now()
	log.Println("[switcher] Starting Ultra-Light Engine (RAM Slate + Zero-Decode Passthrough)...")

	go s.bitrateLoop(ctx)

	srtDataCh := make(chan []byte, 512)
	srtDiedCh := make(chan struct{}, 1)
	forceFallbackCh := make(chan struct{}, 1)

	go s.statsPoller(ctx, forceFallbackCh)
	go s.srtManagerLoop(ctx, srtDataCh, srtDiedCh)

	s.setStateWithReason(StateFallback, "startup")

	s.configMu.RLock()
	timeoutDuration := time.Duration(s.config.SRTTimeout) * time.Millisecond
	s.configMu.RUnlock()

	timeout := time.NewTimer(timeoutDuration)
	defer timeout.Stop()

	bitrateCheckTicker := time.NewTicker(1 * time.Second)
	defer bitrateCheckTicker.Stop()

	// 30fps ticker (33.333333ms) — only active while in StateFallback
	var slateTicker *time.Ticker
	var slateTickCh <-chan time.Time
	slateFrameIdx := 0
	lastSceneID := ""
	wasSubscribed := false
	slateWorkBuf := make([]byte, 0, tsPacketSize*64)

	enableSlateTicker := func() {
		if slateTicker == nil {
			slateTicker = time.NewTicker(33333333 * time.Nanosecond)
			slateTickCh = slateTicker.C
		}
		slateFrameIdx = 0
		s.remapper.NotifySourceSwitch()
	}

	disableSlateTicker := func() {
		if slateTicker != nil {
			slateTicker.Stop()
			slateTicker = nil
			slateTickCh = nil
		}
	}
	defer disableSlateTicker()

	// Start in StateFallback
	enableSlateTicker()

	enterFallback := func(reason string, restartSRT bool) {
		s.setStateWithReason(StateFallback, reason)
		enableSlateTicker()
		if restartSRT {
			s.RestartSRT()
		}
	}

	for {
		switch s.GetState() {
		case StateLive:
			select {
			case data, ok := <-srtDataCh:
				if !ok {
					return nil
				}
				if s.manualFallback.Load() {
					enterFallback("Modo Pausa (BRB)", false)
					continue
				}

				s.lastSRTDataUnixMs.Store(time.Now().UnixMilli())
				s.processPackets(data)

				if s.containsVideo(data) {
					s.configMu.RLock()
					td := time.Duration(s.config.SRTTimeout) * time.Millisecond
					s.configMu.RUnlock()
					resetTimer(timeout, td)
				}

				remapped := s.remapper.Remap(data)
				s.broadcaster.Broadcast(remapped)
				s.packetsForwarded.Add(uint64(len(remapped) / tsPacketSize))

			case <-s.modeChangeCh:
				if s.manualFallback.Load() {
					enterFallback("Modo Pausa (BRB)", false)
				}

			case <-s.restartFallbackCh:
				// No-op during live; scene is already swapped in RAM

			case <-timeout.C:
				log.Println("[switcher] SRT timeout in StateLive -> instant RAM Slate")
				enterFallback("SRT timeout", true)

			case <-bitrateCheckTicker.C:
				if s.checkBitrateThreshold() {
					log.Println("[switcher] Bitrate below minimum -> instant RAM Slate")
					enterFallback("bitrate below minimum", true)
				}

			case <-forceFallbackCh:
				enterFallback("no publishers (API)", true)

			case <-srtDiedCh:
				enterFallback("SRT disconnected", false)

			case <-ctx.Done():
				return nil
			}

		case StateFallback:
			select {
			case <-slateTickCh:
				// Zero-CPU idle optimization: skip copying/remapping when nobody is consuming the stream
				if s.broadcaster.SubscriberCount() == 0 {
					wasSubscribed = false
					continue
				}
				if !wasSubscribed {
					wasSubscribed = true
					slateFrameIdx = 0
					s.remapper.NotifySourceSwitch()
				}

				if s.sceneManager == nil {
					continue
				}
				clip := s.sceneManager.GetActiveClip()
				if clip == nil || len(clip.Frames) == 0 {
					continue
				}

				if clip.SceneID != lastSceneID {
					lastSceneID = clip.SceneID
					slateFrameIdx = 0
					s.remapper.NotifySourceSwitch()
				}

				if slateFrameIdx >= len(clip.Frames) {
					slateFrameIdx = 0
					s.remapper.NotifySourceSwitch()
				}

				srcFrame := clip.Frames[slateFrameIdx]
				slateFrameIdx++

				// Copy into reusable buffer so Remap() never mutates the immutable RAM Slate
				if cap(slateWorkBuf) < len(srcFrame) {
					slateWorkBuf = make([]byte, len(srcFrame))
				} else {
					slateWorkBuf = slateWorkBuf[:len(srcFrame)]
				}
				copy(slateWorkBuf, srcFrame)

				remapped := s.remapper.Remap(slateWorkBuf)
				s.broadcaster.Broadcast(remapped)
				s.packetsForwarded.Add(uint64(len(remapped) / tsPacketSize))

			case data, ok := <-srtDataCh:
				if !ok {
					return nil
				}
				s.lastSRTDataUnixMs.Store(time.Now().UnixMilli())
				s.processPackets(data)

				// Stay on RAM Slate while Manual Pause / BRB is active (keep SRT warm in background!)
				if s.manualFallback.Load() {
					continue
				}

				s.configMu.RLock()
				waitKF := s.config.WaitForKeyframe
				td := time.Duration(s.config.SRTTimeout) * time.Millisecond
				s.configMu.RUnlock()

				if waitKF && !s.containsKeyframe(data) {
					continue
				}
				if !waitKF && !s.containsVideo(data) {
					continue
				}

				disableSlateTicker()
				reason := "SRT stream active"
				if waitKF {
					reason = "SRT keyframe detected"
				}
				s.setStateWithReason(StateLive, reason)
				s.remapper.PrepareSwitch(data)
				resetTimer(timeout, td)

				remapped := s.remapper.Remap(data)
				s.broadcaster.Broadcast(remapped)
				s.packetsForwarded.Add(uint64(len(remapped) / tsPacketSize))

			case <-s.restartFallbackCh:
				slateFrameIdx = 0
				s.remapper.NotifySourceSwitch()

			case <-s.modeChangeCh:
				// If BRB was just turned off, the very next incoming SRT batch will switch to StateLive

			case <-forceFallbackCh:
			case <-srtDiedCh:
			case <-ctx.Done():
				return nil
			}
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
		go s.readInputLoop(ctx, newProc, dataCh, procDiedCh)

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
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

// readInputLoop reads chunks of up to 49 TS packets (9212 bytes) and emits aligned batches
func (s *Switcher) readInputLoop(ctx context.Context, proc *InputProcess, dataCh chan []byte, diedCh chan struct{}) {
	aligner := NewPacketAligner()
	readBuf := make([]byte, 1316*7) // 9212 bytes = 49 TS packets
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
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
		if n > 0 {
			s.bytesReceived.Add(uint64(n))
			s.bitrateBytes.Add(uint64(n))

			aligner.Feed(readBuf[:n])
			batch := aligner.NextBatch()
			if len(batch) > 0 {
				s.packetsReceived.Add(uint64(len(batch) / tsPacketSize))
				select {
				case dataCh <- batch:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// processPackets extracts PAT/PMT PIDs, counts keyframes, and tallies audio payload bytes in one pass
func (s *Switcher) processPackets(data []byte) {
	pmtPID := uint16(s.pmtPID.Load())
	vPID := uint16(s.videoPID.Load())
	aPID := uint16(s.audioPID.Load())
	var audioBatchBytes uint64

	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}

		if info.PID == patPID && info.PUSI && info.HasPayload {
			if parsedPMT := ParsePAT(pkt[info.PayloadOffset:]); parsedPMT != 0 && parsedPMT != pmtPID {
				pmtPID = parsedPMT
				s.pmtPID.Store(uint32(pmtPID))
			}
		} else if pmtPID != 0 && info.PID == pmtPID && info.PUSI && info.HasPayload {
			newV, newA := ParsePMT(pkt[info.PayloadOffset:])
			changed := false
			if newV != 0 && newV != vPID {
				vPID = newV
				s.videoPID.Store(uint32(vPID))
				changed = true
				log.Printf("[switcher] Video PID: %d", vPID)
			}
			if newA != 0 && newA != aPID {
				aPID = newA
				s.audioPID.Store(uint32(aPID))
				changed = true
				log.Printf("[switcher] Audio PID: %d", aPID)
			}
			if changed {
				s.remapper.SetPIDs(vPID, aPID, pmtPID)
			}
		}

		if aPID != 0 && info.PID == aPID && info.HasPayload && info.PayloadOffset < tsPacketSize {
			audioBatchBytes += uint64(tsPacketSize - info.PayloadOffset)
		}

		if vPID != 0 && info.PID == vPID && info.PUSI && info.HasPayload {
			if pes := ExtractPESPayload(pkt, info.PayloadOffset); pes != nil && IsKeyframe(pes) {
				s.keyframesDetected.Add(1)
			}
		}
	}

	if audioBatchBytes > 0 {
		s.audioBytes.Add(audioBatchBytes)
	}
}

func (s *Switcher) containsKeyframe(data []byte) bool {
	vPID := uint16(s.videoPID.Load())
	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}
		if vPID != 0 && info.PID == vPID && info.PUSI && info.HasPayload {
			if pes := ExtractPESPayload(pkt, info.PayloadOffset); pes != nil && IsKeyframe(pes) {
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

			aBytes := s.audioBytes.Swap(0)
			s.currentAudioKbps.Store((aBytes * 8) / 1000)

			s.bitrateHistoryMu.Lock()
			s.bitrateHistory[s.bitrateHistoryIdx] = float64(kbps)
			s.bitrateHistoryIdx = (s.bitrateHistoryIdx + 1) % 7200
			if s.bitrateHistoryLen < 7200 {
				s.bitrateHistoryLen++
			}
			s.bitrateHistoryMu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

func (s *Switcher) containsVideo(data []byte) bool {
	vPID := uint16(s.videoPID.Load())
	if vPID == 0 {
		return true
	}
	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if ok && info.PID == vPID {
			return true
		}
	}
	return false
}
