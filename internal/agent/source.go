package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/release"
	"github.com/lemonity-org/azud/internal/secretsfile"
)

// Candidate is what a Source produces. Everything after it is the same for both
// sources, so there is one transaction.
type Candidate struct {
	Record *Record
	Config *config.Config
	// Move is set when an operator chose this release over what the tag names, and
	// goes into the journal before anything changes.
	Move *moveIntent
}

// Source is the only place image and bundle differ.
type Source interface {
	// Kind returns the source of the records this Source deploys.
	Kind() string
	// Load produces the candidate digest names. The agent has verified digest,
	// and previous is the record a failure would return to. A nil previous skips the
	// downgrade check, because only an operator's move passes it, and a shell on this
	// server already has that authority. An error means the release is refused, and
	// nothing has changed.
	Load(ctx context.Context, digest string, previous *Record) (*Candidate, error)
	// Reapply produces the candidate that re-applies what is deployed, given its record
	// (current, possibly nil) and digest. It needs no registry.
	Reapply(current *Record, deployed string) (*Candidate, error)
	// Previous returns the record a failure returns to, given the record of what
	// is deployed (current, possibly nil) and the deployed digest. A new record
	// it returns is adopted, and nil means there is nothing to return to.
	Previous(current *Record, deployed string) (*Record, error)
	// Activate puts in place what a deploy of rec reads outside its record.
	Activate(rec, previous *Record) error
	// Restore puts back what Activate replaced. previous may be nil.
	Restore(previous *Record) error
}

// ImageSource deploys images the tag names, with the configuration on disk when the
// release is applied. So an edit applies with `azud redeploy` or the next release, and a
// running agent cannot put back what it read when it started. Trust settings stay those
// of Server until the agent restarts.
type ImageSource struct {
	// Server is the configuration the agent loaded when it started.
	Server *config.Config
	// Files are the files Server was loaded from, with environment references
	// resolved. ReadConfigFiles returns them.
	Files []release.File
	// Reload returns the files as they are now, as ReadConfigFiles does. With none, the
	// files are Files.
	Reload      func() ([]release.File, error)
	Destination string
	Repository  string
}

// ReadConfigFiles returns the configuration on disk as written, named as a
// release names them: deploy.yml, then deploy.<destination>.yml if it exists.
func ReadConfigFiles(path, destination string) ([]release.File, error) {
	base, err := readResolvedConfig(path)
	if err != nil {
		return nil, err
	}
	files := []release.File{{Name: "deploy.yml", Data: base}}
	if destination == "" {
		return files, nil
	}
	destPath, err := config.DestinationPath(path, destination)
	if err != nil {
		return nil, err
	}
	data, err := readResolvedConfig(destPath)
	if errors.Is(err, os.ErrNotExist) {
		return files, nil
	}
	if err != nil {
		return nil, err
	}
	return append(files, release.File{Name: "deploy." + destination + ".yml", Data: data}), nil
}

// readResolvedConfig reads a configuration file with its environment references
// resolved, so a record keeps the settings it deployed with: an undo must not start
// the previous image with what the environment holds by then.
func readResolvedConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	resolved, err := config.ResolveEnvReferences(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return resolved, nil
}

// Kind returns SourceImage.
func (s *ImageSource) Kind() string { return SourceImage }

// Load returns the image with the configuration on disk. Server is that configuration
// until the files change. A file that no longer reads refuses the release.
func (s *ImageSource) Load(_ context.Context, digest string, _ *Record) (*Candidate, error) {
	rec, err := s.record(digest, "")
	if err != nil {
		return nil, err
	}
	if rec.HasFiles(s.Files) {
		return &Candidate{Record: rec, Config: s.Server}, nil
	}
	cfg, _, err := rec.Config(s.Server)
	if err != nil {
		return nil, err
	}
	return &Candidate{Record: rec, Config: cfg}, nil
}

// Reapply returns what is deployed with the configuration on disk, so an edit makes a
// new record for the same digest. The configuration comes from the record, so a later
// read of the file cannot change what the record means. The origin is what is deployed's,
// since the image is the same.
func (s *ImageSource) Reapply(current *Record, deployed string) (*Candidate, error) {
	origin := OriginAdopted
	if current != nil {
		if current.Source != SourceImage {
			return nil, fmt.Errorf("release %s is a bundle, and this server follows images. To apply this server's configuration, move the tag to an image", short(current.Digest))
		}
		origin = current.Origin
	}
	rec, err := s.record(deployed, origin)
	if err != nil {
		return nil, err
	}
	cfg, _, err := rec.Config(s.Server)
	if err != nil {
		return nil, err
	}
	return &Candidate{Record: rec, Config: cfg}, nil
}

// Previous returns current, or adopts the deployed digest with today's
// configuration: v1 kept no record of what it deployed, nor does a pushed deploy.
func (s *ImageSource) Previous(current *Record, deployed string) (*Record, error) {
	if current != nil {
		return current, nil
	}
	if deployed == "" {
		return nil, nil
	}
	return s.record(deployed, OriginAdopted)
}

// Activate does nothing: image mode leaves the secrets file to the operator.
func (s *ImageSource) Activate(_, _ *Record) error { return nil }

// Restore does nothing: an undo in image mode does not revert secrets, which
// would mean keeping each release's plaintext.
func (s *ImageSource) Restore(*Record) error { return nil }

func (s *ImageSource) record(digest, origin string) (*Record, error) {
	files := s.Files
	if s.Reload != nil {
		var err error
		if files, err = s.Reload(); err != nil {
			return nil, fmt.Errorf("reading this server's configuration: %w", err)
		}
	}
	return &Record{
		ID:          imageRecordID(digest, files),
		Source:      SourceImage,
		Origin:      origin,
		Digest:      digest,
		Image:       s.Repository + "@" + digest,
		Destination: s.Destination,
		Files:       files,
	}, nil
}

// imageRecordID names an image release: <digest hex>-<8 hex of the configuration
// files' hash>, since image mode can apply one digest with edited configuration.
func imageRecordID(digest string, files []release.File) string {
	sum := sha256.New()
	for _, f := range files {
		sum.Write(f.Data)
	}
	return digestHex(digest) + "-" + hex.EncodeToString(sum.Sum(nil))[:8]
}

// bundleRecordID names a bundle: its digest hex.
func bundleRecordID(digest string) string {
	return digestHex(digest)
}

// BundleSource deploys release bundles. The server's own configuration is the
// trust anchor: config.FromRelease takes its trust settings from it and checks
// the bundle belongs to this service, repository and host.
type BundleSource struct {
	Server *config.Config
	Repo   *registry.Repository
	// Repository is the image repository, which pins the release's image.
	Repository string
	// SecretsPath is the runtime secrets file every release replaces.
	SecretsPath string
	// Identity is the age identity file that decrypts a release's secrets.
	Identity       string
	AllowDowngrade bool
	// Version is the running azud version, checked against a release's
	// minimum_version.
	Version string
	// AccessoryEnv returns the environment an accessory container runs with,
	// or nil when it is not running. Optional.
	AccessoryEnv func(name string) (map[string]string, error)
	Log          *output.Logger
}

// Kind returns SourceBundle.
func (s *BundleSource) Kind() string { return SourceBundle }

// Reapply returns the stored bundle as it is: its configuration is what was signed, and
// nothing on this server edits it.
func (s *BundleSource) Reapply(current *Record, _ string) (*Candidate, error) {
	if current == nil || current.Source != SourceBundle {
		return nil, errors.New("what is deployed has no stored bundle to apply again. Move the tag to a release")
	}
	cfg, _, err := current.Config(s.Server)
	if err != nil {
		return nil, err
	}
	return &Candidate{Record: current, Config: cfg}, nil
}

// Load fetches the bundle digest names and checks it can be applied, before
// anything changes.
func (s *BundleSource) Load(ctx context.Context, digest string, previous *Record) (*Candidate, error) {
	rel, err := release.Fetch(ctx, s.Repo, digest)
	if err != nil {
		return nil, err
	}
	if previous != nil && previous.Info != nil && rel.Info.Created.Before(previous.Info.Created) && !s.AllowDowngrade {
		return nil, fmt.Errorf("release %s (created %s) is older than the deployed release %s (created %s). To bring it back, publish it again with `azud release republish` and promote what that prints; on this server, run `azud rollback %s` or `azud agent run --once --allow-downgrade`",
			short(rel.Digest), rel.Info.Created.Format("2006-01-02 15:04"), short(previous.Digest), previous.Info.Created.Format("2006-01-02 15:04"), rel.Digest)
	}

	rec := &Record{
		ID:          bundleRecordID(rel.Digest),
		Source:      SourceBundle,
		Digest:      rel.Digest,
		Image:       s.Repository + "@" + rel.Info.Image,
		Destination: rel.Info.Destination,
		Info:        &rel.Info,
		Files:       rel.Files,
		Sealed:      rel.Secrets,
	}
	for _, f := range rel.Files {
		data, err := config.ResolveEnvReferences(f.Data)
		if err != nil {
			return nil, fmt.Errorf("release %s: %s: %w", short(rel.Digest), f.Name, err)
		}
		rec.Resolved = append(rec.Resolved, release.File{Name: f.Name, Data: data})
	}
	cfg, ignored, err := rec.Config(s.Server)
	if err != nil {
		return nil, err
	}
	if err := config.ValidateMinimumVersion(cfg, s.Version); err != nil {
		return nil, fmt.Errorf("release %s: %w", short(rel.Digest), err)
	}
	secrets, err := s.open(rel.Secrets)
	if err != nil {
		return nil, err
	}
	values := parseSecrets(secrets)
	if missing := release.MissingSecrets(cfg, values); len(missing) > 0 {
		return nil, fmt.Errorf("release %s lacks secrets its configuration lists: %s", short(rel.Digest), strings.Join(missing, ", "))
	}
	rec.Ignored, rec.NotApplied = ignored, s.unapplied(cfg, values)
	s.Log.Info("Release %s: commit %s, image %s", short(rel.Digest), orDash(rel.Info.Commit), short(rel.Info.Image))
	return &Candidate{Record: rec, Config: cfg}, nil
}

// Previous returns current when it is a bundle this agent applied. Anything else,
// such as an image deployed by tag, has no secrets to put back and no release to
// return to as a whole.
func (s *BundleSource) Previous(current *Record, _ string) (*Record, error) {
	if current != nil && current.Source == SourceBundle {
		return current, nil
	}
	return nil, nil
}

// Activate replaces the runtime secrets file with the release's secrets (an empty
// file for a release without secrets). What Restore needs is read before anything
// is written, so restoring cannot fail on a missing key later.
func (s *BundleSource) Activate(rec, previous *Record) error {
	if _, _, err := s.previousEnv(previous); err != nil {
		return err
	}
	secrets, err := s.open(rec.Sealed)
	if err != nil {
		return err
	}
	if secrets == nil {
		secrets = []byte{}
	}
	if _, err := secretsfile.WriteFile(s.SecretsPath, secrets); err != nil {
		return fmt.Errorf("failed to write the secrets file: %w", err)
	}
	return nil
}

// Restore puts back exactly what the file held without the failed release: the
// previous release's secrets, decrypted again, or before the first release the
// file as it was (kept once in <path>.pre-bundle), or no file at all.
func (s *BundleSource) Restore(previous *Record) error {
	contents, absent, err := s.previousEnv(previous)
	if err != nil {
		return err
	}
	if absent {
		if err := os.Remove(s.SecretsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if _, err := secretsfile.WriteFile(s.SecretsPath, contents); err != nil {
		return err
	}
	s.Log.Warn("Restored the secrets file")
	return nil
}

// previousEnv returns the secrets file contents to restore if this release
// fails, or absent when there was no file before the first release.
func (s *BundleSource) previousEnv(previous *Record) (contents []byte, absent bool, err error) {
	if previous != nil && previous.Source == SourceBundle {
		if previous.Sealed == nil {
			return []byte{}, false, nil
		}
		plaintext, err := s.decrypt(previous.Sealed)
		return plaintext, false, err
	}

	// Before the first release. A marker or backup from an earlier attempt
	// that did not finish wins over the file, which that attempt rewrote.
	backup, marker := s.SecretsPath+".pre-bundle", s.SecretsPath+".pre-bundle-absent"
	if _, err := os.Stat(marker); err == nil {
		return nil, true, nil
	}
	if data, err := os.ReadFile(backup); err == nil {
		return data, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	current, err := os.ReadFile(s.SecretsPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := secretsfile.EnsureDir(filepath.Dir(s.SecretsPath)); err != nil {
			return nil, false, err
		}
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if _, err := secretsfile.WriteFile(backup, current); err != nil {
		return nil, false, fmt.Errorf("failed to keep the secrets file before the first release: %w", err)
	}
	return current, false, nil
}

// Secrets returns rec's decrypted secrets by name.
func (s *BundleSource) Secrets(rec *Record) (map[string]string, error) {
	plaintext, err := s.open(rec.Sealed)
	if err != nil {
		return nil, err
	}
	return parseSecrets(plaintext), nil
}

// open decrypts a release's secrets, and returns nil for a release without any.
func (s *BundleSource) open(sealed []byte) ([]byte, error) {
	if sealed == nil {
		return nil, nil
	}
	return s.decrypt(sealed)
}

func (s *BundleSource) decrypt(ciphertext []byte) ([]byte, error) {
	identity, err := ReadIdentity(s.Identity)
	if err != nil {
		return nil, err
	}
	return release.Decrypt(ciphertext, identity)
}

// unapplied lists what a deploy leaves as it is: accessories and cron jobs that
// differ from this server's configuration, and accessories running with secret
// values other than the release's. They stay listed with every release until
// they match.
func (s *BundleSource) unapplied(cfg *config.Config, values map[string]string) []string {
	var changes []string
	names := cfg.GetAccessoryNames()
	for _, name := range s.Server.GetAccessoryNames() {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		now, ok := cfg.Accessories[name]
		if !reflect.DeepEqual(now, s.Server.Accessories[name]) {
			changes = append(changes, fmt.Sprintf("accessory %s differs from this server's configuration (azud accessory remove %s, then azud accessory boot %s)", name, name, name))
		}
		if !ok || s.AccessoryEnv == nil {
			continue
		}
		running, err := s.AccessoryEnv(name)
		if err != nil {
			s.Log.Debug("Cannot read accessory %s's environment: %v", name, err)
			continue
		}
		for _, key := range now.Env.Secret {
			if running != nil && running[key] != values[key] {
				changes = append(changes, fmt.Sprintf("accessory %s runs with another value of secret %s; it takes the release's value when rebooted", name, key))
			}
		}
	}
	if !reflect.DeepEqual(cfg.Cron, s.Server.Cron) {
		changes = append(changes, "cron jobs differ from this server's configuration (azud cron boot)")
	}
	return changes
}

// parseSecrets reads KEY=VALUE lines, as azud's secrets files hold them.
func parseSecrets(data []byte) map[string]string {
	values := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), release.MaxSecretsBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(key)] = value
		}
	}
	return values
}

// ReadIdentity reads an age identity file that only its owner can read.
func ReadIdentity(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no age identity at %s: run azud agent init on this server", path)
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("age identity %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("age identity %s is readable by others (mode %04o); run chmod 600 %s", path, info.Mode().Perm(), path)
	}
	return os.ReadFile(path)
}

func short(digest string) string {
	if len(digest) > len("sha256:")+12 {
		return digest[:len("sha256:")+12]
	}
	return digest
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
