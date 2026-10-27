package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
)

func useLocalSecretsConfig(t *testing.T, secretsPath string) {
	t.Helper()
	previousConfig := cfg
	previousForce := envForce
	cfg = &config.Config{SecretsPath: secretsPath}
	envForce = false
	t.Cleanup(func() {
		cfg = previousConfig
		envForce = previousForce
	})
}

func newEnvTestCommand(stdin string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	return cmd, &stdout, &stderr
}

func skipPermissionChecksOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not available on Windows")
	}
}

func writeSecretsFixture(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestRunEnvSetCreatesPrivateSecretsFile(t *testing.T) {
	skipPermissionChecksOnWindows(t)
	secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
	useLocalSecretsConfig(t, secretsPath)

	cmd, _, _ := newEnvTestCommand("")
	if err := runEnvSet(cmd, []string{"DATABASE_URL", "postgres://example"}); err != nil {
		t.Fatalf("env set: %v", err)
	}
	if got := fileMode(t, secretsPath); got != 0o600 {
		t.Fatalf("secrets mode = %04o, want 0600", got)
	}
	if got := fileMode(t, filepath.Dir(secretsPath)); got != 0o700 {
		t.Fatalf(".azud mode = %04o, want 0700", got)
	}
	if body := readFileString(t, secretsPath); !strings.Contains(body, "DATABASE_URL=postgres://example\n") {
		t.Fatalf("unexpected secrets file:\n%s", body)
	}
}

func TestRunEnvSetTightensInsecureSecretsFile(t *testing.T) {
	skipPermissionChecksOnWindows(t)
	secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
	writeSecretsFixture(t, secretsPath, "OLD=value\n", 0o644)
	useLocalSecretsConfig(t, secretsPath)

	cmd, stdout, _ := newEnvTestCommand("")
	if err := runEnvSet(cmd, []string{"NEW", "value"}); err != nil {
		t.Fatalf("env set: %v", err)
	}
	if got := fileMode(t, secretsPath); got != 0o600 {
		t.Fatalf("secrets mode = %04o, want 0600", got)
	}
	if !strings.Contains(stdout.String(), "Tightened permissions") {
		t.Fatalf("expected a permission repair warning, got:\n%s", stdout.String())
	}
	body := readFileString(t, secretsPath)
	if !strings.Contains(body, "OLD=value\n") || !strings.Contains(body, "NEW=value\n") {
		t.Fatalf("secrets were not preserved:\n%s", body)
	}
}

func TestRunEnvSetRefusesSymlinkedSecretsFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	writeSecretsFixture(t, victim, "ORIGINAL CONTENT\n", 0o600)
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	if err := os.MkdirAll(filepath.Dir(secretsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, secretsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	useLocalSecretsConfig(t, secretsPath)

	cmd, _, _ := newEnvTestCommand("")
	err := runEnvSet(cmd, []string{"PWNED", "yes"})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("env set error = %v, want a symbolic link refusal", err)
	}
	if body := readFileString(t, victim); body != "ORIGINAL CONTENT\n" {
		t.Fatalf("symlink target was modified:\n%s", body)
	}
}

func TestRunEnvSetRejectsUnsafeKeysAndValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "newline in value", key: "SAFE", value: "value\nINJECTED=1", want: "newlines"},
		{name: "carriage return in value", key: "SAFE", value: "value\rINJECTED=1", want: "newlines"},
		{name: "nul in value", key: "SAFE", value: "value\x00", want: "NUL"},
		{name: "equals in key", key: "A=B", value: "v", want: "invalid secret name"},
		{name: "comment key", key: "#X", value: "v", want: "invalid secret name"},
		{name: "space in key", key: "A B", value: "v", want: "invalid secret name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
			useLocalSecretsConfig(t, secretsPath)

			cmd, _, _ := newEnvTestCommand("")
			err := runEnvSet(cmd, []string{tt.key, tt.value})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("env set error = %v, want %q", err, tt.want)
			}
			if _, err := os.Stat(secretsPath); !os.IsNotExist(err) {
				t.Fatalf("secrets file was written despite the rejection: %v", err)
			}
		})
	}
}

func TestRunEnvSetFailsWithoutRewritingUnreadableSecretsFile(t *testing.T) {
	secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
	// bufio.Scanner rejects lines over 64 KiB; the old code ignored that error
	// and rewrote the file with only the new key.
	original := "KEEP=me\nHUGE=" + strings.Repeat("x", 70*1024) + "\n"
	writeSecretsFixture(t, secretsPath, original, 0o600)
	useLocalSecretsConfig(t, secretsPath)

	cmd, _, _ := newEnvTestCommand("")
	err := runEnvSet(cmd, []string{"NEW", "value"})
	if err == nil || !strings.Contains(err.Error(), "failed to load secrets") {
		t.Fatalf("env set error = %v, want a load failure", err)
	}
	if body := readFileString(t, secretsPath); body != original {
		t.Fatal("secrets file was rewritten after a load failure")
	}
}

func TestRunEnvDeleteKeepsPrivateModeAndRefusesSymlink(t *testing.T) {
	skipPermissionChecksOnWindows(t)
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	writeSecretsFixture(t, secretsPath, "KEEP=1\nOLD=2\n", 0o644)
	useLocalSecretsConfig(t, secretsPath)

	cmd, _, _ := newEnvTestCommand("")
	if err := runEnvDelete(cmd, []string{"OLD"}); err != nil {
		t.Fatalf("env delete: %v", err)
	}
	if got := fileMode(t, secretsPath); got != 0o600 {
		t.Fatalf("secrets mode = %04o, want 0600", got)
	}
	if body := readFileString(t, secretsPath); strings.Contains(body, "OLD=") || !strings.Contains(body, "KEEP=1") {
		t.Fatalf("unexpected secrets file after delete:\n%s", body)
	}

	victim := filepath.Join(dir, "victim.txt")
	writeSecretsFixture(t, victim, "KEEP=1\n", 0o600)
	if err := os.Remove(secretsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, secretsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cmd, _, _ = newEnvTestCommand("")
	if err := runEnvDelete(cmd, []string{"KEEP"}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("env delete error = %v, want a symbolic link refusal", err)
	}
	if body := readFileString(t, victim); body != "KEEP=1\n" {
		t.Fatalf("symlink target was modified:\n%s", body)
	}
}

func TestRemoteSecretsContentRejectsLineBreaksInValues(t *testing.T) {
	content, err := remoteSecretsContent(map[string]string{"Z_NOTE": "harmless", "DB_PASS": "real-password"})
	if err != nil {
		t.Fatalf("remoteSecretsContent: %v", err)
	}
	if body := string(content); !strings.HasSuffix(body, "\n\nDB_PASS=real-password\nZ_NOTE=harmless\n") {
		t.Fatalf("unexpected remote secrets content:\n%s", body)
	}

	for _, value := range []string{"harmless\nDB_PASS=attacker-password", "harmless\rDB_PASS=attacker-password", "harmless\x00DB_PASS=attacker-password"} {
		content, err := remoteSecretsContent(map[string]string{"DB_PASS": "real-password", "Z_NOTE": value})
		if err == nil {
			t.Fatalf("remoteSecretsContent accepted %q:\n%s", value, content)
		}
		if !strings.Contains(err.Error(), "Z_NOTE") || strings.Contains(err.Error(), "attacker-password") || strings.Contains(err.Error(), "harmless") {
			t.Fatalf("error must name the key and never the value: %v", err)
		}
	}
}

func TestRunEnvEditCreatesPrivateFileAndIgnoresDanglingSymlink(t *testing.T) {
	skipPermissionChecksOnWindows(t)
	t.Setenv("EDITOR", "true")
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	useLocalSecretsConfig(t, secretsPath)

	cmd, _, _ := newEnvTestCommand("")
	if err := runEnvEdit(cmd, nil); err != nil {
		t.Fatalf("env edit: %v", err)
	}
	if got := fileMode(t, secretsPath); got != 0o600 {
		t.Fatalf("secrets mode = %04o, want 0600", got)
	}
	if got := fileMode(t, filepath.Dir(secretsPath)); got != 0o700 {
		t.Fatalf(".azud mode = %04o, want 0700", got)
	}

	if err := os.Chmod(secretsPath, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, stdout, _ := newEnvTestCommand("")
	if err := runEnvEdit(cmd, nil); err != nil {
		t.Fatalf("env edit: %v", err)
	}
	if got := fileMode(t, secretsPath); got != 0o600 || !strings.Contains(stdout.String(), "Tightened permissions") {
		t.Fatalf("env edit did not repair a 0644 file (mode %04o):\n%s", got, stdout.String())
	}

	target := filepath.Join(dir, "created-by-link")
	if err := os.Remove(secretsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, secretsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cmd, stdout, _ = newEnvTestCommand("")
	if err := runEnvEdit(cmd, nil); err != nil {
		t.Fatalf("env edit: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("env edit created the symlink target: %v", err)
	}
	if !strings.Contains(stdout.String(), "symbolic link") {
		t.Fatalf("expected a symbolic link warning, got:\n%s", stdout.String())
	}
}
