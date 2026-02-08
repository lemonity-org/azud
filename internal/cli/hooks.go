package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
)

var hooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Manage deployment hooks",
	Long:  `Commands for listing and testing deployment hooks.`,
}

var hooksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all hooks and their status",
	Long: `Show all standard and custom hooks with their status.

Example:
  azud hooks list`,
	RunE: runHooksList,
}

var hooksRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Run a hook with test context",
	Long: `Run a specific hook with a test deployment context.

This lets you test hooks with realistic AZUD_* environment variables
without triggering a real deployment.

Example:
  azud hooks run pre-deploy
  azud hooks run post-deploy`,
	Args: cobra.ExactArgs(1),
	RunE: runHooksRun,
}

func init() {
	hooksCmd.AddCommand(hooksListCmd)
	hooksCmd.AddCommand(hooksRunCmd)

	rootCmd.AddCommand(hooksCmd)
}

func runHooksList(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	log.Header("Hooks")

	runner := newHookRunner()

	// Collect all hook names: standard + any custom ones on disk
	existing, err := runner.List()
	if err != nil {
		return fmt.Errorf("failed to list hooks: %w", err)
	}

	standardSet := make(map[string]bool)
	for _, name := range deploy.StandardHooks {
		standardSet[name] = true
	}

	var rows [][]string

	// Standard hooks first
	for _, name := range deploy.StandardHooks {
		status := hookStatus(cfg.HooksPath, name)
		rows = append(rows, []string{name, status, "standard"})
	}

	// Custom hooks (on disk but not standard)
	for _, name := range existing {
		if standardSet[name] {
			continue
		}
		status := hookStatus(cfg.HooksPath, name)
		rows = append(rows, []string{name, status, "custom"})
	}

	log.Table([]string{"Name", "Status", "Type"}, rows)
	return nil
}

func hookStatus(hooksPath, name string) string {
	info, err := os.Lstat(filepath.Join(hooksPath, name))
	if os.IsNotExist(err) {
		return "missing"
	}
	if err != nil {
		return "error"
	}
	if info.IsDir() {
		return "directory"
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "symlink"
	}
	if info.Mode()&0111 == 0 {
		return "not executable"
	}
	return "ready"
}

func runHooksRun(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)

	name := args[0]

	runner := newHookRunner()

	if !runner.Exists(name) {
		// Only check hookStatus for simple names (no path traversal)
		if !strings.Contains(name, "/") && !strings.Contains(name, "..") {
			status := hookStatus(cfg.HooksPath, name)
			if status == "not executable" {
				return fmt.Errorf("hook %s is not executable, run: chmod +x %s", name, filepath.Join(cfg.HooksPath, name))
			}
		}
		return fmt.Errorf("hook %s not found in %s", name, cfg.HooksPath)
	}

	ctx := newHookContext()
	ctx.Version = "test"

	return runner.Run(cmd.Context(), name, ctx)
}

// newHookRunner creates a HookRunner from the current config.
func newHookRunner() *deploy.HookRunner {
	return deploy.NewHookRunner(cfg.HooksPath, cfg.Hooks.Timeout, output.DefaultLogger)
}

// newHookContext creates a HookContext pre-filled from the current config.
// Callers can override fields (e.g. Version, Hosts, Image) after creation.
func newHookContext() *deploy.HookContext {
	return &deploy.HookContext{
		Service:     cfg.Service,
		Image:       cfg.Image,
		Hosts:       strings.Join(cfg.GetAllHosts(), ","),
		Destination: GetDestination(),
		Performer:   deploy.CurrentUser(),
		RecordedAt:  time.Now().Format(time.RFC3339),
	}
}
