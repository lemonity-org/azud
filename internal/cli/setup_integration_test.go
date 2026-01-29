package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/adriancarayol/azud/internal/ssh"
)

func TestSetupIntegration(t *testing.T) {
	if os.Getenv("AZUD_INTEGRATION") == "" {
		t.Skip("AZUD_INTEGRATION not set")
	}

	host := mustGetEnv(t, "AZUD_INTEGRATION_HOST")
	user := mustGetEnv(t, "AZUD_INTEGRATION_USER")
	keyPath := mustGetEnv(t, "AZUD_INTEGRATION_KEY")
	port := getEnvInt("AZUD_INTEGRATION_PORT", 22)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if err := os.MkdirAll(".azud", 0755); err != nil {
		t.Fatalf("mkdir .azud: %v", err)
	}
	if err := os.WriteFile(".azud/secrets", []byte(""), 0600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

	configPath := filepath.Join(tempDir, "config.yml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(host, user, keyPath, port)), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cleanupRemote(t, host, user, keyPath, port)

	resetCLIState()
	rootCmd.SetArgs([]string{
		"--config", configPath,
		"setup",
		"--skip-push",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	client := ssh.NewClient(&ssh.Config{
		User:                  user,
		Port:                  port,
		Keys:                  []string{keyPath},
		InsecureIgnoreHostKey: true,
	})
	t.Cleanup(func() { _ = client.Close() })

	assertContainerRunning(t, client, host, "azud-proxy")
	assertContainerRunning(t, client, host, "azud-it")
}

func renderIntegrationConfig(host, user, keyPath string, port int) string {
	return fmt.Sprintf(`service: azud-it
image: docker.io/library/nginx:alpine

servers:
  web:
    hosts:
      - %s

proxy:
  host: example.test
  ssl: false
  http_port: 8080
  https_port: 8443
  app_port: 80
  healthcheck:
    path: /

ssh:
  user: %s
  port: %d
  keys:
    - %s
`, host, user, port, keyPath)
}

func assertContainerRunning(t *testing.T, client *ssh.Client, host, name string) {
	t.Helper()

	result, err := client.Execute(host, "podman ps --format '{{.Names}}'")
	if err != nil {
		t.Fatalf("podman ps failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("podman ps exit code %d: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, name) {
		t.Fatalf("expected container %s to be running, got:\n%s", name, result.Stdout)
	}
}

func cleanupRemote(t *testing.T, host, user, keyPath string, port int) {
	t.Helper()

	client := ssh.NewClient(&ssh.Config{
		User:                  user,
		Port:                  port,
		Keys:                  []string{keyPath},
		InsecureIgnoreHostKey: true,
	})
	defer func() { _ = client.Close() }()

	_, _ = client.Execute(host, "podman rm -f azud-proxy azud-it 2>/dev/null || true")
	_, _ = client.Execute(host, "podman network rm azud 2>/dev/null || true")
}

func mustGetEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("missing required env %s", key)
	}
	return value
}

func getEnvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
