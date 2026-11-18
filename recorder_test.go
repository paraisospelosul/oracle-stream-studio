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

func TestRecorder_DedupAndGuardErrors(t *testing.T) {
	tmpDir := t.TempDir()
	broadcaster := NewBroadcaster()
	recorder := NewRecorder(broadcaster, tmpDir)

	if err := recorder.Stop(); err == nil {
		t.Error("Expected Stop() on idle recorder to return error")
	}

	if err := recorder.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if err := recorder.Start(); err == nil {
		t.Error("Expected second Start() while recording to return error")
	}
	if err := recorder.Stop(); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}

	recFolder := filepath.Join(tmpDir, "recordings")
	// Clear file created by Start/Stop above and test .ts/.mp4 deduplication
	entries, _ := os.ReadDir(recFolder)
	for _, e := range entries {
		_ = os.Remove(filepath.Join(recFolder, e.Name()))
	}

	_ = os.WriteFile(filepath.Join(recFolder, "rec_pair.ts"), []byte("rawts"), 0644)
	_ = os.WriteFile(filepath.Join(recFolder, "rec_pair.mp4"), []byte("mp4remuxed"), 0644)
	_ = os.WriteFile(filepath.Join(recFolder, "rec_only_ts.ts"), []byte("rawts2"), 0644)

	list := recorder.ListRecordings()
	if len(list) != 2 {
		t.Fatalf("Expected 2 recordings (hiding paired .ts), got %d: %+v", len(list), list)
	}
	for _, item := range list {
		if item.Name == "rec_pair.ts" {
			t.Error("Expected rec_pair.ts to be hidden when rec_pair.mp4 exists")
		}
	}
}
