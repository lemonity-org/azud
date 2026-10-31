package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lemonity-org/azud/internal/state"
)

// State is what the agent remembers between checks, per service.
type State struct {
	// Ref is the registry tag the agent follows, as repository:tag.
	Ref string `json:"ref"`

	// DeployedDigest is the digest the agent last deployed or found running.
	DeployedDigest string    `json:"deployed_digest,omitempty"`
	DeployedAt     time.Time `json:"deployed_at,omitzero"`

	// FailedDigest failed verification or deployment. The agent does not try
	// it again until the tag moves or an operator asks for a retry.
	FailedDigest string    `json:"failed_digest,omitempty"`
	FailedAt     time.Time `json:"failed_at,omitzero"`
	LastError    string    `json:"last_error,omitempty"`

	// LastSeenDigest is what the tag pointed to at the last check.
	LastSeenDigest string    `json:"last_seen_digest,omitempty"`
	LastCheckAt    time.Time `json:"last_check_at,omitzero"`
}

// Store keeps State in a JSON file under the local Azud state directory.
type Store struct {
	path string
}

// NewStore returns the store for service under stateDir.
func NewStore(stateDir, service string) *Store {
	return &Store{path: filepath.Join(stateDir, "agent", service+".json")}
}

// Path returns the state file path.
func (s *Store) Path() string {
	return s.path
}

// Load reads the state. A missing file is an empty state: the agent has not
// run for this service yet.
func (s *Store) Load() (State, error) {
	var st State
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("failed to read agent state: %w", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("failed to parse agent state %s: %w", s.path, err)
	}
	return st, nil
}

// Save replaces the state file atomically with mode 0600.
func (s *Store) Save(st State) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create agent state directory: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	return nil
}

// Lock serializes checks for one service, so a one-off `agent run --once`
// waits for the running agent's check instead of deploying alongside it.
func (s *Store) Lock() (func(), error) {
	lock, err := state.AcquireFileLock(s.path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("failed to lock agent state: %w", err)
	}
	return func() { _ = lock.Release() }, nil
}
