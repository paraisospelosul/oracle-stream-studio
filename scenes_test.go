package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func buildSyntheticSlateTS() []byte {
	makePkt := func(pid uint16, pusi bool) []byte {
		p := make([]byte, tsPacketSize)
		p[0] = tsSyncByte
		p[1] = byte((pid >> 8) & 0x1F)
		if pusi {
			p[1] |= 0x40
		}
		p[2] = byte(pid & 0xFF)
		p[3] = 0x10
		return p
	}

	var raw []byte
	// Frame 0: PAT + PMT + Video IDR + Audio
	raw = append(raw, makePkt(patPID, true)...)
	raw = append(raw, makePkt(defaultPMTPID, true)...)
	raw = append(raw, makePkt(defaultVideoPID, true)...)
	raw = append(raw, makePkt(defaultAudioPID, true)...)
	// Frame 1: Video P-frame + Audio
	raw = append(raw, makePkt(defaultVideoPID, true)...)
	raw = append(raw, makePkt(defaultAudioPID, true)...)
	return raw
}

func TestSceneManager_LifecycleAndConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	scenesDir := filepath.Join(tmpDir, "scenes")
	if err := os.MkdirAll(scenesDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	tsA := filepath.Join(scenesDir, "scene_a_slate.ts")
	tsB := filepath.Join(scenesDir, "scene_b_slate.ts")
	rawTS := buildSyntheticSlateTS()

	if err := os.WriteFile(tsA, rawTS, 0644); err != nil {
		t.Fatalf("WriteFile tsA failed: %v", err)
	}
	if err := os.WriteFile(tsB, rawTS, 0644); err != nil {
		t.Fatalf("WriteFile tsB failed: %v", err)
	}

	configPath := filepath.Join(tmpDir, "scenes.json")
	initialCfg := scenesConfig{
		Scenes: []Scene{
			{ID: "scene_a", Name: "Cena A", FilePath: tsA, Type: "image"},
			{ID: "scene_b", Name: "Cena B", FilePath: tsB, Type: "image"},
		},
		ActiveID:   "scene_a",
		Resolution: "1080p",
	}
	cfgBytes, err := json.MarshalIndent(initialCfg, "", "  ")
	if err != nil {
		t.Fatalf("Marshal initialCfg failed: %v", err)
	}
	if err := os.WriteFile(configPath, cfgBytes, 0644); err != nil {
		t.Fatalf("WriteFile scenes.json failed: %v", err)
	}

	var changeCount atomic.Int32
	sm := NewSceneManager(tmpDir, configPath, "1080p", func() {
		changeCount.Add(1)
	})

	clip := sm.GetActiveClip()
	if clip == nil || clip.SceneID != "scene_a" || len(clip.Frames) != 2 {
		t.Fatalf("Expected active clip 'scene_a' with 2 frames, got: %+v", clip)
	}

	if err := sm.ActivateScene("nonexistent"); err == nil {
		t.Error("Expected error activating nonexistent scene")
	}

	if err := sm.ActivateScene("scene_b"); err != nil {
		t.Fatalf("ActivateScene('scene_b') failed: %v", err)
	}
	if got := sm.GetActiveClip(); got == nil || got.SceneID != "scene_b" {
		t.Fatalf("Expected active clip 'scene_b', got: %+v", got)
	}
	if changeCount.Load() == 0 {
		t.Error("Expected onSceneChange callback to be invoked")
	}

	// Concurrent 30fps lock-free readers vs ActivateScene + persistConfig writers
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if c := sm.GetActiveClip(); c == nil || len(c.Frames) == 0 {
					t.Errorf("Unexpected nil/empty active clip during concurrent switch")
					return
				}
				_ = sm.GetScenes()
				_ = sm.GetActiveScene()
			}
		}()
	}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				target := "scene_a"
				if (worker+i)%2 == 1 {
					target = "scene_b"
				}
				_ = sm.ActivateScene(target)
			}
		}(w)
	}
	wg.Wait()

	// Ensure 'scene_b' is active, then delete it -> must fall back to 'scene_a' and remove tsB
	_ = sm.ActivateScene("scene_b")
	if err := sm.RemoveScene("scene_b"); err != nil {
		t.Fatalf("RemoveScene('scene_b') failed: %v", err)
	}
	if sm.GetActiveID() != "scene_a" || sm.GetActiveClip().SceneID != "scene_a" {
		t.Errorf("Expected fallback to 'scene_a' after deleting active scene, got %s", sm.GetActiveID())
	}
	if _, err := os.Stat(tsB); !os.IsNotExist(err) {
		t.Error("Expected deleted scene TS file to be removed from disk")
	}

	// Cannot delete the last remaining fallback scene
	if err := sm.RemoveScene("scene_a"); err == nil {
		t.Error("Expected error when attempting to delete the last remaining scene")
	}
}
