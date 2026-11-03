package podman

import (
	"strings"
	"testing"
)

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
