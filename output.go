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
	Bitrate        int         `json:"bitrate"` // kbps, only for h264
	Preset         string      `json:"preset"`  // FFmpeg preset, only for h264
	Enabled        bool        `json:"enabled"`
	OverlayEnabled bool        `json:"overlay_enabled"`
	OverlayX       int         `json:"overlay_x"`
	OverlayY       int         `json:"overlay_y"`
	Running        bool        `json:"running"`
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

// Output represents a single RTMP output destination
type Output struct {
	config        OutputConfig
	cmd           *exec.Cmd
	targetRunning atomic.Bool
	running       atomic.Bool
	stopCh        chan struct{}
	stopClosed    bool
	mu            sync.Mutex

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
		config:     config,
		dataDir:    dataDir,
		stopClosed: true,
		logBuffer:  make([]string, 0, 50),
	}
}

func (o *Output) getConfig() OutputConfig {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.config
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
	o.mu.Lock()
	defer o.mu.Unlock()

	rtmpURL := o.config.URL
	if o.config.StreamKey != "" {
		rtmpURL = strings.TrimRight(rtmpURL, "/") + "/" + o.config.StreamKey
	}

	// Clean MPEG-TS input: PTSRemapper already guarantees monotonic PTS/DTS/PCR and continuous CC,
	// so we avoid -use_wallclock_as_timestamps (which breaks iPhone B-frame presentation order).
	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-fflags", "+discardcorrupt",
		"-f", "mpegts",
		"-i", "pipe:0",
	}

	switch o.config.Codec {
	case CodecH265Passthrough:
		// Zero-CPU video + audio passthrough (Moblin & RAM Slate both emit AAC 48kHz stereo)
		args = append(args,
			"-c:v", "copy",
			"-c:a", "copy",
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
			"-threads", "2",
			"-b:v", fmt.Sprintf("%dk", bitrate),
			"-maxrate", fmt.Sprintf("%dk", bitrate),
			"-bufsize", fmt.Sprintf("%dk", bitrate*2),
			"-g", "60",
		)

		if o.config.OverlayEnabled {
			if o.zmqPort == 0 {
				port, err := findFreePort()
				if err != nil {
					o.zmqPort = 5560
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
				filterStr = fmt.Sprintf("movie=%s [wm]; [in][wm] overlay@my_overlay=x=%d:y=%d:eval=frame [out]; zmq=b=tcp\\\\://127.0.0.1\\\\:%d", watermarkPath, xCoord, yCoord, o.zmqPort)
			} else {
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
	if o.targetRunning.Load() {
		id := o.config.ID
		o.mu.Unlock()
		return fmt.Errorf("output %s already running", id)
	}

	stopCh := make(chan struct{})
	o.stopCh = stopCh
	o.stopClosed = false
	o.targetRunning.Store(true)
	o.mu.Unlock()

	o.lastErrorMu.Lock()
	o.lastError = ""
	o.lastErrorMu.Unlock()

	go o.managerLoop(broadcaster, stopCh)
	go o.bitrateLoop(stopCh)

	return nil
}

func (o *Output) managerLoop(broadcaster *Broadcaster, stopCh chan struct{}) {
	for o.targetRunning.Load() {
		select {
		case <-stopCh:
			return
		default:
		}

		o.runProcess(broadcaster, stopCh)

		if !o.targetRunning.Load() {
			break
		}

		o.appendLog("--- FFmpeg exited or crashed, restarting in 3s ---")
		select {
		case <-time.After(3 * time.Second):
		case <-stopCh:
			return
		}
	}
}

func (o *Output) runProcess(broadcaster *Broadcaster, stopCh chan struct{}) {
	args := o.buildFFmpegArgs()

	o.mu.Lock()
	if !o.targetRunning.Load() || o.stopClosed {
		o.mu.Unlock()
		return
	}

	o.cmd = exec.Command("ffmpeg", args...)
	setProcessDeathSignal(o.cmd)

	stdinPipe, err := o.cmd.StdinPipe()
	if err != nil {
		o.mu.Unlock()
		o.appendLog(fmt.Sprintf("Pipe error: %v", err))
		return
	}

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
	cfg := o.config
	zmqPort := o.zmqPort
	pid := o.cmd.Process.Pid
	cmd := o.cmd
	dataCh := broadcaster.Subscribe(cfg.ID)

	if cfg.OverlayEnabled && zmqPort > 0 {
		go func(port int) {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-stopCh:
				return
			}
			o.mu.Lock()
			defer o.mu.Unlock()
			if !o.running.Load() || o.stopClosed {
				return
			}
			addr := fmt.Sprintf("tcp://127.0.0.1:%d", port)
			o.zmqClient = NewZMQClient(addr)
			if err := o.zmqClient.Connect(); err != nil {
				o.appendLog(fmt.Sprintf("ZMQ connect error: %v", err))
				o.zmqClient = nil
			} else {
				o.appendLog(fmt.Sprintf("ZMQ connected on port %d", port))
			}
		}(zmqPort)
	}
	o.mu.Unlock()

	log.Printf("[output:%s] Started (codec=%s, PID=%d)", cfg.Name, cfg.Codec, pid)
	o.appendLog(fmt.Sprintf("--- Started FFmpeg PID %d ---", pid))

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

	go o.writeLoop(cfg.ID, stdinPipe, dataCh, stopCh)

	err = cmd.Wait()
	broadcaster.UnsubscribeChannel(cfg.ID, dataCh)

	o.mu.Lock()
	if o.cmd == cmd {
		o.running.Store(false)
		if o.zmqClient != nil {
			o.zmqClient.Close()
			o.zmqClient = nil
		}
	}
	o.mu.Unlock()

	if err != nil {
		o.appendLog(fmt.Sprintf("--- Exited with error: %v ---", err))
	} else {
		o.appendLog("--- Exited cleanly ---")
	}
}

func (o *Output) writeLoop(outID string, stdinPipe io.WriteCloser, dataCh <-chan []byte, stopCh <-chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[output %s] PANIC RECOVERED in writeLoop: %v", outID, r)
		}
	}()
	defer stdinPipe.Close()

	for {
		select {
		case data, ok := <-dataCh:
			if !ok {
				return
			}

			if _, err := stdinPipe.Write(data); err != nil {
				o.packetsDropped.Add(1)
				continue
			}
			o.packetsSent.Add(uint64(len(data) / tsPacketSize))
			o.bytesSent.Add(uint64(len(data)))
			o.bitrateBytes.Add(uint64(len(data)))

		case <-stopCh:
			return
		}
	}
}

func (o *Output) bitrateLoop(stopCh <-chan struct{}) {
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

		case <-stopCh:
			return
		}
	}
}

func (o *Output) Stop(broadcaster *Broadcaster) {
	o.mu.Lock()
	if !o.targetRunning.Load() {
		o.mu.Unlock()
		return
	}

	o.targetRunning.Store(false)
	if !o.stopClosed && o.stopCh != nil {
		close(o.stopCh)
		o.stopClosed = true
	}

	if o.cmd != nil && o.cmd.Process != nil {
		_ = o.cmd.Process.Kill()
	}
	if o.zmqClient != nil {
		o.zmqClient.Close()
		o.zmqClient = nil
	}
	id := o.config.ID
	name := o.config.Name
	o.mu.Unlock()

	if broadcaster != nil {
		broadcaster.Unsubscribe(id)
	}

	time.Sleep(100 * time.Millisecond)
	o.running.Store(false)
	log.Printf("[output:%s] Stopped", name)
	o.appendLog("--- Output stopped by user ---")
}

func (o *Output) GetStats() OutputStats {
	o.mu.Lock()
	cfg := o.config
	startTime := o.startTime
	o.mu.Unlock()

	o.lastErrorMu.RLock()
	lastErr := o.lastError
	o.lastErrorMu.RUnlock()

	uptimeStr := ""
	if o.running.Load() && !startTime.IsZero() {
		uptimeStr = time.Since(startTime).Truncate(time.Second).String()
	}

	return OutputStats{
		ID:                cfg.ID,
		Name:              cfg.Name,
		Running:           o.targetRunning.Load(),
		Codec:             string(cfg.Codec),
		Bitrate:           cfg.Bitrate,
		BytesSent:         o.bytesSent.Load(),
		PacketsSent:       o.packetsSent.Load(),
		PacketsDropped:    o.packetsDropped.Load(),
		OutputBitrateKbps: float64(o.currentBitrate.Load()),
		Uptime:            uptimeStr,
		Error:             lastErr,
		OverlayEnabled:    cfg.OverlayEnabled,
		OverlayX:          cfg.OverlayX,
		OverlayY:          cfg.OverlayY,
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

	if _, err := o.zmqClient.SendCommand("my_overlay", "x", fmt.Sprintf("%d", x)); err != nil {
		return fmt.Errorf("failed to send x coordinate: %w", err)
	}
	if _, err := o.zmqClient.SendCommand("my_overlay", "y", fmt.Sprintf("%d", y)); err != nil {
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
	output, exists := om.outputs[id]
	if !exists {
		om.mu.Unlock()
		return fmt.Errorf("output %s not found", id)
	}

	delete(om.outputs, id)
	om.saveConfig()
	om.mu.Unlock()

	if output.targetRunning.Load() {
		output.Stop(om.broadcaster)
	}
	return nil
}

func (om *OutputManager) UpdateOutput(id string, config OutputConfig) error {
	om.mu.Lock()
	oldOutput, exists := om.outputs[id]
	if !exists {
		om.mu.Unlock()
		return fmt.Errorf("output %s not found", id)
	}

	wasRunning := oldOutput.targetRunning.Load()
	config.ID = id
	config.Running = wasRunning
	newOutput := NewOutput(config, om.dataDir)
	om.outputs[id] = newOutput
	om.saveConfig()
	om.mu.Unlock()

	if wasRunning {
		oldOutput.Stop(om.broadcaster)
		_ = newOutput.Start(om.broadcaster)
	}
	return nil
}

func (om *OutputManager) StartOutput(id string) error {
	om.mu.Lock()
	output, exists := om.outputs[id]
	if exists {
		output.mu.Lock()
		url := output.config.URL
		if !strings.HasPrefix(url, "rtmp://") && !strings.HasPrefix(url, "rtmps://") {
			output.mu.Unlock()
			om.mu.Unlock()
			return fmt.Errorf("invalid RTMP URL: must start with rtmp:// or rtmps://")
		}
		output.config.Running = true
		output.mu.Unlock()
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
		output.mu.Lock()
		output.config.Running = false
		output.mu.Unlock()
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

	err := output.MoveOverlay(x, y)
	om.saveConfig()
	return err
}

func (om *OutputManager) GetOutputs() []OutputConfig {
	om.mu.RLock()
	defer om.mu.RUnlock()

	configs := make([]OutputConfig, 0, len(om.outputs))
	for _, o := range om.outputs {
		configs = append(configs, o.getConfig())
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
	toStop := make([]*Output, 0, len(om.outputs))
	for _, o := range om.outputs {
		if o.targetRunning.Load() {
			toStop = append(toStop, o)
		}
	}
	om.mu.RUnlock()

	for _, o := range toStop {
		o.Stop(om.broadcaster)
	}
}

func (om *OutputManager) RestartAll() {
	om.mu.RLock()
	toRestart := make([]*Output, 0, len(om.outputs))
	for _, o := range om.outputs {
		if o.targetRunning.Load() {
			toRestart = append(toRestart, o)
		}
	}
	om.mu.RUnlock()

	for _, o := range toRestart {
		o.Stop(om.broadcaster)
		time.Sleep(200 * time.Millisecond)
		_ = o.Start(om.broadcaster)
	}
	log.Printf("[outputs] All running outputs restarted (%d)", len(toRestart))
}

type savedConfig struct {
	Outputs []OutputConfig `json:"outputs"`
}

func (om *OutputManager) saveConfig() {
	cfg := savedConfig{
		Outputs: make([]OutputConfig, 0, len(om.outputs)),
	}
	for _, o := range om.outputs {
		cfg.Outputs = append(cfg.Outputs, o.getConfig())
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
		return
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
