//go:build security

package jit

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Finding: `azud env set` uses os.WriteFile, which preserves the mode of an
// existing file and follows symlinks.
// ---------------------------------------------------------------------------

func TestFinding_EnvSetPreservesInsecureSecretsMode(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	writeFile(t, secretsPath, "OLD=value\n", 0o644)

	res := runAzud(t, dir, nil, "-c", configPath, "env", "set", "NEW", "value")
	if res.Exit != 0 {
		t.Fatalf("env set failed: %s", res)
	}
	info, err := os.Stat(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("expected mode to remain 0644, got %04o", info.Mode().Perm())
	}
	t.Logf("VULNERABLE: env set left the secrets file world-readable (%04o); it never tightens an existing file", info.Mode().Perm())
}

func TestFinding_EnvSetFollowsSymlinkAndClobbersTarget(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	victim := filepath.Join(dir, "victim.txt")
	writeFile(t, victim, "ORIGINAL CONTENT\n", 0o600)
	if err := os.Remove(secretsPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, secretsPath); err != nil {
		t.Fatal(err)
	}

	res := runAzud(t, dir, nil, "-c", configPath, "env", "set", "PWNED", "yes")
	if res.Exit != 0 {
		t.Fatalf("env set through symlink failed: %s", res)
	}
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "PWNED=yes") || strings.Contains(string(body), "ORIGINAL CONTENT") {
		t.Fatalf("symlink target was not clobbered; body:\n%s", body)
	}
	t.Logf("VULNERABLE: .azud/secrets symlink was followed and %s was overwritten", victim)
}

func TestFinding_EnvSetAllowsNewlineInjection(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	res := runAzud(t, dir, nil, "-c", configPath, "env", "set", "SAFE", "value\nINJECTED=1")
	if res.Exit != 0 {
		t.Fatalf("env set failed: %s", res)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".azud", "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "INJECTED=1") {
		t.Fatalf("expected newline injection in secrets file, got:\n%s", body)
	}
	t.Logf("VULNERABLE (low): a newline in an env set value creates an additional secret entry")
}

// ---------------------------------------------------------------------------
// Finding: env push targets GetAllSSHHosts, which includes builder and bastion
// hosts that never run the application.
// ---------------------------------------------------------------------------

func TestFinding_EnvPushSendsSecretsToRemoteBuilder(t *testing.T) {
	fake := startFakeSSH(t, "0.0.0.0:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts:
      - 127.0.0.2
proxy:
  host: shop.example.com
builder:
  remote:
    host: 127.0.0.1
ssh:
  user: `+currentUser()+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "DB_PASS=supersecret\n", 0o600)

	res := runAzud(t, dir, nil, "-c", configPath, "env", "push")
	if res.Exit != 0 {
		t.Fatalf("env push failed: %s", res)
	}

	// Both configured hosts connect to the same fake listener. Exactly two
	// secret-file writes proves the builder received the application secret,
	// not just the web host.
	writes := fake.commandsMatching("cat > \"$tmp\"")
	if len(writes) != 2 {
		t.Fatalf("expected 2 secret writes (web + builder), got %d:\n%s", len(writes), commandDump(fake.snapshot()))
	}
	for _, write := range writes {
		if !strings.Contains(string(write.Stdin), "DB_PASS=supersecret") {
			t.Fatalf("builder write did not contain the application secret:\n%s", write.Stdin)
		}
	}
	t.Logf("VULNERABLE: env push wrote application secrets to the remote builder host as well as the web host")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

// ---------------------------------------------------------------------------
// Finding: only the literal ".azud/secrets" is excluded from the build
// context. A custom secrets_path is uploaded to a remote builder.
// ---------------------------------------------------------------------------

func TestFinding_BuildContextUploadsCustomSecretsPath(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)

	// Build context contains the custom secrets file and the default one.
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n", 0o644)
	writeFile(t, filepath.Join(dir, "config", "prod.secrets"), "CUSTOM_SECRET=leaked-to-builder\n", 0o600)
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "DEFAULT_SECRET=should-not-upload\n", 0o600)

	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
secrets_provider: file
secrets_path: config/prod.secrets
builder:
  context: `+dir+`
  remote:
    host: 127.0.0.1
ssh:
  user: `+currentUser()+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)

	res := runAzud(t, dir, nil, "-c", configPath, "build", "--no-push")
	if res.Exit != 0 {
		t.Fatalf("remote build failed: %s", res)
	}

	extract := fake.waitCommand(t, "tar xzf -", 5_000_000_000)
	if len(extract.Stdin) == 0 {
		t.Fatalf("no build context archive captured:\n%s", commandDump(fake.snapshot()))
	}
	entries := tarEntries(t, extract.Stdin)
	if _, ok := entries["config/prod.secrets"]; !ok {
		t.Fatalf("custom secrets file was not uploaded; entries: %v", keys(entries))
	}
	if strings.Contains(string(entries[".azud/secrets"]), "should-not-upload") {
		t.Fatalf("default secrets file was uploaded despite the mandatory exclude")
	}
	if !strings.Contains(string(entries["config/prod.secrets"]), "leaked-to-builder") {
		t.Fatalf("custom secrets content missing")
	}
	t.Logf("VULNERABLE: custom secrets_path config/prod.secrets was included in the remote build context")
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Hardening: a world-readable secrets file only produces a warning. The CLI
// still loads it and proceeds.
// ---------------------------------------------------------------------------

func TestFinding_WorldReadableSecretsFileOnlyWarns(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "DB_PASS=hunter2\n", 0o644)

	res := runAzud(t, dir, nil, "-c", configPath, "config")
	if res.Exit != 0 {
		t.Fatalf("config should currently succeed despite insecure mode: %s", res)
	}
	if !strings.Contains(res.Combined, "insecure permissions") {
		t.Fatalf("expected an insecure-permissions warning: %s", res)
	}
	t.Logf("HARDENING: a 0644 secrets file is loaded successfully after a warning; preflight has no mode check")
}

// ---------------------------------------------------------------------------
// Finding: the documented AZUD_ prefix can capture unrelated AZUD_* values and
// env push will deliver them to every host.
// ---------------------------------------------------------------------------

func TestFinding_EnvPrefixPushesUnrelatedSecrets(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
secrets_provider: env
secrets_env_prefix: AZUD_
ssh:
  user: `+currentUser()+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)
	res := runAzud(t, dir, []string{
		"AZUD_SSH_KEY=ssh-key-material",
		"AZUD_APP_SECRET=app-secret-value",
	}, "-c", configPath, "env", "push")
	if res.Exit != 0 {
		t.Fatalf("env push failed: %s", res)
	}
	write := fake.waitCommand(t, "cat > \"$tmp\"", 5*time.Second)
	if !strings.Contains(string(write.Stdin), "ssh-key-material") {
		t.Fatalf("expected unrelated AZUD_* value to be pushed, got:\n%s", write.Stdin)
	}
	t.Logf("VULNERABLE (low/medium): secrets_env_prefix=AZUD_ captured AZUD_SSH_KEY and pushed it to the host")
}

// ---------------------------------------------------------------------------
// Finding: remote build contexts live in a predictable, world-listable /tmp
// directory created with mkdir -p (default umask), not mktemp -d.
// ---------------------------------------------------------------------------

func TestFinding_BuildContextTempDirIsPredictable(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n", 0o644)
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
builder:
  context: `+dir+`
  remote:
    host: 127.0.0.1
ssh:
  user: `+currentUser()+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)
	res := runAzud(t, dir, nil, "-c", configPath, "build", "--no-push")
	if res.Exit != 0 {
		t.Fatalf("build failed: %s", res)
	}
	mkdir := fake.waitCommand(t, "mkdir -p /tmp/azud-build-", 5*time.Second)
	if strings.Contains(mkdir.Command, "chmod 700") || strings.Contains(mkdir.Command, "mktemp") {
		t.Fatalf("remote build dir now uses a private temporary directory; finding may be fixed:\n%s", mkdir.Command)
	}
	t.Logf("VULNERABLE (low): remote build context is created with a predictable name and default umask: %s", mkdir.Command)
}

// Control: env pull writes the local secrets file atomically with private
// modes (contrast with env set above).
func TestControl_EnvPullWritesPrivateSecretsFile(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		if strings.Contains(command, "cat ") {
			return fakeResult{Stdout: "DB_PASS=pulled-secret\n"}
		}
		return fakeResult{}
	})
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))
	res := runAzud(t, dir, nil, "-c", configPath, "env", "pull", "--host", "127.0.0.1")
	if res.Exit != 0 {
		t.Fatalf("env pull failed: %s", res)
	}
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	info, err := os.Stat(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pull wrote secrets mode %04o, want 0600", info.Mode().Perm())
	}
	body, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "DB_PASS=pulled-secret") {
		t.Fatalf("unexpected secrets content:\n%s", body)
	}
}
