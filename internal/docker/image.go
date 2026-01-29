package docker

import (
	"fmt"
	"strings"
	"time"
)

// Image represents a Docker image
type Image struct {
	ID         string
	Repository string
	Tag        string
	Size       string
	Created    time.Time
}

// ImageManager handles image operations
type ImageManager struct {
	client *Client
}

// NewImageManager creates a new image manager
func NewImageManager(client *Client) *ImageManager {
	return &ImageManager{client: client}
}

// Pull pulls an image from a registry
func (m *ImageManager) Pull(host, image string) error {
	result, err := m.client.Execute(host, "pull", image)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to pull image: %s", result.Stderr)
	}

	return nil
}

// PullAll pulls an image on multiple hosts in parallel
func (m *ImageManager) PullAll(hosts []string, image string) map[string]error {
	results := m.client.ExecuteAll(hosts, "pull", image)
	errors := make(map[string]error)

	for _, result := range results {
		if !result.Success() {
			errors[result.Host] = fmt.Errorf("pull failed: %s", result.Stderr)
		}
	}

	return errors
}

// Push pushes an image to a registry
func (m *ImageManager) Push(host, image string) error {
	result, err := m.client.Execute(host, "push", image)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to push image: %s", result.Stderr)
	}

	return nil
}

// Tag tags an image
func (m *ImageManager) Tag(host, source, target string) error {
	result, err := m.client.Execute(host, "tag", source, target)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to tag image: %s", result.Stderr)
	}

	return nil
}

// BuildConfig holds configuration for building an image
type BuildConfig struct {
	// Context directory
	Context string

	// Dockerfile path
	Dockerfile string

	// Image tag
	Tag string

	// Additional tags
	Tags []string

	// Build arguments
	Args map[string]string

	// Target stage
	Target string

	// Cache from images
	CacheFrom []string

	// Platform
	Platform string

	// Don't use cache
	NoCache bool

	// Always pull base image
	Pull bool

	// Squash layers
	Squash bool

	// Build secrets
	Secrets []string

	// SSH agent sockets or keys
	SSH []string

	// Additional build options
	Options []string
}

// BuildCommand generates a docker build command
func (c *BuildConfig) BuildCommand() string {
	var args []string
	args = append(args, "build")

	if c.Dockerfile != "" {
		args = append(args, "-f", c.Dockerfile)
	}

	if c.Tag != "" {
		args = append(args, "-t", c.Tag)
	}

	for _, tag := range c.Tags {
		args = append(args, "-t", tag)
	}

	for key, value := range c.Args {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
	}

	if c.Target != "" {
		args = append(args, "--target", c.Target)
	}

	for _, cache := range c.CacheFrom {
		args = append(args, "--cache-from", cache)
	}

	if c.Platform != "" {
		args = append(args, "--platform", c.Platform)
	}

	if c.NoCache {
		args = append(args, "--no-cache")
	}

	if c.Pull {
		args = append(args, "--pull")
	}

	for _, secret := range c.Secrets {
		args = append(args, "--secret", secret)
	}

	for _, s := range c.SSH {
		args = append(args, "--ssh", s)
	}

	args = append(args, c.Options...)

	// Context must be last
	context := c.Context
	if context == "" {
		context = "."
	}
	args = append(args, context)

	return "docker " + strings.Join(args, " ")
}

// Build builds an image on a host
func (m *ImageManager) Build(host string, config *BuildConfig) error {
	cmd := config.BuildCommand()
	result, err := m.client.ssh.Execute(host, cmd)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to build image: %s", result.Stderr)
	}

	return nil
}

// BuildxConfig holds configuration for buildx builds
type BuildxConfig struct {
	BuildConfig

	// Builder instance
	Builder string

	// Push after build
	Push bool

	// Load into docker
	Load bool

	// Cache to
	CacheTo string

	// Output
	Output string

	// Provenance
	Provenance string

	// SBOM
	SBOM string
}

// BuildxCommand generates a docker buildx build command
func (c *BuildxConfig) BuildxCommand() string {
	var args []string
	args = append(args, "buildx", "build")

	if c.Builder != "" {
		args = append(args, "--builder", c.Builder)
	}

	if c.Push {
		args = append(args, "--push")
	}

	if c.Load {
		args = append(args, "--load")
	}

	if c.Dockerfile != "" {
		args = append(args, "-f", c.Dockerfile)
	}

	if c.Tag != "" {
		args = append(args, "-t", c.Tag)
	}

	for _, tag := range c.Tags {
		args = append(args, "-t", tag)
	}

	for key, value := range c.Args {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
	}

	if c.Target != "" {
		args = append(args, "--target", c.Target)
	}

	for _, cache := range c.CacheFrom {
		args = append(args, "--cache-from", cache)
	}

	if c.CacheTo != "" {
		args = append(args, "--cache-to", c.CacheTo)
	}

	if c.Platform != "" {
		args = append(args, "--platform", c.Platform)
	}

	if c.NoCache {
		args = append(args, "--no-cache")
	}

	if c.Output != "" {
		args = append(args, "--output", c.Output)
	}

	if c.Provenance != "" {
		args = append(args, "--provenance", c.Provenance)
	}

	if c.SBOM != "" {
		args = append(args, "--sbom", c.SBOM)
	}

	args = append(args, c.Options...)

	context := c.Context
	if context == "" {
		context = "."
	}
	args = append(args, context)

	return "docker " + strings.Join(args, " ")
}

// Buildx builds an image using buildx
func (m *ImageManager) Buildx(host string, config *BuildxConfig) error {
	cmd := config.BuildxCommand()
	result, err := m.client.ssh.Execute(host, cmd)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to build image: %s", result.Stderr)
	}

	return nil
}

// List lists images on a host
func (m *ImageManager) List(host string, filters map[string]string) ([]Image, error) {
	args := []string{"images", "--format", "'{{.ID}}|{{.Repository}}|{{.Tag}}|{{.Size}}|{{.CreatedAt}}'"}

	for key, value := range filters {
		args = append(args, "-f", fmt.Sprintf("%s=%s", key, value))
	}

	result, err := m.client.Execute(host, args...)
	if err != nil {
		return nil, err
	}

	if result.ExitCode != 0 {
		return nil, fmt.Errorf("failed to list images: %s", result.Stderr)
	}

	var images []Image
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for _, line := range lines {
		line = strings.Trim(line, "'")
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}

		image := Image{
			ID:         parts[0],
			Repository: parts[1],
			Tag:        parts[2],
			Size:       parts[3],
		}

		if len(parts) > 4 {
			image.Created, _ = time.Parse("2006-01-02 15:04:05 -0700 MST", parts[4])
		}

		images = append(images, image)
	}

	return images, nil
}

// Remove removes an image
func (m *ImageManager) Remove(host, image string, force bool) error {
	args := []string{"rmi"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, image)

	result, err := m.client.Execute(host, args...)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to remove image: %s", result.Stderr)
	}

	return nil
}

// Prune removes unused images
func (m *ImageManager) Prune(host string, all bool) error {
	args := []string{"image", "prune", "-f"}
	if all {
		args = append(args, "-a")
	}

	result, err := m.client.Execute(host, args...)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to prune images: %s", result.Stderr)
	}

	return nil
}

// Exists checks if an image exists on a host
func (m *ImageManager) Exists(host, image string) (bool, error) {
	result, err := m.client.Execute(host, "image", "inspect", image, "--format", "'{{.Id}}'")
	if err != nil {
		return false, err
	}

	return result.ExitCode == 0, nil
}

// GetDigest retrieves the digest of an image
func (m *ImageManager) GetDigest(host, image string) (string, error) {
	result, err := m.client.Execute(host, "image", "inspect", image, "--format", "'{{index .RepoDigests 0}}'")
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to get digest: %s", result.Stderr)
	}

	digest := strings.Trim(result.Stdout, "'\n")
	if strings.Contains(digest, "@") {
		parts := strings.Split(digest, "@")
		return parts[1], nil
	}

	return digest, nil
}

// Save saves an image to a tar archive
func (m *ImageManager) Save(host, image, outputPath string) error {
	result, err := m.client.Execute(host, "save", "-o", outputPath, image)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to save image: %s", result.Stderr)
	}

	return nil
}

// Load loads an image from a tar archive
func (m *ImageManager) Load(host, inputPath string) error {
	result, err := m.client.Execute(host, "load", "-i", inputPath)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to load image: %s", result.Stderr)
	}

	return nil
}
