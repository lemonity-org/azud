package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/ssh"
)

func TestSetupIntegration(t *testing.T) {
	if os.Getenv("AZUD_INTEGRATION") == "" {
		t.Skip("AZUD_INTEGRATION not set")
	}

	host := mustGetEnv(t, "AZUD_INTEGRATION_HOST")
	user := mustGetEnv(t, "AZUD_INTEGRATION_USER")
	keyPath := mustGetEnv(t, "AZUD_INTEGRATION_KEY")
	port := getEnvInt("AZUD_INTEGRATION_PORT", 22)
	rootless := getEnvBool("AZUD_INTEGRATION_ROOTLESS", user != "root")
	proxyRootful := getEnvBool("AZUD_INTEGRATION_PROXY_ROOTFUL", false)
	httpPort := integrationHTTPPort(rootless, proxyRootful)

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
	if err := os.WriteFile(".azud/secrets", []byte("AZUD_IT_TOKEN=integration-secret\n"), 0600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	binaryPath := filepath.Join(tempDir, "azud-integration")
	build := exec.Command("go", "build", "-o", binaryPath, "./cmd/azud")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build integration binary: %v\n%s", err, output)
	}

	configText := renderIntegrationConfig(host, user, keyPath, port, rootless, proxyRootful)
	configPath := filepath.Join(tempDir, "config.yml")
	if err := os.WriteFile(configPath, []byte(configText), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	stateDir := filepath.Join(tempDir, "state")

	cleanupRemote(t, host, user, keyPath, port)

	runAzud(t, binaryPath, configPath, stateDir, tempDir,
		"setup",
		"--skip-push",
	)

	client := ssh.NewClient(&ssh.Config{
		User:                  user,
		Port:                  port,
		Keys:                  []string{keyPath},
		InsecureIgnoreHostKey: true,
	})
	t.Cleanup(func() { _ = client.Close() })

	proxyPodmanCommand := "podman"
	if proxyRootful && user != "root" {
		proxyPodmanCommand = "sudo -n podman"
	}
	assertContainerRunningWithPodman(t, client, host, "azud-proxy", proxyPodmanCommand)
	assertContainerRunning(t, client, host, "azud-it")
	assertContainerRunning(t, client, host, "azud-it-worker")
	assertContainerRunning(t, client, host, "azud-it-cache")
	assertRemoteContains(t, client, host,
		"podman inspect --format '{{ index .Config.Labels \"azud.role\" }}:{{ index .Config.Labels \"azud.integration\" }}' azud-it-worker",
		"worker:worker")
	assertRemoteContains(t, client, host, "stat -c '%a' ~/.azud/secrets", "600")
	assertRemoteSuccess(t, client, host, "podman exec azud-it sh -c 'test \"$AZUD_IT_TOKEN\" = integration-secret'")
	assertHTTPAvailable(t, client, host, httpPort)

	if os.Getenv("AZUD_INTEGRATION_FULL") == "" {
		return
	}

	// Exercise buffered, streaming, and PTY SSH paths against a real remote
	// container. The follow assertion observes a log line before terminating
	// the still-running command, proving output is not buffered until exit.
	if output := runAzud(t, binaryPath, configPath, stateDir, tempDir,
		"app", "exec", "--host", host, "--", "sh", "-c", "echo azud-exec-ok"); !strings.Contains(output, "azud-exec-ok") {
		t.Fatalf("non-interactive exec output missing marker: %s", output)
	}
	if output := runAzud(t, binaryPath, configPath, stateDir, tempDir,
		"app", "exec", "--host", host, "-it", "--", "sh", "-c", "echo azud-pty-ok"); !strings.Contains(output, "azud-pty-ok") {
		t.Fatalf("PTY exec output missing marker: %s", output)
	}
	runAzudExpectFailure(t, binaryPath, configPath, stateDir, tempDir,
		"app", "exec", "--host", host, "--", "sh", "-c", "exit 23")
	assertAppLogsFollowStreams(t, binaryPath, configPath, stateDir, tempDir, host, httpPort, client)

	// Exercise an update and explicit rollback with two distinct, published
	// nginx tags. The HTTP assertion after each state change proves the route
	// still has a live upstream.
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "deploy", "--version", "latest")
	assertHTTPAvailable(t, client, host, httpPort)
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "rollback", "alpine")
	assertHTTPAvailable(t, client, host, httpPort)
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "history", "list")

	// Scaling uses the exact managed labels and must return to one stable web
	// instance before canary traffic tests begin.
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "scale", "web=2", "--host", host)
	assertRemoteContains(t, client, host,
		"podman ps --filter label=azud.service=azud-it --filter label=azud.role=web --format '{{.Names}}' | wc -l",
		"2")
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "scale", "web=1", "--host", host)
	assertHTTPAvailable(t, client, host, httpPort)

	// Canary state is deliberately consumed from a second clean working
	// directory while sharing only AZUD_STATE_DIR.
	secondCheckout := filepath.Join(tempDir, "clean-checkout")
	if err := os.MkdirAll(secondCheckout, 0755); err != nil {
		t.Fatalf("create second checkout: %v", err)
	}
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "canary", "deploy", "--version", "latest", "--weight", "10")
	runAzud(t, binaryPath, configPath, stateDir, secondCheckout, "canary", "status")
	runAzud(t, binaryPath, configPath, stateDir, secondCheckout, "canary", "weight", "50")
	runAzud(t, binaryPath, configPath, stateDir, secondCheckout, "canary", "rollback")
	assertHTTPAvailable(t, client, host, httpPort)

	runAzud(t, binaryPath, configPath, stateDir, tempDir, "cron", "boot", "heartbeat", "--host", host)
	assertContainerRunning(t, client, host, "azud-it-cron-heartbeat")
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "cron", "list")
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "cron", "run", "heartbeat", "--host", host)
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "cron", "stop", "heartbeat", "--host", host)
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "accessory", "stop", "cache", "--host", host)
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "accessory", "boot", "cache", "--host", host)
	assertContainerRunning(t, client, host, "azud-it-cache")

	// Move Caddy's live admin listener away from Azud without interrupting
	// data-plane traffic. The next deploy must fail, remove its temporary
	// container, and leave the prior application reachable.
	assertRemoteSuccess(t, client, host,
		`curl -fsS -X PATCH -H 'Content-Type: application/json' --data '"127.0.0.1:2020"' http://127.0.0.1:2019/config/admin/listen`)
	runAzudExpectFailure(t, binaryPath, configPath, stateDir, tempDir, "deploy", "--version", "latest")
	assertHTTPAvailable(t, client, host, httpPort)
	assertRemoteContains(t, client, host,
		"podman ps -a --filter name=azud-it-new- --format '{{.Names}}' | wc -l", "0")
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "proxy", "reboot")
	assertHTTPAvailable(t, client, host, httpPort)

	// Registry and SSH failures must be visible as non-zero CLI exits.
	badRegistryPath := filepath.Join(tempDir, "bad-registry.yml")
	badRegistry := strings.Replace(configText, "\nproxy:\n", "\nregistry:\n  server: docker.io\n  username: integration\n  password:\n    - AZUD_MISSING_REGISTRY_PASSWORD\n\nproxy:\n", 1)
	if err := os.WriteFile(badRegistryPath, []byte(badRegistry), 0600); err != nil {
		t.Fatalf("write registry failure config: %v", err)
	}
	runAzudExpectFailure(t, binaryPath, badRegistryPath, stateDir, tempDir, "deploy", "--version", "alpine")

	badSSHPath := filepath.Join(tempDir, "bad-ssh.yml")
	badSSH := renderIntegrationConfig(host, user, keyPath, 1, rootless, proxyRootful)
	if err := os.WriteFile(badSSHPath, []byte(badSSH), 0600); err != nil {
		t.Fatalf("write SSH failure config: %v", err)
	}
	runAzudExpectFailure(t, binaryPath, badSSHPath, stateDir, tempDir, "app", "details")
	runAzudExpectFailure(t, binaryPath, badSSHPath, stateDir, tempDir, "preflight")

	// Install per-role Quadlets, remove the imperative containers, then start
	// the generated units. This is the same cold-start path used after reboot.
	runAzud(t, binaryPath, configPath, stateDir, tempDir, "systemd", "enable", "--no-start")
	assertRemoteSuccess(t, client, host, "podman rm -f azud-it azud-it-worker")
	proxyPodman := "podman"
	proxySystemctl := "systemctl"
	if rootless && !proxyRootful {
		proxySystemctl = "systemctl --user"
	} else if user != "root" {
		proxyPodman = "sudo -n podman"
		proxySystemctl = "sudo -n systemctl"
	}
	appSystemctl := "systemctl"
	if rootless {
		appSystemctl = "systemctl --user"
	} else if user != "root" {
		appSystemctl = "sudo -n systemctl"
	}
	assertRemoteSuccess(t, client, host, proxyPodman+" rm -f azud-proxy")
	assertRemoteSuccess(t, client, host, appSystemctl+" start azud-it.service azud-it-worker.service")
	assertRemoteSuccess(t, client, host, proxySystemctl+" start azud-proxy.service")
	assertContainerRunning(t, client, host, "azud-it")
	assertContainerRunning(t, client, host, "azud-it-worker")
	assertHTTPAvailable(t, client, host, httpPort)
}

func renderIntegrationConfig(host, user, keyPath string, port int, rootless, proxyRootful bool) string {
	httpPort := integrationHTTPPort(rootless, proxyRootful)
	httpsPort := 8443
	if httpPort == 80 {
		httpsPort = 443
	}
	return fmt.Sprintf(`service: azud-it
image: docker.io/library/nginx:alpine

servers:
  web:
    hosts:
      - %s
  worker:
    hosts:
      - %s
    cmd: "nginx -g 'daemon off;'"
    labels:
      azud.integration: worker
    env:
      AZUD_ROLE: worker

accessories:
  cache:
    image: docker.io/library/busybox:1.37
    host: %s
    cmd: "while true; do sleep 3600; done"
    boot_timeout: 0s

proxy:
  host: example.test
  ssl: false
  rootful: %t
  http_port: %d
  https_port: %d
  app_port: 80
  healthcheck:
    path: /
    interval: 1s
    timeout: 2s

deploy:
  deploy_timeout: 2m
  drain_timeout: 0s
  rollback_on_failure: true
  canary:
    enabled: true
    initial_weight: 10

podman:
  rootless: %t

env:
  secret:
    - AZUD_IT_TOKEN

ssh:
  user: %s
  port: %d
  keys:
    - %s
  connect_timeout: 10s
  command_timeout: 2m
  insecure_ignore_host_key: true

cron:
  heartbeat:
    schedule: "* * * * *"
    command: "echo azud-cron-ok"
    host: %s
    timeout: 30s
    lock: true
`, host, host, host, proxyRootful, httpPort, httpsPort, rootless, user, port, keyPath, host)
}

func integrationHTTPPort(rootless, proxyRootful bool) int {
	if rootless && proxyRootful {
		return 80
	}
	return 8080
}

func TestIntegrationHTTPPortMatchesTopology(t *testing.T) {
	tests := []struct {
		name                   string
		rootless, proxyRootful bool
		want                   int
	}{
		{name: "rootful bridge", want: 8080},
		{name: "rootless bridge", rootless: true, want: 8080},
		{name: "mixed host network", rootless: true, proxyRootful: true, want: 80},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := integrationHTTPPort(test.rootless, test.proxyRootful); got != test.want {
				t.Fatalf("HTTP port = %d, want %d", got, test.want)
			}
		})
	}
}

func runAzud(t *testing.T, binary, configPath, stateDir, workingDir string, args ...string) string {
	t.Helper()
	output, err := executeAzud(binary, configPath, stateDir, workingDir, args...)
	if err != nil {
		t.Fatalf("azud %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func runAzudExpectFailure(t *testing.T, binary, configPath, stateDir, workingDir string, args ...string) string {
	t.Helper()
	output, err := executeAzud(binary, configPath, stateDir, workingDir, args...)
	if err == nil {
		t.Fatalf("azud %s unexpectedly succeeded:\n%s", strings.Join(args, " "), output)
	}
	return output
}

func executeAzud(binary, configPath, stateDir, workingDir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	commandArgs := append([]string{"--config", configPath}, args...)
	command := exec.CommandContext(ctx, binary, commandArgs...)
	command.Dir = workingDir
	command.Env = append(os.Environ(), "AZUD_STATE_DIR="+stateDir)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return string(output), fmt.Errorf("command timed out: %w", ctx.Err())
	}
	return string(output), err
}

func assertContainerRunning(t *testing.T, client *ssh.Client, host, name string) {
	t.Helper()
	assertContainerRunningWithPodman(t, client, host, name, "podman")
}

func assertContainerRunningWithPodman(t *testing.T, client *ssh.Client, host, name, podmanCommand string) {
	t.Helper()

	result, err := client.Execute(host, podmanCommand+" ps --format '{{.Names}}'")
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

func assertRemoteSuccess(t *testing.T, client *ssh.Client, host, command string) string {
	t.Helper()
	result, err := client.Execute(host, command)
	if err != nil {
		t.Fatalf("remote command failed: %v\ncommand: %s", err, command)
	}
	if result.ExitCode != 0 {
		t.Fatalf("remote command exited %d: %s\ncommand: %s", result.ExitCode, result.Stderr, command)
	}
	return result.Stdout
}

func assertRemoteContains(t *testing.T, client *ssh.Client, host, command, want string) {
	t.Helper()
	got := strings.TrimSpace(assertRemoteSuccess(t, client, host, command))
	if !strings.Contains(got, want) {
		t.Fatalf("remote command output %q does not contain %q\ncommand: %s", got, want, command)
	}
}

func assertHTTPAvailable(t *testing.T, client *ssh.Client, host string, port int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		result, err := client.Execute(host, fmt.Sprintf("curl -fsS -H 'Host: example.test' http://127.0.0.1:%d/", port))
		if err == nil && result.ExitCode == 0 && strings.Contains(result.Stdout, "Welcome to nginx") {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = result.Stderr
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("application did not become reachable through Caddy: %s", last)
}

func assertAppLogsFollowStreams(
	t *testing.T,
	binary, configPath, stateDir, workingDir, host string, httpPort int,
	client *ssh.Client,
) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary,
		"--config", configPath,
		"app", "logs", "--follow", "--tail", "0", "--host", host,
	)
	command.Dir = workingDir
	command.Env = append(os.Environ(), "AZUD_STATE_DIR="+stateDir)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("create logs stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start logs follow: %v", err)
	}
	defer func() {
		cancel()
		_ = command.Wait()
	}()

	lines := make(chan string)
	scanErrors := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		scanErrors <- scanner.Err()
	}()

	assertHTTPAvailable(t, client, host, httpPort)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("logs follow ended before streaming a request: %s", stderr.String())
			}
			if strings.Contains(line, "GET / HTTP/") {
				return
			}
		case err := <-scanErrors:
			if err != nil {
				t.Fatalf("read logs follow output: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("logs follow did not stream before timeout: %s", stderr.String())
		}
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

	_, _ = client.Execute(host, "systemctl --user stop azud-it.service azud-it-worker.service azud-proxy.service 2>/dev/null || true")
	_, _ = client.Execute(host, "sudo -n systemctl stop azud-it.service azud-it-worker.service azud-proxy.service 2>/dev/null || true")
	_, _ = client.Execute(host, "podman rm -f azud-proxy azud-it azud-it-worker azud-it-cache azud-it-cron-heartbeat 2>/dev/null || true")
	_, _ = client.Execute(host, "podman network rm azud 2>/dev/null || true")
	_, _ = client.Execute(host, "sudo -n podman rm -f azud-proxy 2>/dev/null || true")
	_, _ = client.Execute(host, "sudo -n podman network rm azud 2>/dev/null || true")
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

func getEnvBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
