package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web/*
var webFS embed.FS

var serverStartTime = time.Now()

type APIServer struct {
	switcher      *Switcher
	outputManager *OutputManager
	sysStats      *SysStatsMonitor
	preview       *PreviewManager
	audioMeter    *AudioMeter
	recorder      *Recorder
	bboxManager   *BboxManager
	sceneManager  *SceneManager
	autoSwitch    *AutoSwitchEngine
	scheduler     *Scheduler
	dataDir       string
	port          int
	webUser       string
	webPass       string
	tlsCert       string
	tlsKey        string

	upgrader  websocket.Upgrader
	clients   map[*websocket.Conn]bool
	clientsMu sync.Mutex
}

func NewAPIServer(switcher *Switcher, outputManager *OutputManager, sysStats *SysStatsMonitor, preview *PreviewManager, audioMeter *AudioMeter, recorder *Recorder, bboxManager *BboxManager, sceneManager *SceneManager, autoSwitch *AutoSwitchEngine, scheduler *Scheduler, dataDir string, port int, webUser string, webPass string, tlsCert string, tlsKey string) *APIServer {
	return &APIServer{
		switcher:      switcher,
		outputManager: outputManager,
		sysStats:      sysStats,
		preview:       preview,
		audioMeter:    audioMeter,
		recorder:      recorder,
		bboxManager:   bboxManager,
		sceneManager:  sceneManager,
		autoSwitch:    autoSwitch,
		scheduler:     scheduler,
		dataDir:       dataDir,
		port:          port,
		webUser:       webUser,
		webPass:       webPass,
		tlsCert:       tlsCert,
		tlsKey:        tlsKey,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				host := r.Host
				if strings.Contains(origin, host) || strings.Contains(origin, "localhost") || strings.Contains(origin, "127.0.0.1") {
					return true
				}
				return false
			},
		},
		clients:       make(map[*websocket.Conn]bool),
	}
}

func (s *APIServer) basicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.webUser == "" && s.webPass == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.webUser || pass != s.webPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── CORS Middleware ───

func (s *APIServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			// Validate Origin (Allow same-host or localhost for dev)
			host := r.Host
			if strings.Contains(origin, host) || strings.Contains(origin, "localhost") || strings.Contains(origin, "127.0.0.1") {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Max-Age", "3600")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Rate Limiter ───

type rateLimiter struct {
	mu       sync.Mutex
	clients  map[string]*rateBucket
	rate     int           // max requests per window
	window   time.Duration
}

type rateBucket struct {
	count    int
	resetAt  time.Time
}

func newRateLimiter(rate int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		clients: make(map[string]*rateBucket),
		rate:    rate,
		window:  window,
	}
	// Cleanup stale entries every 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rl.mu.Lock()
			now := time.Now()
			for ip, b := range rl.clients {
				if now.After(b.resetAt) {
					delete(rl.clients, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, exists := rl.clients[ip]
	if !exists || now.After(b.resetAt) {
		rl.clients[ip] = &rateBucket{count: 1, resetAt: now.Add(rl.window)}
		return true
	}
	if b.count >= rl.rate {
		return false
	}
	b.count++
	return true
}

func (s *APIServer) rateLimitMiddleware(rl *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip rate limiting for static files and websocket
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		// Skip preview frames (polled frequently by design)
		if r.URL.Path == "/api/preview/frame" {
			next.ServeHTTP(w, r)
			return
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip == "" {
			ip = r.RemoteAddr
		}
		if !rl.allow(ip) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			log.Printf("[server] Rate limit hit: %s %s from %s", r.Method, r.URL.Path, ip)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Body Size Limit Middleware ───

const maxBodySize = 10 << 20 // 10 MB default

func bodySizeLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxBodySize)
		if strings.HasPrefix(r.URL.Path, "/api/upload/") {
			limit = 100 << 20 // 100 MB for file uploads
		}
		if r.Body != nil && r.ContentLength > limit {
			http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		// Also wrap the body reader to enforce the limit even if Content-Length is missing/lying
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Security Headers Middleware ───

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self' https://fonts.googleapis.com https://fonts.gstatic.com https://cdn.jsdelivr.net; img-src 'self' data:; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdn.jsdelivr.net; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net;")
		next.ServeHTTP(w, r)
	})
}

// checkDiskSpace verifies if the volume containing path has at least minFreeBytes available space
func checkDiskSpace(path string, minFreeBytes uint64) error {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return err
	}
	freeBytes := uint64(stat.Bavail) * uint64(stat.Bsize)
	if freeBytes < minFreeBytes {
		return fmt.Errorf("insufficient disk space: only %d MB available, need at least %d MB", freeBytes/(1024*1024), minFreeBytes/(1024*1024))
	}
	return nil
}

type StatusResponse struct {
	System    SystemStats     `json:"system"`
	Switcher  SwitcherStats   `json:"switcher"`
	Outputs   []OutputStats   `json:"outputs"`
	Audio     AudioLevels     `json:"audio"`
	Recording RecordingStatus `json:"recording"`
}

func (s *APIServer) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/outputs", s.handleOutputs)
	mux.HandleFunc("/api/outputs/", s.handleOutputAction)
	mux.HandleFunc("POST /api/upload/fallback", s.handleUploadFallback)
	mux.HandleFunc("POST /api/upload/watermark", s.handleUploadWatermark)
	mux.HandleFunc("/api/preview/frame", s.handlePreviewFrame)
	mux.HandleFunc("/api/preview/settings", s.handlePreviewSettings)
	mux.HandleFunc("/api/actions/", s.handleQuickAction)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("/api/recording", s.handleRecording)
	mux.HandleFunc("/api/recordings", s.handleRecordings)
	mux.HandleFunc("/api/recordings/", s.handleRecordingAction)
	
	// Bbox routes
	mux.HandleFunc("/api/bbox/status", s.handleBboxStatus)
	mux.HandleFunc("/api/bbox/action", s.handleBboxAction)
	mux.HandleFunc("/api/bbox/logs", s.handleBboxLogs)
	mux.HandleFunc("/api/bbox/config", s.handleBboxConfig)
	mux.HandleFunc("/api/bbox/compose", s.handleBboxCompose)

	// Scene Manager Routes
	mux.HandleFunc("GET /api/scenes", s.handleGetScenes)
	mux.HandleFunc("POST /api/scenes/activate", s.handleActivateScene)
	mux.HandleFunc("POST /api/scenes/delete", s.handleDeleteScene)
	mux.HandleFunc("POST /api/upload/scene", s.handleUploadScene)

	// Advanced Switching Routes (Stages 1-4)
	mux.HandleFunc("GET /api/pipelines", s.handleGetPipelines)
	mux.HandleFunc("GET /api/transitions/types", s.handleGetTransitionTypes)
	mux.HandleFunc("/api/transitions/config", s.handleTransitionConfig)
	mux.HandleFunc("/api/autoswitch/rules", s.handleAutoSwitchRules)
	mux.HandleFunc("/api/autoswitch/rules/", s.handleAutoSwitchRuleAction)
	mux.HandleFunc("GET /api/autoswitch/events", s.handleAutoSwitchEvents)
	mux.HandleFunc("/api/schedule", s.handleSchedules)
	mux.HandleFunc("/api/schedule/", s.handleScheduleAction)
	mux.HandleFunc("GET /api/schedule/events", s.handleScheduleEvents)

	// Health Check Route
	mux.HandleFunc("GET /api/health", s.handleHealth)

	mux.HandleFunc("/ws", s.handleWebSocket)

	webContent, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("embed fs: %w", err)
	}
	fileServer := http.FileServer(http.FS(webContent))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		fileServer.ServeHTTP(w, r)
	}))

	go s.broadcastLoop()

	// Build middleware chain: logging → bodyLimit → rateLimit → CORS → basicAuth → mux (Stage 5)
	rl := newRateLimiter(60, 1*time.Second) // 60 API requests per second per IP
	var handler http.Handler = mux
	handler = s.basicAuthMiddleware(handler)
	handler = s.corsMiddleware(handler)
	handler = s.rateLimitMiddleware(rl, handler)
	handler = bodySizeLimitMiddleware(handler)
	handler = securityHeadersMiddleware(handler)
	handler = loggingMiddleware(handler)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: handler,
	}

	if s.tlsCert != "" && s.tlsKey != "" {
		if s.webUser != "" {
			log.Printf("[server] Web UI starting secure at https://0.0.0.0:%d (Protected with Basic Auth)", s.port)
		} else {
			log.Printf("[server] Web UI starting secure at https://0.0.0.0:%d (WARNING: Open access, no auth!)", s.port)
		}
		go func() {
			if err := srv.ListenAndServeTLS(s.tlsCert, s.tlsKey); err != nil && err != http.ErrServerClosed {
				log.Printf("Server ListenAndServeTLS error: %v", err)
			}
		}()
	} else {
		if s.webUser != "" {
			log.Printf("[server] Web UI starting at http://0.0.0.0:%d (Protected with Basic Auth)", s.port)
		} else {
			log.Printf("[server] Web UI starting at http://0.0.0.0:%d (WARNING: Open access, no auth!)", s.port)
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Server ListenAndServe error: %v", err)
			}
		}()
	}

	log.Printf("[server] Security: CORS enabled, rate limit 60/s, body limit 10MB, logging active")

	<-ctx.Done()
	log.Println("[server] Shutting down HTTP server graciosamente...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// ─── Status ───

func (s *APIServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := StatusResponse{
		System:    s.sysStats.GetStats(),
		Switcher:  s.switcher.GetStats(),
		Outputs:   s.outputManager.GetStats(),
		Audio:     s.audioMeter.GetLevels(),
		Recording: s.recorder.GetStatus(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ─── Config ───

func (s *APIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.switcher.GetConfig())
	case http.MethodPut:
		var cfg SwitcherConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		s.switcher.UpdateConfig(cfg)
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Outputs ───

func (s *APIServer) handleOutputs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.outputManager.GetOutputs())
	case http.MethodPost:
		var config OutputConfig
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if config.ID == "" {
			config.ID = fmt.Sprintf("out_%d", time.Now().UnixMilli())
		}
		if config.Name == "" {
			config.Name = config.ID
		}
		if config.Codec == "" {
			config.Codec = CodecH265Passthrough
		}
		if err := s.outputManager.AddOutput(config); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(config)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleOutputAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path[len("/api/outputs/"):]
	id := path
	action := ""
	for _, a := range []string{"/start", "/stop", "/logs"} {
		if len(path) > len(a) && path[len(path)-len(a):] == a {
			id = path[:len(path)-len(a)]
			action = a[1:]
			break
		}
	}
	switch {
	case action == "start" && r.Method == http.MethodPost:
		if err := s.outputManager.StartOutput(id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "started"})
	case action == "stop" && r.Method == http.MethodPost:
		if err := s.outputManager.StopOutput(id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
	case action == "logs" && r.Method == http.MethodGet:
		logs, err := s.outputManager.GetLogs(id)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"logs": logs})
	case action == "" && r.Method == http.MethodPut:
		var config OutputConfig
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.outputManager.UpdateOutput(id, config); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	case action == "" && r.Method == http.MethodDelete:
		if err := s.outputManager.RemoveOutput(id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Quick Actions ───

func (s *APIServer) handleQuickAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	action := r.URL.Path[len("/api/actions/"):]
	switch action {
	case "restart-srt":
		s.switcher.RestartSRT()
		json.NewEncoder(w).Encode(map[string]string{"status": "SRT restart requested"})
	case "restart-fallback":
		s.switcher.RestartFallback()
		json.NewEncoder(w).Encode(map[string]string{"status": "Fallback restart requested"})
	case "restart-outputs":
		s.outputManager.RestartAll()
		json.NewEncoder(w).Encode(map[string]string{"status": "All outputs restarted"})
	case "restart-all":
		s.switcher.RestartSRT()
		s.switcher.RestartFallback()
		s.outputManager.RestartAll()
		json.NewEncoder(w).Encode(map[string]string{"status": "Full restart requested"})
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "unknown action"})
	}
}

// ─── History ───

func (s *APIServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"events": s.switcher.GetSwitchHistory()})
}

// ─── Recording ───

func (s *APIServer) handleRecording(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.recorder.GetStatus())
	case http.MethodPost:
		action := r.URL.Query().Get("action")
		switch action {
		case "start":
			if err := s.recorder.Start(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "recording started"})
		case "stop":
			if err := s.recorder.Stop(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "recording stopped"})
		default:
			http.Error(w, "use ?action=start or ?action=stop", http.StatusBadRequest)
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleRecordings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"recordings": s.recorder.ListRecordings()})
}

func (s *APIServer) handleRecordingAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	rawName := r.URL.Path[len("/api/recordings/"):]
	name := filepath.Base(rawName) // Sanitize: prevent path traversal (../)
	if name == "." || name == "/" || name == "" {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Download file
		filePath := filepath.Join(s.recorder.recordDir, name)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeFile(w, r, filePath)

	case http.MethodDelete:
		// Delete file
		filePath := filepath.Join(s.recorder.recordDir, name)
		if err := os.Remove(filePath); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})

	case http.MethodPut:
		// Rename file
		var body struct {
			NewName string `json:"new_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.NewName == "" {
			http.Error(w, "provide new_name", http.StatusBadRequest)
			return
		}
		oldPath := filepath.Join(s.recorder.recordDir, name)
		newPath := filepath.Join(s.recorder.recordDir, body.NewName)
		if err := os.Rename(oldPath, newPath); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "renamed"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── WebSocket ───

type WSMessage struct {
	Type     string `json:"type"`
	OutputID string `json:"output_id,omitempty"`
	X        int    `json:"x,omitempty"`
	Y        int    `json:"y,omitempty"`
}

func (s *APIServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.clientsMu.Lock()
	s.clients[conn] = true
	s.clientsMu.Unlock()
	
	conn.SetReadLimit(4096) // Limit WebSocket message size to 4KB to prevent DoS memory exhaustion

	go func() {
		defer func() {
			s.clientsMu.Lock()
			delete(s.clients, conn)
			s.clientsMu.Unlock()
			conn.Close()
		}()
		
		msgCount := 0
		lastCheck := time.Now()
		
		for {
			_, msgBytes, err := conn.ReadMessage()
			if err != nil {
				return
			}
			
			// Rate limit incoming messages (max 30 messages/sec) to prevent overlay drag spam DoS
			now := time.Now()
			if now.Sub(lastCheck) >= 1*time.Second {
				msgCount = 0
				lastCheck = now
			}
			msgCount++
			if msgCount > 30 {
				time.Sleep(50 * time.Millisecond) // Throttle the connection
			}
			
			var msg WSMessage
			if err := json.Unmarshal(msgBytes, &msg); err != nil {
				continue
			}
			if msg.Type == "overlay_move" {
				if err := s.outputManager.MoveOverlay(msg.OutputID, msg.X, msg.Y); err != nil {
					log.Printf("[server] Failed to move overlay: %v", err)
				}
			}
		}
	}()
}

func (s *APIServer) broadcastLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		// Snapshot client list under lock
		s.clientsMu.Lock()
		if len(s.clients) == 0 {
			s.clientsMu.Unlock()
			continue
		}
		conns := make([]*websocket.Conn, 0, len(s.clients))
		for conn := range s.clients {
			conns = append(conns, conn)
		}
		s.clientsMu.Unlock()

		// Build status data outside the lock
		resp := StatusResponse{
			System:    s.sysStats.GetStats(),
			Switcher:  s.switcher.GetStats(),
			Outputs:   s.outputManager.GetStats(),
			Audio:     s.audioMeter.GetLevels(),
			Recording: s.recorder.GetStatus(),
		}
		data, err := json.Marshal(resp)
		if err != nil {
			continue
		}

		// Write to each client without holding the lock
		var failed []*websocket.Conn
		for _, conn := range conns {
			conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				failed = append(failed, conn)
			}
		}

		// Remove failed clients
		if len(failed) > 0 {
			s.clientsMu.Lock()
			for _, conn := range failed {
				conn.Close()
				delete(s.clients, conn)
			}
			s.clientsMu.Unlock()
		}
	}
}

// ─── Uploads ───

func (s *APIServer) handleUploadFallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := checkDiskSpace(s.dataDir, 500*1024*1024); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInsufficientStorage)
		w.Write([]byte(fmt.Sprintf(`{"error":"%v"}`, err)))
		return
	}
	r.ParseMultipartForm(50 << 20)
	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(handler.Filename))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".ts" && ext != ".mp4" {
		http.Error(w, "Invalid file type", http.StatusBadRequest)
		return
	}
	destPath := filepath.Join(s.dataDir, "fallback"+ext)
	dest, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Error creating file", http.StatusInternalServerError)
		return
	}
	defer dest.Close()
	io.Copy(dest, file)

	// For images: pre-render to .ts in background to avoid continuous libx265 encoding
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
		prerenderedPath := filepath.Join(s.dataDir, "fallback_prerendered.ts")
		go func() {
			log.Printf("[server] Pre-rendering image fallback: %s → %s", destPath, prerenderedPath)
			cmd := exec.Command("ffmpeg",
				"-hide_banner", "-loglevel", "error", "-y",
				"-loop", "1", "-framerate", "30",
				"-i", destPath,
				"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
				"-c:v", "libx265", "-preset", "ultrafast",
				"-x265-params", "keyint=60:min-keyint=60",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac", "-b:a", "128k",
				"-t", "10", // 10-second loop file
				"-f", "mpegts", prerenderedPath,
			)
			if err := cmd.Run(); err != nil {
				log.Printf("[server] Pre-render failed: %v (using original image)", err)
				s.switcher.UpdateFallbackPath(destPath)
			} else {
				log.Printf("[server] Pre-render complete: %s", prerenderedPath)
				s.switcher.UpdateFallbackPath(prerenderedPath)
			}
			s.switcher.RestartFallback()
		}()
	} else {
		s.switcher.UpdateFallbackPath(destPath)
		s.switcher.RestartFallback()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success":true}`))
}

func (s *APIServer) handleUploadWatermark(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := checkDiskSpace(s.dataDir, 500*1024*1024); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInsufficientStorage)
		w.Write([]byte(fmt.Sprintf(`{"error":"%v"}`, err)))
		return
	}
	r.ParseMultipartForm(10 << 20)
	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if strings.ToLower(filepath.Ext(handler.Filename)) != ".png" {
		http.Error(w, "Watermark must be PNG", http.StatusBadRequest)
		return
	}
	destPath := filepath.Join(s.dataDir, "watermark.png")
	dest, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Error creating file", http.StatusInternalServerError)
		return
	}
	defer dest.Close()
	io.Copy(dest, file)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success":true}`))
}

// ─── Preview ───

func (s *APIServer) handlePreviewFrame(w http.ResponseWriter, r *http.Request) {
	frame := s.preview.GetFrame()
	if frame == nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Write(frame)
}

func (s *APIServer) handlePreviewSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		fps, width, height := s.preview.GetSettings()
		json.NewEncoder(w).Encode(map[string]int{"fps": fps, "width": width, "height": height})
	case http.MethodPut:
		fps, _ := strconv.Atoi(r.URL.Query().Get("fps"))
		width, _ := strconv.Atoi(r.URL.Query().Get("w"))
		height, _ := strconv.Atoi(r.URL.Query().Get("h"))
		if fps < 0 {
			fps = 0
		}
		if width <= 0 {
			width = 640
		}
		if height <= 0 {
			height = 360
		}
		s.preview.UpdateSettings(fps, width, height)
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Bbox ───

func (s *APIServer) handleBboxStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.bboxManager.GetStatus()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func (s *APIServer) handleBboxAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	action := r.URL.Query().Get("action")
	var err error
	switch action {
	case "start":
		err = s.bboxManager.Start()
	case "stop":
		err = s.bboxManager.Stop()
	case "restart":
		err = s.bboxManager.Restart()
	default:
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

func (s *APIServer) handleBboxLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := s.bboxManager.GetLogs(100)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"logs": logs})
}

func (s *APIServer) handleBboxConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		config, err := s.bboxManager.ReadConfig()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]string{"content": "{}"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"content": config})
	} else if r.Method == http.MethodPut {
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		// Validate JSON before saving
		if !json.Valid([]byte(req.Content)) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON content"})
			return
		}
		if err := s.bboxManager.WriteConfig(req.Content); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleBboxCompose(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		config, err := s.bboxManager.ReadCompose()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]string{"content": ""})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"content": config})
	} else if r.Method == http.MethodPut {
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		// Basic YAML validation (no yaml library needed)
		trimmed := strings.TrimSpace(req.Content)
		if len(trimmed) < 10 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "compose content too short or empty"})
			return
		}
		if !strings.Contains(trimmed, "services") {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid compose file: missing 'services' section"})
			return
		}
		if err := s.bboxManager.WriteCompose(req.Content); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Scene Manager Handlers (Stage 4) ───

func (s *APIServer) handleGetScenes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	scenes := s.sceneManager.GetScenes()
	activeID := s.sceneManager.GetActiveID()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"scenes":    scenes,
		"active_id": activeID,
	})
}

func (s *APIServer) handleActivateScene(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	if err := s.sceneManager.ActivateScene(req.ID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "success", "active_id": req.ID})
}

func (s *APIServer) handleDeleteScene(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	if req.ID == "default" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Cannot delete default scene"})
		return
	}

	var filePath string
	for _, sc := range s.sceneManager.GetScenes() {
		if sc.ID == req.ID {
			filePath = sc.FilePath
			break
		}
	}

	if err := s.sceneManager.RemoveScene(req.ID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if filePath != "" {
		// Clean up files on disk
		os.Remove(filePath)
		if strings.HasSuffix(filePath, "_prerendered.ts") {
			orig := strings.TrimSuffix(filePath, "_prerendered.ts")
			os.Remove(orig + ".jpg")
			os.Remove(orig + ".jpeg")
			os.Remove(orig + ".png")
		}
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

func (s *APIServer) handleUploadScene(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	if err := checkDiskSpace(s.dataDir, 500*1024*1024); err != nil {
		w.WriteHeader(http.StatusInsufficientStorage)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	
	r.ParseMultipartForm(50 << 20) // 50MB max upload
	file, handler, err := r.FormFile("file")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Error retrieving file"})
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(handler.Filename))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".ts" && ext != ".mp4" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid file type: only jpg, png, mp4, ts supported"})
		return
	}

	sceneName := r.FormValue("name")
	if sceneName == "" {
		sceneName = strings.TrimSuffix(handler.Filename, ext)
	}

	// Ensure scenes subdirectory exists
	scenesDir := filepath.Join(s.dataDir, "scenes")
	os.MkdirAll(scenesDir, 0755)

	stamp := fmt.Sprintf("%d", time.Now().UnixMilli())
	safeFilename := fmt.Sprintf("scene_%s%s", stamp, ext)
	destPath := filepath.Join(scenesDir, safeFilename)

	dest, err := os.Create(destPath)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to create target file"})
		return
	}
	defer dest.Close()
	io.Copy(dest, file)

	// In case of image: pre-render to .ts in background (Stage 4)
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
		prerenderedPath := filepath.Join(scenesDir, fmt.Sprintf("scene_%s_prerendered.ts", stamp))
		
		// Instantly register scene pointing to original image, then update to .ts when finished
		scene, _ := s.sceneManager.AddScene(sceneName, destPath)
		
		go func() {
			log.Printf("[scenes] Pre-rendering image scene: %s → %s", destPath, prerenderedPath)
			cmd := exec.Command("ffmpeg",
				"-hide_banner", "-loglevel", "error", "-y",
				"-loop", "1", "-framerate", "30",
				"-i", destPath,
				"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
				"-c:v", "libx265", "-preset", "ultrafast",
				"-x265-params", "keyint=60:min-keyint=60",
				"-pix_fmt", "yuv420p",
				"-c:a", "aac", "-b:a", "128k",
				"-t", "10",
				"-f", "mpegts", prerenderedPath,
			)
			if err := cmd.Run(); err != nil {
				log.Printf("[scenes] Pre-rendering scene image failed: %v", err)
			} else {
				log.Printf("[scenes] Pre-rendering scene image complete: %s", prerenderedPath)
				
				s.sceneManager.mu.Lock()
				for i, sc := range s.sceneManager.scenes {
					if sc.ID == scene.ID {
						s.sceneManager.scenes[i].FilePath = prerenderedPath
						break
					}
				}
				s.sceneManager.saveConfig()
				s.sceneManager.mu.Unlock()
				
				// Restart fallback if this is the active scene
				if s.sceneManager.GetActiveID() == scene.ID {
					s.switcher.UpdateFallbackPath(prerenderedPath)
					s.switcher.RestartFallback()
				}
			}
		}()
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "scene": scene})
	} else {
		// Video scene
		scene, err := s.sceneManager.AddScene(sceneName, destPath)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "scene": scene})
	}
}

// ─── Advanced Switching Handlers (Stages 1-4) ───

func (s *APIServer) handleGetPipelines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pipelines": s.switcher.router.GetPipelineStats(),
	})
}

func (s *APIServer) handleGetTransitionTypes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"transitions": GetAvailableTransitions(),
	})
}

func (s *APIServer) handleTransitionConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.switcher.transition.GetConfig())
	case http.MethodPut:
		var cfg TransitionConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		s.switcher.transition.SetConfig(cfg)
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAutoSwitchRules(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"rules": s.autoSwitch.GetRules(),
		})
	case http.MethodPost:
		var rule SwitchRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		s.autoSwitch.AddRule(rule)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"status": "created", "id": rule.ID})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAutoSwitchRuleAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := r.URL.Path[len("/api/autoswitch/rules/"):]

	switch r.Method {
	case http.MethodPut:
		var rule SwitchRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.autoSwitch.UpdateRule(id, rule); err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	case http.MethodDelete:
		if err := s.autoSwitch.RemoveRule(id); err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	case http.MethodPost:
		// Toggle enable/disable
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.autoSwitch.ToggleRule(id, body.Enabled); err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "toggled"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAutoSwitchEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"events": s.autoSwitch.GetEvents(),
	})
}

func (s *APIServer) handleSchedules(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"schedules": s.scheduler.GetSchedules(),
		})
	case http.MethodPost:
		var sched ScheduledSwitch
		if err := json.NewDecoder(r.Body).Decode(&sched); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		s.scheduler.AddSchedule(sched)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"status": "created", "id": sched.ID})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleScheduleAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := r.URL.Path[len("/api/schedule/"):]
	if id == "events" {
		// Handled by separate route
		return
	}

	switch r.Method {
	case http.MethodPut:
		var sched ScheduledSwitch
		if err := json.NewDecoder(r.Body).Decode(&sched); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.scheduler.UpdateSchedule(id, sched); err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	case http.MethodDelete:
		if err := s.scheduler.RemoveSchedule(id); err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	case http.MethodPost:
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.scheduler.ToggleSchedule(id, body.Enabled); err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "toggled"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleScheduleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"events": s.scheduler.GetEvents(),
	})
}

// ─── Health Check Handler ───

func (s *APIServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "healthy",
		"uptime":    time.Since(serverStartTime).Truncate(time.Second).String(),
		"timestamp": time.Now().Unix(),
		"version":   "4.0",
	})
}

// ─── Logging Middleware (Stage 5) ───

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)
		log.Printf("[http] %s %s from %s - Status %d - Took %s", r.Method, r.URL.Path, r.RemoteAddr, lrw.statusCode, time.Since(start).Truncate(time.Microsecond))
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func (lrw *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := lrw.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("response writer does not implement http.Hijacker")
}
