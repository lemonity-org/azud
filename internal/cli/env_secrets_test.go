package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/secretsfile"
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

func TestRunEnvSetReadsValueFromStdin(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		want  string
	}{
		{name: "trailing newline", stdin: "s3cret\n", want: "s3cret"},
		{name: "trailing CRLF", stdin: "s3cret\r\n", want: "s3cret"},
		{name: "no line ending", stdin: "s3cret", want: "s3cret"},
		{name: "equals and spaces", stdin: "a=b c\n", want: "a=b c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
			useLocalSecretsConfig(t, secretsPath)

			cmd, stdout, stderr := newEnvTestCommand(tt.stdin)
			if err := runEnvSet(cmd, []string{"API_TOKEN"}); err != nil {
				t.Fatalf("env set: %v", err)
			}
			if body := readFileString(t, secretsPath); !strings.Contains(body, "API_TOKEN="+tt.want+"\n") {
				t.Fatalf("unexpected secrets file:\n%s", body)
			}
			if strings.Contains(stdout.String()+stderr.String(), tt.want) {
				t.Fatalf("the secret value was echoed:\n%s%s", stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), "deprecated") {
				t.Fatalf("stdin form must not warn:\n%s", stderr.String())
			}
		})
	}
}

func TestRunEnvSetRejectsInvalidStdin(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		want  string
	}{
		{name: "empty", stdin: "", want: "no value provided"},
		{name: "blank line", stdin: "\n", want: "no value provided"},
		{name: "multiple lines", stdin: "value\nINJECTED=1\n", want: "newlines"},
		{name: "embedded carriage return", stdin: "value\rINJECTED=1", want: "newlines"},
		{name: "just over the limit", stdin: strings.Repeat("x", secretsfile.MaxValueBytes+1) + "\n", want: "exceeds 60 KiB"},
		{name: "longer than a readable line", stdin: strings.Repeat("x", 70*1024), want: "exceeds 60 KiB"},
		{name: "far too large", stdin: strings.Repeat("x", 2<<20), want: "exceeds 60 KiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
			useLocalSecretsConfig(t, secretsPath)

			cmd, _, _ := newEnvTestCommand(tt.stdin)
			err := runEnvSet(cmd, []string{"API_TOKEN"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("env set error = %v, want %q", err, tt.want)
			}
			if _, err := os.Stat(secretsPath); !os.IsNotExist(err) {
				t.Fatalf("secrets file was written: %v", err)
			}
		})
	}
}

func TestRunEnvSetLongestValueStaysReadable(t *testing.T) {
	secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
	useLocalSecretsConfig(t, secretsPath)
	long := strings.Repeat("x", secretsfile.MaxValueBytes)

	cmd, _, _ := newEnvTestCommand(long + "\r\n")
	if err := runEnvSet(cmd, []string{"BIG"}); err != nil {
		t.Fatalf("env set BIG: %v", err)
	}
	cmd, _, _ = newEnvTestCommand("other\n")
	if err := runEnvSet(cmd, []string{"OTHER"}); err != nil {
		t.Fatalf("env set OTHER after a long value: %v", err)
	}
	secrets, err := loadSecretsFile(secretsPath)
	if err != nil {
		t.Fatalf("loadSecretsFile: %v", err)
	}
	if secrets["BIG"] != long || secrets["OTHER"] != "other" {
		t.Fatalf("secrets did not round-trip: BIG has %d bytes, OTHER = %q", len(secrets["BIG"]), secrets["OTHER"])
	}

	cmd, _, _ = newEnvTestCommand("")
	if err := runEnvDelete(cmd, []string{"BIG"}); err != nil {
		t.Fatalf("env delete BIG: %v", err)
	}
}

func TestRunEnvSetPromptsWithoutEchoOnTerminal(t *testing.T) {
	secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
	useLocalSecretsConfig(t, secretsPath)
	stdin, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = writer.Close()
	})
	previousIsTerminal, previousReadPassword := stdinIsTerminal, readPassword
	t.Cleanup(func() { stdinIsTerminal, readPassword = previousIsTerminal, previousReadPassword })
	stdinIsTerminal = func(f *os.File) bool { return f == stdin }
	readPassword = func(fd int) ([]byte, error) {
		if uintptr(fd) != stdin.Fd() {
			t.Errorf("readPassword fd = %d, want stdin", fd)
		}
		return []byte("tty-secret"), nil
	}

	cmd, stdout, stderr := newEnvTestCommand("")
	cmd.SetIn(stdin)
	if err := runEnvSet(cmd, []string{"API_TOKEN"}); err != nil {
		t.Fatalf("env set: %v", err)
	}
	if !strings.Contains(stderr.String(), "Value for API_TOKEN: ") {
		t.Fatalf("expected a prompt on stderr, got:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "tty-secret") {
		t.Fatal("the secret value was echoed")
	}
	if body := readFileString(t, secretsPath); !strings.Contains(body, "API_TOKEN=tty-secret\n") {
		t.Fatalf("unexpected secrets file:\n%s", body)
	}
}

func TestRunEnvSetArgvValueWarnsDeprecated(t *testing.T) {
	secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
	useLocalSecretsConfig(t, secretsPath)

	cmd, stdout, stderr := newEnvTestCommand("")
	if err := runEnvSet(cmd, []string{"API_TOKEN", "argv-secret"}); err != nil {
		t.Fatalf("env set: %v", err)
	}
	if !strings.Contains(stderr.String(), "deprecated") || !strings.Contains(stderr.String(), "azud env set API_TOKEN") {
		t.Fatalf("expected a deprecation warning on stderr, got:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "deprecated") {
		t.Fatalf("the deprecation warning must not go to stdout:\n%s", stdout.String())
	}
	if body := readFileString(t, secretsPath); !strings.Contains(body, "API_TOKEN=argv-secret\n") {
		t.Fatalf("unexpected secrets file:\n%s", body)
	}
}

type unreadableStdin struct{ t *testing.T }

func (r unreadableStdin) Read([]byte) (int, error) {
	r.t.Error("stdin must not be read when the key already exists")
	return 0, io.EOF
}

func TestRunEnvSetDoesNotReadValueWhenKeyExistsWithoutForce(t *testing.T) {
	secretsPath := filepath.Join(t.TempDir(), ".azud", "secrets")
	writeSecretsFixture(t, secretsPath, "API_TOKEN=old\n", 0o600)
	useLocalSecretsConfig(t, secretsPath)

	cmd, stdout, _ := newEnvTestCommand("")
	cmd.SetIn(unreadableStdin{t: t})
	if err := runEnvSet(cmd, []string{"API_TOKEN"}); err != nil {
		t.Fatalf("env set: %v", err)
	}
	if !strings.Contains(stdout.String(), "already exists") {
		t.Fatalf("expected an already-exists notice, got:\n%s", stdout.String())
	}
	if body := readFileString(t, secretsPath); body != "API_TOKEN=old\n" {
		t.Fatalf("secrets file changed:\n%s", body)
	}
}
