package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// captureStderr redirects os.Stderr to a file for the duration of fn and
// returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = original }()
	fn()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLoadSecretsFromCommandFailureDoesNotLeakOutput(t *testing.T) {
	cfg := &Config{SecretsCommand: "printf 'DB_PASS=hunter2\\n'; echo 'vault: permission denied' >&2; exit 1"}
	var err error
	stderr := captureStderr(t, func() {
		err = (&Loader{}).loadSecretsFromCommand(cfg)
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "secrets_command failed: exit status 1") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error includes command output: %v", err)
	}
	if !strings.Contains(stderr, "vault: permission denied") {
		t.Fatalf("stderr was not passed through, got %q", stderr)
	}
	if strings.Contains(stderr, "hunter2") {
		t.Fatalf("stdout was written to stderr: %q", stderr)
	}
}

func TestLoadSecretsFromCommandParsesStdoutOnly(t *testing.T) {
	cfg := &Config{SecretsCommand: "echo A=1; echo NOISE=x >&2"}
	var err error
	captureStderr(t, func() {
		err = (&Loader{}).loadSecretsFromCommand(cfg)
	})
	if err != nil {
		t.Fatalf("loadSecretsFromCommand() error: %v", err)
	}
	if want := map[string]string{"A": "1"}; !reflect.DeepEqual(cfg.loadedSecrets, want) {
		t.Fatalf("secrets = %v, want %v", cfg.loadedSecrets, want)
	}
}

func TestLoadSecretsFromCommandBackgroundChildDoesNotHang(t *testing.T) {
	original := secretsCommandWaitDelay
	secretsCommandWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { secretsCommandWaitDelay = original })

	cfg := &Config{SecretsCommand: "sleep 2 2>/dev/null & echo A=1"}
	start := time.Now()
	err := (&Loader{}).loadSecretsFromCommand(cfg)
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("loadSecretsFromCommand blocked for %s", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "background process kept its output open") {
		t.Fatalf("expected a wait delay error, got %v", err)
	}
}

func TestLoadSecretsFromCommandTimeout(t *testing.T) {
	original := secretsCommandTimeout
	secretsCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { secretsCommandTimeout = original })

	cfg := &Config{SecretsCommand: "echo A=1; sleep 5"}
	start := time.Now()
	err := (&Loader{}).loadSecretsFromCommand(cfg)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("loadSecretsFromCommand blocked for %s", elapsed)
	}
	if err == nil || err.Error() != "secrets_command timed out after 200ms" {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}

func TestLoadSecretsFromCommandRejectsOversizedOutput(t *testing.T) {
	cfg := &Config{SecretsCommand: "yes A=1 | head -c 1100000"}
	err := (&Loader{}).loadSecretsFromCommand(cfg)
	if err == nil || !strings.Contains(err.Error(), "output exceeds maximum size") {
		t.Fatalf("expected an output size error, got %v", err)
	}
}

func TestLoaderWithoutSecretsSkipsCommand(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "CANARY")
	path := writeEnvTestConfig(t, envTestBaseConfig+"secrets_provider: command\nsecrets_command: touch "+canary+"\n")

	cfg, err := NewLoader(path, "").WithoutSecrets().Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Service != "test" {
		t.Fatalf("service = %q", cfg.Service)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("WithoutSecrets executed secrets_command (stat err: %v)", err)
	}

	if _, err := NewLoader(path, "").Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("Load() did not execute secrets_command: %v", err)
	}
}
