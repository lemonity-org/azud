package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/adriancarayol/azud/internal/output"
)

// HookRunner executes deployment hooks
type HookRunner struct {
	hooksPath string
	log       *output.Logger
}

// NewHookRunner creates a new hook runner
func NewHookRunner(hooksPath string, log *output.Logger) *HookRunner {
	if hooksPath == "" {
		hooksPath = ".azud/hooks"
	}
	if log == nil {
		log = output.DefaultLogger
	}
	return &HookRunner{
		hooksPath: hooksPath,
		log:       log,
	}
}

// Run executes a hook by name
func (h *HookRunner) Run(name string) error {
	hookPath := filepath.Join(h.hooksPath, name)

	// Check if hook exists
	info, err := os.Stat(hookPath)
	if os.IsNotExist(err) {
		h.log.Debug("Hook %s not found, skipping", name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check hook: %w", err)
	}

	// Check if executable
	if info.Mode()&0111 == 0 {
		h.log.Warn("Hook %s is not executable, skipping", name)
		return nil
	}

	h.log.Info("Running hook: %s", name)

	// Execute the hook
	cmd := exec.Command(hookPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hook %s failed: %w", name, err)
	}

	h.log.Success("Hook %s completed", name)
	return nil
}

// RunWithOutput executes a hook and returns its output
func (h *HookRunner) RunWithOutput(name string) (string, error) {
	hookPath := filepath.Join(h.hooksPath, name)

	// Check if hook exists
	if _, err := os.Stat(hookPath); os.IsNotExist(err) {
		return "", nil
	}

	cmd := exec.Command(hookPath)
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("hook %s failed: %w", name, err)
	}

	return string(output), nil
}

// Exists checks if a hook exists
func (h *HookRunner) Exists(name string) bool {
	hookPath := filepath.Join(h.hooksPath, name)
	_, err := os.Stat(hookPath)
	return err == nil
}

// List returns all available hooks
func (h *HookRunner) List() ([]string, error) {
	entries, err := os.ReadDir(h.hooksPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var hooks []string
	for _, entry := range entries {
		if !entry.IsDir() {
			hooks = append(hooks, entry.Name())
		}
	}

	return hooks, nil
}

// StandardHooks are the hooks that Azud recognizes
var StandardHooks = []string{
	"pre-connect",
	"pre-build",
	"pre-deploy",
	"post-deploy",
}
