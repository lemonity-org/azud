package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/podman"
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

	// Check if we should use remote builder
	if cfg.Builder.Remote.Host != "" {
		version := generateVersion()
		return buildRemote(imageTag, latestTag, version)
	}

	// Build locally
	if err := buildLocal(imageTag, latestTag); err != nil {
		return err
	}

	// Push to registry
	if !buildNoPush {
		log.Info("Pushing image to registry...")
		if err := pushImage(imageTag, latestTag); err != nil {
			return err
		}
		log.Success("Image pushed successfully")
	}

	timer.Stop()
	log.Success("Build complete: %s", imageTag)
	return nil
}

func buildLocal(imageTag, latestTag string) error {
	log := output.DefaultLogger

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
	if cfg.Builder.Arch != "" {
		args = append(args, "--platform", fmt.Sprintf("linux/%s", cfg.Builder.Arch))
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

func buildRemote(imageTag, latestTag, version string) error {
	log := output.DefaultLogger
	log.Info("Building on remote builder: %s", cfg.Builder.Remote.Host)

	// Create SSH client for remote builder
	sshClient := createSSHClient()
	defer sshClient.Close()

	podmanClient := podman.NewClient(sshClient)
	imageManager := podman.NewImageManager(podmanClient)

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
		},
		Push: !buildNoPush,
	}

	if cfg.Builder.Arch != "" {
		buildConfig.Platform = fmt.Sprintf("linux/%s", cfg.Builder.Arch)
	}

	// First, we need to sync the build context to the remote builder
	// For now, we'll assume the code is already on the remote builder
	// In a production implementation, you'd sync the context first

	if err := imageManager.ManifestBuild(cfg.Builder.Remote.Host, buildConfig); err != nil {
		return fmt.Errorf("remote build failed: %w", err)
	}

	log.Success("Remote build complete")
	return nil
}

func pushImage(imageTag, latestTag string) error {
	log := output.DefaultLogger

	// Login to registry first
	if cfg.Registry.Username != "" {
		if err := loginToRegistry(); err != nil {
			return fmt.Errorf("registry login failed: %w", err)
		}
	}

	// Push version tag
	log.Info("Pushing %s...", imageTag)
	pushCmd := exec.Command("podman", "push", imageTag)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("failed to push %s: %w", imageTag, err)
	}

	// Push latest tag
	log.Info("Pushing %s...", latestTag)
	pushCmd = exec.Command("podman", "push", latestTag)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("failed to push %s: %w", latestTag, err)
	}

	return nil
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

	// Try secrets from config
	// This would be loaded by the config loader
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
