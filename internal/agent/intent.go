package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/lemonity-org/azud/internal/state"
)

// Intent is what an operator stopped, so that it stays stopped when the agent restores
// this server. A cron job needs no entry: `azud cron stop` removes its container, and
// the agent starts only containers that exist.
//
// It has a file and a lock of its own, held only to change it, so an operator's command
// neither waits for a deploy nor deadlocks inside a deploy_command script, which runs
// while the agent holds the state lock. A command records intent before it acts, so if
// it dies in between, a restart does what the operator asked for.
type Intent struct {
	Roles       []string `json:"roles,omitempty"`
	Accessories []string `json:"accessories,omitempty"`
}

// operatorStops answers whether an operator has stopped something, from the record as it
// is now, since a restore can wait a long time for the deploy lock.
//
// A stop takes no lock and can come at any moment, but it records itself before it acts.
// So whatever starts something asks before and again after. A stop that came in between
// may have found nothing to stop, and the second answer says to stop it. One that came
// later finds the container running and stops it itself.
//
// A record that cannot be read keeps the last answer, and err says so.
type operatorStops struct {
	store *Store
	last  Intent
	err   error
}

func (o *operatorStops) read() Intent {
	intent, err := o.store.Intent()
	if err != nil {
		o.err = err
		return o.last
	}
	o.last = intent
	return intent
}

func (o *operatorStops) role(name string) bool {
	return slices.Contains(o.read().Roles, name)
}

func (o *operatorStops) accessory(name string) bool {
	return slices.Contains(o.read().Accessories, name)
}

// WithStopped adds names to a list of stopped things.
func WithStopped(list []string, names ...string) []string {
	for _, name := range names {
		if !slices.Contains(list, name) {
			list = append(list, name)
		}
	}
	slices.Sort(list)
	return list
}

// WithStarted removes names from a list of stopped things.
func WithStarted(list []string, names ...string) []string {
	return slices.DeleteFunc(list, func(name string) bool { return slices.Contains(names, name) })
}

func (s *Store) intentPath() string {
	return filepath.Join(filepath.Dir(s.releases), "intent.json")
}

// Intent reads what an operator stopped. A missing file means nothing.
func (s *Store) Intent() (Intent, error) {
	var intent Intent
	data, err := os.ReadFile(s.intentPath())
	if errors.Is(err, os.ErrNotExist) {
		return intent, nil
	}
	if err != nil {
		return intent, fmt.Errorf("failed to read what an operator stopped: %w", err)
	}
	if err := json.Unmarshal(data, &intent); err != nil {
		return Intent{}, fmt.Errorf("failed to parse %s: %w", s.intentPath(), err)
	}
	return intent, nil
}

// UpdateIntent changes what an operator stopped, atomically. When edit changes nothing it
// touches nothing, not even the lock, so a server where nobody stopped anything has no
// files for this.
func (s *Store) UpdateIntent(edit func(*Intent)) error {
	if before, err := s.Intent(); err == nil {
		if _, changed := edited(before, edit); !changed {
			return nil
		}
	}
	path := s.intentPath()
	return state.WithFileLock(path+".lock", func() error {
		before, err := s.Intent()
		if err != nil {
			return err
		}
		after, changed := edited(before, edit)
		if !changed {
			return nil
		}
		data, err := json.Marshal(after)
		if err != nil {
			return err
		}
		return writeFileAtomic(path, data)
	})
}

func edited(before Intent, edit func(*Intent)) (Intent, bool) {
	after := Intent{Roles: slices.Clone(before.Roles), Accessories: slices.Clone(before.Accessories)}
	edit(&after)
	return after, !slices.Equal(before.Roles, after.Roles) || !slices.Equal(before.Accessories, after.Accessories)
}

// writeFileAtomic replaces path with data, with mode 0600, and has it on disk first.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
