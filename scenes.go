package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Scene represents a fallback scene
type Scene struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	FilePath string `json:"file_path"`
	Type     string `json:"type"` // "image" or "video"
}

// SceneManager manages multiple fallback scenes
type SceneManager struct {
	scenes     []Scene
	activeID   string
	mu         sync.RWMutex
	configPath string
	dataDir    string

	// Called when active scene changes
	onSceneChange func()
}

func NewSceneManager(dataDir, configPath string, onSceneChange func()) *SceneManager {
	sm := &SceneManager{
		scenes:        make([]Scene, 0),
		configPath:    configPath,
		dataDir:       dataDir,
		onSceneChange: onSceneChange,
	}

	// Ensure scenes directory exists
	scenesDir := filepath.Join(dataDir, "scenes")
	os.MkdirAll(scenesDir, 0755)

	sm.loadConfig()

	// If no scenes exist, create a default one from the fallback file
	if len(sm.scenes) == 0 {
		sm.createDefaultScene(dataDir)
	}

	return sm
}

func (sm *SceneManager) createDefaultScene(dataDir string) {
	// Look for existing fallback files
	for _, name := range []string{"fallback.ts", "fallback.mp4", "fallback.jpg", "fallback.jpeg", "fallback.png"} {
		path := filepath.Join(dataDir, name)
		if _, err := os.Stat(path); err == nil {
			ext := strings.ToLower(filepath.Ext(name))
			sceneType := "video"
			if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
				sceneType = "image"
			}
			sm.scenes = append(sm.scenes, Scene{
				ID:       "default",
				Name:     "Default Fallback",
				FilePath: path,
				Type:     sceneType,
			})
			sm.activeID = "default"
			sm.saveConfig()
			log.Printf("[scenes] Created default scene from %s", path)
			return
		}
	}

	// Also check scripts directory
	for _, path := range []string{
		filepath.Join(dataDir, "..", "scripts", "queda.jpeg"),
		filepath.Join(dataDir, "..", "scripts", "fallback.ts"),
	} {
		if _, err := os.Stat(path); err == nil {
			ext := strings.ToLower(filepath.Ext(path))
			sceneType := "video"
			if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
				sceneType = "image"
			}
			sm.scenes = append(sm.scenes, Scene{
				ID:       "default",
				Name:     "Default Fallback",
				FilePath: path,
				Type:     sceneType,
			})
			sm.activeID = "default"
			sm.saveConfig()
			log.Printf("[scenes] Created default scene from %s", path)
			return
		}
	}
}

func (sm *SceneManager) GetScenes() []Scene {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	scenes := make([]Scene, len(sm.scenes))
	copy(scenes, sm.scenes)
	return scenes
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
			return &s
		}
	}
	if len(sm.scenes) > 0 {
		return &sm.scenes[0]
	}
	return nil
}

func (sm *SceneManager) AddScene(name, filePath string) (Scene, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Detect type
	ext := strings.ToLower(filepath.Ext(filePath))
	sceneType := "video"
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
		sceneType = "image"
	}

	scene := Scene{
		ID:       fmt.Sprintf("scene_%d", time.Now().UnixMilli()),
		Name:     name,
		FilePath: filePath,
		Type:     sceneType,
	}

	sm.scenes = append(sm.scenes, scene)

	// If this is the first scene, make it active
	if len(sm.scenes) == 1 {
		sm.activeID = scene.ID
	}

	sm.saveConfig()
	log.Printf("[scenes] Added scene: %s (%s)", name, scene.ID)
	return scene, nil
}

func (sm *SceneManager) RemoveScene(id string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	idx := -1
	for i, s := range sm.scenes {
		if s.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("scene %s not found", id)
	}

	sm.scenes = append(sm.scenes[:idx], sm.scenes[idx+1:]...)

	// If we removed the active scene, switch to the first one
	if sm.activeID == id && len(sm.scenes) > 0 {
		sm.activeID = sm.scenes[0].ID
		go sm.onSceneChange()
	}

	sm.saveConfig()
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

	if sm.activeID == id {
		sm.mu.Unlock()
		return nil // Already active
	}

	sm.activeID = id
	sm.saveConfig()
	sm.mu.Unlock()

	log.Printf("[scenes] Activated scene: %s", id)

	// Trigger fallback restart
	if sm.onSceneChange != nil {
		go sm.onSceneChange()
	}
	return nil
}

type scenesConfig struct {
	Scenes   []Scene `json:"scenes"`
	ActiveID string  `json:"active_id"`
}

func (sm *SceneManager) saveConfig() {
	cfg := scenesConfig{
		Scenes:   sm.scenes,
		ActiveID: sm.activeID,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("[scenes] Save error: %v", err)
		return
	}
	os.WriteFile(sm.configPath, data, 0644)
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
	log.Printf("[scenes] Loaded %d scenes (active: %s)", len(sm.scenes), sm.activeID)
}
