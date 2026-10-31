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
	// deploy deploys image (pinned by digest) with cfg on this server.
	deploy func(ctx context.Context, cfg *config.Config, image string) error
	log    *output.Logger
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

	var notApplied []string
	if prev != nil {
		notApplied = b.unapplied(prev, cfg, values)
	}
	if err := b.store(rel, bundleNotes{Ignored: ignored, NotApplied: notApplied}); err != nil {
		return err
	}

	envPath, err := localSecretsPath(config.RemoteSecretsPath(cfg))
	if err != nil {
		return err
	}
	restore, err := b.swapEnv(envPath, secrets, prev)
	if err != nil {
		return err
	}

	image := imageRepository(cfg.Image) + "@" + rel.Info.Image
	b.log.Info("Release %s: commit %s, image %s", short(rel.Digest), orDash(rel.Info.Commit), short(rel.Info.Image))
	if err := b.deploy(ctx, cfg, image); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restoring the previous secrets file: %w", restoreErr))
		}
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

// swapEnv writes the release's secrets to the runtime secrets file and
// returns a function that puts the previous contents back. The previous
// contents come from the previous release, decrypted again; before the first
// release, the file is copied once to <path>.pre-bundle.
func (b *bundleApplier) swapEnv(path string, secrets []byte, prev *storedRelease) (func() error, error) {
	if secrets == nil {
		return func() error { return nil }, nil
	}
	backup := path + ".pre-bundle"
	if prev == nil {
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if current, err := os.ReadFile(path); err == nil {
				if _, err := secretsfile.WriteFile(backup, current); err != nil {
					return nil, fmt.Errorf("failed to keep the secrets file before the first release: %w", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	if _, err := secretsfile.WriteFile(path, secrets); err != nil {
		return nil, fmt.Errorf("failed to write the secrets file: %w", err)
	}

	return func() error {
		var previous []byte
		switch {
		case prev != nil && prev.Secrets != nil:
			plaintext, err := b.decrypt(prev.Secrets)
			if err != nil {
				return err
			}
			previous = plaintext
		case prev == nil:
			data, err := os.ReadFile(backup)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			previous = data
		default:
			return nil
		}
		_, err := secretsfile.WriteFile(path, previous)
		return err
	}, nil
}

// unapplied lists changes from the previous release that a deploy does not
// apply: they need a push-mode command from a workstation.
func (b *bundleApplier) unapplied(prev *storedRelease, cfg *config.Config, values map[string]string) []string {
	prevCfg, _, err := b.config(prev.Release)
	if err != nil {
		b.log.Debug("Cannot compare with the previous release: %v", err)
		return nil
	}
	var changes []string
	names := make([]string, 0, len(cfg.Accessories)+len(prevCfg.Accessories))
	for name := range cfg.Accessories {
		names = append(names, name)
	}
	for name := range prevCfg.Accessories {
		if _, ok := cfg.Accessories[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	var prevValues map[string]string
	if prev.Secrets != nil {
		if plaintext, err := b.decrypt(prev.Secrets); err == nil {
			prevValues = parseSecrets(plaintext)
		}
	}
	for _, name := range names {
		now, before := cfg.Accessories[name], prevCfg.Accessories[name]
		if !reflect.DeepEqual(now, before) {
			changes = append(changes, fmt.Sprintf("accessory %s changed (azud accessory stop %s, then azud accessory boot %s)", name, name, name))
			continue
		}
		for _, key := range now.Env.Secret {
			if prevValues != nil && prevValues[key] != values[key] {
				changes = append(changes, fmt.Sprintf("secret %s of accessory %s changed; it keeps the old value until it is rebooted", key, name))
			}
		}
	}
	if !reflect.DeepEqual(globalProxy(cfg.Proxy), globalProxy(prevCfg.Proxy)) {
		changes = append(changes, "proxy settings shared by every app on the host changed (azud proxy boot)")
	}
	if !reflect.DeepEqual(cfg.Cron, prevCfg.Cron) {
		changes = append(changes, "cron jobs changed (azud cron boot)")
	}
	if !reflect.DeepEqual(cfg.Podman, prevCfg.Podman) {
		changes = append(changes, "podman settings changed (azud setup)")
	}
	return changes
}

// globalProxy keeps the proxy settings shared by every app on the host. The
// app's own route (hosts, port, health check) is applied by each deploy.
func globalProxy(p config.ProxyConfig) config.ProxyConfig {
	return config.ProxyConfig{
		SSL:            p.SSL,
		SSLRedirect:    p.SSLRedirect,
		ACMEEmail:      p.ACMEEmail,
		ACMEStaging:    p.ACMEStaging,
		SSLCertificate: p.SSLCertificate,
		SSLPrivateKey:  p.SSLPrivateKey,
		HTTPPort:       p.HTTPPort,
		HTTPSPort:      p.HTTPSPort,
		Rootful:        p.Rootful,
	}
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
