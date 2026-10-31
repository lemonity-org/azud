package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/release"
	"github.com/lemonity-org/azud/internal/secretsfile"
)

// bundleApplier deploys release bundles on the server the agent runs on. The
// server's own configuration is the trust anchor: config.FromRelease takes its
// trust settings from it and checks the bundle belongs to this service,
// repository and host.
type bundleApplier struct {
	server         *config.Config
	repo           *registry.Repository
	dir            string // <state dir>/agent/<service>/releases
	identity       string
	allowDowngrade bool
	// deployed returns the digest of the release deployed now, or "".
	deployed func() (string, error)
	// withLock runs fn holding the host's deploy lock, the one pushed
	// deploys take, so a pushed deploy never starts containers while the
	// secrets file holds another release's secrets.
	withLock func(fn func() error) error
	// deploy deploys image (pinned by digest) with cfg on this server. The
	// deploy lock is already held.
	deploy func(ctx context.Context, cfg *config.Config, image string) error
	// accessoryEnv returns the environment an accessory container runs
	// with, or nil when it is not running. Optional.
	accessoryEnv func(name string) (map[string]string, error)
	log          *output.Logger
}

// bundleNotes records what the agent did not apply from a release.
type bundleNotes struct {
	Ignored    []string `json:"ignored,omitempty"`
	NotApplied []string `json:"not_applied,omitempty"`
}

// storedRelease is a release kept on the server after it was applied: its
// files, release.json and the still-encrypted secrets. Restoring the
// previous environment decrypts it again, so no plaintext copy is kept.
type storedRelease struct {
	*release.Release
}

// Deploy applies the bundle that ref (repository@digest) names. The agent has
// verified the digest's signature before calling it.
func (b *bundleApplier) Deploy(ctx context.Context, ref string) error {
	digest := ref[strings.LastIndex(ref, "@")+1:]
	rel, err := release.Fetch(ctx, b.repo, digest)
	if err != nil {
		return err
	}

	prev, err := b.previous()
	if err != nil {
		return err
	}
	if prev != nil && rel.Info.Created.Before(prev.Info.Created) && !b.allowDowngrade {
		return fmt.Errorf("release %s (created %s) is older than the deployed release %s (created %s); to roll back to it, run: azud agent run --once --allow-downgrade",
			short(rel.Digest), rel.Info.Created.Format("2006-01-02 15:04"), short(prev.Digest), prev.Info.Created.Format("2006-01-02 15:04"))
	}

	cfg, ignored, err := b.config(rel)
	if err != nil {
		return err
	}
	var secrets []byte
	if rel.Secrets != nil {
		if secrets, err = b.decrypt(rel.Secrets); err != nil {
			return err
		}
	}
	values := parseSecrets(secrets)
	if missing := missingReleaseSecrets(cfg, values); len(missing) > 0 {
		return fmt.Errorf("release %s lacks secrets its configuration lists: %s", short(rel.Digest), strings.Join(missing, ", "))
	}

	notApplied := b.unapplied(cfg, values)
	if err := b.store(rel, bundleNotes{Ignored: ignored, NotApplied: notApplied}); err != nil {
		return err
	}

	envPath, err := localSecretsPath(config.RemoteSecretsPath(cfg))
	if err != nil {
		return err
	}
	// The agent rolls a failed release back as a whole (secrets, config and
	// image together) below. The deployer's own fleet rollback would restart
	// earlier roles with this release's configuration and secrets.
	deployCfg := *cfg
	deployCfg.Deploy.RollbackOnFailure = false
	image := imageRepository(cfg.Image) + "@" + rel.Info.Image
	b.log.Info("Release %s: commit %s, image %s", short(rel.Digest), orDash(rel.Info.Commit), short(rel.Info.Image))

	err = b.withLock(func() error {
		restore, err := b.swapEnv(envPath, secrets, prev)
		if err != nil {
			return err
		}
		deployErr := b.deploy(ctx, &deployCfg, image)
		if deployErr == nil {
			return nil
		}
		if err := restore(); err != nil {
			return errors.Join(deployErr, fmt.Errorf("restoring the previous secrets file: %w", err))
		}
		if prev == nil {
			b.log.Warn("Restored the secrets file. Roles that switched before the failure keep this release until the next deploy")
			return deployErr
		}
		b.log.Warn("Rolling back to release %s", short(prev.Digest))
		prevCfg, _, err := b.config(prev.Release)
		if err != nil {
			return errors.Join(deployErr, fmt.Errorf("rolling back to %s: %w", short(prev.Digest), err))
		}
		prevDeploy := *prevCfg
		prevDeploy.Deploy.RollbackOnFailure = false
		if err := b.deploy(ctx, &prevDeploy, imageRepository(prevCfg.Image)+"@"+prev.Info.Image); err != nil {
			return errors.Join(deployErr, fmt.Errorf("rolling back to %s: %w", short(prev.Digest), err))
		}
		return deployErr
	})
	if err != nil {
		return err
	}

	if len(ignored) > 0 {
		b.log.Info("Kept this server's settings for %s (the release's values are ignored)", strings.Join(ignored, ", "))
	}
	for _, change := range notApplied {
		b.log.Warn("Not applied: %s", change)
	}
	return nil
}

// config builds the configuration the release deploys with.
func (b *bundleApplier) config(rel *release.Release) (*config.Config, []string, error) {
	var base, dest []byte
	for _, f := range rel.Files {
		if f.Name == "deploy.yml" {
			base = f.Data
		} else {
			dest = f.Data
		}
	}
	return config.FromRelease(base, dest, b.server)
}

// previous returns the deployed release when it is a bundle this agent
// applied, or nil (nothing deployed yet, or an image deployed by tag).
func (b *bundleApplier) previous() (*storedRelease, error) {
	digest, err := b.deployed()
	if err != nil || digest == "" {
		return nil, err
	}
	return b.load(digest)
}

func (b *bundleApplier) releaseDir(digest string) string {
	return filepath.Join(b.dir, strings.TrimPrefix(digest, "sha256:"))
}

// store keeps the release's files, release.json, encrypted secrets and notes.
func (b *bundleApplier) store(rel *release.Release, notes bundleNotes) error {
	dir := b.releaseDir(rel.Digest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := json.Marshal(rel.Info)
	if err != nil {
		return err
	}
	notesJSON, err := json.Marshal(notes)
	if err != nil {
		return err
	}
	files := map[string][]byte{"release.json": info, "notes.json": notesJSON}
	for _, f := range rel.Files {
		files[f.Name] = f.Data
	}
	if rel.Secrets != nil {
		files[release.SecretsFile] = rel.Secrets
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// load reads a stored release, or returns nil when there is none.
func (b *bundleApplier) load(digest string) (*storedRelease, error) {
	dir := b.releaseDir(digest)
	infoJSON, err := os.ReadFile(filepath.Join(dir, "release.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rel := &release.Release{Digest: digest}
	if err := json.Unmarshal(infoJSON, &rel.Info); err != nil {
		return nil, fmt.Errorf("stored release %s: %w", short(digest), err)
	}
	names := []string{"deploy.yml"}
	if rel.Info.Destination != "" {
		names = append(names, "deploy."+rel.Info.Destination+".yml")
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) && name != "deploy.yml" {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stored release %s: %w", short(digest), err)
		}
		rel.Files = append(rel.Files, release.File{Name: name, Data: data})
	}
	if secrets, err := os.ReadFile(filepath.Join(dir, release.SecretsFile)); err == nil {
		rel.Secrets = secrets
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &storedRelease{rel}, nil
}

func (b *bundleApplier) decrypt(ciphertext []byte) ([]byte, error) {
	identity, err := readIdentity(b.identity)
	if err != nil {
		return nil, err
	}
	return release.Decrypt(ciphertext, identity)
}

// swapEnv replaces the runtime secrets file with the release's secrets (an
// empty file for a release without secrets) and returns a function that puts
// back exactly what was there: the previous release's secrets, decrypted
// again, or before the first release the file as it was (kept once in
// <path>.pre-bundle), or no file at all. The previous state is read before
// anything is written, so restoring cannot fail on a missing key later.
func (b *bundleApplier) swapEnv(path string, secrets []byte, prev *storedRelease) (func() error, error) {
	previous, absent, err := b.previousEnv(path, prev)
	if err != nil {
		return nil, err
	}
	if secrets == nil {
		secrets = []byte{}
	}
	if _, err := secretsfile.WriteFile(path, secrets); err != nil {
		return nil, fmt.Errorf("failed to write the secrets file: %w", err)
	}
	return func() error {
		if absent {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		_, err := secretsfile.WriteFile(path, previous)
		return err
	}, nil
}

// previousEnv returns the secrets file contents to restore if this release
// fails, or absent when there was no file before the first release.
func (b *bundleApplier) previousEnv(path string, prev *storedRelease) (contents []byte, absent bool, err error) {
	if prev != nil {
		if prev.Secrets == nil {
			return []byte{}, false, nil
		}
		plaintext, err := b.decrypt(prev.Secrets)
		return plaintext, false, err
	}

	// Before the first release. A marker or backup from an earlier attempt
	// that did not finish wins over the file, which that attempt rewrote.
	backup, marker := path+".pre-bundle", path+".pre-bundle-absent"
	if _, err := os.Stat(marker); err == nil {
		return nil, true, nil
	}
	if data, err := os.ReadFile(backup); err == nil {
		return data, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	current, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := secretsfile.EnsureDir(filepath.Dir(path)); err != nil {
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

// unapplied lists what a deploy leaves as it is: accessories, cron jobs and
// Podman settings that differ from this server's configuration, and
// accessories running with secret values other than the release's. They
// stay listed with every release until they match.
func (b *bundleApplier) unapplied(cfg *config.Config, values map[string]string) []string {
	var changes []string
	names := cfg.GetAccessoryNames()
	for _, name := range b.server.GetAccessoryNames() {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		now, ok := cfg.Accessories[name]
		if !reflect.DeepEqual(now, b.server.Accessories[name]) {
			changes = append(changes, fmt.Sprintf("accessory %s differs from this server's configuration (azud accessory stop %s, then azud accessory boot %s, from a workstation)", name, name, name))
		}
		if !ok || b.accessoryEnv == nil {
			continue
		}
		running, err := b.accessoryEnv(name)
		if err != nil {
			b.log.Debug("Cannot read accessory %s's environment: %v", name, err)
			continue
		}
		for _, key := range now.Env.Secret {
			if running != nil && running[key] != values[key] {
				changes = append(changes, fmt.Sprintf("accessory %s runs with another value of secret %s; it takes the release's value when rebooted", name, key))
			}
		}
	}
	if !reflect.DeepEqual(cfg.Cron, b.server.Cron) {
		changes = append(changes, "cron jobs differ from this server's configuration (azud cron boot)")
	}
	if !reflect.DeepEqual(cfg.Podman, b.server.Podman) {
		changes = append(changes, "podman settings differ from this server's configuration (azud setup)")
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

// agentIdentityPath returns agent.identity, or the default
// ~/.config/azud/agent/<service>.age, with ~ expanded.
func agentIdentityPath(cfg *config.Config) (string, error) {
	path := cfg.Agent.Identity
	if path == "" {
		path = filepath.Join("~", ".config", "azud", "agent", cfg.Service+".age")
	}
	return expandHome(path)
}

// readIdentity reads an age identity file that only its owner can read.
func readIdentity(path string) ([]byte, error) {
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

// localSecretsPath turns the remote secrets path, which may use $HOME or ~,
// into a path on this machine.
func localSecretsPath(path string) (string, error) {
	for _, prefix := range []string{"$HOME/", "${HOME}/"} {
		if strings.HasPrefix(path, prefix) {
			path = "~/" + strings.TrimPrefix(path, prefix)
		}
	}
	return expandHome(path)
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}

func short(digest string) string {
	if len(digest) > len("sha256:")+12 {
		return digest[:len("sha256:")+12]
	}
	return digest
}

// agentReleasesDir is where the agent keeps the releases it applied.
func agentReleasesDir(stateDir, service string) string {
	return filepath.Join(stateDir, "agent", service, "releases")
}

func runAgentInit(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	path, err := agentIdentityPath(cfg)
	if err != nil {
		return err
	}
	log.Header("Agent / %s", cfg.Service)
	if _, err := os.Lstat(path); err == nil {
		identity, err := readIdentity(path)
		if err != nil {
			return err
		}
		recipient := recipientFromIdentityFile(identity)
		log.Info("An identity already exists at %s; it is kept", path)
		if recipient != "" {
			fmt.Println(recipient)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	identity, recipient, err := release.GenerateIdentity()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(identity); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	log.Success("Created %s", path)
	log.Info("Add this recipient to release.recipients in the repository's config/deploy.yml:")
	fmt.Println(recipient)
	return nil
}

// recipientFromIdentityFile returns the public key recorded in an identity
// file's "# public key:" comment, which azud agent init writes.
func recipientFromIdentityFile(identity []byte) string {
	for _, line := range strings.Split(string(identity), "\n") {
		if after, ok := strings.CutPrefix(line, "# public key: "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// bundleStatusRows describes the deployed release bundle for agent status.
func bundleStatusRows(stateDir string, cfg *config.Config, deployed string) [][]string {
	if deployed == "" {
		return nil
	}
	applier := &bundleApplier{dir: agentReleasesDir(stateDir, cfg.Service)}
	rel, err := applier.load(deployed)
	if err != nil || rel == nil {
		return nil
	}
	rows := [][]string{
		{"Release commit", orDash(rel.Info.Commit)},
		{"Release image", rel.Info.Image},
		{"Release secrets", orDash(strings.Join(rel.Info.Secrets, ", "))},
	}
	var notes bundleNotes
	if data, err := os.ReadFile(filepath.Join(applier.releaseDir(deployed), "notes.json")); err == nil {
		_ = json.Unmarshal(data, &notes)
	}
	if len(notes.Ignored) > 0 {
		rows = append(rows, []string{"Server settings kept", strings.Join(notes.Ignored, ", ")})
	}
	for _, change := range notes.NotApplied {
		rows = append(rows, []string{"Not applied", change})
	}
	return rows
}

// accessoryEnvironment reads the environment an accessory container runs
// with, or nil when the container does not exist.
func accessoryEnvironment(podmanClient *podman.Client, service, host string) func(string) (map[string]string, error) {
	return func(name string) (map[string]string, error) {
		container := service + "-" + name
		result, err := podmanClient.Execute(host, "container", "inspect", "--format", "{{json .Config.Env}}", container) // safe: Execute quotes each argv element
		if err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			return nil, nil
		}
		var env []string
		if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &env); err != nil {
			return nil, err
		}
		values := make(map[string]string, len(env))
		for _, kv := range env {
			if key, value, ok := strings.Cut(kv, "="); ok {
				values[key] = value
			}
		}
		return values, nil
	}
}
