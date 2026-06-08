package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	// Configure default slog JSON handler
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	srtAddr := flag.String("srt-addr", "localhost:8282", "SRT source address")
	srtMode := flag.String("srt-mode", "caller", "SRT mode: caller or listener")
	fallbackPath := flag.String("fallback", "", "Path to fallback media file")
	webPort := flag.Int("port", 8081, "Web UI port")
	srtTimeout := flag.Int("srt-timeout", 2000, "SRT timeout in ms")
	statsURL := flag.String("stats-url", "", "HTTP URL for bbox stats")
	dataDir := flag.String("data-dir", "", "Directory for config/data files")
	bboxDir := flag.String("bbox-dir", "", "Directory where bbox docker-compose.yml is located (default: data-dir)")
	webUser := flag.String("web-user", "", "Web UI username for basic auth")
	webPass := flag.String("web-pass", "", "Web UI password for basic auth")
	tlsCert := flag.String("tls-cert", "", "Path to TLS certificate file")
	tlsKey := flag.String("tls-key", "", "Path to TLS private key file")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Oracle Stream Studio v1.8 — H.265 Failover Relay + Multi-Output RTMP\n\n")
		fmt.Fprintf(os.Stderr, "Usage: oracle-stream-studio [flags]\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *dataDir == "" {
		exe, err := os.Executable()
		if err != nil {
			*dataDir = "."
		} else {
			*dataDir = filepath.Dir(exe)
		}
	}

	// Load .env
	loadEnvFile(filepath.Join(*dataDir, ".env"))
	loadEnvFile(".env")

	if *srtAddr == "localhost:8282" {
		if v := os.Getenv("SRT_ADDR"); v != "" {
			*srtAddr = v
		}
	}
	if *srtMode == "caller" {
		if v := os.Getenv("SRT_MODE"); v != "" {
			*srtMode = v
		}
	}
	if *srtTimeout == 2000 {
		if v := os.Getenv("SRT_TIMEOUT"); v != "" {
			fmt.Sscanf(v, "%d", srtTimeout)
		}
	}
	if *statsURL == "" {
		if v := os.Getenv("STATS_URL"); v != "" {
			*statsURL = v
		}
	}
	if *fallbackPath == "" {
		if v := os.Getenv("FALLBACK_PATH"); v != "" {
			*fallbackPath = v
		}
	}
	if *fallbackPath == "" {
		*fallbackPath = filepath.Join(*dataDir, "fallback.ts")
	}
	if *webUser == "" {
		if v := os.Getenv("WEB_USER"); v != "" {
			*webUser = v
		}
	}
	if *webPass == "" {
		if v := os.Getenv("WEB_PASS"); v != "" {
			*webPass = v
		}
	}
	if *tlsCert == "" {
		if v := os.Getenv("TLS_CERT"); v != "" {
			*tlsCert = v
		}
	}
	if *tlsKey == "" {
		if v := os.Getenv("TLS_KEY"); v != "" {
			*tlsKey = v
		}
	}

	configPath := filepath.Join(*dataDir, "outputs.json")
	switcherConfigPath := filepath.Join(*dataDir, "switcher.json")

	os.MkdirAll(*dataDir, 0755)

	fmt.Println("╔═══════════════════════════════════════════════╗")
	fmt.Println("║         Oracle Stream Studio v1.8             ║")
	fmt.Println("║   H.265 Failover Relay + Multi-Output RTMP    ║")
	fmt.Println("╚═══════════════════════════════════════════════╝")
	fmt.Println()
	log.Printf("SRT Source:    %s (mode: %s)", *srtAddr, *srtMode)
	log.Printf("Fallback:      %s", *fallbackPath)
	if *tlsCert != "" && *tlsKey != "" {
		log.Printf("Web UI:        https://0.0.0.0:%d (SSL Enabled)", *webPort)
	} else {
		log.Printf("Web UI:        http://0.0.0.0:%d", *webPort)
	}
	log.Printf("SRT Timeout:   %d ms", *srtTimeout)
	if *statsURL != "" {
		log.Printf("Stats URL:     %s", *statsURL)
	}
	log.Printf("Data Dir:      %s", *dataDir)
	fmt.Println()

	if err := checkFFmpeg(); err != nil {
		log.Fatalf("ERROR: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sysStats := NewSysStatsMonitor()
	go sysStats.Start(ctx)

	switcher := NewSwitcher(*srtAddr, *srtMode, *fallbackPath, *srtTimeout, *statsURL, *dataDir, switcherConfigPath)
	
	// Instantiate SceneManager
	var sceneManager *SceneManager
	sceneConfigPath := filepath.Join(*dataDir, "scenes.json")
	sceneManager = NewSceneManager(*dataDir, sceneConfigPath, func() {
		if sceneManager != nil {
			if active := sceneManager.GetActiveScene(); active != nil {
				switcher.UpdateFallbackPath(active.FilePath)
				switcher.RestartFallback()
			}
		}
	})

	// Set initial fallback path from active scene if it exists
	if active := sceneManager.GetActiveScene(); active != nil {
		switcher.UpdateFallbackPath(active.FilePath)
	}

	outputManager := NewOutputManager(switcher.broadcaster, configPath, *dataDir)
	preview := NewPreviewManager(switcher.broadcaster)
	go preview.Start()
	audioMeter := NewAudioMeter(switcher.broadcaster)
	go audioMeter.Start()
	
	if *bboxDir == "" {
		*bboxDir = *dataDir
	}
	bboxManager := NewBboxManager(*bboxDir)
	recorder := NewRecorder(switcher.broadcaster, *dataDir)

	// Instantiate Auto-Switch Engine (Stage 4)
	autoSwitchConfigPath := filepath.Join(*dataDir, "autoswitch.json")
	autoSwitch := NewAutoSwitchEngine(autoSwitchConfigPath, func(sceneID, transType string, transDurMs int) {
		switcher.SwitchScene(sceneID, transType, transDurMs)
	})
	// Wire data providers
	autoSwitch.SetProviders(
		func() float64 { return float64(switcher.currentBitrateKbps.Load()) },
		func() bool { return switcher.IsSRTInputActive() },
		func() float64 { return audioMeter.GetLevels().PeakL },
	)
	go autoSwitch.Run(ctx)

	// Instantiate Scheduler (Stage 4)
	schedulerConfigPath := filepath.Join(*dataDir, "scheduler.json")
	scheduler := NewScheduler(schedulerConfigPath, func(sceneID, transType string, transDurMs int) {
		switcher.SwitchScene(sceneID, transType, transDurMs)
	})
	go scheduler.Run(ctx)

	// Pass all components to APIServer
	apiServer := NewAPIServer(switcher, outputManager, sysStats, preview, audioMeter, recorder, bboxManager, sceneManager, autoSwitch, scheduler, *dataDir, *webPort, *webUser, *webPass, *tlsCert, *tlsKey)

	// Auto-start outputs that were running
	for _, oc := range outputManager.GetOutputs() {
		if oc.Running {
			log.Printf("[outputs] Auto-starting output %s on boot...", oc.Name)
			go outputManager.StartOutput(oc.ID)
		}
	}

	go func() {
		if err := switcher.Run(ctx); err != nil {
			log.Printf("Switcher error: %v", err)
		}
	}()

	go func() {
		if err := apiServer.Run(ctx); err != nil {
			log.Printf("Server stopped: %v", err)
		}
	}()

	sig := <-sigCh
	log.Printf("Received signal %v, shutting down...", sig)
	cancel()
	outputManager.StopAll()
	preview.Stop()
	audioMeter.Stop()
	if recorder.recording.Load() {
		recorder.Stop()
	}
	log.Println("Oracle Stream Studio stopped.")
}

func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
	log.Printf("Loaded env from %s", path)
}

func checkFFmpeg() error {
	path, err := findExecutable("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found in PATH")
	}
	log.Printf("FFmpeg found: %s", path)
	return nil
}

func findExecutable(name string) (string, error) {
	paths := []string{"/usr/bin/" + name, "/usr/local/bin/" + name, "/snap/bin/" + name}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found", name)
}
