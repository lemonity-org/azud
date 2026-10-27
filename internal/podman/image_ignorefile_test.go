package podman

import (
	"strings"
	"testing"
)

func TestBuildCommand_WithIgnoreFile(t *testing.T) {
	cfg := &BuildConfig{
		Context:    "app dir",
		IgnoreFile: "/tmp/azud ignore/containerignore",
	}

	cmd := cfg.BuildCommand()

	if !strings.Contains(cmd, "--ignorefile '/tmp/azud ignore/containerignore'") {
		t.Errorf("expected a quoted --ignorefile flag, got: %s", cmd)
	}
	if !strings.HasSuffix(cmd, "'app dir'") {
		t.Errorf("expected context last, got: %s", cmd)
	}
	if strings.Contains((&BuildConfig{}).BuildCommand(), "--ignorefile") {
		t.Error("expected no --ignorefile flag when IgnoreFile is empty")
	}
}

func TestManifestBuildCommands_WithIgnoreFile(t *testing.T) {
	cfg := &ManifestBuildConfig{
		BuildConfig: BuildConfig{
			Context:    ".",
			Tag:        "myapp:latest",
			IgnoreFile: "/tmp/azud-ignore/containerignore",
		},
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}

	cmds := cfg.ManifestBuildCommands()

	for _, cmd := range cmds[1:3] {
		if !strings.Contains(cmd, "--ignorefile /tmp/azud-ignore/containerignore") {
			t.Errorf("expected --ignorefile in every platform build, got: %s", cmd)
		}
	}
}
