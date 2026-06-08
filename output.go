package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// OutputCodec defines the codec for an output
type OutputCodec string

const (
	CodecH265Passthrough OutputCodec = "h265"
	CodecH264Transcode   OutputCodec = "h264"
)

// OutputConfig holds the configuration for a single output
type OutputConfig struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	URL            string      `json:"url"`
	StreamKey      string      `json:"stream_key"`
	Codec          OutputCodec `json:"codec"`
	Bitrate        int         `json:"bitrate"`   // kbps, only for h264
	Preset         string      `json:"preset"`    // FFmpeg preset, only for h264
	Enabled        bool        `json:"enabled"`
	OverlayEnabled bool        `json:"overlay_enabled"`
	OverlayX       int         `json:"overlay_x"`
	OverlayY       int         `json:"overlay_y"`
	Running        bool        `json:"running"` // Persist running state on boot (Stage 6)
}

// OutputStats holds runtime statistics for an output
type OutputStats struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Running           bool    `json:"running"`
	Codec             string  `json:"codec"`
	Bitrate           int     `json:"bitrate"`
	BytesSent         uint64  `json:"bytes_sent"`
	PacketsSent       uint64  `json:"packets_sent"`
	PacketsDropped    uint64  `json:"packets_dropped"`
	OutputBitrateKbps float64 `json:"output_bitrate_kbps"`
	Uptime            string  `json:"uptime"`
	Error             string  `json:"error,omitempty"`
	OverlayEnabled    bool    `json:"overlay_enabled"`
	OverlayX          int     `json:"overlay_x"`
	OverlayY          int     `json:"overlay_y"`
}

// stdinWrapper wraps io.WriteCloser to allow atomic storing of nil values without panicking
type stdinWrapper struct {
	pipe io.WriteCloser
}

// Output represents a single RTMP output destination
type Output struct {
	config    OutputConfig
	cmd       *exec.Cmd
	stdinRef  atomic.Value // stores *stdinWrapper — lock-free access in writeLoop
	targetRunning atomic.Bool
	running   atomic.Bool
	dataCh    chan []byte
	stopCh    chan struct{}
	mu        sync.Mutex

	// ZMQ Controls
	zmqPort   int
	zmqClient *ZMQClient

	// Stats
	bytesSent      atomic.Uint64
	packetsSent    atomic.Uint64
	packetsDropped atomic.Uint64
	bitrateBytes   atomic.Uint64
	currentBitrate atomic.Uint64
	startTime      time.Time
	lastError      string
	lastErrorMu    sync.RWMutex

	// Logs
	logBuffer []string
	logMu     sync.RWMutex
	dataDir   string
}

func NewOutput(config OutputConfig, dataDir string) *Output {
	return &Output{
		config:    config,
		dataDir:   dataDir,
		dataCh:    make(chan []byte, 5000),
		stopCh:    make(chan struct{}),
		logBuffer: make([]string, 0, 50),
	}
}

func (o *Output) appendLog(line string) {
	o.logMu.Lock()
	defer o.logMu.Unlock()
	o.logBuffer = append(o.logBuffer, time.Now().Format("15:04:05")+" "+line)
	if len(o.logBuffer) > 50 {
		o.logBuffer = o.logBuffer[1:]
	}
}

func (o *Output) GetLogs() []string {
	o.logMu.RLock()
	defer o.logMu.RUnlock()
	logs := make([]string, len(o.logBuffer))
	copy(logs, o.logBuffer)
	return logs
}

func findFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.Port, nil
}

func (o *Output) buildFFmpegArgs() []string {
	rtmpURL := o.config.URL
	if o.config.StreamKey != "" {
		rtmpURL = rtmpURL + "/" + o.config.StreamKey
	}

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-use_wallclock_as_timestamps", "1",
		"-fflags", "+genpts+discardcorrupt+igndts",
		"-f", "mpegts",
		"-i", "pipe:0",
	}

	switch o.config.Codec {
	case CodecH265Passthrough:
		// Remux video, transcode audio to standard AAC for RTMP
		args = append(args,
			"-c:v", "copy",
			"-c:a", "aac", "-b:a", "128k", "-ar", "48000",
		)
	case CodecH264Transcode:
		preset := o.config.Preset
		if preset == "" {
			preset = "ultrafast"
		}
		bitrate := o.config.Bitrate
		if bitrate <= 0 {
			bitrate = 6000
		}
		args = append(args,
			"-c:v", "libx264",
			"-preset", preset,
			"-tune", "zerolatency",
			"-b:v", fmt.Sprintf("%dk", bitrate),
			"-maxrate", fmt.Sprintf("%dk", bitrate),
			"-bufsize", fmt.Sprintf("%dk", bitrate*2),
			"-g", "60", // 2 second GOP at 30fps
		)

		if o.config.OverlayEnabled {
			if o.zmqPort == 0 {
				port, err := findFreePort()
				if err != nil {
					o.zmqPort = 5560 // Fallback
				} else {
					o.zmqPort = port
				}
			}

			watermarkPath := filepath.Join(o.dataDir, "watermark.png")
			xCoord := o.config.OverlayX
			yCoord := o.config.OverlayY
			if xCoord <= 0 {
				xCoord = 10
			}
			if yCoord <= 0 {
				yCoord = 10
			}

			var filterStr string
			if _, err := os.Stat(watermarkPath); err == nil {
				// Watermark file exists
				// Position: dynamic via overlay@my_overlay with eval=frame
				filterStr = fmt.Sprintf("movie=%s [wm]; [in][wm] overlay@my_overlay=x=%d:y=%d:eval=frame [out]; zmq=b=tcp\\\\://127.0.0.1\\\\:%d", watermarkPath, xCoord, yCoord, o.zmqPort)
			} else {
				// Fallback to text if watermark not uploaded yet
				filterStr = fmt.Sprintf("drawtext@my_overlay=text='Oracle Stream Studio':fontcolor=white:fontsize=24:box=1:boxcolor=black@0.5:x=%d:y=%d:eval=frame, zmq=b=tcp\\\\://127.0.0.1\\\\:%d", xCoord, yCoord, o.zmqPort)
			}
			args = append(args, "-vf", filterStr)
		}

		args = append(args,
			"-c:a", "aac",
			"-b:a", "128k",
			"-ar", "48000",
		)
	}

	args = append(args,
		"-f", "flv",
		"-flvflags", "no_duration_filesize",
		rtmpURL,
	)

	return args
}

func (o *Output) Start(broadcaster *Broadcaster) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.targetRunning.Load() {
		return fmt.Errorf("output %s already running", o.config.ID)
	}

	o.targetRunning.Store(true)
	o.stopCh = make(chan struct{})

	go o.managerLoop(broadcaster)
	go o.bitrateLoop()

	return nil
}

func (o *Output) managerLoop(broadcaster *Broadcaster) {
	for o.targetRunning.Load() {
		o.runProcess(broadcaster)

		if !o.targetRunning.Load() {
			break
		}

		o.appendLog("--- FFmpeg exited or crashed, restarting in 3s ---")
		select {
		case <-time.After(3 * time.Second):
		case <-o.stopCh:
			return
		}
	}
}

func (o *Output) runProcess(broadcaster *Broadcaster) {
	args := o.buildFFmpegArgs()

	o.mu.Lock()
	o.cmd = exec.Command("ffmpeg", args...)
	o.cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}

	stdinPipe, err := o.cmd.StdinPipe()
	if err != nil {
		o.mu.Unlock()
		o.appendLog(fmt.Sprintf("Pipe error: %v", err))
		return
	}
	o.stdinRef.Store(&stdinWrapper{pipe: stdinPipe})
	
	stderr, err := o.cmd.StderrPipe()
	if err != nil {
		o.mu.Unlock()
		o.appendLog(fmt.Sprintf("Stderr error: %v", err))
		return
	}

	if err := o.cmd.Start(); err != nil {
		o.mu.Unlock()
		o.appendLog(fmt.Sprintf("Start error: %v", err))
		return
	}

	o.running.Store(true)
	o.startTime = time.Now()

	// Subscribe to broadcaster
	o.dataCh = broadcaster.Subscribe(o.config.ID)

	// Connect ZMQ client if overlay enabled
	if o.config.OverlayEnabled && o.zmqPort > 0 {
		go func() {
			time.Sleep(500 * time.Millisecond)
			o.mu.Lock()
			defer o.mu.Unlock()
			if !o.running.Load() {
				return
			}
			addr := fmt.Sprintf("tcp://127.0.0.1:%d", o.zmqPort)
			o.zmqClient = NewZMQClient(addr)
			if err := o.zmqClient.Connect(); err != nil {
				o.appendLog(fmt.Sprintf("ZMQ connect error: %v", err))
				o.zmqClient = nil
			} else {
				o.appendLog(fmt.Sprintf("ZMQ connected on port %d", o.zmqPort))
			}
		}()
	}
	o.mu.Unlock()

	log.Printf("[output:%s] Started (codec=%s, PID=%d)", o.config.Name, o.config.Codec, o.cmd.Process.Pid)
	o.appendLog(fmt.Sprintf("--- Started FFmpeg PID %d ---", o.cmd.Process.Pid))

	// Stderr reader
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			o.appendLog(line)
			o.lastErrorMu.Lock()
			o.lastError = line
			o.lastErrorMu.Unlock()
		}
	}()

	// Data writer goroutine
	go o.writeLoop()

	// Wait for process to exit
	err = o.cmd.Wait()
	o.running.Store(false)

	broadcaster.Unsubscribe(o.config.ID)

	// Close ZMQ client
	o.mu.Lock()
	if o.zmqClient != nil {
		o.zmqClient.Close()
		o.zmqClient = nil
	}
	o.mu.Unlock()

	if err != nil {
		o.appendLog(fmt.Sprintf("--- Exited with error: %v ---", err))
	} else {
		o.appendLog("--- Exited cleanly ---")
	}
}

func (o *Output) writeLoop() {
	defer func() {
		if v := o.stdinRef.Load(); v != nil {
			if wrapper, ok := v.(*stdinWrapper); ok && wrapper.pipe != nil {
				wrapper.pipe.Close()
			}
		}
		o.stdinRef.Store(&stdinWrapper{pipe: nil})
	}()

	for {
		select {
		case data, ok := <-o.dataCh:
			if !ok {
				return
			}

			// Lock-free atomic load — no mutex per packet
			v := o.stdinRef.Load()
			if v == nil {
				continue
			}
			wrapper, ok := v.(*stdinWrapper)
			if !ok || wrapper == nil || wrapper.pipe == nil {
				continue
			}

			_, err := wrapper.pipe.Write(data)
			if err != nil {
				o.packetsDropped.Add(1)
				continue
			}
			o.packetsSent.Add(1)
			o.bytesSent.Add(uint64(len(data)))
			o.bitrateBytes.Add(uint64(len(data)))

		case <-o.stopCh:
			return
		}
	}
}

func (o *Output) bitrateLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !o.targetRunning.Load() {
				return
			}
			bytes := o.bitrateBytes.Swap(0)
			kbps := (bytes * 8) / 1000
			o.currentBitrate.Store(kbps)

		case <-o.stopCh:
			return
		}
	}
}

func (o *Output) Stop(broadcaster *Broadcaster) {
	if !o.targetRunning.Load() {
		return
	}

	o.targetRunning.Store(false)

	select {
	case <-o.stopCh:
		// Already stopped
	default:
		close(o.stopCh)
	}

	// Kill the process — runProcess() will handle Unsubscribe when it exits
	o.mu.Lock()
	if o.cmd != nil && o.cmd.Process != nil {
		o.cmd.Process.Kill()
	}
	if o.zmqClient != nil {
		o.zmqClient.Close()
		o.zmqClient = nil
	}
	o.mu.Unlock()

	// Give runProcess a moment to clean up
	time.Sleep(100 * time.Millisecond)
	o.running.Store(false)
	log.Printf("[output:%s] Stopped", o.config.Name)
	o.appendLog("--- Output stopped by user ---")
}

func (o *Output) GetStats() OutputStats {
	o.lastErrorMu.RLock()
	lastErr := o.lastError
	o.lastErrorMu.RUnlock()

	uptimeStr := ""
	if o.running.Load() && !o.startTime.IsZero() {
		uptimeStr = time.Since(o.startTime).Truncate(time.Second).String()
	}

	return OutputStats{
		ID:                o.config.ID,
		Name:              o.config.Name,
		Running:           o.targetRunning.Load(), // Show as running if target is running (even if restarting)
		Codec:             string(o.config.Codec),
		Bitrate:           o.config.Bitrate,
		BytesSent:         o.bytesSent.Load(),
		PacketsSent:       o.packetsSent.Load(),
		PacketsDropped:    o.packetsDropped.Load(),
		OutputBitrateKbps: float64(o.currentBitrate.Load()),
		Uptime:            uptimeStr,
		Error:             lastErr,
		OverlayEnabled:    o.config.OverlayEnabled,
		OverlayX:          o.config.OverlayX,
		OverlayY:          o.config.OverlayY,
	}
}

func (o *Output) MoveOverlay(x, y int) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.config.OverlayX = x
	o.config.OverlayY = y

	if !o.running.Load() || o.zmqClient == nil {
		return nil
	}

	// Send ZMQ command to move watermark on overlay filter instance "my_overlay"
	_, err := o.zmqClient.SendCommand("my_overlay", "x", fmt.Sprintf("%d", x))
	if err != nil {
		return fmt.Errorf("failed to send x coordinate: %w", err)
	}
	_, err = o.zmqClient.SendCommand("my_overlay", "y", fmt.Sprintf("%d", y))
	if err != nil {
		return fmt.Errorf("failed to send y coordinate: %w", err)
	}

	return nil
}

// OutputManager manages all outputs
type OutputManager struct {
	outputs     map[string]*Output
	mu          sync.RWMutex
	broadcaster *Broadcaster
	configPath  string
	dataDir     string
}

func NewOutputManager(broadcaster *Broadcaster, configPath string, dataDir string) *OutputManager {
	om := &OutputManager{
		outputs:     make(map[string]*Output),
		broadcaster: broadcaster,
		configPath:  configPath,
		dataDir:     dataDir,
	}
	om.loadConfig()
	return om
}

func (om *OutputManager) AddOutput(config OutputConfig) error {
	om.mu.Lock()
	defer om.mu.Unlock()

	if _, exists := om.outputs[config.ID]; exists {
		return fmt.Errorf("output %s already exists", config.ID)
	}

	om.outputs[config.ID] = NewOutput(config, om.dataDir)
	om.saveConfig()
	log.Printf("[outputs] Added output: %s (%s)", config.Name, config.ID)
	return nil
}

func (om *OutputManager) RemoveOutput(id string) error {
	om.mu.Lock()
	defer om.mu.Unlock()

	output, exists := om.outputs[id]
	if !exists {
		return fmt.Errorf("output %s not found", id)
	}

	if output.targetRunning.Load() {
		output.Stop(om.broadcaster)
	}

	delete(om.outputs, id)
	om.saveConfig()
	return nil
}

func (om *OutputManager) UpdateOutput(id string, config OutputConfig) error {
	om.mu.Lock()
	defer om.mu.Unlock()

	output, exists := om.outputs[id]
	if !exists {
		return fmt.Errorf("output %s not found", id)
	}

	wasRunning := output.targetRunning.Load()
	if wasRunning {
		output.Stop(om.broadcaster)
	}

	config.ID = id
	om.outputs[id] = NewOutput(config, om.dataDir)

	if wasRunning {
		om.outputs[id].Start(om.broadcaster)
	}

	om.saveConfig()
	return nil
}

func (om *OutputManager) StartOutput(id string) error {
	om.mu.Lock()
	output, exists := om.outputs[id]
	if exists {
		// Validate RTMP URL (Stage 6)
		url := output.config.URL
		if !strings.HasPrefix(url, "rtmp://") && !strings.HasPrefix(url, "rtmps://") {
			om.mu.Unlock()
			return fmt.Errorf("invalid RTMP URL: must start with rtmp:// or rtmps://")
		}
		output.config.Running = true
		om.saveConfig()
	}
	om.mu.Unlock()

	if !exists {
		return fmt.Errorf("output %s not found", id)
	}

	return output.Start(om.broadcaster)
}

func (om *OutputManager) StopOutput(id string) error {
	om.mu.Lock()
	output, exists := om.outputs[id]
	if exists {
		output.config.Running = false
		om.saveConfig()
	}
	om.mu.Unlock()

	if !exists {
		return fmt.Errorf("output %s not found", id)
	}

	output.Stop(om.broadcaster)
	return nil
}

func (om *OutputManager) MoveOverlay(id string, x, y int) error {
	om.mu.Lock()
	defer om.mu.Unlock()

	output, exists := om.outputs[id]
	if !exists {
		return fmt.Errorf("output %s not found", id)
	}

	// Update coordinates in the config
	output.config.OverlayX = x
	output.config.OverlayY = y

	// Call MoveOverlay on the actual output (sends via ZMQ if active)
	err := output.MoveOverlay(x, y)

	// Save coordinates to outputs.json
	om.saveConfig()

	return err
}

func (om *OutputManager) GetOutputs() []OutputConfig {
	om.mu.RLock()
	defer om.mu.RUnlock()

	configs := make([]OutputConfig, 0, len(om.outputs))
	for _, o := range om.outputs {
		configs = append(configs, o.config)
	}
	return configs
}

func (om *OutputManager) GetStats() []OutputStats {
	om.mu.RLock()
	defer om.mu.RUnlock()

	stats := make([]OutputStats, 0, len(om.outputs))
	for _, o := range om.outputs {
		stats = append(stats, o.GetStats())
	}
	return stats
}

func (om *OutputManager) GetLogs(id string) ([]string, error) {
	om.mu.RLock()
	output, exists := om.outputs[id]
	om.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("output %s not found", id)
	}

	return output.GetLogs(), nil
}

func (om *OutputManager) StopAll() {
	om.mu.RLock()
	defer om.mu.RUnlock()

	for _, o := range om.outputs {
		if o.targetRunning.Load() {
			o.Stop(om.broadcaster)
		}
	}
}

func (om *OutputManager) RestartAll() {
	om.mu.RLock()
	defer om.mu.RUnlock()

	for _, o := range om.outputs {
		if o.targetRunning.Load() {
			o.Stop(om.broadcaster)
			time.Sleep(200 * time.Millisecond)
			o.Start(om.broadcaster)
		}
	}
	log.Printf("[outputs] All running outputs restarted")
}

// Config persistence
type savedConfig struct {
	Outputs []OutputConfig `json:"outputs"`
}

func (om *OutputManager) saveConfig() {
	cfg := savedConfig{
		Outputs: make([]OutputConfig, 0, len(om.outputs)),
	}
	for _, o := range om.outputs {
		cfg.Outputs = append(cfg.Outputs, o.config)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("[outputs] Failed to marshal config: %v", err)
		return
	}

	if err := os.WriteFile(om.configPath, data, 0644); err != nil {
		log.Printf("[outputs] Failed to save config: %v", err)
	}
}

func (om *OutputManager) loadConfig() {
	data, err := os.ReadFile(om.configPath)
	if err != nil {
		return // File doesn't exist yet
	}

	var cfg savedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("[outputs] Failed to parse config: %v", err)
		return
	}

	for _, oc := range cfg.Outputs {
		om.outputs[oc.ID] = NewOutput(oc, om.dataDir)
	}

	log.Printf("[outputs] Loaded %d outputs from config", len(cfg.Outputs))
}
