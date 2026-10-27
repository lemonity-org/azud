package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestInitCreatesConfigAndHooks(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	resetCLIState()

	rootCmd.SetArgs([]string{"init"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	assertFileExists(t, "config/deploy.yml")
	assertFileExists(t, ".azud/secrets")
	assertFileExists(t, ".azud/hooks/pre-connect")
	assertFileExists(t, ".azud/hooks/pre-build")
	assertFileExists(t, ".azud/hooks/pre-deploy")
	assertFileExists(t, ".azud/hooks/post-deploy")
	assertFileExists(t, ".gitignore")

	gitignore, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if strings.Count(string(gitignore), ".azud/secrets") != 1 {
		t.Fatalf("expected exactly one .azud/secrets entry, got:\n%s", gitignore)
	}

	if _, err := config.NewLoader("config/deploy.yml", "").Load(); err != nil {
		t.Fatalf("generated configuration must load and validate: %v", err)
	}
}

func TestInitCreatesGitHubWorkflowAndGitignoreEntry(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if err := os.WriteFile(".gitignore", []byte("node_modules/\n"), 0644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}

	resetCLIState()

	rootCmd.SetArgs([]string{"init", "--github-actions"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("init --github-actions failed: %v", err)
	}

	assertFileExists(t, filepath.Join(".github", "workflows", "deploy.yml"))

	content, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(content), ".azud/secrets") {
		t.Fatalf("expected .gitignore to include .azud/secrets, got:\n%s", string(content))
	}

	generatedConfig, err := os.ReadFile(filepath.Join("config", "deploy.yml"))
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if !strings.Contains(string(generatedConfig), "secrets_provider: env") ||
		!strings.Contains(string(generatedConfig), "secrets_env_prefix: AZUD_SECRET_") {
		t.Fatalf("GitHub Actions config must use the environment provider:\n%s", generatedConfig)
	}

	workflow, err := os.ReadFile(filepath.Join(".github", "workflows", "deploy.yml"))
	if err != nil {
		t.Fatalf("read generated workflow: %v", err)
	}
	if !strings.Contains(string(workflow), "uses: lemonity-org/azud@v1") {
		t.Fatalf("generated workflow must use the maintained stable action tag")
	}
	if !strings.Contains(string(workflow), "actions/cache@caa296126883cff596d87d8935842f9db880ef25") ||
		!strings.Contains(string(workflow), "cancel-in-progress: false") {
		t.Fatalf("generated workflow must persist and serialize durable deployment state")
	}
	if !strings.Contains(string(workflow), "NO_COLOR: \"1\"") {
		t.Fatalf("generated workflow must declare deterministic plain Azud output")
	}

	if _, err := config.NewLoader("config/deploy.yml", "").Load(); err != nil {
		t.Fatalf("generated GitHub Actions configuration must validate: %v", err)
	}
}

func TestInitCreatesPrivateAzudDir(t *testing.T) {
	skipPermissionChecksOnWindows(t)
	chdirToTempDir(t)
	resetCLIState()

	rootCmd.SetArgs([]string{"init"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	if got := fileMode(t, ".azud"); got != 0o700 {
		t.Errorf(".azud mode = %04o, want 0700", got)
	}
	if got := fileMode(t, filepath.Join(".azud", "secrets")); got != 0o600 {
		t.Errorf(".azud/secrets mode = %04o, want 0600", got)
	}
	// Hooks keep their umask-derived modes; they only need to stay executable.
	if got := fileMode(t, filepath.Join(".azud", "hooks", "pre-deploy")); got&0o100 == 0 {
		t.Errorf("pre-deploy hook mode = %04o, want owner executable", got)
	}
}

func TestInitKeepsExistingSecretsAndHooks(t *testing.T) {
	skipPermissionChecksOnWindows(t)
	chdirToTempDir(t)
	writeSecretsFixture(t, filepath.Join(".azud", "secrets"), "REAL=1\n", 0o644)
	if err := os.MkdirAll(filepath.Join(".azud", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".azud", "hooks", "pre-deploy"), []byte("#!/bin/sh\necho custom\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resetCLIState()

	rootCmd.SetArgs([]string{"init"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	if body := readFileString(t, filepath.Join(".azud", "secrets")); body != "REAL=1\n" {
		t.Fatalf("init overwrote the existing secrets file:\n%s", body)
	}
	if got := fileMode(t, filepath.Join(".azud", "secrets")); got != 0o600 {
		t.Fatalf("existing secrets mode = %04o, want 0600", got)
	}
	if body := readFileString(t, filepath.Join(".azud", "hooks", "pre-deploy")); !strings.Contains(body, "echo custom") {
		t.Fatalf("init overwrote an existing hook:\n%s", body)
	}
	assertFileExists(t, filepath.Join(".azud", "hooks", "post-deploy"))
}

func TestInitRefusesSymlinkedSecretsFile(t *testing.T) {
	dir := chdirToTempDir(t)
	victim := filepath.Join(dir, "victim.txt")
	writeSecretsFixture(t, victim, "ORIGINAL\n", 0o600)
	if err := os.MkdirAll(".azud", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(".azud", "secrets")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resetCLIState()

	rootCmd.SetArgs([]string{"init"})
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("init error = %v, want a symbolic link refusal", err)
	}
	if body := readFileString(t, victim); body != "ORIGINAL\n" {
		t.Fatalf("symlink target was modified:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join("config", "deploy.yml")); !os.IsNotExist(err) {
		t.Fatalf("init wrote a config before refusing: %v", err)
	}
}

func TestInitGitignoreRequiresAnExactActiveRule(t *testing.T) {
	const both = "# Azud secrets\n.azud/secrets\n.azud/.secrets-*.tmp\n"
	const tempOnly = "# Azud secrets\n.azud/.secrets-*.tmp\n"
	tests := []struct {
		name         string
		gitignore    string
		wantAppended string
	}{
		{name: "comment mention", gitignore: "# .azud/secrets are documented elsewhere\nnode_modules/\n", wantAppended: both},
		{name: "directory only rule", gitignore: ".azud/secrets/\n", wantAppended: both},
		{name: "near miss", gitignore: ".azud/secrets.bak\n", wantAppended: both},
		{name: "monorepo path", gitignore: "services/api/.azud/secrets\n", wantAppended: both},
		{name: "negation", gitignore: ".azud/secrets\n!.azud/secrets\n", wantAppended: both},
		{name: "exact rule", gitignore: ".azud/secrets\n", wantAppended: tempOnly},
		{name: "anchored rule", gitignore: "/.azud/secrets\n", wantAppended: tempOnly},
		{name: "temp file negation", gitignore: ".azud/secrets\n.azud/.secrets-*.tmp\n!*.tmp\n", wantAppended: tempOnly},
		{name: "exact rules", gitignore: ".azud/secrets\n.azud/.secrets-*.tmp\n", wantAppended: ""},
		{name: "directory rule", gitignore: ".azud/\n", wantAppended: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chdirToTempDir(t)
			if err := os.WriteFile(".gitignore", []byte(tt.gitignore), 0o644); err != nil {
				t.Fatal(err)
			}
			resetCLIState()

			rootCmd.SetArgs([]string{"init"})
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("init failed: %v", err)
			}

			body := readFileString(t, ".gitignore")
			if !strings.HasPrefix(body, tt.gitignore) {
				t.Fatalf("init rewrote the existing .gitignore:\n%s", body)
			}
			if appended := strings.TrimPrefix(body, tt.gitignore); appended != tt.wantAppended {
				t.Fatalf("appended %q, want %q", appended, tt.wantAppended)
			}
		})
	}
}

func TestInitRefusesSymlinkedGitignore(t *testing.T) {
	dir := chdirToTempDir(t)
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ".gitignore"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resetCLIState()

	rootCmd.SetArgs([]string{"init"})
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("init error = %v, want a symbolic link refusal", err)
	}
	if body := readFileString(t, target); body != "node_modules/\n" {
		t.Fatalf("symlink target was modified:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join("config", "deploy.yml")); !os.IsNotExist(err) {
		t.Fatalf("init wrote a config before refusing: %v", err)
	}
}

// chdirToTempDir runs the test from a fresh directory and returns its path.
func chdirToTempDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	return dir
}

func resetCLIState() {
	initBundle = ""
	initGitHubActions = false
	configPath = ""
	destination = ""
	verbose = false
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}
