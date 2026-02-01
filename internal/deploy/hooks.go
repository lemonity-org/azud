package deploy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/adriancarayol/azud/internal/output"
)

// HookContext provides deployment context to hook scripts via AZUD_* environment variables.
type HookContext struct {
	Service     string // AZUD_SERVICE
	Image       string // AZUD_IMAGE
	Version     string // AZUD_VERSION
	Hosts       string // AZUD_HOSTS (comma-separated)
	Destination string // AZUD_DESTINATION
	Performer   string // AZUD_PERFORMER
	Role        string // AZUD_ROLE
	HookName    string // AZUD_HOOK
	RecordedAt  string // AZUD_RECORDED_AT (RFC3339)
	Runtime     string // AZUD_RUNTIME (seconds, post-deploy only)
}

// Environ returns os.Environ() with AZUD_* entries appended. Empty fields are omitted.
func (ctx *HookContext) Environ() []string {
	env := os.Environ()

	add := func(key, val string) {
		if val != "" {
			env = append(env, key+"="+val)
		}
	}

	add("AZUD_SERVICE", ctx.Service)
	add("AZUD_IMAGE", ctx.Image)
	add("AZUD_VERSION", ctx.Version)
	add("AZUD_HOSTS", ctx.Hosts)
	add("AZUD_DESTINATION", ctx.Destination)
	add("AZUD_PERFORMER", ctx.Performer)
	add("AZUD_ROLE", ctx.Role)
	add("AZUD_HOOK", ctx.HookName)
	add("AZUD_RECORDED_AT", ctx.RecordedAt)
	add("AZUD_RUNTIME", ctx.Runtime)

	return env
}

// CurrentUser returns the current username from $USER, $LOGNAME, or "unknown".
func CurrentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}

// HookRunner executes deployment hooks
type HookRunner struct {
	hooksPath string
	timeout   time.Duration
	log       *output.Logger
}

// NewHookRunner creates a new hook runner
func NewHookRunner(hooksPath string, timeout time.Duration, log *output.Logger) *HookRunner {
	if hooksPath == "" {
		hooksPath = ".azud/hooks"
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if log == nil {
		log = output.DefaultLogger
	}
	return &HookRunner{
		hooksPath: hooksPath,
		timeout:   timeout,
		log:       log,
	}
}

// resolveHook validates that a hook exists and is executable. It returns the
// resolved path or ("", nil) when the hook should be silently skipped.
func (h *HookRunner) resolveHook(name string) (string, error) {
	hookPath := filepath.Join(h.hooksPath, name)

	info, err := os.Stat(hookPath)
	if os.IsNotExist(err) {
		h.log.Debug("Hook %s not found, skipping", name)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to check hook: %w", err)
	}

	if info.Mode()&0111 == 0 {
		h.log.Warn("Hook %s is not executable, skipping", name)
		return "", nil
	}

	return hookPath, nil
}

// hookCmd holds the prepared command and its timeout context, allowing callers
// to run the command and check for deadline errors through a single value.
type hookCmd struct {
	*exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
}

// prepareCmd builds an exec.Cmd for the given hook path and context, applying
// timeout and AZUD_* environment variables.
func (h *HookRunner) prepareCmd(hookPath, name string, ctx *HookContext) *hookCmd {
	if ctx != nil {
		ctx.HookName = name
	}

	runCtx, cancel := context.WithTimeout(context.Background(), h.timeout)
	cmd := exec.CommandContext(runCtx, hookPath)

	if ctx != nil {
		cmd.Env = ctx.Environ()
	} else {
		cmd.Env = os.Environ()
	}

	return &hookCmd{Cmd: cmd, ctx: runCtx, cancel: cancel}
}

// wrapError returns a timeout-aware error for a hook execution failure.
func (h *HookRunner) wrapError(name string, hc *hookCmd, err error) error {
	if hc.ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("hook %s timed out after %s", name, h.timeout)
	}
	return fmt.Errorf("hook %s failed: %w", name, err)
}

// Run executes a hook by name with the given context.
func (h *HookRunner) Run(name string, ctx *HookContext) error {
	hookPath, err := h.resolveHook(name)
	if hookPath == "" || err != nil {
		return err
	}

	h.log.Info("Running hook: %s", name)

	hc := h.prepareCmd(hookPath, name, ctx)
	defer hc.cancel()

	hc.Stdout = os.Stdout
	hc.Stderr = os.Stderr

	if err := hc.Run(); err != nil {
		return h.wrapError(name, hc, err)
	}

	h.log.Success("Hook %s completed", name)
	return nil
}

// RunWithOutput executes a hook and returns its output.
func (h *HookRunner) RunWithOutput(name string, ctx *HookContext) (string, error) {
	hookPath, err := h.resolveHook(name)
	if hookPath == "" || err != nil {
		return "", err
	}

	hc := h.prepareCmd(hookPath, name, ctx)
	defer hc.cancel()

	out, err := hc.CombinedOutput()
	if err != nil {
		return string(out), h.wrapError(name, hc, err)
	}

	return string(out), nil
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
	"post-build",
	"pre-deploy",
	"pre-app-boot",
	"post-app-boot",
	"post-deploy",
	"pre-proxy-reboot",
	"post-proxy-reboot",
	"post-rollback",
}
