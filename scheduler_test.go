package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestScheduler_CRUD(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "scheduler_test.json")

	scheduler := NewScheduler(configPath, nil)

	// Verify initially empty
	if len(scheduler.GetSchedules()) != 0 {
		t.Error("Expected 0 schedules initially")
	}

	// Add schedule
	sched := ScheduledSwitch{
		Name:           "Test Schedule",
		Time:           "12:00",
		SceneID:        "scene_test",
		TransitionType: "dip",
		TransDurMs:     1000,
		Repeat:         "daily",
		Enabled:        true,
	}

	scheduler.AddSchedule(sched)

	schedules := scheduler.GetSchedules()
	if len(schedules) != 1 {
		t.Fatalf("Expected 1 schedule, got %d", len(schedules))
	}

	addedSched := schedules[0]
	if addedSched.ID == "" {
		t.Error("Scheduler should assign an ID to added schedule if empty")
	}
	if addedSched.Name != "Test Schedule" {
		t.Errorf("Expected name 'Test Schedule', got %s", addedSched.Name)
	}

	// Update schedule
	addedSched.Name = "Updated Schedule Name"
	err := scheduler.UpdateSchedule(addedSched.ID, addedSched)
	if err != nil {
		t.Fatalf("Failed to update schedule: %v", err)
	}

	schedules = scheduler.GetSchedules()
	if schedules[0].Name != "Updated Schedule Name" {
		t.Errorf("Expected name 'Updated Schedule Name', got %s", schedules[0].Name)
	}

	// Toggle schedule
	err = scheduler.ToggleSchedule(addedSched.ID, false)
	if err != nil {
		t.Fatalf("Failed to toggle schedule: %v", err)
	}

	schedules = scheduler.GetSchedules()
	if schedules[0].Enabled {
		t.Error("Expected schedule to be disabled")
	}

	// Remove schedule
	err = scheduler.RemoveSchedule(addedSched.ID)
	if err != nil {
		t.Fatalf("Failed to remove schedule: %v", err)
	}

	if len(scheduler.GetSchedules()) != 0 {
		t.Error("Expected 0 schedules after removal")
	}

	// Verify config file was written
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("Config file was not saved")
	}
}

func TestScheduler_ShouldFire(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "scheduler_fire.json")

	var switchedScene string
	var mu sync.Mutex

	onSwitch := func(sceneID, transType string, transDurMs int) {
		mu.Lock()
		defer mu.Unlock()
		switchedScene = sceneID
	}

	scheduler := NewScheduler(configPath, onSwitch)

	// 1. One-time schedule
	oneTimeSched := ScheduledSwitch{
		ID:      "one_time",
		Name:    "One-time Switch",
		Time:    time.Now().Add(1 * time.Second).Format(time.RFC3339),
		SceneID: "scene_one_time",
		Repeat:  "once",
		Enabled: true,
	}

	// 2. Daily schedule
	dailySched := ScheduledSwitch{
		ID:      "daily",
		Name:    "Daily Switch",
		Time:    "14:30", // We will mock check time to match this
		SceneID: "scene_daily",
		Repeat:  "daily",
		Enabled: true,
	}

	// 3. Weekly schedule (only fires on specific day of week)
	weeklySched := ScheduledSwitch{
		ID:        "weekly",
		Name:      "Weekly Switch",
		Time:      "09:00",
		SceneID:   "scene_weekly",
		Repeat:    "weekly",
		DayOfWeek: int(time.Tuesday), // We will evaluate on a Tuesday
		Enabled:   true,
	}

	scheduler.AddSchedule(oneTimeSched)
	scheduler.AddSchedule(dailySched)
	scheduler.AddSchedule(weeklySched)

	// Test shouldFire for One-Time schedule
	// mock current time to be 1.5 seconds after the scheduled time
	scheduledTime, _ := time.Parse(time.RFC3339, oneTimeSched.Time)
	currentTime := scheduledTime.Add(500 * time.Millisecond)

	if !scheduler.shouldFire(oneTimeSched, currentTime) {
		t.Error("Expected oneTimeSched to want to fire at its scheduled time")
	}

	// Should not fire if already fired recently
	oneTimeSched.LastFired = currentTime.Format(time.RFC3339)
	if scheduler.shouldFire(oneTimeSched, currentTime) {
		t.Error("Expected oneTimeSched not to fire again if fired recently")
	}

	// Test shouldFire for Daily schedule
	// Current time matches 14:30:00
	mockTimeDaily, _ := time.Parse("2006-01-02 15:04:05", "2026-06-08 14:30:00")
	if !scheduler.shouldFire(dailySched, mockTimeDaily) {
		t.Error("Expected dailySched to fire at 14:30:00")
	}

	// Current time matches 14:30:05 (outside first 2 seconds of the minute)
	mockTimeDailyLate := mockTimeDaily.Add(5 * time.Second)
	if scheduler.shouldFire(dailySched, mockTimeDailyLate) {
		t.Error("Expected dailySched not to fire late in the minute")
	}

	// Test shouldFire for Weekly schedule
	// Mock time is a Tuesday at 09:00:00 (June 9, 2026 is Tuesday)
	mockTimeTuesday, _ := time.Parse("2006-01-02 15:04:05", "2026-06-09 09:00:00")
	if !scheduler.shouldFire(weeklySched, mockTimeTuesday) {
		t.Error("Expected weeklySched to fire on Tuesday 09:00")
	}

	// Mock time is a Monday at 09:00:00 (June 8, 2026 is Monday)
	mockTimeMonday, _ := time.Parse("2006-01-02 15:04:05", "2026-06-08 09:00:00")
	if scheduler.shouldFire(weeklySched, mockTimeMonday) {
		t.Error("Expected weeklySched not to fire on Monday")
	}
}

func TestScheduler_RunCtxCancel(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "scheduler_run.json")
	scheduler := NewScheduler(configPath, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		scheduler.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Error("Scheduler did not exit on context cancel")
	}
}
