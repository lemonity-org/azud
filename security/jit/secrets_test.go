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
// Control: env push targets workload hosts only. The remote builder and SSH
// bastion never receive application secrets.
// ---------------------------------------------------------------------------

func envPushBuilderConfig(t *testing.T, port int) string {
	return `service: shop
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
  user: ` + currentUser() + `
  port: ` + itoa(port) + `
  keys: ["` + filepath.ToSlash(clientKeyPath(t)) + `"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`
}

func TestControl_EnvPushTargetsOnlyWorkloadHosts(t *testing.T) {
	fake := startFakeSSH(t, "0.0.0.0:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, envPushBuilderConfig(t, fake.port()))
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "DB_PASS=supersecret\n", 0o600)

	res := runAzud(t, dir, nil, "-c", configPath, "env", "push")
	if res.Exit != 0 {
		t.Fatalf("env push failed: %s", res)
	}

	// Both configured hosts connect to the same fake listener. Exactly one
	// secret-file write proves only the web host received the secret.
	writes := fake.commandsMatching("cat > \"$tmp\"")
	if len(writes) != 1 {
		t.Fatalf("expected 1 secret write (web only), got %d:\n%s", len(writes), commandDump(fake.snapshot()))
	}
	if !strings.Contains(string(writes[0].Stdin), "DB_PASS=supersecret") {
		t.Fatalf("web write did not contain the application secret:\n%s", writes[0].Stdin)
	}
}

func TestControl_EnvPushRejectsRemoteBuilderHost(t *testing.T) {
	fake := startFakeSSH(t, "0.0.0.0:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, envPushBuilderConfig(t, fake.port()))
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "DB_PASS=supersecret\n", 0o600)

	for _, command := range [][]string{{"env", "push", "--host", "127.0.0.1"}, {"env", "pull", "--host", "127.0.0.1"}} {
		res := runAzud(t, dir, nil, append([]string{"-c", configPath}, command...)...)
		if res.Exit == 0 {
			t.Fatalf("%v targeting the remote builder succeeded: %s", command, res)
		}
		if !strings.Contains(res.Combined, "does not run application containers") {
			t.Fatalf("%v: expected a workload-host error, got: %s", command, res)
		}
	}
	if got := fake.snapshot(); len(got) != 0 {
		t.Fatalf("expected no SSH commands for builder-targeted env push/pull, got:\n%s", commandDump(got))
	}
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
// Control: the configured secrets_path and any nested .azud/secrets are never
// uploaded in a remote build context, even when an ignore file negates them.
// ---------------------------------------------------------------------------

func TestControl_BuildContextExcludesConfiguredSecretsPath(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)

	// Build context contains the custom secrets file and the default one.
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n", 0o644)
	writeFile(t, filepath.Join(dir, "config", "prod.secrets"), "CUSTOM_SECRET=leaked-to-builder\n", 0o600)
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "DEFAULT_SECRET=should-not-upload\n", 0o600)
	writeFile(t, filepath.Join(dir, ".dockerignore"), "!config/prod.secrets\n", 0o644)

	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
secrets_provider: file
secrets_path: ./config/prod.secrets
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

	extract := fake.waitCommand(t, "tar xzf -", 5*time.Second)
	if len(extract.Stdin) == 0 {
		t.Fatalf("no build context archive captured:\n%s", commandDump(fake.snapshot()))
	}
	entries := tarEntries(t, extract.Stdin)
	if _, ok := entries["Dockerfile"]; !ok {
		t.Fatalf("Dockerfile missing from the build context; entries: %v", keys(entries))
	}
	for _, secret := range []string{"config/prod.secrets", ".azud/secrets"} {
		if _, ok := entries[secret]; ok {
			t.Fatalf("secrets file %s was uploaded to the remote builder; entries: %v", secret, keys(entries))
		}
	}
}

func TestControl_BuildContextExcludesNestedProjectSecrets(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	repo := t.TempDir()
	project := filepath.Join(repo, "services", "shop")
	writeFile(t, filepath.Join(repo, "Dockerfile"), "FROM scratch\n", 0o644)
	writeFile(t, filepath.Join(project, ".azud", "secrets"), "NESTED=leaked\n", 0o600)

	// The project lives below the build context (monorepo layout), so the
	// default secrets file is at services/shop/.azud/secrets in the archive.
	configPath := writeConfig(t, project, remoteBuildConfig(t, repo, fake.port()))
	res := runAzud(t, project, nil, "-c", configPath, "build", "--no-push")
	if res.Exit != 0 {
		t.Fatalf("remote build failed: %s", res)
	}

	extract := fake.waitCommand(t, "tar xzf -", 5*time.Second)
	entries := tarEntries(t, extract.Stdin)
	if _, ok := entries["Dockerfile"]; !ok {
		t.Fatalf("Dockerfile missing from the build context; entries: %v", keys(entries))
	}
	if _, ok := entries["services/shop/.azud/secrets"]; ok {
		t.Fatalf("nested project secrets were uploaded to the remote builder; entries: %v", keys(entries))
	}
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

	// `env list` loads secrets; `config` deliberately does not.
	res := runAzud(t, dir, nil, "-c", configPath, "env", "list")
	if res.Exit != 0 {
		t.Fatalf("env list should currently succeed despite insecure mode: %s", res)
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
// Control: remote build contexts live in a private mktemp directory that is
// removed on success and on every failure after it was created.
// ---------------------------------------------------------------------------

func remoteBuildConfig(t *testing.T, dir string, port int) string {
	return `service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
builder:
  context: ` + dir + `
  remote:
    host: 127.0.0.1
ssh:
  user: ` + currentUser() + `
  port: ` + itoa(port) + `
  keys: ["` + filepath.ToSlash(clientKeyPath(t)) + `"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`
}

func TestControl_BuildContextUsesPrivateTempDir(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n", 0o644)
	configPath := writeConfig(t, dir, remoteBuildConfig(t, dir, fake.port()))
	res := runAzud(t, dir, nil, "-c", configPath, "build", "--no-push")
	if res.Exit != 0 {
		t.Fatalf("build failed: %s", res)
	}
	mktemp := fake.waitCommand(t, "mktemp -d /tmp/azud-build-shop.", 5*time.Second)
	for _, want := range []string{"umask 077", "chmod 700"} {
		if !strings.Contains(mktemp.Command, want) {
			t.Fatalf("remote build dir command lacks %q:\n%s", want, mktemp.Command)
		}
	}
	if got := fake.commandsMatching("mkdir -p /tmp/azud-build-"); len(got) != 0 {
		t.Fatalf("remote build dir still created with mkdir -p:\n%s", commandDump(got))
	}
	extract := fake.waitCommand(t, "tar xzf -", 5*time.Second)
	if !strings.Contains(extract.Command, fakeBuildDir) {
		t.Fatalf("context was not extracted into the mktemp directory:\n%s", extract.Command)
	}
	fake.waitCommand(t, "rm -rf -- "+fakeBuildDir, 5*time.Second)
}

func TestControl_BuildContextCleanedUpOnSyncFailure(t *testing.T) {
	base := permissive()
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		if strings.Contains(command, "tar xzf -") {
			_, _ = io.ReadAll(stdin)
			return fakeResult{Stderr: "tar: write error", Exit: 2}
		}
		return base(command, stdin)
	})
	dir := newWorkdir(t)
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n", 0o644)
	configPath := writeConfig(t, dir, remoteBuildConfig(t, dir, fake.port()))
	res := runAzud(t, dir, nil, "-c", configPath, "build", "--no-push")
	if res.Exit == 0 {
		t.Fatalf("build succeeded despite a failed context extraction: %s", res)
	}
	fake.waitCommand(t, "rm -rf -- "+fakeBuildDir, 5*time.Second)
}

func TestControl_BuildContextRejectsUnexpectedTempDir(t *testing.T) {
	base := permissive()
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		if strings.Contains(command, "mktemp -d") {
			return fakeResult{Stdout: "/etc\n"}
		}
		return base(command, stdin)
	})
	dir := newWorkdir(t)
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM scratch\n", 0o644)
	configPath := writeConfig(t, dir, remoteBuildConfig(t, dir, fake.port()))
	res := runAzud(t, dir, nil, "-c", configPath, "build", "--no-push")
	if res.Exit == 0 || !strings.Contains(res.Combined, "unexpected path") {
		t.Fatalf("expected an unexpected-path failure, got: %s", res)
	}
	for _, forbidden := range []string{"tar xzf -", "rm -rf"} {
		if got := fake.commandsMatching(forbidden); len(got) != 0 {
			t.Fatalf("CLI ran %q against an unexpected build directory:\n%s", forbidden, commandDump(got))
		}
	}
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

// ---------------------------------------------------------------------------
// Control: env pull distinguishes a missing remote file from a read failure
// and never replaces the local file after a failed or partial read.
// ---------------------------------------------------------------------------

func TestControl_EnvPullFailsOnUnreadableRemoteFile(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		if strings.Contains(command, "cat ") {
			// Partial stdout followed by a failure must not be written.
			return fakeResult{Stdout: "DB_PASS=partial\n", Stderr: "cat: permission denied", Exit: 1}
		}
		return fakeResult{}
	})
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	writeFile(t, secretsPath, "DB_PASS=local\nOTHER=kept\n", 0o600)

	res := runAzud(t, dir, nil, "-c", configPath, "env", "pull", "--host", "127.0.0.1")
	if res.Exit == 0 {
		t.Fatalf("env pull succeeded despite the remote read failure: %s", res)
	}
	if !strings.Contains(res.Combined, "permission denied") {
		t.Fatalf("expected the remote error in the output, got: %s", res)
	}
	read := fake.waitCommand(t, "cat ", 5*time.Second)
	if strings.Contains(read.Command, "|| echo") {
		t.Fatalf("read command still masks failures:\n%s", read.Command)
	}
	body, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "DB_PASS=local\nOTHER=kept\n" {
		t.Fatalf("local secrets file changed after a failed pull:\n%s", body)
	}
}

func TestControl_EnvPullReportsMissingRemoteFile(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		if strings.Contains(command, "cat ") {
			return fakeResult{Exit: 3}
		}
		return fakeResult{}
	})
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "env", "pull", "--host", "127.0.0.1")
	if res.Exit != 0 {
		t.Fatalf("env pull failed for a missing remote file: %s", res)
	}
	if !strings.Contains(res.Combined, "No secrets file on 127.0.0.1") {
		t.Fatalf("expected a missing-file message, got: %s", res)
	}
	if _, err := os.Stat(filepath.Join(dir, ".azud", "secrets")); !os.IsNotExist(err) {
		t.Fatalf("env pull created a local secrets file for a missing remote file (err=%v)", err)
	}
}
