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

// ScheduledSwitch defines a time-based scene switch
type ScheduledSwitch struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Time           string `json:"time"`            // HH:MM format or RFC3339 for one-time
	SceneID        string `json:"scene_id"`
	TransitionType string `json:"transition"`       // "cut", "fade", "dip"
	TransDurMs     int    `json:"transition_dur_ms"`
	Repeat         string `json:"repeat"`           // "once", "daily", "weekly"
	DayOfWeek      int    `json:"day_of_week"`      // 0=Sun, 1=Mon, ..., 6=Sat (for "weekly")
	Enabled        bool   `json:"enabled"`
	LastFired      string `json:"last_fired,omitempty"`
}

// SchedulerEvent records a scheduled switch execution
type SchedulerEvent struct {
	Time       string `json:"time"`
	ScheduleID string `json:"schedule_id"`
	Name       string `json:"name"`
	SceneID    string `json:"scene_id"`
}

// Scheduler manages time-based scene switching
type Scheduler struct {
	schedules  []ScheduledSwitch
	mu         sync.RWMutex
	configPath string

	// Callback to execute a switch
	onSwitch func(sceneID, transType string, transDurMs int)

	// Event history
	events   []SchedulerEvent
	eventsMu sync.RWMutex
}

// NewScheduler creates a new scheduler
func NewScheduler(configPath string, onSwitch func(string, string, int)) *Scheduler {
	s := &Scheduler{
		schedules:  make([]ScheduledSwitch, 0),
		configPath: configPath,
		onSwitch:   onSwitch,
		events:     make([]SchedulerEvent, 0, 50),
	}
	s.loadConfig()
	return s
}

// Run starts the scheduler loop, checking every 1 second
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Printf("[scheduler] Started with %d schedules", len(s.schedules))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.check()
		}
	}
}

// check evaluates all schedules against the current time
func (s *Scheduler) check() {
	now := time.Now()
	s.mu.RLock()
	schedules := make([]ScheduledSwitch, len(s.schedules))
	copy(schedules, s.schedules)
	s.mu.RUnlock()

	for i, sched := range schedules {
		if !sched.Enabled {
			continue
		}
		if s.shouldFire(sched, now) {
			s.fire(sched)

			// Update last_fired and handle "once" schedules
			s.mu.Lock()
			if i < len(s.schedules) && s.schedules[i].ID == sched.ID {
				s.schedules[i].LastFired = now.Format(time.RFC3339)
				if sched.Repeat == "once" {
					s.schedules[i].Enabled = false
				}
			}
			s.saveConfig()
			s.mu.Unlock()
		}
	}
}

// shouldFire checks if a schedule should fire at the given time
func (s *Scheduler) shouldFire(sched ScheduledSwitch, now time.Time) bool {
	// Parse the schedule time
	schedTime, err := time.Parse("15:04", sched.Time)
	if err != nil {
		// Try RFC3339 for one-time schedules
		schedTime, err = time.Parse(time.RFC3339, sched.Time)
		if err != nil {
			return false
		}
		// One-time absolute schedule
		diff := now.Sub(schedTime)
		if diff >= 0 && diff < 2*time.Second {
			return !s.firedRecently(sched, now)
		}
		return false
	}

	// Compare HH:MM with current time
	if now.Hour() != schedTime.Hour() || now.Minute() != schedTime.Minute() {
		return false
	}
	// Only fire in the first 2 seconds of the minute
	if now.Second() > 1 {
		return false
	}

	// Check day-of-week for weekly schedules
	if sched.Repeat == "weekly" {
		if int(now.Weekday()) != sched.DayOfWeek {
			return false
		}
	}

	return !s.firedRecently(sched, now)
}

// firedRecently checks if the schedule was fired within the last minute
func (s *Scheduler) firedRecently(sched ScheduledSwitch, now time.Time) bool {
	if sched.LastFired == "" {
		return false
	}
	lastFired, err := time.Parse(time.RFC3339, sched.LastFired)
	if err != nil {
		return false
	}
	return now.Sub(lastFired) < 60*time.Second
}

// fire executes a scheduled switch
func (s *Scheduler) fire(sched ScheduledSwitch) {
	log.Printf("[scheduler] Firing: %s → scene %s", sched.Name, sched.SceneID)

	event := SchedulerEvent{
		Time:       time.Now().Format(time.RFC3339),
		ScheduleID: sched.ID,
		Name:       sched.Name,
		SceneID:    sched.SceneID,
	}
	s.eventsMu.Lock()
	s.events = append(s.events, event)
	if len(s.events) > 50 {
		s.events = s.events[len(s.events)-50:]
	}
	s.eventsMu.Unlock()

	if s.onSwitch != nil {
		transType := sched.TransitionType
		if transType == "" {
			transType = "cut"
		}
		go s.onSwitch(sched.SceneID, transType, sched.TransDurMs)
	}
}

// GetSchedules returns all schedules
func (s *Scheduler) GetSchedules() []ScheduledSwitch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ScheduledSwitch, len(s.schedules))
	copy(result, s.schedules)
	return result
}

// AddSchedule adds a new schedule
func (s *Scheduler) AddSchedule(sched ScheduledSwitch) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sched.ID == "" {
		sched.ID = fmt.Sprintf("sched_%d", time.Now().UnixMilli())
	}
	s.schedules = append(s.schedules, sched)
	s.saveConfig()
	log.Printf("[scheduler] Schedule added: %s at %s", sched.Name, sched.Time)
}

// UpdateSchedule updates an existing schedule
func (s *Scheduler) UpdateSchedule(id string, updated ScheduledSwitch) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, sched := range s.schedules {
		if sched.ID == id {
			updated.ID = id
			s.schedules[i] = updated
			s.saveConfig()
			return nil
		}
	}
	return fmt.Errorf("schedule %s not found", id)
}

// RemoveSchedule removes a schedule by ID
func (s *Scheduler) RemoveSchedule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, sched := range s.schedules {
		if sched.ID == id {
			s.schedules = append(s.schedules[:i], s.schedules[i+1:]...)
			s.saveConfig()
			return nil
		}
	}
	return fmt.Errorf("schedule %s not found", id)
}

// ToggleSchedule enables/disables a schedule
func (s *Scheduler) ToggleSchedule(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, sched := range s.schedules {
		if sched.ID == id {
			s.schedules[i].Enabled = enabled
			s.saveConfig()
			return nil
		}
	}
	return fmt.Errorf("schedule %s not found", id)
}

// GetEvents returns recent scheduler events
func (s *Scheduler) GetEvents() []SchedulerEvent {
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	events := make([]SchedulerEvent, len(s.events))
	copy(events, s.events)
	return events
}

// Config persistence
type schedulerConfig struct {
	Schedules []ScheduledSwitch `json:"schedules"`
}

func (s *Scheduler) saveConfig() {
	cfg := schedulerConfig{Schedules: s.schedules}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("[scheduler] Save error: %v", err)
		return
	}
	os.WriteFile(s.configPath, data, 0644)
}

func (s *Scheduler) loadConfig() {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return
	}
	var cfg schedulerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("[scheduler] Parse error: %v", err)
		return
	}
	s.schedules = cfg.Schedules
	log.Printf("[scheduler] Loaded %d schedules", len(s.schedules))
}
