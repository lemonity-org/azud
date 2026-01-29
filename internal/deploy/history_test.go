package deploy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryStore_Record(t *testing.T) {
	// Create temp directory for test
	tmpDir := t.TempDir()

	store := NewHistoryStore(tmpDir, 100, nil)

	record := NewDeploymentRecord("test-service", "test:v1", "v1", "production", []string{"host1"})
	record.Complete()

	err := store.Record(record)
	if err != nil {
		t.Fatalf("Failed to record deployment: %v", err)
	}

	// Verify file was created
	historyDir := filepath.Join(tmpDir, ".azud", "history")
	files, err := filepath.Glob(filepath.Join(historyDir, "test-service_*.json"))
	if err != nil {
		t.Fatalf("Failed to list history files: %v", err)
	}

	if len(files) != 1 {
		t.Errorf("Expected 1 history file, got %d", len(files))
	}
}

func TestHistoryStore_List(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewHistoryStore(tmpDir, 100, nil)

	// Create multiple records
	for i := 0; i < 5; i++ {
		record := NewDeploymentRecord("test-service", "test:v"+string(rune('0'+i)), "v"+string(rune('0'+i)), "production", []string{"host1"})
		record.StartedAt = time.Now().Add(time.Duration(i) * time.Second)
		record.Complete()
		if err := store.Record(record); err != nil {
			t.Fatalf("Failed to record deployment %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond) // Ensure different timestamps
	}

	// List all records
	records, err := store.List("test-service", 0)
	if err != nil {
		t.Fatalf("Failed to list records: %v", err)
	}

	if len(records) != 5 {
		t.Errorf("Expected 5 records, got %d", len(records))
	}

	// Verify sorted by newest first
	for i := 0; i < len(records)-1; i++ {
		if records[i].StartedAt.Before(records[i+1].StartedAt) {
			t.Errorf("Records not sorted by newest first")
		}
	}

	// Test with limit
	limited, err := store.List("test-service", 3)
	if err != nil {
		t.Fatalf("Failed to list records with limit: %v", err)
	}

	if len(limited) != 3 {
		t.Errorf("Expected 3 records with limit, got %d", len(limited))
	}
}

func TestHistoryStore_GetLastSuccessful(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewHistoryStore(tmpDir, 100, nil)

	// Create records with different statuses
	record1 := NewDeploymentRecord("test-service", "test:v1", "v1", "", []string{"host1"})
	record1.Complete()
	store.Record(record1)

	time.Sleep(10 * time.Millisecond)

	record2 := NewDeploymentRecord("test-service", "test:v2", "v2", "", []string{"host1"})
	record2.Fail(nil)
	store.Record(record2)

	time.Sleep(10 * time.Millisecond)

	record3 := NewDeploymentRecord("test-service", "test:v3", "v3", "", []string{"host1"})
	record3.Complete()
	store.Record(record3)

	// Get last successful
	last, err := store.GetLastSuccessful("test-service")
	if err != nil {
		t.Fatalf("Failed to get last successful: %v", err)
	}

	if last.Version != "v3" {
		t.Errorf("Expected last successful version v3, got %s", last.Version)
	}
}

func TestHistoryStore_GetLastDeployment(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewHistoryStore(tmpDir, 100, nil)

	// Create records
	record1 := NewDeploymentRecord("test-service", "test:v1", "v1", "", []string{"host1"})
	record1.Complete()
	store.Record(record1)

	time.Sleep(10 * time.Millisecond)

	record2 := NewDeploymentRecord("test-service", "test:v2", "v2", "", []string{"host1"})
	record2.Fail(nil)
	store.Record(record2)

	// Get last deployment (regardless of status)
	last, err := store.GetLastDeployment("test-service")
	if err != nil {
		t.Fatalf("Failed to get last deployment: %v", err)
	}

	if last.Version != "v2" {
		t.Errorf("Expected last deployment version v2, got %s", last.Version)
	}
}

func TestHistoryStore_Cleanup(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewHistoryStore(tmpDir, 3, nil) // Retain only 3 records

	// Create 5 records
	for i := 0; i < 5; i++ {
		record := NewDeploymentRecord("test-service", "test:v"+string(rune('0'+i)), "v"+string(rune('0'+i)), "", []string{"host1"})
		record.Complete()
		if err := store.Record(record); err != nil {
			t.Fatalf("Failed to record deployment %d: %v", i, err)
		}
		time.Sleep(50 * time.Millisecond) // Give cleanup goroutine time to run
	}

	// Wait for cleanup goroutine
	time.Sleep(100 * time.Millisecond)

	// Verify only 3 records remain
	records, err := store.List("test-service", 0)
	if err != nil {
		t.Fatalf("Failed to list records: %v", err)
	}

	if len(records) > 3 {
		t.Errorf("Expected at most 3 records after cleanup, got %d", len(records))
	}
}

func TestDeploymentRecord_StatusTransitions(t *testing.T) {
	record := NewDeploymentRecord("test-service", "test:v1", "v1", "prod", []string{"host1"})

	// Initial status
	if record.Status != StatusPending {
		t.Errorf("Expected initial status Pending, got %s", record.Status)
	}

	// Start
	record.Start()
	if record.Status != StatusRunning {
		t.Errorf("Expected status Running after Start, got %s", record.Status)
	}

	// Complete
	record.Complete()
	if record.Status != StatusSuccess {
		t.Errorf("Expected status Success after Complete, got %s", record.Status)
	}
	if record.Duration == 0 {
		t.Error("Expected non-zero duration after Complete")
	}
}

func TestDeploymentRecord_Failure(t *testing.T) {
	record := NewDeploymentRecord("test-service", "test:v1", "v1", "", []string{"host1"})
	record.Start()

	err := os.ErrNotExist
	record.Fail(err)

	if record.Status != StatusFailed {
		t.Errorf("Expected status Failed, got %s", record.Status)
	}
	if record.Error == "" {
		t.Error("Expected error message to be set")
	}
}

func TestGenerateDeploymentID(t *testing.T) {
	id1 := GenerateDeploymentID()
	id2 := GenerateDeploymentID()

	if id1 == id2 {
		t.Error("Expected unique deployment IDs")
	}

	if len(id1) < 10 {
		t.Errorf("Expected deployment ID to be at least 10 chars, got %d", len(id1))
	}
}
