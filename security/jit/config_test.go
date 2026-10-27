//go:build security

package jit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

// ---------------------------------------------------------------------------
// Control: environment references are expanded inside parsed YAML scalar
// values only. An environment value is data and can never become YAML syntax.
// The tests run `env list`, which loads secrets, so an injected
// secrets_command would execute.
// ---------------------------------------------------------------------------

func TestControl_ConfigEnvExpansionCannotInjectYAMLStructure(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: ${SERVICE_VALUE}
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
`)
	canary := filepath.Join(dir, "CANARY")
	injected := "shop\nsecrets_provider: command\nsecrets_command: \"touch " + canary + "\""
	res := runAzud(t, dir, []string{"SERVICE_VALUE=" + injected}, "-c", configPath, "env", "list")
	if res.Exit == 0 {
		t.Fatalf("multi-line environment value was accepted: %s", res)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("injected secrets_command executed (stat err: %v): %s", err, res)
	}
	if !strings.Contains(res.Combined, "SERVICE_VALUE") || !strings.Contains(res.Combined, "control character") {
		t.Fatalf("expected an error naming the variable: %s", res)
	}
	if strings.Contains(res.Combined, "touch") {
		t.Fatalf("error echoed the environment value: %s", res)
	}
}

func TestControl_ConfigEnvExpansionCannotOverrideSSHSecurity(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: ${SERVICE_VALUE}
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
`)
	t.Setenv("SERVICE_VALUE", "shop\nssh:\n  insecure_ignore_host_key: true")

	cfg, err := config.NewLoader(configPath, "").Load()
	if err == nil {
		t.Fatalf("injected config loaded; insecure_ignore_host_key=%v", cfg.SSH.InsecureIgnoreHostKey)
	}
	if cfg != nil {
		t.Fatalf("Load returned a config alongside error %v", err)
	}
}

func TestControl_ConfigEnvExpansionInCommentIsInert(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `# built from ${BRANCH}
service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
`)
	canary := filepath.Join(dir, "CANARY")
	injected := "main\nsecrets_provider: command\nsecrets_command: \"touch " + canary + "\""
	res := runAzud(t, dir, []string{"BRANCH=" + injected}, "-c", configPath, "env", "list")
	if res.Exit != 0 {
		t.Fatalf("a reference in a comment broke loading: %s", res)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("reference in a comment executed secrets_command (stat err: %v): %s", err, res)
	}
}

func TestControl_ConfigEnvExpansionFlowSequenceStaysOneScalar(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [${HOSTS}]
proxy:
  host: shop.example.com
`)
	res := runAzud(t, dir, []string{"HOSTS=127.0.0.1, 10.6.6.6"}, "-c", configPath, "env", "list")
	if res.Exit == 0 {
		t.Fatalf("comma in an environment value added a deploy host: %s", res)
	}
	if !strings.Contains(res.Combined, "servers.web.hosts[0]: invalid host address") {
		t.Fatalf("expected the value to stay one invalid host: %s", res)
	}
}

// ---------------------------------------------------------------------------
// Control: -d/--destination is validated before it is interpolated into a
// filename, so it cannot read a YAML file outside the config directory.
// ---------------------------------------------------------------------------

func TestControl_DestinationPathTraversalIsRejected(t *testing.T) {
	dir := newWorkdir(t)
	base := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
`)
	// filepath.Join("config", "deploy."+"/../../victim"+".yml") would resolve
	// to victim.yml in the current directory.
	writeFile(t, filepath.Join(dir, "victim.yml"), "service: attacker-owned\nimage: attacker/image:v1\n", 0o600)

	res := runAzud(t, dir, nil, "-c", base, "-d", "/../../victim", "config")
	if res.Exit == 0 {
		t.Fatalf("traversing destination was accepted: %s", res)
	}
	if !strings.Contains(res.Combined, "invalid destination") {
		t.Fatalf("expected an invalid destination error: %s", res)
	}
	if strings.Contains(res.Combined, "attacker-owned") {
		t.Fatalf("destination traversal loaded the outside file: %s", res)
	}
}

// ---------------------------------------------------------------------------
// Control: a failing secrets_provider=command never echoes its stdout, which
// commonly contains the secrets it was printing. stderr is passed through.
// ---------------------------------------------------------------------------

func TestControl_SecretsCommandFailureDoesNotLeakStdout(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
secrets_provider: command
secrets_command: |
  printf 'DB_PASS=hunter2\n'
  echo 'vault: permission denied' >&2
  exit 1
`)
	// `env list` loads secrets; `config` deliberately does not.
	res := runAzud(t, dir, nil, "-c", configPath, "env", "list")
	if res.Exit == 0 {
		t.Fatalf("expected failure from secrets_command: %s", res)
	}
	if !strings.Contains(res.Combined, "secrets_command failed") {
		t.Fatalf("expected a secrets_command error: %s", res)
	}
	if strings.Contains(res.Combined, "hunter2") {
		t.Fatalf("secret value from stdout was echoed: %s", res)
	}
	if !strings.Contains(res.Combined, "vault: permission denied") {
		t.Fatalf("expected stderr diagnostics to be passed through: %s", res)
	}
}

// ---------------------------------------------------------------------------
// Control: read-only and shell-completion commands never execute
// secrets_command from the configuration in the current directory.
// ---------------------------------------------------------------------------

func writeCanarySecretsConfig(t *testing.T, dir string) string {
	t.Helper()
	canary := filepath.Join(dir, "CANARY")
	writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
secrets_provider: command
secrets_command: "touch `+canary+`"
`)
	return canary
}

func TestControl_ConfigCommandDoesNotExecuteSecretsCommand(t *testing.T) {
	dir := newWorkdir(t)
	canary := writeCanarySecretsConfig(t, dir)

	res := runAzud(t, dir, nil, "config")
	if res.Exit != 0 {
		t.Fatalf("azud config failed: %s", res)
	}
	if !strings.Contains(res.Combined, "Service: shop") {
		t.Fatalf("unexpected output: %s", res)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("azud config executed secrets_command (stat err: %v): %s", err, res)
	}

	// The same configuration does execute secrets_command for a command that
	// needs secrets, so the check above is meaningful.
	res = runAzud(t, dir, nil, "env", "list")
	if res.Exit != 0 {
		t.Fatalf("azud env list failed: %s", res)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("azud env list did not execute secrets_command: %s", res)
	}
}

func TestControl_ShellCompletionDoesNotExecuteSecretsCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "__complete", args: []string{"__complete", ""}},
		{name: "__completeNoDesc", args: []string{"__completeNoDesc", "de"}},
		{name: "completion bash", args: []string{"completion", "bash"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newWorkdir(t)
			canary := writeCanarySecretsConfig(t, dir)

			res := runAzud(t, dir, nil, tt.args...)
			if res.Exit != 0 {
				t.Fatalf("azud %v failed: %s", tt.args, res)
			}
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatalf("azud %v executed secrets_command (stat err: %v): %s", tt.args, err, res)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Control: document-level limits and validators that must reject hostile input.
// ---------------------------------------------------------------------------

func TestControl_ConfigRejectsOversizedDuplicateAndDeepDocuments(t *testing.T) {
	base := `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
`
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "oversized",
			content: base + "\n# " + strings.Repeat("A", (1<<20)+1) + "\n",
		},
		{
			name:    "duplicate key",
			content: base + "\nservice: other\n",
		},
		{
			name: "deeply nested",
			content: "service: shop\nimage: example/shop:v1\nservers:\n  web:\n    hosts: " +
				strings.Repeat("[", 12000) + strings.Repeat("]", 12000) + "\nproxy:\n  host: shop.example.com\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newWorkdir(t)
			path := writeConfig(t, dir, tt.content)
			res := runAzud(t, dir, nil, "-c", path, "config")
			if res.Exit == 0 {
				t.Fatalf("hostile config was accepted: %s", res)
			}
			t.Logf("rejected as expected: %s", firstLine(res.Combined))
		})
	}
}

func TestControl_ConfigRejectsShellMetacharactersInExecutedFields(t *testing.T) {
	base := `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
`
	tests := []struct {
		name string
		body string
	}{
		{
			name: "health path",
			body: base + `proxy:
  host: shop.example.com
  healthcheck:
    path: /$(touch /tmp/pwn)
`,
		},
		{
			name: "remote secrets path",
			body: base + `proxy:
  host: shop.example.com
secrets_remote_path: "$HOME/.azud/secrets;touch /tmp/pwn"
`,
		},
		{
			name: "host",
			body: `service: shop
image: example/shop:v1
servers:
  web:
    hosts: ["127.0.0.1;touch /tmp/pwn"]
proxy:
  host: shop.example.com
`,
		},
		{
			name: "proxy host",
			body: base + `proxy:
  host: "shop.example.com;touch /tmp/pwn"
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newWorkdir(t)
			path := writeConfig(t, dir, tt.body)
			res := runAzud(t, dir, nil, "-c", path, "config")
			if res.Exit == 0 {
				t.Fatalf("config with shell metacharacters was accepted: %s", res)
			}
			if !strings.Contains(res.Combined, "invalid") && !strings.Contains(res.Combined, "must") {
				t.Fatalf("config was rejected for an unexpected reason: %s", res)
			}
			t.Logf("rejected as expected: %s", firstLine(res.Combined))
		})
	}
}

func TestControl_ImageReferenceValidationRejectsNonReferences(t *testing.T) {
	base := `service: shop
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
`
	tests := []struct {
		name string
		body string
	}{
		{name: "option-shaped image", body: "image: --privileged\n" + base},
		{name: "shell metacharacter image", body: "image: foo;bar\n" + base},
		{name: "leading dash path", body: "image: -x/y\n" + base},
		{
			name: "accessory image",
			body: "image: example/shop:v1\n" + base + `accessories:
  db:
    image: --privileged
    host: 127.0.0.1
`,
		},
		{
			name: "healthcheck helper image",
			body: "image: example/shop:v1\n" + base + `  healthcheck:
    helper_image: --privileged
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newWorkdir(t)
			path := writeConfig(t, dir, tt.body)
			res := runAzud(t, dir, nil, "-c", path, "config")
			if res.Exit == 0 {
				t.Fatalf("invalid image reference was accepted: %s", res)
			}
			if !strings.Contains(res.Combined, "invalid image reference") {
				t.Fatalf("config was rejected for an unexpected reason: %s", res)
			}
		})
	}
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Control (JIT-17): init only treats an exact, active .gitignore rule as
// protecting .azud/secrets. Comments, near misses and negations get the rule
// appended, and a symlinked .gitignore is never written through.
// ---------------------------------------------------------------------------

func TestControl_InitGitignoreIgnoresCommentMentions(t *testing.T) {
	for name, existing := range map[string]string{
		"comment":        "# .azud/secrets are documented elsewhere\nnode_modules/\n",
		"directory only": ".azud/secrets/\n",
		"near miss":      ".azud/secrets.bak\n",
		"monorepo path":  "sub/.azud/secrets\n",
		"negation":       "!.azud/secrets\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, ".gitignore"), existing, 0o644)
			res := runAzud(t, dir, nil, "init")
			if res.Exit != 0 {
				t.Fatalf("init failed: %s", res)
			}
			body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(body), "\n")
			hasExactRule := false
			for _, line := range lines {
				if line == ".azud/secrets" {
					hasExactRule = true
				}
			}
			if !hasExactRule {
				t.Fatalf(".gitignore has no exact .azud/secrets rule:\n%s", body)
			}
			// Where git is available, confirm git itself ignores the file and
			// the secrets writer's temporary copies without relying on the
			// developer's global excludes.
			if _, err := exec.LookPath("git"); err == nil {
				out, code := runShell(t, dir, "git init -q && git -c core.excludesFile=/dev/null check-ignore -q .azud/secrets")
				if code != 0 {
					t.Fatalf("git does not ignore .azud/secrets (exit %d): %s\n%s", code, out, body)
				}
				out, code = runShell(t, dir, "git -c core.excludesFile=/dev/null check-ignore -q .azud/.secrets-123.tmp")
				if code != 0 {
					t.Fatalf("git does not ignore .azud/.secrets-123.tmp (exit %d): %s\n%s", code, out, body)
				}
			}
		})
	}
}

func TestControl_InitRefusesSymlinkedGitignore(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.txt")
	writeFile(t, victim, "ORIGINAL CONTENT\n", 0o644)
	if err := os.Symlink(victim, filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	res := runAzud(t, dir, nil, "init")
	if res.Exit == 0 || !strings.Contains(res.Combined, "symbolic link") {
		t.Fatalf("init must refuse a symlinked .gitignore: %s", res)
	}
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ORIGINAL CONTENT\n" {
		t.Fatalf("symlink target was modified:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// Finding: init protects .azud/secrets in .gitignore but not in a container
// ignore file, and local builds use the raw context. A default Dockerfile that
// does COPY . . can bake the secrets file into the image.
// ---------------------------------------------------------------------------

func TestFinding_InitDoesNotProtectBuildContext(t *testing.T) {
	dir := newWorkdir(t)
	res := runAzud(t, dir, nil, "init")
	if res.Exit != 0 {
		t.Fatalf("init failed: %s", res)
	}
	if _, err := os.Stat(filepath.Join(dir, ".azud", "secrets")); err != nil {
		t.Fatalf("expected generated secrets file: %v", err)
	}
	for _, ignoreFile := range []string{".dockerignore", ".containerignore"} {
		body, err := os.ReadFile(filepath.Join(dir, ignoreFile))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", ignoreFile, err)
		}
		if strings.Contains(string(body), ".azud") {
			t.Fatalf("%s now excludes .azud; finding may be fixed:\n%s", ignoreFile, body)
		}
	}
	t.Logf("VULNERABLE (low/medium): init creates .azud/secrets but no .dockerignore/.containerignore; local `azud build` sends the raw context, so COPY . . can embed the secrets file")
}
