package podman

import (
	"strings"
	"testing"
)

func TestManifestBuildCommands_OptionsAreQuoted(t *testing.T) {
	cfg := &ManifestBuildConfig{
		BuildConfig: BuildConfig{
			Context: ".",
			Tag:     "myapp:latest",
			Options: []string{"--annotation=a b", "--label=$(id)"},
		},
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}

	cmds := cfg.ManifestBuildCommands()

	for _, cmd := range cmds[1:3] {
		for _, want := range []string{"'--annotation=a b'", "'--label=$(id)'"} {
			if !strings.Contains(cmd, want) {
				t.Errorf("expected %q in platform build command, got: %s", want, cmd)
			}
		}
		if !strings.HasSuffix(cmd, " .") {
			t.Errorf("expected context to stay last, got: %s", cmd)
		}
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
	if !strings.Contains(cmds[1], "--build-arg 'NODE_ENV=production'") {
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

func TestManifestBuildCommandsPassCacheTo(t *testing.T) {
	config := &ManifestBuildConfig{
		BuildConfig: BuildConfig{Context: ".", Tag: "app:v1", CacheTo: "type=registry,ref=cache"},
		Platforms:   []string{"linux/amd64", "linux/arm64"},
	}
	got := strings.Join(config.ManifestBuildCommands(), "\n")
	if count := strings.Count(got, "--cache-to"); count != 2 {
		t.Fatalf("--cache-to appears %d times, want once per platform:\n%s", count, got)
	}
}
