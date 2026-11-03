package deploy

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/localtest"
	"github.com/lemonity-org/azud/internal/output"
)

func TestNewPreDeployContainerConfig(t *testing.T) {
	tests := []struct {
		name          string
		cfg           *config.Config
		image         string
		containerName string
		wantSecretEnv int
		wantEnvFile   bool
		wantEnvKeys   map[string]string
	}{
		{
			name: "basic with clear env vars",
			cfg: &config.Config{
				Service: "my-app",
				Env: config.EnvConfig{
					Clear: map[string]string{
						"DB_HOST": "localhost",
						"DB_PORT": "5432",
					},
				},
			},
			image:         "ghcr.io/org/app:v1",
			containerName: "my-app-pre-deploy-123",
			wantEnvKeys:   map[string]string{"DB_HOST": "localhost", "DB_PORT": "5432"},
		},
		{
			name: "with secrets sets env file",
			cfg: &config.Config{
				Service: "my-app",
				Env: config.EnvConfig{
					Clear:  map[string]string{},
					Secret: []string{"DATABASE_URL", "API_KEY"},
				},
				SecretsRemotePath: "/home/deploy/.azud/secrets",
			},
			image:         "app:v1",
			containerName: "my-app-pre-deploy-456",
			wantSecretEnv: 2,
			wantEnvFile:   true,
		},
		{
			name: "without secrets leaves env file empty",
			cfg: &config.Config{
				Service: "my-app",
				Env: config.EnvConfig{
					Clear: map[string]string{"FOO": "bar"},
				},
			},
			image:         "app:v1",
			containerName: "my-app-pre-deploy-789",
			wantEnvKeys:   map[string]string{"FOO": "bar"},
		},
		{
			name: "no healthcheck configured",
			cfg: &config.Config{
				Service: "my-app",
			},
			image:         "app:v1",
			containerName: "my-app-pre-deploy-000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newPreDeployContainerConfig(tt.cfg, tt.image, tt.containerName)

			if got.Name != tt.containerName {
				t.Errorf("Name: got %s, want %s", got.Name, tt.containerName)
			}
			if got.Image != tt.image {
				t.Errorf("Image: got %s, want %s", got.Image, tt.image)
			}
			if !got.Remove {
				t.Error("expected Remove to be true")
			}
			if got.Detach {
				t.Error("expected Detach to be false")
			}
			if got.Restart != "" {
				t.Errorf("expected no restart policy, got %s", got.Restart)
			}
			if got.Network != "azud" {
				t.Errorf("Network: got %s, want azud", got.Network)
			}
			if got.Labels["azud.managed"] != "true" {
				t.Error("missing azud.managed label")
			}
			if got.Labels["azud.service"] != tt.cfg.Service {
				t.Errorf("azud.service label: got %s, want %s", got.Labels["azud.service"], tt.cfg.Service)
			}
			if got.HealthCmd != "" {
				t.Errorf("expected no health command on pre-deploy container, got %s", got.HealthCmd)
			}
			if len(got.SecretEnv) != tt.wantSecretEnv {
				t.Errorf("SecretEnv count: got %d, want %d", len(got.SecretEnv), tt.wantSecretEnv)
			}
			if tt.wantEnvFile && got.EnvFile == "" {
				t.Error("expected EnvFile to be set")
			}
			if !tt.wantEnvFile && got.EnvFile != "" {
				t.Errorf("expected empty EnvFile, got %s", got.EnvFile)
			}
			for k, want := range tt.wantEnvKeys {
				if got.Env[k] != want {
					t.Errorf("Env[%s]: got %s, want %s", k, got.Env[k], want)
				}
			}
		})
	}
}

func TestParseCommandArgs(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want []string
	}{
		{
			name: "simple command",
			cmd:  "./migrate postgres up",
			want: []string{"./migrate", "postgres", "up"},
		},
		{
			name: "single word",
			cmd:  "./simple-command",
			want: []string{"./simple-command"},
		},
		{
			name: "shell AND operator",
			cmd:  "./migrate postgres up && ./migrate clickhouse up",
			want: []string{"sh", "-c", "./migrate postgres up && ./migrate clickhouse up"},
		},
		{
			name: "shell OR operator",
			cmd:  "./migrate up || exit 1",
			want: []string{"sh", "-c", "./migrate up || exit 1"},
		},
		{
			name: "shell variable expansion",
			cmd:  "echo $HOME",
			want: []string{"sh", "-c", "echo $HOME"},
		},
		{
			name: "embedded single quotes",
			cmd:  "./run 'my arg'",
			want: []string{"sh", "-c", "./run 'my arg'"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCommandArgs(tt.cmd)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// undoTestDeployer deploys one standalone role against a runner that answers every
// command with success, with a hook for each name in hooks that records it ran.
func undoTestDeployer(t *testing.T, hooks ...string) (*Deployer, *localtest.Fake, string) {
	t.Helper()
	t.Setenv("AZUD_STATE_DIR", t.TempDir())
	hooksDir, ran := t.TempDir(), filepath.Join(t.TempDir(), "ran")
	for _, name := range hooks {
		script := "#!/bin/sh\necho " + name + " >> " + ran + "\n"
		if err := os.WriteFile(filepath.Join(hooksDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Service:   "shop",
		Image:     "example/shop@sha256:abc",
		Servers:   map[string]config.RoleConfig{"worker": {Hosts: []string{"host"}, Cmd: "work"}},
		HooksPath: hooksDir,
		Deploy:    config.DeployConfig{PreDeployCommand: "./migrate up"},
	}
	fake := &localtest.Fake{
		Handle: func(cmd string, _ io.Reader) (string, string, int) {
			if strings.Contains(cmd, "RestartCount") {
				return "true 0 0", "", 0
			}
			return "", "", 0
		},
		UserName: "deploy",
	}
	return NewDeployer(cfg, fake, output.NewLogger(io.Discard, io.Discard, false)), fake, ran
}

func ranMigration(fake *localtest.Fake) bool {
	return slices.ContainsFunc(fake.Commands(), func(cmd string) bool { return strings.Contains(cmd, "pre-deploy") })
}

func TestDeployRunsThePreDeployCommandAndItsHooks(t *testing.T) {
	d, fake, ran := undoTestDeployer(t, "pre-deploy", "post-deploy", "post-rollback")
	if err := d.Deploy(context.Background(), &DeployOptions{SkipPull: true, LockHeld: true, Hosts: []string{"host"}}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !ranMigration(fake) {
		t.Fatalf("a forward deploy skipped the pre-deploy command: %q", fake.Commands())
	}
	got, _ := os.ReadFile(ran)
	if string(got) != "pre-deploy\npost-deploy\n" {
		t.Fatalf("hooks = %q", got)
	}
}

func TestUndoSkipsThePreDeployCommandAndRunsPostRollback(t *testing.T) {
	d, fake, ran := undoTestDeployer(t, "pre-deploy", "post-deploy", "post-rollback")
	if err := d.Deploy(context.Background(), &DeployOptions{SkipPull: true, LockHeld: true, Undo: true, Hosts: []string{"host"}}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if ranMigration(fake) {
		t.Fatalf("an undo ran the pre-deploy command: %q", fake.Commands())
	}
	got, _ := os.ReadFile(ran)
	if string(got) != "post-rollback\n" {
		t.Fatalf("hooks = %q; an undo runs post-rollback alone", got)
	}
}
