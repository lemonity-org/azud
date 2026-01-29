package podman

import (
	"strings"
	"testing"
)

func TestBuildCommand_Basic(t *testing.T) {
	cfg := &BuildConfig{
		Context: ".",
		Tag:     "myapp:latest",
	}

	cmd := cfg.BuildCommand()

	if !strings.HasPrefix(cmd, "podman ") {
		t.Errorf("expected command to start with 'podman ', got: %s", cmd)
	}
	if !strings.Contains(cmd, "build") {
		t.Error("expected 'build' in command")
	}
	if !strings.Contains(cmd, "-t myapp:latest") {
		t.Error("expected '-t myapp:latest'")
	}
}

func TestBuildCommand_NeverDocker(t *testing.T) {
	cfg := &BuildConfig{
		Context: ".",
	}

	cmd := cfg.BuildCommand()

	if strings.HasPrefix(cmd, "docker ") {
		t.Errorf("command must not start with 'docker', got: %s", cmd)
	}
	if !strings.HasPrefix(cmd, "podman ") {
		t.Errorf("command must start with 'podman', got: %s", cmd)
	}
}

func TestBuildCommand_WithDockerfile(t *testing.T) {
	cfg := &BuildConfig{
		Context:    ".",
		Dockerfile: "Dockerfile.prod",
	}

	cmd := cfg.BuildCommand()

	if !strings.Contains(cmd, "-f Dockerfile.prod") {
		t.Error("expected '-f Dockerfile.prod'")
	}
}

func TestBuildCommand_WithPlatform(t *testing.T) {
	cfg := &BuildConfig{
		Context:  ".",
		Platform: "linux/arm64",
	}

	cmd := cfg.BuildCommand()

	if !strings.Contains(cmd, "--platform linux/arm64") {
		t.Error("expected '--platform linux/arm64'")
	}
}

func TestBuildCommand_WithNoCache(t *testing.T) {
	cfg := &BuildConfig{
		Context: ".",
		NoCache: true,
	}

	cmd := cfg.BuildCommand()

	if !strings.Contains(cmd, "--no-cache") {
		t.Error("expected '--no-cache'")
	}
}

func TestBuildCommand_WithPull(t *testing.T) {
	cfg := &BuildConfig{
		Context: ".",
		Pull:    true,
	}

	cmd := cfg.BuildCommand()

	if !strings.Contains(cmd, "--pull") {
		t.Error("expected '--pull'")
	}
}

func TestBuildCommand_WithTarget(t *testing.T) {
	cfg := &BuildConfig{
		Context: ".",
		Target:  "production",
	}

	cmd := cfg.BuildCommand()

	if !strings.Contains(cmd, "--target production") {
		t.Error("expected '--target production'")
	}
}

func TestBuildCommand_ContextLast(t *testing.T) {
	cfg := &BuildConfig{
		Context: "/my/context",
		Tag:     "test:latest",
	}

	cmd := cfg.BuildCommand()

	if !strings.HasSuffix(cmd, "/my/context") {
		t.Errorf("expected context to be last argument, got: %s", cmd)
	}
}

func TestBuildCommand_DefaultContext(t *testing.T) {
	cfg := &BuildConfig{}

	cmd := cfg.BuildCommand()

	if !strings.HasSuffix(cmd, " .") {
		t.Errorf("expected default context '.', got: %s", cmd)
	}
}

func TestBuildCommand_MultipleTags(t *testing.T) {
	cfg := &BuildConfig{
		Context: ".",
		Tag:     "myapp:v1",
		Tags:    []string{"myapp:latest", "myapp:stable"},
	}

	cmd := cfg.BuildCommand()

	if !strings.Contains(cmd, "-t myapp:v1") {
		t.Error("expected primary tag")
	}
	if !strings.Contains(cmd, "-t myapp:latest") {
		t.Error("expected additional tag myapp:latest")
	}
	if !strings.Contains(cmd, "-t myapp:stable") {
		t.Error("expected additional tag myapp:stable")
	}
}

func TestManifestBuildCommands_Basic(t *testing.T) {
	cfg := &ManifestBuildConfig{
		BuildConfig: BuildConfig{
			Context: ".",
			Tag:     "myapp:latest",
		},
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}

	cmds := cfg.ManifestBuildCommands()

	if len(cmds) < 3 {
		t.Fatalf("expected at least 3 commands (create + 2 builds), got %d", len(cmds))
	}

	// First command: manifest create
	if !strings.Contains(cmds[0], "podman manifest create myapp:latest") {
		t.Errorf("expected manifest create command, got: %s", cmds[0])
	}

	// Platform-specific builds
	if !strings.Contains(cmds[1], "--platform linux/amd64") {
		t.Errorf("expected linux/amd64 build, got: %s", cmds[1])
	}
	if !strings.Contains(cmds[1], "--manifest myapp:latest") {
		t.Errorf("expected --manifest flag, got: %s", cmds[1])
	}

	if !strings.Contains(cmds[2], "--platform linux/arm64") {
		t.Errorf("expected linux/arm64 build, got: %s", cmds[2])
	}
}

func TestManifestBuildCommands_WithPush(t *testing.T) {
	cfg := &ManifestBuildConfig{
		BuildConfig: BuildConfig{
			Context: ".",
			Tag:     "myapp:latest",
		},
		Platforms: []string{"linux/amd64"},
		Push:      true,
	}

	cmds := cfg.ManifestBuildCommands()

	lastCmd := cmds[len(cmds)-1]
	if !strings.Contains(lastCmd, "podman manifest push") {
		t.Errorf("expected last command to be manifest push, got: %s", lastCmd)
	}
}

func TestManifestBuildCommands_NoPush(t *testing.T) {
	cfg := &ManifestBuildConfig{
		BuildConfig: BuildConfig{
			Context: ".",
			Tag:     "myapp:latest",
		},
		Platforms: []string{"linux/amd64"},
		Push:      false,
	}

	cmds := cfg.ManifestBuildCommands()

	for _, cmd := range cmds {
		if strings.Contains(cmd, "manifest push") {
			t.Error("should not contain manifest push when Push is false")
		}
	}
}

func TestManifestBuildCommands_DefaultTag(t *testing.T) {
	cfg := &ManifestBuildConfig{
		Platforms: []string{"linux/amd64"},
	}

	cmds := cfg.ManifestBuildCommands()

	if !strings.Contains(cmds[0], "localhost/build:latest") {
		t.Errorf("expected default tag, got: %s", cmds[0])
	}
}

func TestManifestBuildCommands_NeverDocker(t *testing.T) {
	cfg := &ManifestBuildConfig{
		BuildConfig: BuildConfig{
			Context: ".",
			Tag:     "test:latest",
		},
		Platforms: []string{"linux/amd64"},
		Push:      true,
	}

	cmds := cfg.ManifestBuildCommands()

	for i, cmd := range cmds {
		if strings.HasPrefix(cmd, "docker ") {
			t.Errorf("command %d must not start with 'docker': %s", i, cmd)
		}
		if !strings.HasPrefix(cmd, "podman ") {
			t.Errorf("command %d must start with 'podman': %s", i, cmd)
		}
	}
}

func TestManifestBuildCommands_WithBuildArgs(t *testing.T) {
	cfg := &ManifestBuildConfig{
		BuildConfig: BuildConfig{
			Context: ".",
			Tag:     "myapp:latest",
			Args:    map[string]string{"NODE_ENV": "production"},
		},
		Platforms: []string{"linux/amd64"},
	}

	cmds := cfg.ManifestBuildCommands()

	// Check the build command (index 1) has the build arg
	if !strings.Contains(cmds[1], "--build-arg NODE_ENV=production") {
		t.Errorf("expected build arg in build command, got: %s", cmds[1])
	}
}

func TestManifestBuildCommands_WithDockerfile(t *testing.T) {
	cfg := &ManifestBuildConfig{
		BuildConfig: BuildConfig{
			Context:    ".",
			Tag:        "myapp:latest",
			Dockerfile: "Dockerfile.prod",
		},
		Platforms: []string{"linux/amd64"},
	}

	cmds := cfg.ManifestBuildCommands()

	if !strings.Contains(cmds[1], "-f Dockerfile.prod") {
		t.Errorf("expected dockerfile flag in build command, got: %s", cmds[1])
	}
}
