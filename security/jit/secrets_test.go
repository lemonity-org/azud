//go:build security

package jit

import (
	"bytes"
	"io"
	"os"
	"os/exec"
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
// Control (L-05): `azud env set KEY` reads the value from stdin, so it never
// appears in argv; the KEY VALUE form still works but warns on stderr.
// ---------------------------------------------------------------------------

func TestControl_EnvSetReadsSecretFromStdin(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	res := runAzudWithStdin(t, dir, "stdin-only-secret\n", "-c", configPath, "env", "set", "API_TOKEN")
	if res.Exit != 0 {
		t.Fatalf("env set from stdin failed: %s", res)
	}
	if strings.Contains(res.Combined, "stdin-only-secret") || strings.Contains(res.Combined, "deprecated") {
		t.Fatalf("stdin form echoed the value or warned: %s", res)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".azud", "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "API_TOKEN=stdin-only-secret\n") {
		t.Fatalf("value from stdin was not stored:\n%s", body)
	}
}

func TestControl_EnvSetStdinRejectsEmbeddedNewline(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	res := runAzudWithStdin(t, dir, "value\nINJECTED=1\n", "-c", configPath, "env", "set", "API_TOKEN")
	if res.Exit == 0 {
		t.Fatalf("env set accepted a multi-line stdin value: %s", res)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".azud", "secrets"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "INJECTED") {
		t.Fatalf("stdin injection reached the secrets file:\n%s", body)
	}
}

func TestControl_EnvSetArgvFormWarns(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	res := runAzud(t, dir, nil, "-c", configPath, "env", "set", "API_TOKEN", "argv-secret")
	if res.Exit != 0 {
		t.Fatalf("the deprecated argv form must keep working: %s", res)
	}
	if !strings.Contains(res.Stderr, "deprecated") {
		t.Fatalf("expected a deprecation warning on stderr: %s", res)
	}
}

func runAzudWithStdin(t *testing.T, dir, stdin string, args ...string) cliResult {
	t.Helper()
	bin := requireCLI(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run azud %v: %v", args, err)
		}
		exit = exitErr.ExitCode()
	}
	return cliResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Combined: stdout.String() + stderr.String(),
		Exit:     exit,
	}
}

// ---------------------------------------------------------------------------
// Control: env push and pull refuse a host that runs no workload. The SSH
// bastion never receives application secrets.
// ---------------------------------------------------------------------------

func envPushBastionConfig(t *testing.T, port int) string {
	return `service: shop
image: example/shop:v1
servers:
  web:
    hosts:
      - 127.0.0.2
proxy:
  host: shop.example.com
ssh:
  user: ` + currentUser() + `
  port: ` + itoa(port) + `
  keys: ["` + filepath.ToSlash(clientKeyPath(t)) + `"]
  proxy:
    host: 127.0.0.1
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`
}

func TestControl_EnvPushRejectsBastionHost(t *testing.T) {
	fake := startFakeSSH(t, "0.0.0.0:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, envPushBastionConfig(t, fake.port()))
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "DB_PASS=supersecret\n", 0o600)

	for _, command := range [][]string{{"env", "push", "--host", "127.0.0.1"}, {"env", "pull", "--host", "127.0.0.1"}} {
		res := runAzud(t, dir, nil, append([]string{"-c", configPath}, command...)...)
		if res.Exit == 0 {
			t.Fatalf("%v targeting the SSH bastion succeeded: %s", command, res)
		}
		if !strings.Contains(res.Combined, "does not run application containers") {
			t.Fatalf("%v: expected a workload-host error, got: %s", command, res)
		}
	}
	if got := fake.snapshot(); len(got) != 0 {
		t.Fatalf("expected no SSH commands for bastion-targeted env push/pull, got:\n%s", commandDump(got))
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
// Control (JIT-15): a world-readable secrets file still loads with an
// actionable warning (backward compatible), but `azud preflight` fails on it.
// ---------------------------------------------------------------------------

func TestControl_WorldReadableSecretsFileStillLoadsWithWarning(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "DB_PASS=hunter2\n", 0o644)

	// `env list` loads secrets; `config` deliberately does not.
	res := runAzud(t, dir, nil, "-c", configPath, "env", "list")
	if res.Exit != 0 {
		t.Fatalf("secret loading must keep loading a 0644 secrets file: %s", res)
	}
	if !strings.Contains(res.Combined, "insecure permissions") || !strings.Contains(res.Combined, "chmod 600") {
		t.Fatalf("expected an actionable insecure-permissions warning: %s", res)
	}
}

func TestControl_PreflightRejectsWorldReadableSecretsFile(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", 1, clientKeyPath(t), "shop"))
	secretsPath := filepath.Join(dir, ".azud", "secrets")
	writeFile(t, secretsPath, "DB_PASS=hunter2\n", 0o644)

	// The mode check runs before DNS and SSH, so no fake server is needed.
	res := runAzud(t, dir, nil, "-c", configPath, "preflight")
	if res.Exit == 0 {
		t.Fatalf("preflight must fail on a world-readable secrets file: %s", res)
	}
	if !strings.Contains(res.Combined, "readable by group or others") || !strings.Contains(res.Combined, "chmod 600") {
		t.Fatalf("expected a secrets mode blocker with a fix hint: %s", res)
	}

	if err := os.Chmod(secretsPath, 0o600); err != nil {
		t.Fatal(err)
	}
	res = runAzud(t, dir, nil, "-c", configPath, "preflight")
	if strings.Contains(res.Combined, "readable by group or others") {
		t.Fatalf("preflight still reports a 0600 secrets file: %s", res)
	}
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
