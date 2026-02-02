package cli

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/podman"
	"github.com/adriancarayol/azud/internal/ssh"
)

var buildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build and push the application image",
	Long: `Build the container image and push it to the registry.

This command builds the container image locally (or on a remote builder) and
pushes it to the configured registry.

Example:
  azud build                    # Build and push
  azud build --no-push          # Build only, don't push
  azud build --no-cache         # Build without cache`,
	RunE: runBuild,
}

var (
	buildNoPush  bool
	buildNoCache bool
	buildPull    bool
)

func init() {
	buildCmd.Flags().BoolVar(&buildNoPush, "no-push", false, "Don't push the image after building")
	buildCmd.Flags().BoolVar(&buildNoCache, "no-cache", false, "Don't use cache when building")
	buildCmd.Flags().BoolVar(&buildPull, "pull", false, "Always pull the base image")

	rootCmd.AddCommand(buildCmd)
}

func runBuild(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger
	timer := log.NewTimer("Build")

	// Generate version tag using template (supports {destination}, {version}, {timestamp})
	dest := GetDestination()
	imageTag := generateImageTag(cfg.Image, dest)
	latestTag := fmt.Sprintf("%s:latest", cfg.Image)

	// Add destination suffix to latest tag if destination is specified
	if dest != "" {
		latestTag = fmt.Sprintf("%s:%s-latest", cfg.Image, dest)
	}

	log.Header("Building %s", imageTag)

	// Run pre-build hook
	hooks := newHookRunner()
	hookCtx := newHookContext()
	hookCtx.Image = imageTag
	hookCtx.Version = generateVersion()
	if err := hooks.Run(cmd.Context(), "pre-build", hookCtx); err != nil {
		return fmt.Errorf("pre-build hook failed: %w", err)
	}

	multiarch := isMultiarchBuild()

	// Check if we should use remote builder
	if cfg.Builder.Remote.Host != "" {
		version := generateVersion()
		if err := buildRemote(imageTag, latestTag, version, multiarch); err != nil {
			return err
		}
		if err := hooks.Run(cmd.Context(), "post-build", hookCtx); err != nil {
			log.Warn("post-build hook failed: %v", err)
		}
		return nil
	}

	// Build locally
	if err := buildLocal(imageTag, latestTag, multiarch); err != nil {
		return err
	}

	// Run post-build hook
	if err := hooks.Run(cmd.Context(), "post-build", hookCtx); err != nil {
		log.Warn("post-build hook failed: %v", err)
	}

	// Push to registry
	if !buildNoPush {
		log.Info("Pushing image to registry...")
		if err := pushImage(imageTag, latestTag, multiarch); err != nil {
			return err
		}
		log.Success("Image pushed successfully")
	}

	timer.Stop()
	log.Success("Build complete: %s", imageTag)
	return nil
}

func buildLocal(imageTag, latestTag string, multiarch bool) error {
	log := output.DefaultLogger

	platforms, err := resolveBuildPlatforms(false)
	if err != nil {
		return err
	}

	cacheFrom, cacheTo := resolveCacheSpecs(cfg.Builder.Cache)

	if multiarch {
		buildConfig := &podman.ManifestBuildConfig{
			BuildConfig: podman.BuildConfig{
				Context:    cfg.Builder.Context,
				Dockerfile: cfg.Builder.Dockerfile,
				Tag:        imageTag,
				Tags:       []string{latestTag},
				Args:       cfg.Builder.Args,
				NoCache:    buildNoCache,
				Pull:       buildPull,
				Secrets:    cfg.Builder.Secrets,
				CacheFrom:  cacheFrom,
				CacheTo:    cacheTo,
				Target:     cfg.Builder.Target,
				SSH:        cfg.Builder.SSH,
			},
			Platforms: platforms,
			Push:      false,
		}

		commands := buildConfig.ManifestBuildCommands()
		if len(commands) == 0 {
			return fmt.Errorf("no build commands generated (check builder.platforms)")
		}

		for _, cmd := range commands {
			log.Command(cmd)
			buildCmd := exec.Command("sh", "-c", cmd)
			buildCmd.Stdout = os.Stdout
			buildCmd.Stderr = os.Stderr
			if err := buildCmd.Run(); err != nil {
				return fmt.Errorf("build failed: %w", err)
			}
		}

		return nil
	}

	// Prepare build arguments
	args := []string{"build"}

	// Dockerfile
	dockerfile := cfg.Builder.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	args = append(args, "-f", dockerfile)

	// Tags
	args = append(args, "-t", imageTag)
	args = append(args, "-t", latestTag)

	// Build args from config
	for key, value := range cfg.Builder.Args {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
	}

	// Platform
	if arch := effectiveBuildArch(false); arch != "" {
		args = append(args, "--platform", fmt.Sprintf("linux/%s", arch))
	}

	if cfg.Builder.Target != "" {
		args = append(args, "--target", cfg.Builder.Target)
	}

	for _, sshSpec := range cfg.Builder.SSH {
		args = append(args, "--ssh", sshSpec)
	}

	// Build secrets
	for _, secret := range cfg.Builder.Secrets {
		args = append(args, "--secret", secret)
	}

	for _, cache := range cacheFrom {
		args = append(args, "--cache-from", cache)
	}
	if cacheTo != "" {
		args = append(args, "--cache-to", cacheTo)
	}

	// Cache settings
	if buildNoCache {
		args = append(args, "--no-cache")
	}

	if buildPull {
		args = append(args, "--pull")
	}

	// Context
	context := cfg.Builder.Context
	if context == "" {
		context = "."
	}
	args = append(args, context)

	log.Info("Running podman build...")
	log.Command("podman " + strings.Join(args, " "))

	// Execute build
	buildCmd := exec.Command("podman", args...)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr

	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	return nil
}

func buildRemote(imageTag, latestTag, version string, multiarch bool) error {
	log := output.DefaultLogger
	log.Info("Building on remote builder: %s", cfg.Builder.Remote.Host)

	// Create SSH client for remote builder
	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	imageManager := podman.NewImageManager(podmanClient)

	if cfg.Registry.Username != "" {
		if err := loginToRegistryRemote(sshClient, cfg.Builder.Remote.Host); err != nil {
			return fmt.Errorf("remote registry login failed: %w", err)
		}
	}

	platforms, err := resolveBuildPlatforms(true)
	if err != nil {
		return err
	}

	cacheFrom, cacheTo := resolveCacheSpecs(cfg.Builder.Cache)

	if !multiarch {
		buildConfig := &podman.BuildConfig{
			Context:    cfg.Builder.Context,
			Dockerfile: cfg.Builder.Dockerfile,
			Tag:        imageTag,
			Tags:       []string{latestTag},
			Args:       cfg.Builder.Args,
			NoCache:    buildNoCache,
			Pull:       buildPull,
			Secrets:    cfg.Builder.Secrets,
			CacheFrom:  cacheFrom,
			CacheTo:    cacheTo,
		}
		if arch := effectiveBuildArch(true); arch != "" {
			buildConfig.Platform = fmt.Sprintf("linux/%s", arch)
		}
		buildConfig.Target = cfg.Builder.Target
		buildConfig.SSH = cfg.Builder.SSH

		if err := imageManager.Build(cfg.Builder.Remote.Host, buildConfig); err != nil {
			return fmt.Errorf("remote build failed: %w", err)
		}

		if !buildNoPush {
			if err := pushRemoteImage(sshClient, cfg.Builder.Remote.Host, imageTag, latestTag, false); err != nil {
				return fmt.Errorf("remote push failed: %w", err)
			}
		}

		log.Success("Remote build complete")
		return nil
	}

	// Prepare multi-platform manifest build config
	buildConfig := &podman.ManifestBuildConfig{
		BuildConfig: podman.BuildConfig{
			Context:    cfg.Builder.Context,
			Dockerfile: cfg.Builder.Dockerfile,
			Tag:        imageTag,
			Tags:       []string{latestTag},
			Args:       cfg.Builder.Args,
			NoCache:    buildNoCache,
			Pull:       buildPull,
			Secrets:    cfg.Builder.Secrets,
			CacheFrom:  cacheFrom,
			CacheTo:    cacheTo,
		},
		Platforms: platforms,
		Push:      !buildNoPush,
	}

	buildConfig.Target = cfg.Builder.Target
	buildConfig.SSH = cfg.Builder.SSH

	// First, we need to sync the build context to the remote builder
	// For now, we'll assume the code is already on the remote builder
	// In a production implementation, you'd sync the context first

	if err := imageManager.ManifestBuild(cfg.Builder.Remote.Host, buildConfig); err != nil {
		return fmt.Errorf("remote build failed: %w", err)
	}

	log.Success("Remote build complete")
	return nil
}

func pushImage(imageTag, latestTag string, multiarch bool) error {
	log := output.DefaultLogger

	// Login to registry first
	if cfg.Registry.Username != "" {
		if err := loginToRegistry(); err != nil {
			return fmt.Errorf("registry login failed: %w", err)
		}
	}

	// Push version tag
	log.Info("Pushing %s...", imageTag)
	pushArgs := []string{"push", imageTag}
	if multiarch {
		pushArgs = []string{"manifest", "push", imageTag, imageTag}
	}
	pushCmd := exec.Command("podman", pushArgs...)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("failed to push %s: %w", imageTag, err)
	}

	// Push latest tag
	log.Info("Pushing %s...", latestTag)
	if multiarch {
		pushCmd = exec.Command("podman", "manifest", "push", imageTag, latestTag)
	} else {
		pushCmd = exec.Command("podman", "push", latestTag)
	}
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("failed to push %s: %w", latestTag, err)
	}

	return nil
}

func pushRemoteImage(sshClient *ssh.Client, host, imageTag, latestTag string, multiarch bool) error {
	pushCmd := fmt.Sprintf("podman push %s", imageTag)
	if multiarch {
		pushCmd = fmt.Sprintf("podman manifest push %s %s", imageTag, imageTag)
	}
	if result, err := sshClient.Execute(host, pushCmd); err != nil {
		return err
	} else if result.ExitCode != 0 {
		return fmt.Errorf("failed to push %s: %s", imageTag, result.Stderr)
	}

	var latestCmd string
	if multiarch {
		latestCmd = fmt.Sprintf("podman manifest push %s %s", imageTag, latestTag)
	} else {
		latestCmd = fmt.Sprintf("podman push %s", latestTag)
	}
	if result, err := sshClient.Execute(host, latestCmd); err != nil {
		return err
	} else if result.ExitCode != 0 {
		return fmt.Errorf("failed to push %s: %s", latestTag, result.Stderr)
	}

	return nil
}

func loginToRegistryRemote(sshClient *ssh.Client, host string) error {
	server := cfg.Registry.Server
	if server == "" {
		server = "docker.io"
	}

	password := ""
	if len(cfg.Registry.Password) > 0 {
		secretKey := cfg.Registry.Password[0]
		password = os.Getenv(secretKey)
		if password == "" {
			if p, ok := getSecret(secretKey); ok {
				password = p
			}
		}
	}

	if password == "" {
		return fmt.Errorf("registry password not found")
	}

	cmd := fmt.Sprintf("podman login --username %s --password-stdin %s", shellQuote(cfg.Registry.Username), shellQuote(server))
	result, err := sshClient.ExecuteWithStdin(host, cmd, strings.NewReader(password+"\n"))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman login failed: %s", result.Stderr)
	}
	return nil
}

func isMultiarchBuild() bool {
	return cfg.Builder.Multiarch || len(cfg.Builder.Platforms) > 0
}

func resolveBuildPlatforms(remote bool) ([]string, error) {
	if len(cfg.Builder.Platforms) > 0 {
		return cfg.Builder.Platforms, nil
	}
	if arch := effectiveBuildArch(remote); arch != "" {
		return []string{fmt.Sprintf("linux/%s", arch)}, nil
	}
	if cfg.Builder.Multiarch {
		return []string{"linux/amd64", "linux/arm64"}, nil
	}
	return nil, nil
}

func effectiveBuildArch(remote bool) string {
	if cfg.Builder.Arch != "" {
		return cfg.Builder.Arch
	}
	if remote && cfg.Builder.Remote.Arch != "" {
		return cfg.Builder.Remote.Arch
	}
	return ""
}

func resolveCacheSpecs(cache config.CacheConfig) ([]string, string) {
	opts := make(map[string]string, len(cache.Options))
	for k, v := range cache.Options {
		opts[k] = v
	}

	fromOverride := strings.TrimSpace(opts["from"])
	toOverride := strings.TrimSpace(opts["to"])
	delete(opts, "from")
	delete(opts, "to")

	spec := buildCacheSpec(cache.Type, opts)
	if spec == "" && fromOverride == "" && toOverride == "" {
		return nil, ""
	}

	if fromOverride == "" {
		fromOverride = spec
	}

	cacheFrom := []string{}
	if fromOverride != "" {
		cacheFrom = []string{fromOverride}
	}

	cacheTo := strings.TrimSpace(toOverride)
	if cacheTo == "" && spec != "" && strings.TrimSpace(cache.Options["from"]) == "" {
		cacheTo = spec
	}

	return cacheFrom, cacheTo
}

func buildCacheSpec(cacheType string, options map[string]string) string {
	cacheType = strings.TrimSpace(cacheType)
	if cacheType == "" && len(options) == 0 {
		return ""
	}

	parts := []string{}
	if cacheType != "" {
		parts = append(parts, fmt.Sprintf("type=%s", cacheType))
	}

	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		val := strings.TrimSpace(options[key])
		if val == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", key, val))
	}

	return strings.Join(parts, ",")
}

func loginToRegistry() error {
	server := cfg.Registry.Server
	if server == "" {
		server = "docker.io"
	}

	// Get password from secrets
	password := ""
	if len(cfg.Registry.Password) > 0 {
		// Password is a reference to a secret
		secretKey := cfg.Registry.Password[0]
		password = os.Getenv(secretKey)
		if password == "" {
			// Try loading from secrets file
			if p, ok := getSecret(secretKey); ok {
				password = p
			}
		}
	}

	if password == "" {
		return fmt.Errorf("registry password not found")
	}

	// Login using podman CLI
	cmd := exec.Command("podman", "login", "--username", cfg.Registry.Username, "--password-stdin", server)
	cmd.Stdin = strings.NewReader(password)
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func getSecret(key string) (string, bool) {
	// Try environment first
	if val := os.Getenv(key); val != "" {
		return val, true
	}

	// Try secrets from config (loaded by config loader)
	if val, ok := config.GetSecret(key); ok && val != "" {
		return val, true
	}

	return "", false
}

func generateVersion() string {
	// Try to get git commit hash
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err == nil {
		commit := strings.TrimSpace(string(out))
		// Check if working directory is clean
		statusCmd := exec.Command("git", "status", "--porcelain")
		statusOut, _ := statusCmd.Output()
		if len(statusOut) == 0 {
			return commit
		}
		// Dirty working directory
		return fmt.Sprintf("%s-dirty", commit)
	}

	// Fallback to timestamp
	return time.Now().Format("20060102150405")
}

// generateImageTag creates an image tag using the configured template
// Template placeholders:
//   - {version}: Git commit hash or timestamp
//   - {destination}: Current deployment destination (e.g., staging, production)
//   - {timestamp}: Current timestamp (YYYYMMDDHHMMSS)
//
// Default template is "{version}" for backward compatibility.
// Recommended for multi-environment: "{destination}-{version}"
func generateImageTag(baseImage, destination string) string {
	version := generateVersion()
	template := cfg.Builder.TagTemplate

	// Use default template if not configured
	if template == "" {
		template = "{version}"
	}

	// Replace placeholders
	tag := template
	tag = strings.ReplaceAll(tag, "{version}", version)
	tag = strings.ReplaceAll(tag, "{timestamp}", time.Now().Format("20060102150405"))

	// Only include destination if provided and placeholder exists
	if destination != "" {
		tag = strings.ReplaceAll(tag, "{destination}", destination)
	} else {
		// If no destination, remove the placeholder and any trailing/leading hyphen
		tag = strings.ReplaceAll(tag, "{destination}-", "")
		tag = strings.ReplaceAll(tag, "-{destination}", "")
		tag = strings.ReplaceAll(tag, "{destination}", "")
	}

	return fmt.Sprintf("%s:%s", baseImage, tag)
}
