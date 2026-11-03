package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/local"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/secretsfile"
	"github.com/lemonity-org/azud/pkg/version"
)

// runAccessoryBootRelease verifies the release a tag or digest names and boots one
// accessory from its spec and secrets, on this machine, so a database can exist before
// the first deploy. It applies nothing else, so a later deploy finds this server as
// it was: it writes no agent state, no record and no runtime secrets file. It runs
// while a journal is open, because it does not read what is running.
func runAccessoryBootRelease(ctx context.Context, log *output.Logger, name, release string) error {
	if err := checkAccessoryReleaseSource(cfg, name); err != nil {
		return err
	}
	if err := checkAgentUser(cfg); err != nil {
		return err
	}
	host, err := agentHost(cfg)
	if err != nil {
		return err
	}
	if accessoryHost != "" && accessoryHost != host {
		return fmt.Errorf("--release boots on this server (%s), not %s", host, accessoryHost)
	}

	repository := imageRepository(cfg.Image)
	repo, err := registry.ParseRepository(repository)
	if err != nil {
		return fmt.Errorf("invalid image: %w", err)
	}
	creds, err := registryCredentials(cfg, repo)
	if err != nil {
		return err
	}
	resolver := newRegistryResolver()
	digest, ref := release, repository+"@"+release
	if !registry.ValidDigest(release) {
		ref = repository + ":" + release
		tag, err := registry.ParseReference(ref)
		if err != nil {
			return fmt.Errorf("invalid release %q: %w", release, err)
		}
		if digest, err = resolver.Digest(ctx, tag, creds); err != nil {
			return fmt.Errorf("failed to resolve %s: %w", ref, err)
		}
	}

	image := repository + "@" + digest
	if verify := agentVerifier(cfg, ref, log); verify != nil {
		if err := verify(ctx, image); err != nil {
			return fmt.Errorf("verification of %s failed: %w", image, err)
		}
	}

	identity, err := agentIdentityPath(cfg)
	if err != nil {
		return err
	}
	source := &agent.BundleSource{
		Server:     cfg,
		Repo:       resolver.Repository(repo.Registry, repo.Repository, creds),
		Repository: repository,
		Identity:   identity,
		Version:    version.Version,
		Log:        log,
	}
	candidate, err := source.Load(ctx, digest, nil)
	if err != nil {
		return err
	}
	accessory, err := accessoryOnThisServer(candidate.Config, name, host, ref)
	if err != nil {
		return err
	}

	// The accessory reads its secrets from a file of its own, so the runtime secrets
	// file stays the transaction's to write.
	boot := *candidate.Config
	if len(accessory.Env.Secret) > 0 {
		secrets, err := source.Secrets(candidate.Record)
		if err != nil {
			return err
		}
		file, remove, err := writeAccessorySecrets(config.RemoteSecretsPath(cfg), name, accessory.Env.Secret, secrets)
		if err != nil {
			return err
		}
		defer remove()
		boot.SecretsRemotePath = file
	}
	cfg = &boot

	runner := local.New(local.Options{
		Context:            ctx,
		User:               cfg.SSH.User,
		CommandTimeout:     cfg.SSH.CommandTimeout,
		LongCommandTimeout: cfg.Deploy.PullTimeout,
		Host:               host,
	})
	defer func() { _ = runner.Close() }()

	log.Info("Starting accessory %s from release %s...", name, digest)
	if err := deployAccessoriesOnHost(runner, log, host, name); err != nil {
		return err
	}
	log.Info("Applied nothing else from this release; the agent deploys it when its tag names it")
	return nil
}

// checkAccessoryReleaseSource refuses a server whose releases carry no accessories.
// A tag naming an image says nothing about an accessory, so verifying it would prove
// nothing about the accessory's spec or secrets.
func checkAccessoryReleaseSource(server *config.Config, name string) error {
	switch {
	case !server.Agent.Configured():
		return fmt.Errorf("--release needs a server with an agent section and agent.source: bundle; to boot from this configuration, run `azud accessory boot %s`", name)
	case !server.Agent.Bundles():
		return fmt.Errorf("--release needs agent.source: bundle. With agent.source: image a release is only an image, which carries no accessory spec or secrets to verify. The accessory's spec is this server's configuration, so boot it with `azud accessory boot %s`, which needs no release", name)
	}
	return nil
}

// accessoryOnThisServer returns the accessory if the release can boot it on host. A
// release that places it elsewhere must not start an empty database here.
func accessoryOnThisServer(release *config.Config, name, host, ref string) (config.AccessoryConfig, error) {
	accessory, ok := release.Accessories[name]
	if !ok {
		return accessory, fmt.Errorf("release %s has no accessory %s", ref, name)
	}
	if hosts := accessoryHosts(accessory); !slices.Contains(hosts, host) {
		return accessory, fmt.Errorf("release %s places accessory %s on %s, not on this server (%s), so nothing was started", ref, name, strings.Join(hosts, ", "), host)
	}
	return accessory, checkAccessoryFiles(release, name, accessory)
}

// writeAccessorySecrets writes the named secrets to a private file beside the runtime
// secrets file, for Podman to read when it creates the container. The returned function
// removes it.
func writeAccessorySecrets(runtimeFile, accessory string, names []string, values map[string]string) (string, func(), error) {
	selected := make(map[string]string, len(names))
	for _, name := range names {
		value, ok := values[name]
		if !ok {
			return "", nil, fmt.Errorf("the release lacks secret %s for accessory %s", name, accessory)
		}
		selected[name] = value
	}
	content, err := secretsfile.Format("", selected)
	if err != nil {
		return "", nil, err
	}
	path, err := localPath(runtimeFile)
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Dir(path)
	if err := secretsfile.EnsureDir(dir); err != nil {
		return "", nil, err
	}
	file, err := os.CreateTemp(dir, ".accessory-"+accessory+"-*")
	if err != nil {
		return "", nil, err
	}
	remove := func() { _ = os.Remove(file.Name()) }
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		remove()
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		remove()
		return "", nil, err
	}
	return file.Name(), remove, nil
}

// checkAccessoryFiles refuses an accessory's files when a bundle specifies it. Each
// names a local path to copy to the host, so a release could read any file this
// user can. A bundle carries no files of its own to copy instead.
func checkAccessoryFiles(release *config.Config, name string, accessory config.AccessoryConfig) error {
	if !release.Agent.Bundles() || len(accessory.Files) == 0 {
		return nil
	}
	return fmt.Errorf("accessories.%s.files is not applied from a release, because it names paths on this machine; put the files in a volume on this server", name)
}
