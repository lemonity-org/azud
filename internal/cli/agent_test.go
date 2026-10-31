package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/ssh"
)

func TestImageRepository(t *testing.T) {
	tests := map[string]string{
		"ghcr.io/lemonity-org/app":                                   "ghcr.io/lemonity-org/app",
		"ghcr.io/lemonity-org/app:production":                        "ghcr.io/lemonity-org/app",
		"ghcr.io/lemonity-org/app@sha256:" + strings.Repeat("a", 64): "ghcr.io/lemonity-org/app",
		"localhost:5000/app":                                         "localhost:5000/app",
		"localhost:5000/app:v1":                                      "localhost:5000/app",
	}
	for image, want := range tests {
		if got := imageRepository(image); got != want {
			t.Errorf("imageRepository(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestAgentRegistryCredentialsStayWithTheirRegistry(t *testing.T) {
	t.Setenv("AGENT_TEST_REGISTRY_TOKEN", "read-only")
	cfg := &config.Config{Registry: config.RegistryConfig{
		Server:   "ghcr.io",
		Username: "deploy",
		Password: []string{"AGENT_TEST_REGISTRY_TOKEN"},
	}}

	creds, err := agentRegistryCredentials(cfg, registry.Reference{Registry: "ghcr.io", Repository: "o/app", Tag: "production"})
	if err != nil || creds == nil || creds.Username != "deploy" || creds.Password != "read-only" {
		t.Fatalf("credentials for the configured registry = %+v, %v", creds, err)
	}
	if creds, err := agentRegistryCredentials(cfg, registry.Reference{Registry: "docker.io", Repository: "library/nginx", Tag: "1"}); creds != nil || err != nil {
		t.Fatalf("credentials sent to another registry: %+v, %v", creds, err)
	}

	cfg.Registry.Server = ""
	if creds, err := agentRegistryCredentials(cfg, registry.Reference{Registry: "docker.io", Repository: "acme/app", Tag: "prod"}); creds == nil || err != nil {
		t.Fatalf("an empty registry.server should mean Docker Hub: %+v, %v", creds, err)
	}
}

func TestAgentRegistryCredentialsMissingPassword(t *testing.T) {
	cfg := &config.Config{
		SecretsProvider: "file",
		SecretsPath:     ".azud/agent-secrets",
		Registry: config.RegistryConfig{
			Server:   "ghcr.io",
			Username: "deploy",
			Password: []string{"AGENT_TEST_MISSING_REGISTRY_TOKEN"},
		},
	}

	_, err := agentRegistryCredentials(cfg, registry.Reference{Registry: "ghcr.io", Repository: "o/app", Tag: "production"})
	if err == nil || !strings.Contains(err.Error(), "AGENT_TEST_MISSING_REGISTRY_TOKEN") || !strings.Contains(err.Error(), ".azud/agent-secrets") {
		t.Fatalf("missing password error = %v, want the key and where to put it", err)
	}
}

func TestAgentUnit(t *testing.T) {
	unit := agentUnit("app", "/usr/local/bin/azud", "/home/deploy/my app/config/deploy.yml", "production", "/home/deploy/my app%")

	for _, want := range []string{
		"Description=Azud pull agent for app",
		`ExecStart=/usr/local/bin/azud agent run --config "/home/deploy/my app/config/deploy.yml" --destination production`,
		"WorkingDirectory=/home/deploy/my app%%\n",
		"KillMode=process",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %q:\n%s", want, unit)
		}
	}

	if unit := agentUnit("app", "/usr/local/bin/azud", "/srv/deploy.yml", "", "/srv"); strings.Contains(unit, "--destination") {
		t.Errorf("unit without a destination passes --destination:\n%s", unit)
	}
}

func TestSystemdQuote(t *testing.T) {
	tests := map[string]string{
		"/usr/local/bin/azud": "/usr/local/bin/azud",
		"/srv/my app":         `"/srv/my app"`,
		`/srv/a"b`:            `"/srv/a\"b"`,
		"/srv/$HOME%i":        `"/srv/$$HOME%%i"`,
		"":                    `""`,
	}
	for in, want := range tests {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunningDigestRequiresEveryRoleOnTheSameDigest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the local transport needs a POSIX shell")
	}
	t.Setenv("HOME", t.TempDir())

	digest := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	fakePodman := filepath.Join(t.TempDir(), "podman")
	writeFakePodman := func(web, worker string) {
		t.Helper()
		script := "#!/bin/sh\n" +
			"case \"$5\" in\n" +
			"  app) " + web + " ;;\n" +
			"  app-worker) " + worker + " ;;\n" +
			"  *) echo 'no such container' >&2; exit 125 ;;\n" +
			"esac\n"
		if err := os.WriteFile(fakePodman, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		Service: "app",
		Servers: map[string]config.RoleConfig{
			"web":    {Hosts: []string{"app-1"}},
			"worker": {Hosts: []string{"app-1"}},
		},
	}
	client := ssh.NewClient(&ssh.Config{LocalHost: "app-1"})
	defer func() { _ = client.Close() }()
	running := runningDigest(podman.NewClientWithCommand(client, fakePodman), cfg, "app-1", "ghcr.io/o/app")

	tests := []struct {
		name        string
		web, worker string
		want        string
	}{
		{"both roles on the digest", "echo ghcr.io/o/app@" + digest, "echo ghcr.io/o/app@" + digest, digest},
		{"roles on different digests", "echo ghcr.io/o/app@" + digest, "echo ghcr.io/o/app@" + other, ""},
		{"a role deployed by tag", "echo ghcr.io/o/app@" + digest, "echo ghcr.io/o/app:latest", ""},
		{"a role missing", "echo ghcr.io/o/app@" + digest, "exit 125", ""},
		{"another image", "echo ghcr.io/o/other@" + digest, "echo ghcr.io/o/other@" + digest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeFakePodman(tt.web, tt.worker)
			got, err := running(t.Context())
			if err != nil || got != tt.want {
				t.Fatalf("runningDigest = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestRunAgentRunPreconditions(t *testing.T) {
	previous := cfg
	t.Cleanup(func() {
		cfg = previous
		agentOnce, agentRetryFailed = false, false
	})

	cfg = &config.Config{Service: "app"}
	agentOnce, agentRetryFailed = false, true
	if err := runAgentRun(agentRunCmd, nil); err == nil || !strings.Contains(err.Error(), "--once") {
		t.Fatalf("--retry-failed without --once = %v", err)
	}

	agentOnce, agentRetryFailed = true, false
	if err := runAgentRun(agentRunCmd, nil); err == nil || !strings.Contains(err.Error(), "agent.track") {
		t.Fatalf("missing agent.track = %v", err)
	}
}
