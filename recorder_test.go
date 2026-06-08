package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecorder_StartStop(t *testing.T) {
	tmpDir := t.TempDir()
	broadcaster := NewBroadcaster()

	recorder := NewRecorder(broadcaster, tmpDir)

	// Verify initial inactive status
	status := recorder.GetStatus()
	if status.Active {
		t.Error("Recorder should not be active initially")
	}

	// Start recording
	err := recorder.Start()
	if err != nil {
		t.Fatalf("Failed to start recording: %v", err)
	}

	status = recorder.GetStatus()
	if !status.Active {
		t.Error("Recorder should be active after Start()")
	}

	// Feed data through broadcaster to record loop
	broadcaster.Broadcast([]byte("tsdata123"))

	// Give a brief moment for the record loop to process
	time.Sleep(50 * time.Millisecond)

	// Check file exists and has size
	recorder.mu.RLock()
	tsPath := recorder.tsPath
	recorder.mu.RUnlock()

	if tsPath == "" {
		t.Fatal("Recorder tsPath should be set")
	}

	info, err := os.Stat(tsPath)
	if err != nil {
		t.Fatalf("Recording TS file was not created: %v", err)
	}
	if info.Size() != 9 {
		t.Errorf("Expected file size 9 bytes, got %d", info.Size())
	}

	// Stop recording
	err = recorder.Stop()
	if err != nil {
		t.Fatalf("Failed to stop recording: %v", err)
	}

	status = recorder.GetStatus()
	if status.Active {
		t.Error("Recorder should be inactive after Stop()")
	}
}

func TestRecorder_ListRecordings(t *testing.T) {
	tmpDir := t.TempDir()
	broadcaster := NewBroadcaster()
	recorder := NewRecorder(broadcaster, tmpDir)

	recordings := recorder.ListRecordings()
	if len(recordings) != 0 {
		t.Errorf("Expected 0 recordings, got %d", len(recordings))
	}

	// Manually create some files in the recordings folder
	recFolder := filepath.Join(tmpDir, "recordings")
	err := os.WriteFile(filepath.Join(recFolder, "rec_dummy.mp4"), []byte("mp4content"), 0644)
	if err != nil {
		t.Fatalf("Failed to write dummy file: %v", err)
	}

	recordings = recorder.ListRecordings()
	if len(recordings) != 1 {
		t.Errorf("Expected 1 recording, got %d", len(recordings))
	} else {
		entry := recordings[0]
		if entry.Name != "rec_dummy.mp4" {
			t.Errorf("Expected recording name 'rec_dummy.mp4', got %s", entry.Name)
		}
	}
}

func TestRecorder_JSON(t *testing.T) {
	tmpDir := t.TempDir()
	broadcaster := NewBroadcaster()
	recorder := NewRecorder(broadcaster, tmpDir)

	data, err := recorder.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON failed: %v", err)
	}

	var status RecordingStatus
	err = json.Unmarshal(data, &status)
	if err != nil {
		t.Fatalf("Unmarshal status failed: %v", err)
	}

	if status.Active {
		t.Error("Expected unmarshaled status active to be false")
	}
}
