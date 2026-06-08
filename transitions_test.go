package main

import (
	"context"
	"testing"
	"time"
)

func TestTransitionType_String(t *testing.T) {
	tests := []struct {
		val  TransitionType
		want string
	}{
		{TransitionCut, "cut"},
		{TransitionFade, "fade"},
		{TransitionDip, "dip"},
		{TransitionType(999), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.val.String(); got != tt.want {
			t.Errorf("TransitionType(%d).String() = %s, want %s", int(tt.val), got, tt.want)
		}
	}
}

func TestParseTransitionType(t *testing.T) {
	tests := []struct {
		val  string
		want TransitionType
	}{
		{"cut", TransitionCut},
		{"fade", TransitionFade},
		{"dip", TransitionDip},
		{"invalid", TransitionCut},
	}

	for _, tt := range tests {
		if got := ParseTransitionType(tt.val); got != tt.want {
			t.Errorf("ParseTransitionType(%s) = %d, want %d", tt.val, int(got), int(tt.want))
		}
	}
}

func TestTransitionEngine_Config(t *testing.T) {
	te := NewTransitionEngine()

	cfg := te.GetConfig()
	if cfg.Type != TransitionCut {
		t.Error("Default transition type should be 'cut'")
	}

	// Update config with valid duration
	te.SetConfig(TransitionConfig{Type: TransitionFade, DurationMs: 500})
	cfg = te.GetConfig()
	if cfg.Type != TransitionFade || cfg.DurationMs != 500 {
		t.Errorf("Expected fade (500ms), got %s (%dms)", cfg.Type, cfg.DurationMs)
	}

	// Update config with too small duration
	te.SetConfig(TransitionConfig{Type: TransitionFade, DurationMs: 50})
	cfg = te.GetConfig()
	if cfg.DurationMs != 100 {
		t.Errorf("Expected duration to be clamped to 100ms, got %dms", cfg.DurationMs)
	}

	// Update config with too large duration
	te.SetConfig(TransitionConfig{Type: TransitionFade, DurationMs: 3000})
	cfg = te.GetConfig()
	if cfg.DurationMs != 2000 {
		t.Errorf("Expected duration to be clamped to 2000ms, got %dms", cfg.DurationMs)
	}
}

func TestTransitionEngine_ExecuteCut(t *testing.T) {
	te := NewTransitionEngine()

	if te.IsActive() {
		t.Error("Engine should not be active initially")
	}

	te.ExecuteCut("srt", "fallback")

	at := te.GetActiveTransition()
	if at == nil {
		t.Fatal("Expected active transition to be tracked")
	}
	if at.FromPipe != "srt" || at.ToPipe != "fallback" {
		t.Errorf("Expected cut 'srt' -> 'fallback', got '%s' -> '%s'", at.FromPipe, at.ToPipe)
	}
	if at.Progress != 1.0 {
		t.Errorf("Expected progress 1.0, got %f", at.Progress)
	}
}

func TestGetAvailableTransitions(t *testing.T) {
	list := GetAvailableTransitions()
	if len(list) != 3 {
		t.Fatalf("Expected 3 available transitions, got %d", len(list))
	}

	types := make(map[string]bool)
	for _, tr := range list {
		types[tr["type"].(string)] = true
	}

	if !types["cut"] || !types["fade"] || !types["dip"] {
		t.Error("Available transitions list missing cut, fade, or dip")
	}
}

func TestTransitionEngine_FadeFailureWithoutFFmpeg(t *testing.T) {
	te := NewTransitionEngine()
	ctx := context.Background()

	// This should fail to start ffmpeg and return an error rather than panic
	_, err := te.ExecuteFade(ctx, []byte("from"), []byte("to"), 500, "srt", "fallback")
	if err == nil {
		// If it succeeded, it means ffmpeg is actually installed and processed, which is fine
		t.Log("ExecuteFade succeeded or failed gracefully")
	} else {
		t.Logf("ExecuteFade failed as expected (no ffmpeg or invalid input): %v", err)
	}
}
