package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/release"
)

// Where a record came from.
const (
	SourceImage  = "image"
	SourceBundle = "bundle"
)

// Why this server accepted a record.
const (
	// OriginVerified means verify_command passed.
	OriginVerified = "verified"
	// OriginVerifySkipped means an operator set insecure_skip_verify.
	OriginVerifySkipped = "verify-skipped"
	// OriginAdopted means the release was already deployed when the agent
	// first needed a record of it.
	OriginAdopted = "adopted"
)

// Files of a stored release. infoFile, notesFile, the configuration files and
// release.SecretsFile are what v1 reads, so they stay where it wrote them.
const (
	recordFile = "record.json"
	infoFile   = "release.json"
	notesFile  = "notes.json"
)

// Record is a release this server accepted, stored whole and never changed, so
// anything stored can be returned to with no network. It exists only if this
// server verified it, an operator skipped verification, or it was adopted.
type Record struct {
	// ID names the record's directory. See bundleRecordID and imageRecordID.
	ID     string `json:"id"`
	Source string `json:"source"`
	Origin string `json:"origin"`
	// Digest is what the tag named: the image in image mode, the bundle in
	// bundle mode.
	Digest string `json:"digest"`
	// Image is what the release deploys, pinned as repository@digest.
	Image       string    `json:"image"`
	Destination string    `json:"destination,omitempty"`
	StoredAt    time.Time `json:"stored_at,omitzero"`

	// Info is the bundle's release.json, and nil in image mode.
	Info *release.Info `json:"-"`
	// Files are the configuration files as written: deploy.yml first, then the
	// destination file if there is one.
	Files []release.File `json:"-"`
	// Sealed is a bundle's encrypted secrets, nil if it has none.
	Sealed []byte `json:"-"`
	// Ignored and NotApplied are what the agent did not take from a bundle.
	Ignored    []string `json:"-"`
	NotApplied []string `json:"-"`
}

// Config returns the configuration the record deploys with: its own files, with
// every trust setting taken from server, the configuration this agent runs with.
func (r *Record) Config(server *config.Config) (*config.Config, []string, error) {
	var base, dest []byte
	for _, f := range r.Files {
		if f.Name == "deploy.yml" {
			base = f.Data
		} else {
			dest = f.Data
		}
	}
	return config.FromRelease(base, dest, server)
}

// bundleNotes is notes.json.
type bundleNotes struct {
	Ignored    []string `json:"ignored,omitempty"`
	NotApplied []string `json:"not_applied,omitempty"`
}

// Records reads and writes the records of one service, in
// <state>/agent/<service>/releases/<id>/.
type Records struct {
	dir string
	// repository pins the image of a stored v1 release, whose release.json
	// names only the digest.
	repository string
}

// Dir returns the directory of the record id.
func (r *Records) Dir(id string) string {
	return filepath.Join(r.dir, id)
}

// Load reads a record, or returns nil when there is none. A release v1 stored
// has no record file, and is a verified bundle: v1 refused to apply one that had
// not passed verify_command.
func (r *Records) Load(id string) (*Record, error) {
	dir := r.Dir(id)
	rec, err := r.loadMeta(id)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.Source == SourceBundle {
		if err := r.loadBundleFiles(rec); err != nil {
			return nil, fmt.Errorf("stored release %s: %w", short(rec.Digest), err)
		}
	}
	names := []string{"deploy.yml"}
	if rec.Destination != "" {
		names = append(names, "deploy."+rec.Destination+".yml")
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) && name != "deploy.yml" {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stored release %s: %w", short(rec.Digest), err)
		}
		rec.Files = append(rec.Files, release.File{Name: name, Data: data})
	}
	return rec, nil
}

func (r *Records) loadMeta(id string) (*Record, error) {
	dir := r.Dir(id)
	data, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err == nil {
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("stored release %s: %w", id, err)
		}
		return &rec, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	info, err := os.Stat(filepath.Join(dir, infoFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Record{
		ID:       id,
		Source:   SourceBundle,
		Origin:   OriginVerified,
		Digest:   "sha256:" + id,
		StoredAt: info.ModTime().UTC(),
	}, nil
}

func (r *Records) loadBundleFiles(rec *Record) error {
	dir := r.Dir(rec.ID)
	infoJSON, err := os.ReadFile(filepath.Join(dir, infoFile))
	if err != nil {
		return err
	}
	var info release.Info
	if err := json.Unmarshal(infoJSON, &info); err != nil {
		return err
	}
	rec.Info = &info
	rec.Destination = info.Destination
	if rec.Image == "" {
		rec.Image = r.repository + "@" + info.Image
	}
	if secrets, err := os.ReadFile(filepath.Join(dir, release.SecretsFile)); err == nil {
		rec.Sealed = secrets
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if data, err := os.ReadFile(filepath.Join(dir, notesFile)); err == nil {
		var notes bundleNotes
		if err := json.Unmarshal(data, &notes); err != nil {
			return err
		}
		rec.Ignored, rec.NotApplied = notes.Ignored, notes.NotApplied
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Store writes a record unless it is already there. Records are immutable, so
// the first one stored under an id stays: re-applying a release does not change
// its origin or notes. It appears all at once, so a crash leaves no half-written
// record. A directory that cannot be read as a record, such as what a v1 agent
// that died while storing left behind, is replaced.
func (r *Records) Store(rec *Record) error {
	if existing, err := r.Load(rec.ID); err == nil && existing != nil {
		return nil
	}
	files, err := rec.files()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(r.dir, ".store-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), data, 0o600); err != nil {
			return err
		}
	}
	dir := r.Dir(rec.ID)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

func (r *Record) files() (map[string][]byte, error) {
	meta, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{recordFile: meta}
	for _, f := range r.Files {
		files[f.Name] = f.Data
	}
	if r.Source != SourceBundle {
		return files, nil
	}
	info, err := json.Marshal(r.Info)
	if err != nil {
		return nil, err
	}
	notes, err := json.Marshal(bundleNotes{Ignored: r.Ignored, NotApplied: r.NotApplied})
	if err != nil {
		return nil, err
	}
	files[infoFile], files[notesFile] = info, notes
	if r.Sealed != nil {
		files[release.SecretsFile] = r.Sealed
	}
	return files, nil
}
