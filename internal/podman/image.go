package podman

import (
	"fmt"
	"strings"
	"time"
)

type Image struct {
	ID         string
	Repository string
	Tag        string
	Size       string
	Created    time.Time
}

// ImageManager handles image operations via Podman.
type ImageManager struct {
	client *Client
}

func NewImageManager(client *Client) *ImageManager {
	return &ImageManager{client: client}
}

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

// BuildConfig holds configuration for building an image.
type BuildConfig struct {
	Context    string
	Dockerfile string
	Tag        string
	Tags       []string
	Args       map[string]string
	Target     string
	CacheFrom  []string
	Platform   string
	NoCache    bool
	Pull       bool
	Squash     bool
	Secrets    []string
	SSH        []string
	Options    []string
}

func (c *BuildConfig) BuildCommand() string {
	args := []string{"build"}

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

	context := c.Context
	if context == "" {
		context = "."
	}
	args = append(args, context)

	return "podman " + strings.Join(args, " ")
}

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

// ManifestBuildConfig holds configuration for multi-arch manifest builds.
type ManifestBuildConfig struct {
	BuildConfig
	Platforms []string // e.g., ["linux/amd64", "linux/arm64"]
	Push      bool
	CacheTo   string
	Output    string
}

// ManifestBuildCommands generates manifest create, per-platform builds,
// and optional manifest push commands.
func (c *ManifestBuildConfig) ManifestBuildCommands() []string {
	var commands []string

	tag := c.Tag
	if tag == "" {
		tag = "localhost/build:latest"
	}

	commands = append(commands, fmt.Sprintf("podman manifest create %s", tag))

	for _, platform := range c.Platforms {
		args := []string{"build"}

		args = append(args, "--platform", platform)
		args = append(args, "--manifest", tag)

		if c.Dockerfile != "" {
			args = append(args, "-f", c.Dockerfile)
		}

		for _, t := range c.Tags {
			args = append(args, "-t", t)
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

		context := c.Context
		if context == "" {
			context = "."
		}
		args = append(args, context)

		commands = append(commands, "podman "+strings.Join(args, " "))
	}

	if c.Push {
		commands = append(commands, fmt.Sprintf("podman manifest push %s %s", tag, tag))
	}

	return commands
}

func (m *ImageManager) ManifestBuild(host string, config *ManifestBuildConfig) error {
	commands := config.ManifestBuildCommands()

	for _, cmd := range commands {
		result, err := m.client.ssh.Execute(host, cmd)
		if err != nil {
			return err
		}

		if result.ExitCode != 0 {
			return fmt.Errorf("failed to build image: %s", result.Stderr)
		}
	}

	return nil
}

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

func (m *ImageManager) Exists(host, image string) (bool, error) {
	result, err := m.client.Execute(host, "image", "inspect", image, "--format", "'{{.Id}}'")
	if err != nil {
		return false, err
	}

	return result.ExitCode == 0, nil
}

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
