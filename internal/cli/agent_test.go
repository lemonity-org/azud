package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/local"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/state"
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

	creds, err := registryCredentials(cfg, registry.Reference{Registry: "ghcr.io", Repository: "o/app", Tag: "production"})
	if err != nil || creds == nil || creds.Username != "deploy" || creds.Password != "read-only" {
		t.Fatalf("credentials for the configured registry = %+v, %v", creds, err)
	}
	if creds, err := registryCredentials(cfg, registry.Reference{Registry: "docker.io", Repository: "library/nginx", Tag: "1"}); creds != nil || err != nil {
		t.Fatalf("credentials sent to another registry: %+v, %v", creds, err)
	}

	cfg.Registry.Server = ""
	if creds, err := registryCredentials(cfg, registry.Reference{Registry: "docker.io", Repository: "acme/app", Tag: "prod"}); creds == nil || err != nil {
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

	_, err := registryCredentials(cfg, registry.Reference{Registry: "ghcr.io", Repository: "o/app", Tag: "production"})
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
	client := local.New(local.Options{Host: "app-1"})
	defer func() { _ = client.Close() }()
	running := runningDigest(podman.NewClientWithCommand(client, fakePodman), cfg, "app-1", "ghcr.io/o/app")

	tests := []struct {
		name        string
		web, worker string
		want        string
	}{
		{"both roles on the digest", "echo true ghcr.io/o/app@" + digest, "echo true ghcr.io/o/app@" + digest, digest},
		{"roles on different digests", "echo true ghcr.io/o/app@" + digest, "echo true ghcr.io/o/app@" + other, ""},
		{"a role deployed by tag", "echo true ghcr.io/o/app@" + digest, "echo true ghcr.io/o/app:latest", ""},
		{"a role missing", "echo true ghcr.io/o/app@" + digest, "exit 125", ""},
		{"another image", "echo true ghcr.io/o/other@" + digest, "echo true ghcr.io/o/other@" + digest, ""},
		{"a role stopped", "echo true ghcr.io/o/app@" + digest, "echo false ghcr.io/o/app@" + digest, ""},
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

type recordingLocker struct {
	host, lockFile string
	timeout        time.Duration
}

func (l *recordingLocker) WithRemoteLock(host, lockFile string, timeout time.Duration, fn func() error) error {
	l.host, l.lockFile, l.timeout = host, lockFile, timeout
	return fn()
}

func TestCommandDeployRunsUnderTheDeployLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	envFile := filepath.Join(t.TempDir(), "env")
	cfg := &config.Config{
		Service: "app",
		SSH:     config.SSHConfig{User: "deploy"},
		Deploy:  config.DeployConfig{DeployTimeout: time.Minute},
		Agent: config.AgentConfig{
			DeployCommand: `printf '%s\n' "$AZUD_AGENT_IMAGE" "$AZUD_SERVICE" "$AZUD_AGENT_REF" "$AZUD_DEPLOY_LOCK" > ` + envFile,
			DeployTimeout: time.Minute,
		},
	}
	locker := &recordingLocker{}
	image := "ghcr.io/acme/app@sha256:" + strings.Repeat("a", 64)
	deployFn := commandDeploy(cfg, "ghcr.io/acme/app:production", io.Discard)
	lock := deployLock(cfg, locker, "203.0.113.10")
	if err := lock(func() error { return deployFn(context.Background(), image) }); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	wantLock, wantTimeout := deploy.DeployLock(cfg)
	if locker.host != "203.0.113.10" || locker.lockFile != wantLock || locker.timeout != wantTimeout {
		t.Fatalf("lock = %+v; want %s on the agent host", locker, wantLock)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	absoluteLock := filepath.Join(home, state.UserStateDirName, "app.deploy.lock")
	want := strings.Join([]string{image, "app", "ghcr.io/acme/app:production", absoluteLock}, "\n") + "\n"
	if string(got) != want {
		t.Fatalf("command environment = %q, want %q", got, want)
	}
}

func TestCommandDeployReportsTheCommandFailure(t *testing.T) {
	cfg := &config.Config{
		Service: "app",
		SSH:     config.SSHConfig{User: "deploy"},
		Agent:   config.AgentConfig{DeployCommand: `echo "contract preflight failed" >&2; exit 1`},
	}
	err := commandDeploy(cfg, "r", io.Discard)(context.Background(), "ghcr.io/acme/app@sha256:"+strings.Repeat("b", 64))
	if err == nil || !strings.Contains(err.Error(), "contract preflight failed") {
		t.Fatalf("deploy = %v; want the command output", err)
	}
}

func TestCommandDeployLockIsTheFileTheShellLocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &config.Config{
		Service: "app",
		SSH:     config.SSHConfig{User: "deploy"},
		Deploy:  config.DeployConfig{DeployTimeout: time.Minute},
	}
	lockFile, _ := deploy.DeployLock(cfg)
	expanded, err := exec.Command("/bin/sh", "-c", `printf %s "`+lockFile+`"`).Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := localPath(lockFile)
	if err != nil || got != string(expanded) {
		t.Fatalf("AZUD_DEPLOY_LOCK = %q, %v; the shell locks %q", got, err, expanded)
	}
}

func TestBundleAgentRefusesSecretsPathsThatCollide(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	bundle := config.AgentConfig{Source: config.AgentSourceBundle}

	cfg := &config.Config{SecretsPath: ".azud/secrets", Agent: bundle}
	if err := checkBundleSecretsPaths(cfg); err == nil || !strings.Contains(err.Error(), "registry password") {
		t.Fatalf("defaults run from the home directory = %v; each release would overwrite the agent's secrets", err)
	}

	for name, ok := range map[string]*config.Config{
		"another runtime file": {SecretsPath: ".azud/secrets", SecretsRemotePath: "$HOME/run/secrets", Agent: bundle},
		"another own file":     {SecretsPath: "agent/secrets", Agent: bundle},
		"image source":         {SecretsPath: ".azud/secrets"},
		"another provider":     {SecretsPath: ".azud/secrets", SecretsProvider: "env", Agent: bundle},
	} {
		if err := checkBundleSecretsPaths(ok); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	other := t.TempDir()
	t.Chdir(other)
	if err := checkBundleSecretsPaths(&config.Config{SecretsPath: ".azud/secrets", Agent: bundle}); err != nil {
		t.Fatalf("defaults run from another directory = %v", err)
	}

	// The same file under two names is still the same file.
	runtimeFile := filepath.Join(home, ".azud", "secrets")
	if err := os.MkdirAll(filepath.Dir(runtimeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(runtimeFile, filepath.Join(other, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := checkBundleSecretsPaths(&config.Config{SecretsPath: "linked", Agent: bundle}); err == nil {
		t.Fatal("a symlink to the runtime file was accepted")
	}
}

func TestReleaseClientUsesTheReleasesCommandTimeout(t *testing.T) {
	host := "app-1"
	server := &config.Config{SSH: config.SSHConfig{User: "deploy", CommandTimeout: 200 * time.Millisecond}}
	own := local.New(local.Options{Context: context.Background(), User: "deploy", CommandTimeout: server.SSH.CommandTimeout, Host: host})
	defer func() { _ = own.Close() }()

	same, closeSame := releaseClient(context.Background(), own, server, server, host)
	defer closeSame()
	if same != own {
		t.Fatal("a release with the server's timeout got a client of its own")
	}

	longer := &config.Config{SSH: config.SSHConfig{User: "deploy", CommandTimeout: time.Minute}}
	client, closeClient := releaseClient(context.Background(), own, server, longer, host)
	defer closeClient()
	if result, err := own.Execute(host, "sleep 1"); err == nil && result.ExitCode == 0 {
		t.Fatal("the agent's own client did not enforce its timeout")
	}
	if result, err := client.Execute(host, "sleep 1"); err != nil || result.ExitCode != 0 {
		t.Fatalf("release client = %+v, %v; want the release's longer timeout", result, err)
	}
}

func TestReleaseClientUsesTheReleasesPullTimeout(t *testing.T) {
	host := "app-1"
	server := &config.Config{
		SSH:    config.SSHConfig{User: "deploy", CommandTimeout: 200 * time.Millisecond},
		Deploy: config.DeployConfig{PullTimeout: 200 * time.Millisecond},
	}
	own := local.New(local.Options{Context: context.Background(), User: "deploy", CommandTimeout: server.SSH.CommandTimeout, LongCommandTimeout: server.Deploy.PullTimeout, Host: host})
	defer func() { _ = own.Close() }()

	longer := *server
	longer.Deploy.PullTimeout = time.Minute
	client, closeClient := releaseClient(context.Background(), own, server, &longer, host)
	defer closeClient()
	if client == own {
		t.Fatal("a release with a longer pull timeout kept the agent's client")
	}
	if result, err := client.ExecuteLong(host, "sleep 1"); err != nil || result.ExitCode != 0 {
		t.Fatalf("release client = %+v, %v; want the release's longer pull timeout", result, err)
	}
	if result, err := client.Execute(host, "sleep 1"); err == nil && result.ExitCode == 0 {
		t.Fatal("the release's pull timeout reached other commands")
	}
}

func TestDeployLockReportsALockItCannotGet(t *testing.T) {
	cfg := &config.Config{Service: "app", SSH: config.SSHConfig{User: "deploy"}}
	lock := deployLock(cfg, failingLocker{}, "h")
	ran := false
	err := lock(func() error { ran = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "failed to acquire deployment lock") || ran {
		t.Fatalf("lock = %v, ran = %v", err, ran)
	}

	want := errors.New("deploy failed")
	if err := deployLock(cfg, &recordingLocker{}, "h")(func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("lock = %v; the function's own error comes back", err)
	}
}

type failingLocker struct{}

func (failingLocker) WithRemoteLock(string, string, time.Duration, func() error) error {
	return errors.New("timed out")
}

func TestImagePresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the local transport needs a POSIX shell")
	}
	t.Setenv("HOME", t.TempDir())
	fakePodman := filepath.Join(t.TempDir(), "podman")
	script := "#!/bin/sh\ncase \"$3\" in\n  here) echo sha256:abc ;;\n  *) echo 'no such image' >&2; exit 125 ;;\nesac\n"
	if err := os.WriteFile(fakePodman, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	client := local.New(local.Options{Host: "app-1"})
	defer func() { _ = client.Close() }()
	podmanClient := podman.NewClientWithCommand(client, fakePodman)
	if !imagePresent(podmanClient, "app-1", "here") {
		t.Fatal("an image on this server was not found")
	}
	if imagePresent(podmanClient, "app-1", "gone") {
		t.Fatal("an image not on this server was found, so the deploy would not pull it")
	}
}
