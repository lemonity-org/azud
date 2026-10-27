//go:build security

package jit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

// ---------------------------------------------------------------------------
// Finding: environment expansion happens on the raw YAML bytes, before the
// document is parsed. A value containing a newline is therefore YAML syntax,
// not data.
// ---------------------------------------------------------------------------

func TestFinding_ConfigEnvExpansionBecomesYAMLStructure(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: ${SERVICE_VALUE}
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
`)
	canary := filepath.Join(dir, "CANARY")
	// The injected value closes the scalar and appends top-level keys. The
	// command is executed by loadSecretsFromCommand during `azud config`.
	injected := "shop\nsecrets_provider: command\nsecrets_command: \"touch " + canary + "\""
	res := runAzud(t, dir, []string{"SERVICE_VALUE=" + injected}, "-c", configPath, "config")
	if res.Exit != 0 {
		t.Fatalf("azud config failed unexpectedly: %s", res)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("YAML injection did not execute secrets_command; canary absent: %s", res)
	}
	if !strings.Contains(res.Combined, "Service: shop") {
		t.Fatalf("expected injected service value in output: %s", res)
	}
	t.Logf("VULNERABLE: env value %q was parsed as YAML and executed secrets_command", injected)
}

func TestFinding_ConfigEnvExpansionOverridesSSHSecurity(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: ${SERVICE_VALUE}
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
`)
	injected := "shop\nssh:\n  insecure_ignore_host_key: true"
	t.Setenv("SERVICE_VALUE", injected)

	loader := config.NewLoader(configPath, "")
	cfg, err := loader.Load()
	if err != nil {
		t.Fatalf("load injected config: %v", err)
	}
	if !cfg.SSH.InsecureIgnoreHostKey {
		t.Fatalf("injected ssh.insecure_ignore_host_key did not take effect")
	}
	t.Logf("VULNERABLE: env injection silently enabled ssh.insecure_ignore_host_key")
}

// ---------------------------------------------------------------------------
// Finding: -d/--destination is interpolated into a filename without validation,
// so it can read an arbitrary YAML file outside the config directory.
// ---------------------------------------------------------------------------

func TestFinding_DestinationPathTraversal(t *testing.T) {
	dir := newWorkdir(t)
	base := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
`)
	// filepath.Join("config", "deploy."+"/../../victim"+".yml") resolves to
	// victim.yml in the current directory.
	writeFile(t, filepath.Join(dir, "victim.yml"), "service: attacker-owned\nimage: attacker/image:v1\n", 0o600)

	res := runAzud(t, dir, nil, "-c", base, "-d", "/../../victim", "config")
	if res.Exit != 0 {
		t.Fatalf("config with traversing destination failed: %s", res)
	}
	if !strings.Contains(res.Combined, "Service: attacker-owned") {
		t.Fatalf("destination traversal did not load the outside file: %s", res)
	}
	t.Logf("VULNERABLE: -d %q merged %s", "/../../victim", filepath.Join(dir, "victim.yml"))
}

// ---------------------------------------------------------------------------
// Finding: a failing secrets_provider=command leaks its output, which commonly
// contains the secrets it was printing.
// ---------------------------------------------------------------------------

func TestFinding_SecretsCommandOutputLeaksOnFailure(t *testing.T) {
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  worker:
    hosts: [127.0.0.1]
secrets_provider: command
secrets_command: |
  printf 'DB_PASS=hunter2\n'
  exit 1
`)
	res := runAzud(t, dir, nil, "-c", configPath, "config")
	if res.Exit == 0 {
		t.Fatalf("expected failure from secrets_command: %s", res)
	}
	if !strings.Contains(res.Combined, "hunter2") {
		t.Fatalf("expected secret value to be echoed in the error, got: %s", res)
	}
	t.Logf("VULNERABLE: secrets_command output (including secret values) is embedded in the error message")
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

func TestFinding_ImageReferenceValidationIsPermissive(t *testing.T) {
	dir := newWorkdir(t)
	// isValidImageRef only rejects whitespace/control characters. A leading
	// dash or shell metacharacter is accepted. Shell quoting prevents command
	// injection, but the value can become a podman option.
	path := writeConfig(t, dir, `service: shop
image: --privileged
servers:
  worker:
    hosts: [127.0.0.1]
`)
	res := runAzud(t, dir, nil, "-c", path, "config")
	if res.Exit != 0 {
		t.Fatalf("expected permissive image validation to accept --privileged: %s", res)
	}
	if !strings.Contains(res.Combined, "Image: --privileged") {
		t.Fatalf("unexpected output: %s", res)
	}
	t.Logf("VULNERABLE (low): image reference %q passed validation; repository syntax is not checked", "--privileged")
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
// Finding: init's .gitignore check is a substring test, so a comment that
// merely mentions .azud/secrets suppresses the real ignore rule.
// ---------------------------------------------------------------------------

func TestFinding_InitGitignoreSubstringCheck(t *testing.T) {
	dir := newWorkdir(t)
	writeFile(t, filepath.Join(dir, ".gitignore"), "# .azud/secrets are documented elsewhere\nnode_modules/\n", 0o644)
	res := runAzud(t, dir, nil, "init")
	if res.Exit != 0 {
		t.Fatalf("init failed: %s", res)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	hasExactRule := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == ".azud/secrets" {
			hasExactRule = true
		}
	}
	if hasExactRule {
		t.Fatalf(".gitignore now contains an exact rule; finding may be fixed:\n%s", body)
	}
	t.Logf("VULNERABLE (low/medium): a comment containing '.azud/secrets' suppressed the ignore rule; generated secrets file is not ignored:\n%s", body)
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
