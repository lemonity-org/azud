package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/lemonity-org/azud/internal/output"
)

// DeploymentStatus represents the status of a deployment
type DeploymentStatus string

const (
	StatusPending    DeploymentStatus = "pending"
	StatusRunning    DeploymentStatus = "running"
	StatusSuccess    DeploymentStatus = "success"
	StatusFailed     DeploymentStatus = "failed"
	StatusRolledBack DeploymentStatus = "rolled_back"
)

// DeploymentRecord represents a single deployment event
type DeploymentRecord struct {
	// Unique identifier for this deployment
	ID string `json:"id"`

	// Service name being deployed
	Service string `json:"service"`

	// Full image reference (including tag)
	Image string `json:"image"`

	// Version/tag being deployed
	Version string `json:"version"`

	// Destination environment (staging, production, etc.)
	Destination string `json:"destination,omitempty"`

	// Hosts targeted by this deployment
	Hosts []string `json:"hosts"`

	// Current status of the deployment
	Status DeploymentStatus `json:"status"`

	// When the deployment started
	StartedAt time.Time `json:"started_at"`

	// When the deployment completed (success or failure)
	CompletedAt time.Time `json:"completed_at,omitempty"`

	// Total duration of the deployment
	Duration time.Duration `json:"duration,omitempty"`

	// Error message if deployment failed
	Error string `json:"error,omitempty"`

	// Whether this deployment was automatically rolled back
	RolledBack bool `json:"rolled_back"`

	// Previous version (for rollback reference)
	PreviousVersion string `json:"previous_version,omitempty"`

	// Additional metadata
	Metadata map[string]string `json:"metadata,omitempty"`
}

// HistoryStore manages deployment history persistence
type HistoryStore struct {
	basePath   string
	retainDays int
	mu         sync.RWMutex
	log        *output.Logger
}

// NewHistoryStore creates a new history store
func NewHistoryStore(basePath string, retainCount int, log *output.Logger) *HistoryStore {
	if log == nil {
		log = output.DefaultLogger
	}

	return &HistoryStore{
		basePath:   filepath.Join(basePath, ".azud", "history"),
		retainDays: retainCount,
		log:        log,
	}
}

// Record saves a deployment record to the store
func (h *HistoryStore) Record(record *DeploymentRecord) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Ensure directory exists
	if err := os.MkdirAll(h.basePath, 0755); err != nil {
		return fmt.Errorf("failed to create history directory: %w", err)
	}

	// Generate filename based on service and timestamp
	filename := fmt.Sprintf("%s_%s.json", record.Service, record.StartedAt.Format("20060102_150405"))
	filepath := filepath.Join(h.basePath, filename)

	// Marshal record to JSON
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}

	// Write to file
	if err := os.WriteFile(filepath, data, 0644); err != nil {
		return fmt.Errorf("failed to write record: %w", err)
	}

	// Cleanup old records
	go h.cleanup(record.Service)

	return nil
}

// Update updates an existing deployment record
func (h *HistoryStore) Update(record *DeploymentRecord) error {
	return h.Record(record)
}

// List returns deployment records for a service, sorted by start time (newest first)
func (h *HistoryStore) List(service string, limit int) ([]*DeploymentRecord, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	pattern := filepath.Join(h.basePath, fmt.Sprintf("%s_*.json", service))
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list history files: %w", err)
	}

	var records []*DeploymentRecord
	for _, file := range files {
		record, err := h.loadRecord(file)
		if err != nil {
			h.log.Debug("Failed to load record %s: %v", file, err)
			continue
		}
		records = append(records, record)
	}

	// Sort by start time (newest first)
	sort.Slice(records, func(i, j int) bool {
		return records[i].StartedAt.After(records[j].StartedAt)
	})

	// Apply limit
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}

	return records, nil
}

// Get retrieves a specific deployment record by ID
func (h *HistoryStore) Get(id string) (*DeploymentRecord, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Search all files for matching ID
	files, err := filepath.Glob(filepath.Join(h.basePath, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to list history files: %w", err)
	}

	for _, file := range files {
		record, err := h.loadRecord(file)
		if err != nil {
			continue
		}
		if record.ID == id {
			return record, nil
		}
	}

	return nil, fmt.Errorf("deployment record not found: %s", id)
}

// GetLastSuccessful returns the most recent successful deployment for a service
func (h *HistoryStore) GetLastSuccessful(service string) (*DeploymentRecord, error) {
	records, err := h.List(service, 0)
	if err != nil {
		return nil, err
	}

	for _, record := range records {
		if record.Status == StatusSuccess {
			return record, nil
		}
	}

	return nil, fmt.Errorf("no successful deployments found for %s", service)
}

// GetLastDeployment returns the most recent deployment for a service
func (h *HistoryStore) GetLastDeployment(service string) (*DeploymentRecord, error) {
	records, err := h.List(service, 1)
	if err != nil {
		return nil, err
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("no deployments found for %s", service)
	}

	return records[0], nil
}

// loadRecord loads a deployment record from a file
func (h *HistoryStore) loadRecord(path string) (*DeploymentRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var record DeploymentRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}

	return &record, nil
}

// cleanup removes old history records beyond the retention limit
func (h *HistoryStore) cleanup(service string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	pattern := filepath.Join(h.basePath, fmt.Sprintf("%s_*.json", service))
	files, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	// Sort by modification time (oldest first)
	sort.Slice(files, func(i, j int) bool {
		infoI, _ := os.Stat(files[i])
		infoJ, _ := os.Stat(files[j])
		if infoI == nil || infoJ == nil {
			return false
		}
		return infoI.ModTime().Before(infoJ.ModTime())
	})

	// Remove files beyond retention limit
	if h.retainDays > 0 && len(files) > h.retainDays {
		toRemove := files[:len(files)-h.retainDays]
		for _, file := range toRemove {
			_ = os.Remove(file)
		}
	}
}

// GenerateID creates a unique deployment ID
func GenerateDeploymentID() string {
	return fmt.Sprintf("deploy_%d", time.Now().UnixNano())
}

// NewDeploymentRecord creates a new deployment record with initial values
func NewDeploymentRecord(service, image, version, destination string, hosts []string) *DeploymentRecord {
	return &DeploymentRecord{
		ID:          GenerateDeploymentID(),
		Service:     service,
		Image:       image,
		Version:     version,
		Destination: destination,
		Hosts:       hosts,
		Status:      StatusPending,
		StartedAt:   time.Now(),
		Metadata:    make(map[string]string),
	}
}

// Start marks the deployment as running
func (r *DeploymentRecord) Start() {
	r.Status = StatusRunning
	r.StartedAt = time.Now()
}

// Complete marks the deployment as successful
func (r *DeploymentRecord) Complete() {
	r.Status = StatusSuccess
	r.CompletedAt = time.Now()
	r.Duration = r.CompletedAt.Sub(r.StartedAt)
}

// Fail marks the deployment as failed
func (r *DeploymentRecord) Fail(err error) {
	r.Status = StatusFailed
	r.CompletedAt = time.Now()
	r.Duration = r.CompletedAt.Sub(r.StartedAt)
	if err != nil {
		r.Error = err.Error()
	}
}

// MarkRolledBack marks the deployment as rolled back
func (r *DeploymentRecord) MarkRolledBack() {
	r.Status = StatusRolledBack
	r.RolledBack = true
}
