package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lemonity-org/azud/internal/state"
)

// State is what the agent remembers between checks, per service. Its file stays
// readable by v1, whose parser ignores the fields it does not know.
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

	// RecoverTries and RecoverAfter are where v1 kept an undo's backoff. Load
	// moves them to the journal, so they are empty in anything saved.
	RecoverTries int       `json:"recover_tries,omitempty"`
	RecoverAfter time.Time `json:"recover_after,omitzero"`

	// LastSeenDigest is what the tag pointed to at the last check.
	LastSeenDigest string    `json:"last_seen_digest,omitempty"`
	LastCheckAt    time.Time `json:"last_check_at,omitzero"`

	// Current is the record of the whole release known to be applied. Empty
	// when the deployed digest has no record yet.
	Current string `json:"current,omitempty"`
	// Applied lists records that were applied, newest first, bounded by
	// deploy.retain_history.
	Applied []string `json:"applied,omitempty"`
	// Journal is the transaction in progress. Set means the server may be
	// running parts of two releases.
	Journal *Journal `json:"journal,omitempty"`

	// fromPending reports that Journal holds a v1 pending file, which Save
	// removes once the journal is on disk.
	fromPending bool
}

// Phase is the direction of an open journal.
type Phase string

const (
	// PhaseForward means the journal's target is being applied.
	PhaseForward Phase = "forward"
	// PhaseBack means the target failed and is being undone.
	PhaseBack Phase = "back"
)

// Journal is one transaction in progress, saved before anything changes, so an
// agent that dies at any step resumes or undoes it on its next check.
type Journal struct {
	// Target is the record being applied, and Digest what the tag named for it.
	Target string `json:"target"`
	Digest string `json:"digest"`
	// Previous is the record to return to, and PreviousDigest what the tag named
	// for it. Both are empty when there is none.
	Previous       string `json:"previous,omitempty"`
	PreviousDigest string `json:"previous_digest,omitempty"`
	Phase          Phase  `json:"phase"`
	// First reports that nothing of this service ran before Target, so an undo
	// may stop everything Target started.
	First bool `json:"first,omitempty"`
	// Tries counts failed undos, and NextTry is when the next one may run.
	Tries     int       `json:"tries,omitempty"`
	NextTry   time.Time `json:"next_try,omitzero"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

// Store keeps State in a JSON file under the local Azud state directory.
type Store struct {
	path     string
	releases string
}

// NewStore returns the store for service under stateDir.
func NewStore(stateDir, service string) *Store {
	return &Store{
		path:     filepath.Join(stateDir, "agent", service+".json"),
		releases: filepath.Join(stateDir, "agent", service, "releases"),
	}
}

// Path returns the state file path.
func (s *Store) Path() string {
	return s.path
}

// Records returns the records kept beside this state. repository is the image
// repository a stored v1 release deploys from.
func (s *Store) Records(repository string) *Records {
	return &Records{dir: s.releases, repository: repository}
}

// Load reads the state. A missing file is an empty state: the agent has not
// run for this service yet. Files v1 wrote load as they are: see fromV1.
func (s *Store) Load() (State, error) {
	var st State
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &st); err != nil {
			return State{}, fmt.Errorf("failed to parse agent state %s: %w", s.path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return State{}, fmt.Errorf("failed to read agent state: %w", err)
	}
	if err := s.fromV1(&st); err != nil {
		return State{}, err
	}
	return st, nil
}

// fromV1 fills what v1 did not write. Current follows deployed_digest, which v1
// also writes and which is the only one of the two it would have changed. A
// pending file is an interrupted release, and v1 keeps it only until the release
// ends, so it wins over a journal for another release.
func (s *Store) fromV1(st *State) error {
	if st.DeployedDigest != "" && recordDigest(st.Current) != digestHex(st.DeployedDigest) {
		st.Current = ""
		stored := filepath.Join(s.releases, digestHex(st.DeployedDigest), infoFile)
		if _, err := os.Stat(stored); err == nil {
			st.Current = digestHex(st.DeployedDigest)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to read agent state: %w", err)
		}
	}
	if st.Current != "" && (len(st.Applied) == 0 || st.Applied[0] != st.Current) {
		st.Applied = prependApplied(st.Applied, st.Current, 0)
	}

	data, err := os.ReadFile(s.pendingPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.RecoverTries, st.RecoverAfter = 0, time.Time{}
		return nil
	case err != nil:
		return fmt.Errorf("failed to read agent state: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	st.fromPending = true
	if st.Journal == nil || st.Journal.Digest != lines[0] {
		st.Journal = &Journal{
			Target:  digestHex(lines[0]),
			Digest:  lines[0],
			Phase:   PhaseForward,
			First:   slices.Contains(lines[1:], pendingFirstRelease),
			Tries:   st.RecoverTries,
			NextTry: st.RecoverAfter,
		}
		if info, err := os.Stat(s.pendingPath()); err == nil {
			st.Journal.StartedAt = info.ModTime().UTC()
		}
		if st.Current != "" {
			st.Journal.Previous = st.Current
			st.Journal.PreviousDigest = st.DeployedDigest
		}
	}
	st.RecoverTries, st.RecoverAfter = 0, time.Time{}
	return nil
}

// pendingFirstRelease is the second line of a pending file for a release that
// nothing of this service ran before.
const pendingFirstRelease = "first-release"

func (s *Store) pendingPath() string {
	return filepath.Join(s.releases, "pending")
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
	// The journal must be on disk before anything it protects changes.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("failed to write agent state: %w", err)
	}
	if st.fromPending {
		if err := os.Remove(s.pendingPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove the pending file: %w", err)
		}
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

// prependApplied puts id first, once, and keeps at most limit entries. A limit
// of zero or less keeps them all.
func prependApplied(applied []string, id string, limit int) []string {
	out := []string{id}
	for _, other := range applied {
		if other != id {
			out = append(out, other)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func digestHex(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}

// recordDigest returns the digest hex a record's id starts with.
func recordDigest(id string) string {
	hex, _, _ := strings.Cut(id, "-")
	return hex
}
