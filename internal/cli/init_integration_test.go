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

	if _, err := config.NewLoader("config/deploy.yml", "").Load(); err != nil {
		t.Fatalf("generated GitHub Actions configuration must validate: %v", err)
	}
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
