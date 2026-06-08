package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// TriggerType defines what condition triggers an auto-switch
type TriggerType struct {
	Type      string  `json:"type"`       // "bitrate_drop", "srt_timeout", "srt_return", "silence", "schedule"
	Threshold float64 `json:"threshold"`  // e.g. bitrate < 500kbps
	DurationS int     `json:"duration_s"` // seconds condition must persist before triggering
}

// SwitchAction defines what happens when a rule triggers
type SwitchAction struct {
	TargetScene    string `json:"target_scene"`
	TransitionType string `json:"transition"`           // "cut", "fade", "dip"
	TransDurMs     int    `json:"transition_duration_ms"`
}

// SwitchRule defines an automatic switching rule
type SwitchRule struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Trigger TriggerType `json:"trigger"`
	Action  SwitchAction `json:"action"`
	Enabled bool        `json:"enabled"`
}

// AutoSwitchEvent records when an auto-switch rule fired
type AutoSwitchEvent struct {
	Time     string `json:"time"`
	RuleID   string `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Trigger  string `json:"trigger"`
	Action   string `json:"action"`
}

// AutoSwitchEngine manages automatic scene switching based on configurable rules
type AutoSwitchEngine struct {
	rules      []SwitchRule
	mu         sync.RWMutex
	configPath string

	// Callback to execute a switch
	onSwitch func(sceneID, transType string, transDurMs int)

	// Data providers (set by Switcher)
	getBitrate    func() float64
	getSRTState   func() bool // true = SRT connected
	getAudioLevel func() float64 // dB level

	// Event history
	events   []AutoSwitchEvent
	eventsMu sync.RWMutex

	// Condition trackers
	trackers map[string]*conditionTracker
}

// conditionTracker tracks how long a condition has been true
type conditionTracker struct {
	conditionMet bool
	startTime    time.Time
}

// NewAutoSwitchEngine creates a new auto-switch engine
func NewAutoSwitchEngine(configPath string, onSwitch func(string, string, int)) *AutoSwitchEngine {
	ase := &AutoSwitchEngine{
		rules:      make([]SwitchRule, 0),
		configPath: configPath,
		onSwitch:   onSwitch,
		events:     make([]AutoSwitchEvent, 0, 50),
		trackers:   make(map[string]*conditionTracker),
	}
	ase.loadConfig()
	return ase
}

// SetProviders sets the data provider functions
func (ase *AutoSwitchEngine) SetProviders(
	getBitrate func() float64,
	getSRTState func() bool,
	getAudioLevel func() float64,
) {
	ase.mu.Lock()
	defer ase.mu.Unlock()
	ase.getBitrate = getBitrate
	ase.getSRTState = getSRTState
	ase.getAudioLevel = getAudioLevel
}

// Run starts the auto-switch evaluation loop
func (ase *AutoSwitchEngine) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Printf("[autoswitch] Engine started with %d rules", len(ase.rules))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ase.evaluate()
		}
	}
}

// evaluate checks all enabled rules against current conditions
func (ase *AutoSwitchEngine) evaluate() {
	ase.mu.RLock()
	rules := make([]SwitchRule, len(ase.rules))
	copy(rules, ase.rules)
	ase.mu.RUnlock()

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if ase.checkTrigger(rule) {
			ase.fireRule(rule)
		}
	}
}

// checkTrigger evaluates if a rule's trigger condition is met
func (ase *AutoSwitchEngine) checkTrigger(rule SwitchRule) bool {
	tracker, ok := ase.trackers[rule.ID]
	if !ok {
		tracker = &conditionTracker{}
		ase.trackers[rule.ID] = tracker
	}

	conditionNow := false

	switch rule.Trigger.Type {
	case "bitrate_drop":
		if ase.getBitrate != nil {
			bitrate := ase.getBitrate()
			conditionNow = bitrate > 0 && bitrate < rule.Trigger.Threshold
		}

	case "srt_timeout":
		if ase.getSRTState != nil {
			conditionNow = !ase.getSRTState()
		}

	case "srt_return":
		if ase.getSRTState != nil {
			conditionNow = ase.getSRTState()
		}

	case "silence":
		if ase.getAudioLevel != nil {
			level := ase.getAudioLevel()
			conditionNow = level < rule.Trigger.Threshold // threshold in dB (e.g. -40)
		}
	}

	if conditionNow {
		if !tracker.conditionMet {
			tracker.conditionMet = true
			tracker.startTime = time.Now()
		}
		// Check if duration requirement is met
		requiredDur := time.Duration(rule.Trigger.DurationS) * time.Second
		if requiredDur == 0 {
			requiredDur = 1 * time.Second // minimum 1s
		}
		return time.Since(tracker.startTime) >= requiredDur
	}

	// Condition not met — reset tracker
	tracker.conditionMet = false
	return false
}

// fireRule executes a triggered rule's action
func (ase *AutoSwitchEngine) fireRule(rule SwitchRule) {
	// Reset the tracker to prevent re-firing
	if tracker, ok := ase.trackers[rule.ID]; ok {
		tracker.conditionMet = false
	}

	log.Printf("[autoswitch] Rule fired: %s (trigger=%s, target=%s)",
		rule.Name, rule.Trigger.Type, rule.Action.TargetScene)

	// Record event
	event := AutoSwitchEvent{
		Time:     time.Now().Format(time.RFC3339),
		RuleID:   rule.ID,
		RuleName: rule.Name,
		Trigger:  rule.Trigger.Type,
		Action:   fmt.Sprintf("switch to %s (%s)", rule.Action.TargetScene, rule.Action.TransitionType),
	}
	ase.eventsMu.Lock()
	ase.events = append(ase.events, event)
	if len(ase.events) > 50 {
		ase.events = ase.events[len(ase.events)-50:]
	}
	ase.eventsMu.Unlock()

	// Execute the switch
	if ase.onSwitch != nil {
		transType := rule.Action.TransitionType
		if transType == "" {
			transType = "cut"
		}
		go ase.onSwitch(rule.Action.TargetScene, transType, rule.Action.TransDurMs)
	}
}

// GetRules returns all rules
func (ase *AutoSwitchEngine) GetRules() []SwitchRule {
	ase.mu.RLock()
	defer ase.mu.RUnlock()
	rules := make([]SwitchRule, len(ase.rules))
	copy(rules, ase.rules)
	return rules
}

// AddRule adds a new auto-switch rule
func (ase *AutoSwitchEngine) AddRule(rule SwitchRule) {
	ase.mu.Lock()
	defer ase.mu.Unlock()

	if rule.ID == "" {
		rule.ID = fmt.Sprintf("rule_%d", time.Now().UnixMilli())
	}
	ase.rules = append(ase.rules, rule)
	ase.saveConfig()
	log.Printf("[autoswitch] Rule added: %s (%s)", rule.Name, rule.ID)
}

// UpdateRule updates an existing rule
func (ase *AutoSwitchEngine) UpdateRule(id string, updated SwitchRule) error {
	ase.mu.Lock()
	defer ase.mu.Unlock()

	for i, r := range ase.rules {
		if r.ID == id {
			updated.ID = id
			ase.rules[i] = updated
			ase.saveConfig()
			return nil
		}
	}
	return fmt.Errorf("rule %s not found", id)
}

// RemoveRule removes a rule by ID
func (ase *AutoSwitchEngine) RemoveRule(id string) error {
	ase.mu.Lock()
	defer ase.mu.Unlock()

	for i, r := range ase.rules {
		if r.ID == id {
			ase.rules = append(ase.rules[:i], ase.rules[i+1:]...)
			ase.saveConfig()
			delete(ase.trackers, id)
			return nil
		}
	}
	return fmt.Errorf("rule %s not found", id)
}

// ToggleRule enables/disables a rule
func (ase *AutoSwitchEngine) ToggleRule(id string, enabled bool) error {
	ase.mu.Lock()
	defer ase.mu.Unlock()

	for i, r := range ase.rules {
		if r.ID == id {
			ase.rules[i].Enabled = enabled
			ase.saveConfig()
			return nil
		}
	}
	return fmt.Errorf("rule %s not found", id)
}

// GetEvents returns recent auto-switch events
func (ase *AutoSwitchEngine) GetEvents() []AutoSwitchEvent {
	ase.eventsMu.RLock()
	defer ase.eventsMu.RUnlock()
	events := make([]AutoSwitchEvent, len(ase.events))
	copy(events, ase.events)
	return events
}

// Config persistence
type autoSwitchConfig struct {
	Rules []SwitchRule `json:"rules"`
}

func (ase *AutoSwitchEngine) saveConfig() {
	cfg := autoSwitchConfig{Rules: ase.rules}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("[autoswitch] Save error: %v", err)
		return
	}
	os.WriteFile(ase.configPath, data, 0644)
}

func (ase *AutoSwitchEngine) loadConfig() {
	data, err := os.ReadFile(ase.configPath)
	if err != nil {
		return
	}
	var cfg autoSwitchConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("[autoswitch] Parse error: %v", err)
		return
	}
	ase.rules = cfg.Rules
	log.Printf("[autoswitch] Loaded %d rules", len(ase.rules))
}
