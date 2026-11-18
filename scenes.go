package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Scene represents a fallback scene stored on disk and cached in RAM as a SlateClip
type Scene struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OrigPath  string `json:"orig_path,omitempty"`
	FilePath  string `json:"file_path"`
	ThumbPath string `json:"thumb_path,omitempty"`
	Type      string `json:"type"` // "image" or "video"
	Ready     bool   `json:"ready"`
	RAMBytes  int    `json:"ram_bytes"`
	Frames    int    `json:"frames"`
}

// SceneManager manages fallback scenes and keeps pre-sliced SlateClips in RAM
// for 0ms lock-free failover and instant 1-click scene changes.
type SceneManager struct {
	scenes     []Scene
	activeID   string
	resolution string // "1080p" or "720p"
	clips      map[string]*SlateClip
	activeClip atomic.Pointer[SlateClip]

	mu            sync.RWMutex
	saveMu        sync.Mutex
	configPath    string
	dataDir       string
	onSceneChange func()
}

type scenesConfig struct {
	Scenes     []Scene `json:"scenes"`
	ActiveID   string  `json:"active_id"`
	Resolution string  `json:"resolution,omitempty"`
}

func NewSceneManager(dataDir, configPath, initialResolution string, onSceneChange func()) *SceneManager {
	if initialResolution == "" {
		initialResolution = "1080p"
	}
	sm := &SceneManager{
		scenes:        make([]Scene, 0),
		clips:         make(map[string]*SlateClip),
		resolution:    initialResolution,
		configPath:    configPath,
		dataDir:       dataDir,
		onSceneChange: onSceneChange,
	}

	scenesDir := filepath.Join(dataDir, "scenes")
	os.MkdirAll(scenesDir, 0755)

	sm.loadConfig()
	if len(sm.scenes) == 0 {
		sm.discoverOrCreateDefaultScene()
	}
	sm.prepareAllScenesOnBoot()

	return sm
}

func (sm *SceneManager) scenesDir() string {
	return filepath.Join(sm.dataDir, "scenes")
}

func (sm *SceneManager) discoverOrCreateDefaultScene() {
	candidates := []string{
		filepath.Join(sm.dataDir, "fallback.jpg"),
		filepath.Join(sm.dataDir, "fallback.jpeg"),
		filepath.Join(sm.dataDir, "fallback.png"),
		filepath.Join(sm.dataDir, "..", "scripts", "queda.jpeg"),
		filepath.Join(sm.dataDir, "scripts", "queda.jpeg"),
		filepath.Join(sm.dataDir, "fallback.ts"),
		filepath.Join(sm.dataDir, "fallback.mp4"),
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			ext := strings.ToLower(filepath.Ext(p))
			sType := "video"
			if isImageExtension(ext) {
				sType = "image"
			}
			sc := Scene{
				ID:       "default",
				Name:     "Cena Padrão (Queda)",
				OrigPath: p,
				FilePath: p,
				Type:     sType,
			}
			sm.scenes = append(sm.scenes, sc)
			sm.activeID = "default"
			sm.persistConfig()
			log.Printf("[scenes] Registered default scene from %s", p)
			return
		}
	}

	// No user file found — generate fast built-in 1.6s dark slate (<0.5s)
	w, h := ParseFallbackResolution(sm.resolution)
	tsPath, thumbPath, err := EnsureBuiltInSlate(sm.scenesDir(), w, h)
	if err != nil {
		log.Printf("[scenes] Warning: could not build initial slate (ffmpeg needed at runtime): %v", err)
		return
	}
	sc := Scene{
		ID:        "default",
		Name:      "Tela Escura Padrão",
		FilePath:  tsPath,
		ThumbPath: thumbPath,
		Type:      "image",
	}
	sm.scenes = append(sm.scenes, sc)
	sm.activeID = "default"
	sm.persistConfig()
}

// prepareAllScenesOnBoot ensures every registered scene has a compiled .ts and is loaded in RAM
func (sm *SceneManager) prepareAllScenesOnBoot() {
	w, h := ParseFallbackResolution(sm.resolution)

	sm.mu.Lock()
	for i := range sm.scenes {
		sc := &sm.scenes[i]
		src := sc.OrigPath
		if src == "" {
			src = sc.FilePath
		}

		// Compile to normalized .ts if FilePath is still an image/mp4 or missing
		needCompile := !strings.HasSuffix(strings.ToLower(sc.FilePath), ".ts")
		if _, err := os.Stat(sc.FilePath); err != nil {
			needCompile = true
		}

		if needCompile && src != "" {
			if _, err := os.Stat(src); err == nil {
				outTS := filepath.Join(sm.scenesDir(), fmt.Sprintf("%s_slate.ts", sc.ID))
				outThumb := filepath.Join(sm.scenesDir(), fmt.Sprintf("%s_thumb.jpg", sc.ID))
				if err := CompileSceneSlate(src, outTS, outThumb, w, h); err != nil {
					log.Printf("[scenes] Failed to compile scene %s (%s): %v", sc.Name, src, err)
				} else {
					sc.FilePath = outTS
					if _, tErr := os.Stat(outThumb); tErr == nil {
						sc.ThumbPath = outThumb
					}
				}
			}
		} else if sc.ThumbPath == "" && src != "" {
			outThumb := filepath.Join(sm.scenesDir(), fmt.Sprintf("%s_thumb.jpg", sc.ID))
			if _, err := os.Stat(outThumb); err == nil {
				sc.ThumbPath = outThumb
			}
		}

		if clip, err := LoadSlateClipFromFile(sc.ID, sc.FilePath, w, h); err == nil {
			sm.clips[sc.ID] = clip
			sc.Ready = true
			sc.RAMBytes = clip.RawBytes
			sc.Frames = len(clip.Frames)
			log.Printf("[scenes] Loaded '%s' into RAM (%d frames, %.1f KB)", sc.Name, len(clip.Frames), float64(clip.RawBytes)/1024.0)
		} else {
			sc.Ready = false
		}
	}

	if sm.activeID == "" && len(sm.scenes) > 0 {
		sm.activeID = sm.scenes[0].ID
	}
	if clip, ok := sm.clips[sm.activeID]; ok {
		sm.activeClip.Store(clip)
	}
	sm.mu.Unlock()
	sm.persistConfig()
}

// GetActiveClip returns the currently active RAM SlateClip via lock-free atomic load
func (sm *SceneManager) GetActiveClip() *SlateClip {
	return sm.activeClip.Load()
}

func (sm *SceneManager) GetResolution() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.resolution
}

// SetResolution updates the target fallback resolution ("1080p" or "720p") and recompiles scenes in background
func (sm *SceneManager) SetResolution(res string) {
	if res != "720p" {
		res = "1080p"
	}
	sm.mu.Lock()
	if sm.resolution == res {
		sm.mu.Unlock()
		return
	}
	sm.resolution = res
	scenesCopy := make([]Scene, len(sm.scenes))
	copy(scenesCopy, sm.scenes)
	sm.mu.Unlock()
	sm.persistConfig()

	go func() {
		w, h := ParseFallbackResolution(res)
		log.Printf("[scenes] Recompiling %d scenes for resolution %s (%dx%d)...", len(scenesCopy), res, w, h)
		for _, sc := range scenesCopy {
			src := sc.OrigPath
			if src == "" {
				src = sc.FilePath
			}
			if src == "" {
				continue
			}
			if _, err := os.Stat(src); err != nil {
				if sc.ID == "default" {
					tsPath, thumbPath, bErr := EnsureBuiltInSlate(sm.scenesDir(), w, h)
					if bErr == nil {
						sm.reloadSceneClip(sc.ID, tsPath, thumbPath, w, h)
					}
				}
				continue
			}
			outTS := filepath.Join(sm.scenesDir(), fmt.Sprintf("%s_slate.ts", sc.ID))
			outThumb := filepath.Join(sm.scenesDir(), fmt.Sprintf("%s_thumb.jpg", sc.ID))
			if err := CompileSceneSlate(src, outTS, outThumb, w, h); err != nil {
				log.Printf("[scenes] Recompile error for %s: %v", sc.Name, err)
				continue
			}
			sm.reloadSceneClip(sc.ID, outTS, outThumb, w, h)
		}
	}()
}

func (sm *SceneManager) reloadSceneClip(id, tsPath, thumbPath string, w, h int) {
	clip, err := LoadSlateClipFromFile(id, tsPath, w, h)
	if err != nil {
		log.Printf("[scenes] Failed to load compiled clip for %s: %v", id, err)
		return
	}

	var notify bool
	sm.mu.Lock()
	sm.clips[id] = clip
	for i := range sm.scenes {
		if sm.scenes[i].ID == id {
			sm.scenes[i].FilePath = tsPath
			if thumbPath != "" {
				if _, sErr := os.Stat(thumbPath); sErr == nil {
					sm.scenes[i].ThumbPath = thumbPath
				}
			}
			sm.scenes[i].Ready = true
			sm.scenes[i].RAMBytes = clip.RawBytes
			sm.scenes[i].Frames = len(clip.Frames)
			break
		}
	}
	if sm.activeID == id {
		sm.activeClip.Store(clip)
		notify = true
	}
	sm.mu.Unlock()
	sm.persistConfig()

	if notify && sm.onSceneChange != nil {
		sm.onSceneChange()
	}
}

func (sm *SceneManager) GetScenes() []Scene {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	scenes := make([]Scene, len(sm.scenes))
	copy(scenes, sm.scenes)
	return scenes
}

func (sm *SceneManager) GetSceneByID(id string) (Scene, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, s := range sm.scenes {
		if s.ID == id {
			return s, true
		}
	}
	return Scene{}, false
}

func (sm *SceneManager) GetActiveID() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.activeID
}

func (sm *SceneManager) GetActiveScene() *Scene {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, s := range sm.scenes {
		if s.ID == sm.activeID {
			sc := s
			return &sc
		}
	}
	if len(sm.scenes) > 0 {
		sc := sm.scenes[0]
		return &sc
	}
	return nil
}

// AddAndCompileScene compiles an uploaded image/video synchronously (~0.5s for images),
// loads its frames into RAM, and optionally activates it immediately.
func (sm *SceneManager) AddAndCompileScene(name, uploadedPath string, activateNow bool) (Scene, error) {
	ext := strings.ToLower(filepath.Ext(uploadedPath))
	sceneType := "video"
	if isImageExtension(ext) {
		sceneType = "image"
	}

	id := fmt.Sprintf("scene_%d", time.Now().UnixMilli())
	outTS := filepath.Join(sm.scenesDir(), fmt.Sprintf("%s_slate.ts", id))
	outThumb := filepath.Join(sm.scenesDir(), fmt.Sprintf("%s_thumb.jpg", id))

	sm.mu.RLock()
	res := sm.resolution
	sm.mu.RUnlock()
	w, h := ParseFallbackResolution(res)

	if err := CompileSceneSlate(uploadedPath, outTS, outThumb, w, h); err != nil {
		return Scene{}, err
	}

	clip, err := LoadSlateClipFromFile(id, outTS, w, h)
	if err != nil {
		return Scene{}, err
	}

	thumbFinal := ""
	if _, err := os.Stat(outThumb); err == nil {
		thumbFinal = outThumb
	}

	scene := Scene{
		ID:        id,
		Name:      name,
		OrigPath:  uploadedPath,
		FilePath:  outTS,
		ThumbPath: thumbFinal,
		Type:      sceneType,
		Ready:     true,
		RAMBytes:  clip.RawBytes,
		Frames:    len(clip.Frames),
	}

	var notify bool
	sm.mu.Lock()
	sm.scenes = append(sm.scenes, scene)
	sm.clips[id] = clip
	if len(sm.scenes) == 1 || activateNow {
		sm.activeID = id
		sm.activeClip.Store(clip)
		notify = true
	}
	sm.mu.Unlock()
	sm.persistConfig()

	log.Printf("[scenes] Added & loaded RAM scene '%s' (%s): %d frames, %.1f KB", name, id, scene.Frames, float64(scene.RAMBytes)/1024.0)
	if notify && sm.onSceneChange != nil {
		sm.onSceneChange()
	}
	return scene, nil
}

func (sm *SceneManager) RemoveScene(id string) error {
	sm.mu.Lock()
	if len(sm.scenes) <= 1 {
		sm.mu.Unlock()
		return fmt.Errorf("cannot delete the only remaining fallback scene")
	}

	idx := -1
	var removed Scene
	for i, s := range sm.scenes {
		if s.ID == id {
			idx = i
			removed = s
			break
		}
	}
	if idx == -1 {
		sm.mu.Unlock()
		return fmt.Errorf("scene %s not found", id)
	}

	sm.scenes = append(sm.scenes[:idx], sm.scenes[idx+1:]...)
	delete(sm.clips, id)

	var switched bool
	if sm.activeID == id && len(sm.scenes) > 0 {
		sm.activeID = sm.scenes[0].ID
		if nextClip, ok := sm.clips[sm.activeID]; ok {
			sm.activeClip.Store(nextClip)
		}
		switched = true
	}
	sm.mu.Unlock()
	sm.persistConfig()

	// Clean up files on disk outside lock
	for _, p := range []string{removed.FilePath, removed.OrigPath, removed.ThumbPath} {
		if p != "" {
			_ = os.Remove(p)
		}
	}

	if switched && sm.onSceneChange != nil {
		sm.onSceneChange()
	}
	return nil
}

func (sm *SceneManager) ActivateScene(id string) error {
	sm.mu.Lock()
	found := false
	for _, s := range sm.scenes {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		sm.mu.Unlock()
		return fmt.Errorf("scene %s not found", id)
	}

	clip, ok := sm.clips[id]
	if !ok {
		sm.mu.Unlock()
		return fmt.Errorf("scene %s is still compiling or unavailable", id)
	}

	sm.activeID = id
	sm.activeClip.Store(clip)
	sm.mu.Unlock()
	sm.persistConfig()

	log.Printf("[scenes] Instant RAM switch to scene: %s", id)
	if sm.onSceneChange != nil {
		sm.onSceneChange()
	}
	return nil
}

// persistConfig serializes and writes scenes.json outside sm.mu so disk I/O never stalls 30fps readers.
func (sm *SceneManager) persistConfig() {
	sm.saveMu.Lock()
	defer sm.saveMu.Unlock()

	sm.mu.RLock()
	scenesCopy := make([]Scene, len(sm.scenes))
	copy(scenesCopy, sm.scenes)
	cfg := scenesConfig{
		Scenes:     scenesCopy,
		ActiveID:   sm.activeID,
		Resolution: sm.resolution,
	}
	sm.mu.RUnlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("[scenes] Save error: %v", err)
		return
	}
	_ = os.WriteFile(sm.configPath, data, 0644)
}

func (sm *SceneManager) loadConfig() {
	data, err := os.ReadFile(sm.configPath)
	if err != nil {
		return
	}
	var cfg scenesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("[scenes] Parse error: %v", err)
		return
	}
	sm.scenes = cfg.Scenes
	sm.activeID = cfg.ActiveID
	if cfg.Resolution != "" {
		sm.resolution = cfg.Resolution
	}
	log.Printf("[scenes] Loaded %d scenes (active: %s, res: %s)", len(sm.scenes), sm.activeID, sm.resolution)
}
