package podman

import (
	"strings"
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

func TestLoginCommandTerminatesOptions(t *testing.T) {
	cmd := loginCommand("deploy", "deployer", "ghcr.io")
	if !strings.HasSuffix(cmd, "podman login --username deployer --password-stdin -- ghcr.io") {
		t.Fatalf("login command = %q, want -- before the registry server", cmd)
	}

	cmd = loginCommand("deploy", "deployer", "--tls-verify=false")
	if !strings.HasSuffix(cmd, "--password-stdin -- '--tls-verify=false'") {
		t.Fatalf("login command = %q, want the server after --", cmd)
	}
}

func TestRegistryManagerRejectsOptionLikeServer(t *testing.T) {
	sshClient := ssh.NewClient(&ssh.Config{User: "deploy"})
	t.Cleanup(func() { _ = sshClient.Close() })
	manager := NewRegistryManager(NewClient(sshClient))

	for _, server := range []string{"--tls-verify=false", "-R", "--all"} {
		if err := manager.Login("203.0.113.10", &RegistryConfig{Server: server, Username: "u", Password: "p"}); err == nil || !strings.Contains(err.Error(), "invalid registry server") {
			t.Errorf("Login(%q) error = %v, want invalid registry server", server, err)
		}
		if err := manager.Logout("203.0.113.10", server); err == nil || !strings.Contains(err.Error(), "invalid registry server") {
			t.Errorf("Logout(%q) error = %v, want invalid registry server", server, err)
		}
		errs := manager.LogoutAll([]string{"203.0.113.10", "203.0.113.11"}, server)
		if len(errs) != 2 {
			t.Errorf("LogoutAll(%q) errors = %v, want one per host", server, errs)
		}
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
