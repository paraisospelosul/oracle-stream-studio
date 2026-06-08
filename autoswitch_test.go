package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAutoSwitchEngine_CRUD(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "autoswitch_test.json")

	// Create engine
	engine := NewAutoSwitchEngine(configPath, nil)

	// Verify no rules Initially
	if len(engine.GetRules()) != 0 {
		t.Error("Expected 0 rules initially")
	}

	// Add rule
	rule := SwitchRule{
		Name: "Test Rule",
		Trigger: TriggerType{
			Type:      "srt_timeout",
			DurationS: 2,
		},
		Action: SwitchAction{
			TargetScene:    "fallback",
			TransitionType: "fade",
			TransDurMs:     500,
		},
		Enabled: true,
	}

	engine.AddRule(rule)

	rules := engine.GetRules()
	if len(rules) != 1 {
		t.Fatalf("Expected 1 rule, got %d", len(rules))
	}

	addedRule := rules[0]
	if addedRule.ID == "" {
		t.Error("Engine should assign an ID to added rule if empty")
	}
	if addedRule.Name != "Test Rule" {
		t.Errorf("Expected name 'Test Rule', got %s", addedRule.Name)
	}

	// Update rule
	addedRule.Name = "Updated Name"
	err := engine.UpdateRule(addedRule.ID, addedRule)
	if err != nil {
		t.Fatalf("Failed to update rule: %v", err)
	}

	rules = engine.GetRules()
	if rules[0].Name != "Updated Name" {
		t.Errorf("Expected updated name 'Updated Name', got %s", rules[0].Name)
	}

	// Toggle rule
	err = engine.ToggleRule(addedRule.ID, false)
	if err != nil {
		t.Fatalf("Failed to toggle rule: %v", err)
	}

	rules = engine.GetRules()
	if rules[0].Enabled {
		t.Error("Expected rule to be disabled")
	}

	// Remove rule
	err = engine.RemoveRule(addedRule.ID)
	if err != nil {
		t.Fatalf("Failed to remove rule: %v", err)
	}

	if len(engine.GetRules()) != 0 {
		t.Error("Expected 0 rules after removal")
	}

	// Verify config file was written
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("Config file was not saved")
	}
}

func TestAutoSwitchEngine_Triggers(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "autoswitch_trigger.json")

	var switchedScene string
	var switchedTrans string
	var switchedDur int
	var mu sync.Mutex

	onSwitch := func(sceneID, transType string, transDurMs int) {
		mu.Lock()
		defer mu.Unlock()
		switchedScene = sceneID
		switchedTrans = transType
		switchedDur = transDurMs
	}

	engine := NewAutoSwitchEngine(configPath, onSwitch)

	// Add a bitrate drop rule and a srt timeout rule
	ruleBitrate := SwitchRule{
		ID:   "bitrate_rule",
		Name: "Bitrate Drop",
		Trigger: TriggerType{
			Type:      "bitrate_drop",
			Threshold: 500, // < 500 kbps
			DurationS: 1,
		},
		Action: SwitchAction{
			TargetScene:    "scene_bitrate",
			TransitionType: "cut",
		},
		Enabled: true,
	}

	ruleTimeout := SwitchRule{
		ID:   "timeout_rule",
		Name: "SRT Timeout",
		Trigger: TriggerType{
			Type:      "srt_timeout",
			DurationS: 1,
		},
		Action: SwitchAction{
			TargetScene:    "scene_timeout",
			TransitionType: "fade",
			TransDurMs:     300,
		},
		Enabled: true,
	}

	engine.AddRule(ruleBitrate)
	engine.AddRule(ruleTimeout)

	// Setup providers
	var currentBitrate float64
	var srtConnected bool

	engine.SetProviders(
		func() float64 { return currentBitrate },
		func() bool { return srtConnected },
		func() float64 { return -50.0 },
	)

	// 1. Initially: Bitrate = 1000, SRT = connected -> No triggers
	currentBitrate = 1000
	srtConnected = true
	engine.evaluate()

	mu.Lock()
	if switchedScene != "" {
		t.Errorf("Should not have switched, got scene: %s", switchedScene)
	}
	mu.Unlock()

	// 2. SRT drops. Evaluating immediately: trigger has duration=1s. Tracker starts.
	srtConnected = false
	engine.evaluate()

	mu.Lock()
	if switchedScene != "" {
		t.Error("Should not trigger instantly (needs 1s duration)")
	}
	mu.Unlock()

	// Modify the tracker's start time manually to simulate 1.5 seconds passing
	if tracker, ok := engine.trackers["timeout_rule"]; ok {
		tracker.startTime = time.Now().Add(-1500 * time.Millisecond)
	} else {
		t.Fatal("Tracker not found")
	}

	// Evaluate again -> should trigger now!
	engine.evaluate()

	// Wait for goroutine callback to run
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	if switchedScene != "scene_timeout" {
		t.Errorf("Expected switch to 'scene_timeout', got '%s'", switchedScene)
	}
	if switchedTrans != "fade" || switchedDur != 300 {
		t.Errorf("Expected transition 'fade' (300ms), got '%s' (%dms)", switchedTrans, switchedDur)
	}
	switchedScene = "" // reset
	mu.Unlock()

	// 3. Bitrate drops below threshold. Evaluated with duration=1s.
	currentBitrate = 300
	engine.evaluate()

	if tracker, ok := engine.trackers["bitrate_rule"]; ok {
		tracker.startTime = time.Now().Add(-1500 * time.Millisecond)
	} else {
		t.Fatal("Tracker not found")
	}

	engine.evaluate()
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	if switchedScene != "scene_bitrate" {
		t.Errorf("Expected switch to 'scene_bitrate', got '%s'", switchedScene)
	}
	mu.Unlock()

	// Check that events are recorded
	events := engine.GetEvents()
	if len(events) < 2 {
		t.Errorf("Expected at least 2 events recorded, got %d", len(events))
	}
}

func TestAutoSwitchEngine_RunCtxCancel(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "autoswitch_run.json")
	engine := NewAutoSwitchEngine(configPath, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		engine.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Error("Engine did not exit on context cancel")
	}
}
