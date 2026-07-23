package podman

import (
	"testing"

	"github.com/lemonity-org/azud/internal/ssh"
	"github.com/lemonity-org/azud/internal/state"
)

func TestNewRegistryManagerUsesEffectiveSSHUser(t *testing.T) {
	sshClient := ssh.NewClient(&ssh.Config{User: "deploy"})
	t.Cleanup(func() {
		if err := sshClient.Close(); err != nil {
			t.Errorf("close SSH client: %v", err)
		}
	})

	manager := NewRegistryManager(NewClient(sshClient))

	if manager.user != "deploy" {
		t.Fatalf("registry manager user = %q, want deploy", manager.user)
	}
	if got := state.DirQuoted(manager.user); got != `"${HOME}/.local/share/azud"` {
		t.Fatalf("registry state directory = %q, want non-root user state directory", got)
	}
}

func TestQualifyImage(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"postgres:18", "docker.io/library/postgres:18"},
		{"postgres", "docker.io/library/postgres"},
		{"redis:7-alpine", "docker.io/library/redis:7-alpine"},
		{"myuser/myimage:v1", "docker.io/myuser/myimage:v1"},
		{"myuser/myimage", "docker.io/myuser/myimage"},
		{"ghcr.io/org/img:tag", "ghcr.io/org/img:tag"},
		{"registry.example.com:5000/myimage:latest", "registry.example.com:5000/myimage:latest"},
		{"docker.io/library/postgres:18", "docker.io/library/postgres:18"},
		{"curlimages/curl:8.5.0", "docker.io/curlimages/curl:8.5.0"},
		{"postgres@sha256:abc123", "docker.io/library/postgres@sha256:abc123"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := QualifyImage(tt.input)
			if got != tt.want {
				t.Errorf("QualifyImage(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
