package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
