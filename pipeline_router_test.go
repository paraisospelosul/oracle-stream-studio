package main

import (
	"context"
	"testing"
	"time"
)

func TestPipelineRouter_AddRemove(t *testing.T) {
	router := NewPipelineRouter(3)

	ch := make(chan []byte, 1)
	p := router.AddPipeline("test-pipe", "srt", ch)

	if p.ID != "test-pipe" {
		t.Errorf("Expected ID 'test-pipe', got %s", p.ID)
	}
	if p.Source != "srt" {
		t.Errorf("Expected source 'srt', got %s", p.Source)
	}
	if p.Active {
		t.Error("New pipeline should not be active initially")
	}

	found := router.GetPipeline("test-pipe")
	if found == nil || found.ID != "test-pipe" {
		t.Error("Pipeline was not retrieved correctly from router")
	}

	router.RemovePipeline("test-pipe")
	found = router.GetPipeline("test-pipe")
	if found != nil {
		t.Error("Pipeline should have been removed from router")
	}
}

func TestPipelineRouter_SwitchTo(t *testing.T) {
	router := NewPipelineRouter(3)

	ch1 := make(chan []byte, 1)
	ch2 := make(chan []byte, 1)

	p1 := router.AddPipeline("p1", "srt", ch1)
	p2 := router.AddPipeline("p2", "scene1", ch2)

	// Switch to p1
	err := router.SwitchTo("p1")
	if err != nil {
		t.Fatalf("Failed to switch to p1: %v", err)
	}

	active := router.GetActive()
	if active == nil || active.ID != "p1" {
		t.Errorf("Expected active pipeline to be p1, got %v", active)
	}
	if !p1.Active {
		t.Error("p1 should be active")
	}
	if p2.Active {
		t.Error("p2 should not be active")
	}

	// Switch to p2
	err = router.SwitchTo("p2")
	if err != nil {
		t.Fatalf("Failed to switch to p2: %v", err)
	}

	active = router.GetActive()
	if active == nil || active.ID != "p2" {
		t.Errorf("Expected active pipeline to be p2, got %v", active)
	}
	if p1.Active {
		t.Error("p1 should no longer be active")
	}
	if !p2.Active {
		t.Error("p2 should now be active")
	}

	// Switch to non-existent pipeline
	err = router.SwitchTo("invalid")
	if err == nil {
		t.Error("Expected error switching to non-existent pipeline")
	}
}

func TestPipelineRouter_PreWarm(t *testing.T) {
	router := NewPipelineRouter(2)

	if !router.CanPreWarm() {
		t.Error("Should be able to pre-warm when no pipelines exist")
	}

	ch1 := make(chan []byte, 1)
	ch2 := make(chan []byte, 1)
	ch3 := make(chan []byte, 1)

	router.AddPipeline("p1", "srt", ch1)
	router.AddPipeline("p2", "scene1", ch2)

	if router.PreWarmCount() != 2 {
		t.Errorf("Expected 2 pre-warmed pipelines, got %d", router.PreWarmCount())
	}

	if router.CanPreWarm() {
		t.Error("Should not be able to pre-warm since we reached max limit of 2")
	}

	// Activate one pipeline
	_ = router.SwitchTo("p1")

	if router.PreWarmCount() != 1 {
		t.Errorf("Expected 1 pre-warmed pipeline after activating one, got %d", router.PreWarmCount())
	}

	if !router.CanPreWarm() {
		t.Error("Should be able to pre-warm again since one pipeline became active")
	}

	router.AddPipeline("p3", "scene2", ch3)
	if router.CanPreWarm() {
		t.Error("Should not be able to pre-warm as we hit limit again")
	}
}

func TestPipelineRouter_DrainInactive(t *testing.T) {
	router := NewPipelineRouter(3)

	ch := make(chan []byte, 2)
	p := router.AddPipeline("p1", "scene1", ch)

	// Send data on inactive channel
	ch <- []byte("packet1")
	ch <- []byte("packet2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run DrainInactive in a goroutine
	go router.DrainInactive(ctx)

	// Wait a bit for the drain to execute
	time.Sleep(50 * time.Millisecond)

	p.mu.Lock()
	isReady := p.Ready
	hasLastData := !p.LastData.IsZero()
	p.mu.Unlock()

	if !isReady {
		t.Error("Expected pipeline to be marked ready after draining data")
	}
	if !hasLastData {
		t.Error("Expected LastData to be updated after draining data")
	}

	// Verify channel was drained
	select {
	case data := <-ch:
		t.Errorf("Channel was not drained, got data: %s", string(data))
	default:
		// success, channel is empty
	}
}
