package deploy

import (
	"testing"

	"github.com/adriancarayol/azud/internal/config"
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
