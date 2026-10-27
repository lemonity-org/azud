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
// Control (JIT-04): every local secrets write goes through one hardened
// writer. It repairs loose modes, refuses symlinks, and rejects values or keys
// that would add entries to the line-based file.
// ---------------------------------------------------------------------------

func TestControl_EnvSetTightensInsecureSecretsMode(t *testing.T) {
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
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env set left the secrets file at %04o, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(res.Combined, "Tightened permissions") {
		t.Fatalf("expected a permission repair warning: %s", res)
	}
	dirInfo, err := os.Stat(filepath.Dir(secretsPath))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf(".azud mode = %04o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestControl_EnvSetRefusesSymlinkedSecretsFile(t *testing.T) {
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
	if res.Exit == 0 {
		t.Fatalf("env set through a symlink must fail: %s", res)
	}
	if !strings.Contains(res.Combined, "symbolic link") {
		t.Fatalf("expected a symbolic link refusal: %s", res)
	}
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ORIGINAL CONTENT\n" {
		t.Fatalf("symlink target was modified:\n%s", body)
	}
}

func TestControl_EnvDeleteRefusesSymlinkedSecretsFile(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	victim := filepath.Join(dir, "victim.txt")
	writeFile(t, victim, "KEEP=1\nDROP=2\n", 0o600)
	if err := os.Symlink(victim, secretsPath); err != nil {
		t.Fatal(err)
	}

	res := runAzud(t, dir, nil, "-c", configPath, "env", "delete", "DROP")
	if res.Exit == 0 || !strings.Contains(res.Combined, "symbolic link") {
		t.Fatalf("env delete through a symlink must fail with a refusal: %s", res)
	}
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "KEEP=1\nDROP=2\n" {
		t.Fatalf("symlink target was modified:\n%s", body)
	}
}

func TestControl_EnvSetRejectsNewlineInValue(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	for _, value := range []string{"value\nINJECTED=1", "value\rINJECTED=1"} {
		res := runAzud(t, dir, nil, "-c", configPath, "env", "set", "SAFE", value)
		if res.Exit == 0 {
			t.Fatalf("env set accepted a value with a line break: %s", res)
		}
	}
	body, err := os.ReadFile(filepath.Join(dir, ".azud", "secrets"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "INJECTED") {
		t.Fatalf("line break injection reached the secrets file:\n%s", body)
	}
}

func TestControl_EnvSetRejectsInvalidKey(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	for _, key := range []string{"A=B", "#X", "A B"} {
		res := runAzud(t, dir, nil, "-c", configPath, "env", "set", key, "value")
		if res.Exit == 0 || !strings.Contains(res.Combined, "invalid secret name") {
			t.Fatalf("env set accepted key %q: %s", key, res)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".azud", "secrets")); !os.IsNotExist(err) {
		t.Fatalf("secrets file was written for an invalid key: %v", err)
	}
}

func TestControl_EnvPushRejectsNewlineInEnvProviderValue(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	cfg := strings.Replace(envPrefixConfig(t, fake.port(), ""), "secrets_env_prefix: AZUD_", "secrets_env_prefix: APPSEC_", 1)
	configPath := writeConfig(t, dir, cfg)

	res := runAzud(t, dir, []string{
		"APPSEC_DB_PASS=real-password",
		"APPSEC_Z_NOTE=harmless\nDB_PASS=attacker-password",
	}, "-c", configPath, "env", "push")
	if res.Exit == 0 {
		t.Fatalf("env push accepted a newline-bearing value: %s", res)
	}
	if writes := fake.commandsMatching("cat > \"$tmp\""); len(writes) != 0 {
		t.Fatalf("secrets were written despite the invalid value:\n%s", commandDump(writes))
	}
	if !strings.Contains(res.Combined, "Z_NOTE") || strings.Contains(res.Combined, "attacker-password") || strings.Contains(res.Combined, "harmless") {
		t.Fatalf("error must name the key and never the value: %s", res)
	}
}

func TestControl_InitCreatesPrivateAzudDir(t *testing.T) {
	dir := t.TempDir()
	res := runAzud(t, dir, nil, "init")
	if res.Exit != 0 {
		t.Fatalf("init failed: %s", res)
	}
	for path, want := range map[string]os.FileMode{".azud": 0o700, ".azud/secrets": 0o600} {
		info, err := os.Stat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}

func TestControl_InitKeepsExistingSecretsAndHooks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "REAL=1\n", 0o600)
	writeFile(t, filepath.Join(dir, ".azud", "hooks", "pre-deploy"), "#!/bin/sh\necho custom\n", 0o755)

	res := runAzud(t, dir, nil, "init")
	if res.Exit != 0 {
		t.Fatalf("init failed: %s", res)
	}
	secrets, err := os.ReadFile(filepath.Join(dir, ".azud", "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if string(secrets) != "REAL=1\n" {
		t.Fatalf("init overwrote the existing secrets file:\n%s", secrets)
	}
	hook, err := os.ReadFile(filepath.Join(dir, ".azud", "hooks", "pre-deploy"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hook), "echo custom") {
		t.Fatalf("init overwrote an existing hook:\n%s", hook)
	}
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
// Control: a broad AZUD_ prefix never captures Azud-owned AZUD_* variables or
// infrastructure credentials as application secrets, and it is warned about.
// ---------------------------------------------------------------------------

func envPrefixConfig(t *testing.T, port int, extra string) string {
	return `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
secrets_provider: env
secrets_env_prefix: AZUD_
` + extra + `ssh:
  user: ` + currentUser() + `
  port: ` + itoa(port) + `
  keys: ["` + filepath.ToSlash(clientKeyPath(t)) + `"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`
}

func TestControl_EnvPrefixSkipsReservedAzudVariables(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, envPrefixConfig(t, fake.port(), ""))
	stateDir := t.TempDir()
	res := runAzud(t, dir, []string{
		"AZUD_SSH_KEY=ssh-key-material",
		"AZUD_STATE_DIR=" + stateDir,
		"AZUD_APP_SECRET=app-secret-value",
	}, "-c", configPath, "env", "push")
	if res.Exit != 0 {
		t.Fatalf("env push failed: %s", res)
	}
	write := fake.waitCommand(t, "cat > \"$tmp\"", 5*time.Second)
	if !strings.Contains(string(write.Stdin), "APP_SECRET=app-secret-value") {
		t.Fatalf("application secret was not pushed:\n%s", write.Stdin)
	}
	for _, leaked := range []string{"ssh-key-material", "SSH_KEY=", "STATE_DIR="} {
		if strings.Contains(string(write.Stdin), leaked) {
			t.Fatalf("Azud-owned variable leaked into the runtime secrets (%s):\n%s", leaked, write.Stdin)
		}
	}
	if !strings.Contains(res.Stderr, "WARN") || !strings.Contains(res.Stderr, "AZUD_SECRET_") || !strings.Contains(res.Stderr, "AZUD_SSH_KEY") {
		t.Fatalf("expected broad-prefix and skipped-variable warnings on stderr, got: %s", res)
	}
	if strings.Contains(res.Combined, "ssh-key-material") {
		t.Fatalf("warning printed a secret value: %s", res)
	}
}

func TestControl_EnvPrefixDoesNotPushRegistryCredential(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, envPrefixConfig(t, fake.port(), `registry:
  server: ghcr.io
  username: deployer
  password: [AZUD_REGISTRY_PASSWORD]
`))
	env := []string{
		"AZUD_REGISTRY_PASSWORD=reg-token-xyz",
		"AZUD_APP_SECRET=app-secret-value",
	}

	res := runAzud(t, dir, env, "-c", configPath, "env", "push")
	if res.Exit != 0 {
		t.Fatalf("env push failed: %s", res)
	}
	write := fake.waitCommand(t, "cat > \"$tmp\"", 5*time.Second)
	if strings.Contains(string(write.Stdin), "reg-token-xyz") {
		t.Fatalf("registry credential was pushed into the runtime secrets:\n%s", write.Stdin)
	}

	// Registry login still resolves the credential from the environment.
	res = runAzud(t, dir, env, "-c", configPath, "registry", "login")
	if res.Exit != 0 {
		t.Fatalf("registry login failed: %s", res)
	}
	login := fake.waitCommand(t, "--password-stdin", 5*time.Second)
	if !strings.Contains(string(login.Stdin), "reg-token-xyz") {
		t.Fatalf("registry login did not receive the credential:\n%s", login.Stdin)
	}
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
