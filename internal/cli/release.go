package cli

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/release"
	"github.com/lemonity-org/azud/pkg/version"
)

var releaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Publish signed release bundles for pull-mode servers",
	Long: `Publish release bundles: one OCI artifact per release that carries the
image digest, the configuration files as written, and the runtime secrets
encrypted to the servers in release.recipients.

In CI: build and push the image, then

  azud release publish --image <digest> --digest-file bundle.digest
  cosign sign --yes <repository>@$(cat bundle.digest)
  azud release promote $(cat bundle.digest) --track production

publish never tags, so a tag only ever points at a signed bundle.`,
}

var releasePublishCmd = &cobra.Command{
	Use:   "publish",
	Short: "Build and push a release bundle for an image",
	Long: `Build a release bundle for the image with the given digest and push it to
the image's repository, by digest only.

The bundle carries config/deploy.yml and the destination file (with
--destination) as written, and the runtime secrets that 'azud env push' would
write, loaded with secrets_provider and encrypted to release.recipients. It
prints the bundle digest; sign it, then move a tag with 'azud release promote'.`,
	RunE: runReleasePublish,
}

var releasePromoteCmd = &cobra.Command{
	Use:   "promote <bundle digest>",
	Short: "Point a tag at a published release bundle",
	Long: `Point a tag at a release bundle of this service, after it has been signed.

The registry password is read from the environment (the variable named in
registry.password).`,
	Args: cobra.ExactArgs(1),
	Annotations: map[string]string{
		skipSecretsAnnotation: "true",
	},
	RunE: runReleasePromote,
}

var releaseInspectCmd = &cobra.Command{
	Use:   "inspect <tag | bundle digest>",
	Short: "Show what a release bundle contains",
	Long: `Show a release bundle's image, commit, secret names, recipients and
configuration files. Secret values are never decrypted or shown.

The registry password is read from the environment (the variable named in
registry.password).`,
	Args: cobra.ExactArgs(1),
	Annotations: map[string]string{
		skipSecretsAnnotation: "true",
	},
	RunE: runReleaseInspect,
}

var (
	releaseImage      string
	releaseCommit     string
	releaseDigestFile string
	releaseTrack      string
	releaseShowConfig bool
)

// newRegistryResolver builds the registry client release and agent commands
// use. Tests replace it with one that trusts a test registry.
var newRegistryResolver = func() *registry.Resolver {
	return &registry.Resolver{UserAgent: "azud/" + version.Version}
}

func init() {
	releasePublishCmd.Flags().StringVar(&releaseImage, "image", "", "Digest of the image to release (sha256:...), as printed by the build")
	releasePublishCmd.Flags().StringVar(&releaseCommit, "commit", "", "Commit the release is built from (default: $GITHUB_SHA, then git rev-parse HEAD)")
	releasePublishCmd.Flags().StringVar(&releaseDigestFile, "digest-file", "", "Also write the bundle digest to this file")
	_ = releasePublishCmd.MarkFlagRequired("image")
	releasePromoteCmd.Flags().StringVar(&releaseTrack, "track", "", "Tag to point at the bundle (default: agent.track)")
	releaseInspectCmd.Flags().BoolVar(&releaseShowConfig, "config", false, "Print the configuration files")

	releaseCmd.AddCommand(releasePublishCmd)
	releaseCmd.AddCommand(releasePromoteCmd)
	releaseCmd.AddCommand(releaseInspectCmd)
	rootCmd.AddCommand(releaseCmd)
}

// releaseRepository returns a client for the image's repository, where the
// service's bundles live.
func releaseRepository(cfg *config.Config) (*registry.Repository, error) {
	ref, err := registry.ParseRepository(imageRepository(cfg.Image))
	if err != nil {
		return nil, fmt.Errorf("invalid image: %w", err)
	}
	creds, err := registryCredentials(cfg, ref)
	if err != nil {
		return nil, err
	}
	return newRegistryResolver().Repository(ref.Registry, ref.Repository, creds), nil
}

func runReleasePublish(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger
	ctx := cmd.Context()

	if !registry.ValidDigest(releaseImage) {
		return fmt.Errorf("--image must be the image's digest (sha256:...), not %q", releaseImage)
	}
	if len(cfg.Release.Recipients) == 0 {
		return fmt.Errorf("release.recipients is empty: add each pull-mode server's key (azud agent init)")
	}

	log.Header("Release / %s", cfg.Service)

	repo, err := releaseRepository(cfg)
	if err != nil {
		return err
	}
	image, err := repo.Manifest(ctx, releaseImage)
	if err != nil {
		return fmt.Errorf("image %s: %w", releaseImage, err)
	}

	files, err := releaseConfigFiles()
	if err != nil {
		return err
	}

	loaded, err := loadSecretsForPush()
	if err != nil {
		return fmt.Errorf("failed to load secrets: %w", err)
	}
	secrets := remoteRuntimeSecrets(cfg, loaded)
	if missing := missingReleaseSecrets(cfg, secrets); len(missing) > 0 {
		return fmt.Errorf("secrets listed in the configuration were not loaded: %s", strings.Join(missing, ", "))
	}
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	var encrypted []byte
	if len(secrets) > 0 {
		content, err := remoteSecretsContent(secrets)
		if err != nil {
			return err
		}
		if encrypted, err = release.Encrypt(content, cfg.Release.Recipients); err != nil {
			return err
		}
	}

	info := release.Info{
		Service:     cfg.Service,
		Repository:  repo.Name(),
		Image:       image.Digest,
		Destination: GetDestination(),
		Commit:      releaseCommitID(),
		Created:     time.Now().UTC(),
		Secrets:     names,
		Recipients:  cfg.Release.Recipients,
	}
	bundle, err := release.Build(info, release.Descriptor{MediaType: image.MediaType, Digest: image.Digest, Size: int64(len(image.Body))}, files, encrypted)
	if err != nil {
		return err
	}
	if err := release.Publish(ctx, repo, bundle); err != nil {
		return fmt.Errorf("failed to publish the release: %w", err)
	}

	log.Success("Published %s@%s", repo.Name(), bundle.Digest)
	log.Info("Image %s, %d secret(s) for %d recipient(s), commit %s", image.Digest, len(names), len(cfg.Release.Recipients), orDash(info.Commit))
	if releaseDigestFile != "" {
		if err := os.WriteFile(releaseDigestFile, []byte(bundle.Digest+"\n"), 0o644); err != nil {
			return fmt.Errorf("failed to write %s: %w", releaseDigestFile, err)
		}
	}
	return nil
}

func runReleasePromote(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger
	ctx := cmd.Context()

	digest := args[0]
	if !registry.ValidDigest(digest) {
		return fmt.Errorf("%q is not a bundle digest (sha256:...)", digest)
	}
	track := releaseTrack
	if track == "" {
		track = cfg.Agent.Track
	}
	if track == "" {
		return fmt.Errorf("--track is required (or set agent.track)")
	}

	repo, err := releaseRepository(cfg)
	if err != nil {
		return err
	}
	if err := release.Promote(ctx, repo, digest, track, cfg.Service); err != nil {
		return err
	}
	log.Success("%s:%s -> %s", repo.Name(), track, digest)
	return nil
}

func runReleaseInspect(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger
	ctx := cmd.Context()

	repo, err := releaseRepository(cfg)
	if err != nil {
		return err
	}
	digest := args[0]
	if !registry.ValidDigest(digest) {
		m, err := repo.Manifest(ctx, digest)
		if err != nil {
			return err
		}
		digest = m.Digest
	}
	rel, err := release.Fetch(ctx, repo, digest)
	if err != nil {
		return err
	}

	log.Header("Release / %s", rel.Info.Service)
	fileNames := make([]string, 0, len(rel.Files))
	for _, f := range rel.Files {
		fileNames = append(fileNames, f.Name+" ("+strconv.Itoa(len(f.Data))+" bytes)")
	}
	log.Table([]string{"Field", "Value"}, [][]string{
		{"Bundle", repo.Name() + "@" + rel.Digest},
		{"Image", rel.Info.Image},
		{"Commit", orDash(rel.Info.Commit)},
		{"Destination", orDash(rel.Info.Destination)},
		{"Created", rel.Info.Created.Local().Format(time.RFC3339)},
		{"Secrets", orDash(strings.Join(rel.Info.Secrets, ", "))},
		{"Recipients", orDash(strings.Join(rel.Info.Recipients, ", "))},
		{"Config", strings.Join(fileNames, ", ")},
	})
	if releaseShowConfig {
		for _, f := range rel.Files {
			fmt.Printf("\n# %s\n%s", f.Name, f.Data)
		}
	}
	return nil
}

// releaseConfigFiles reads the configuration file and the destination file,
// if any, as written. They are named deploy.yml and deploy.<destination>.yml
// in the bundle whatever their local names.
func releaseConfigFiles() ([]release.File, error) {
	path := configPath
	if path == "" {
		path = findConfigFile()
	}
	if path == "" {
		return nil, fmt.Errorf("no configuration file found")
	}
	files := []release.File{}
	data, err := readReleaseFile(path)
	if err != nil {
		return nil, err
	}
	files = append(files, release.File{Name: "deploy.yml", Data: data})

	if dest := GetDestination(); dest != "" {
		destPath, err := config.DestinationPath(path, dest)
		if err != nil {
			return nil, err
		}
		data, err := readReleaseFile(destPath)
		switch {
		case err == nil:
			files = append(files, release.File{Name: "deploy." + dest + ".yml", Data: data})
		case !os.IsNotExist(err):
			return nil, err
		}
	}
	return files, nil
}

func readReleaseFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > release.MaxConfigBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, release.MaxConfigBytes)
	}
	return os.ReadFile(path)
}

// missingReleaseSecrets lists the secrets the app or an accessory needs that
// the bundle would not carry.
func missingReleaseSecrets(cfg *config.Config, secrets map[string]string) []string {
	needed := append([]string(nil), cfg.Env.Secret...)
	for _, accessory := range cfg.Accessories {
		needed = append(needed, accessory.Env.Secret...)
	}
	var missing []string
	for _, name := range needed {
		if _, ok := secrets[name]; !ok && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

func releaseCommitID() string {
	if releaseCommit != "" {
		return releaseCommit
	}
	if sha := os.Getenv("GITHUB_SHA"); sha != "" {
		return sha
	}
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
