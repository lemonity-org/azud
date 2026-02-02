package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookContext_Environ(t *testing.T) {
	ctx := &HookContext{
		Service:     "my-app",
		Image:       "ghcr.io/org/my-app:abc123",
		Version:     "abc123",
		Hosts:       "10.0.0.1,10.0.0.2",
		Destination: "production",
		Performer:   "alice",
		Role:        "",
		HookName:    "pre-deploy",
		RecordedAt:  "2025-01-01T00:00:00Z",
		Runtime:     "",
	}

	env := ctx.Environ()

	lookup := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			lookup[parts[0]] = parts[1]
		}
	}

	// Non-empty fields must be present
	expected := map[string]string{
		"AZUD_SERVICE":     "my-app",
		"AZUD_IMAGE":       "ghcr.io/org/my-app:abc123",
		"AZUD_VERSION":     "abc123",
		"AZUD_HOSTS":       "10.0.0.1,10.0.0.2",
		"AZUD_DESTINATION": "production",
		"AZUD_PERFORMER":   "alice",
		"AZUD_HOOK":        "pre-deploy",
		"AZUD_RECORDED_AT": "2025-01-01T00:00:00Z",
	}

	for key, want := range expected {
		got, ok := lookup[key]
		if !ok {
			t.Errorf("expected %s in env, not found", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// Empty fields must be omitted
	for _, key := range []string{"AZUD_ROLE", "AZUD_RUNTIME"} {
		if _, ok := lookup[key]; ok {
			t.Errorf("expected %s to be omitted (empty field), but found in env", key)
		}
	}
}

func TestHookContext_Environ_Role(t *testing.T) {
	ctx := &HookContext{
		Service: "my-app",
		Role:    "web",
	}

	env := ctx.Environ()

	lookup := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			lookup[parts[0]] = parts[1]
		}
	}

	if got := lookup["AZUD_ROLE"]; got != "web" {
		t.Errorf("AZUD_ROLE = %q, want %q", got, "web")
	}
}

func TestHookRunner_Run_NotFound(t *testing.T) {
	dir := t.TempDir()
	runner := NewHookRunner(dir, 5*time.Second, nil)

	err := runner.Run(context.Background(), "nonexistent", nil)
	if err != nil {
		t.Errorf("Run on non-existent hook should return nil, got: %v", err)
	}
}

func TestHookRunner_Run_NotExecutable(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "my-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	err := runner.Run(context.Background(), "my-hook", nil)
	if err != nil {
		t.Errorf("Run on non-executable hook should return nil (skip), got: %v", err)
	}
}

func TestHookRunner_Run_Success(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "ok-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	err := runner.Run(context.Background(), "ok-hook", nil)
	if err != nil {
		t.Errorf("Run on exit-0 hook should succeed, got: %v", err)
	}
}

func TestHookRunner_Run_Failure(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "fail-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	err := runner.Run(context.Background(), "fail-hook", nil)
	if err == nil {
		t.Fatal("Run on exit-1 hook should return error")
	}
	if !strings.Contains(err.Error(), "fail-hook failed") {
		t.Errorf("error should mention hook name, got: %v", err)
	}
}

func TestHookRunner_Run_Timeout(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "slow-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nsleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 100*time.Millisecond, nil)

	err := runner.Run(context.Background(), "slow-hook", nil)
	if err == nil {
		t.Fatal("Run on slow hook with short timeout should return error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
}

func TestHookRunner_Run_ParentContextCancellation(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "slow-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nsleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}

	// Long timeout so only parent cancellation should stop it
	runner := NewHookRunner(dir, 5*time.Minute, nil)

	parent, cancel := context.WithCancel(context.Background())
	// Cancel the parent context after a short delay
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := runner.Run(parent, "slow-hook", nil)
	if err == nil {
		t.Fatal("Run should return error when parent context is cancelled")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error should mention cancellation, got: %v", err)
	}
}

func TestHookRunner_Run_EnvVars(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "env-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho \"$AZUD_SERVICE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	ctx := &HookContext{
		Service: "test-svc",
	}

	out, err := runner.RunWithOutput(context.Background(), "env-hook", ctx)
	if err != nil {
		t.Fatalf("RunWithOutput should succeed, got: %v", err)
	}
	if !strings.Contains(out, "test-svc") {
		t.Errorf("output should contain service name, got: %q", out)
	}
}

func TestHookRunner_List(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"hook-a", "hook-b"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)
	hooks, err := runner.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 {
		t.Errorf("expected 2 hooks, got %d", len(hooks))
	}
}

func TestHookRunner_Exists(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "exists-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	if !runner.Exists("exists-hook") {
		t.Error("Exists should return true for existing hook")
	}
	if runner.Exists("no-such-hook") {
		t.Error("Exists should return false for non-existing hook")
	}
}

func TestHookContext_Environ_FiltersStaleVars(t *testing.T) {
	// Set a stale AZUD_ variable in the process environment
	t.Setenv("AZUD_SERVICE", "stale-service")
	t.Setenv("AZUD_STALE", "should-be-removed")

	ctx := &HookContext{
		Service: "fresh-service",
	}

	env := ctx.Environ()

	lookup := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			lookup[parts[0]] = parts[1]
		}
	}

	// Fresh value must win over stale
	if got := lookup["AZUD_SERVICE"]; got != "fresh-service" {
		t.Errorf("AZUD_SERVICE = %q, want %q", got, "fresh-service")
	}

	// Stale AZUD_ vars not in the struct must be filtered out
	if _, ok := lookup["AZUD_STALE"]; ok {
		t.Error("expected AZUD_STALE to be filtered out, but found in env")
	}
}

func TestHookRunner_Run_Directory(t *testing.T) {
	dir := t.TempDir()
	// Create a directory with a hook name
	hookDir := filepath.Join(dir, "my-hook")
	if err := os.Mkdir(hookDir, 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	err := runner.Run(context.Background(), "my-hook", nil)
	if err != nil {
		t.Errorf("Run on directory hook should return nil (skip), got: %v", err)
	}
}

func TestHookRunner_List_ExcludesHidden(t *testing.T) {
	dir := t.TempDir()

	// Create a normal hook and a hidden file
	for _, name := range []string{"hook-a", ".gitkeep"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)
	hooks, err := runner.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Errorf("expected 1 hook (hidden excluded), got %d: %v", len(hooks), hooks)
	}
	if len(hooks) > 0 && hooks[0] != "hook-a" {
		t.Errorf("expected hook-a, got %s", hooks[0])
	}
}

func TestHookRunner_Exists_NotExecutable(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "no-exec-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	if runner.Exists("no-exec-hook") {
		t.Error("Exists should return false for non-executable hook")
	}
}

func TestHookRunner_Exists_Directory(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, "dir-hook")
	if err := os.Mkdir(hookDir, 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	if runner.Exists("dir-hook") {
		t.Error("Exists should return false for directory")
	}
}

func TestHookRunner_Run_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	runner := NewHookRunner(dir, 5*time.Second, nil)

	err := runner.Run(context.Background(), "../../../etc/passwd", nil)
	if err == nil {
		t.Fatal("Run with path traversal should return error")
	}
	if !strings.Contains(err.Error(), "escapes hooks directory") {
		t.Errorf("error should mention escaping hooks directory, got: %v", err)
	}
}

func TestHookRunner_Run_Symlink(t *testing.T) {
	dir := t.TempDir()
	// Create a real executable file
	realPath := filepath.Join(dir, "real-hook")
	if err := os.WriteFile(realPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// Create a symlink to it
	linkPath := filepath.Join(dir, "link-hook")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	// Real hook should work
	if err := runner.Run(context.Background(), "real-hook", nil); err != nil {
		t.Errorf("Run on real hook should succeed, got: %v", err)
	}

	// Symlink should be skipped
	if err := runner.Run(context.Background(), "link-hook", nil); err != nil {
		t.Errorf("Run on symlink hook should return nil (skip), got: %v", err)
	}
}

func TestHookRunner_Exists_Symlink(t *testing.T) {
	dir := t.TempDir()
	// Create a real executable file
	realPath := filepath.Join(dir, "real-hook")
	if err := os.WriteFile(realPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// Create a symlink to it
	linkPath := filepath.Join(dir, "link-hook")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	if !runner.Exists("real-hook") {
		t.Error("Exists should return true for real executable hook")
	}
	if runner.Exists("link-hook") {
		t.Error("Exists should return false for symlink hook")
	}
}

func TestHookRunner_Run_NilContext_FiltersAzudVars(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "check-env")
	// Script that prints AZUD_STALE if set, or "clean" if not
	script := "#!/bin/sh\nif [ -n \"$AZUD_STALE\" ]; then echo \"leaked\"; else echo \"clean\"; fi\n"
	if err := os.WriteFile(hookPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	// Set a stale AZUD_ variable
	t.Setenv("AZUD_STALE", "should-not-appear")

	runner := NewHookRunner(dir, 5*time.Second, nil)

	out, err := runner.RunWithOutput(context.Background(), "check-env", nil)
	if err != nil {
		t.Fatalf("RunWithOutput should succeed, got: %v", err)
	}
	if strings.Contains(out, "leaked") {
		t.Error("expected AZUD_STALE to be filtered out when context is nil, but it leaked through")
	}
	if !strings.Contains(out, "clean") {
		t.Errorf("expected 'clean' in output, got: %q", out)
	}
}

func TestHookRunner_List_ExcludesSymlinks(t *testing.T) {
	dir := t.TempDir()
	// Create a regular executable hook
	realPath := filepath.Join(dir, "real-hook")
	if err := os.WriteFile(realPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// Create a symlink hook
	linkPath := filepath.Join(dir, "link-hook")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)
	hooks, err := runner.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Errorf("expected 1 hook (symlink excluded), got %d: %v", len(hooks), hooks)
	}
	if len(hooks) > 0 && hooks[0] != "real-hook" {
		t.Errorf("expected real-hook, got %s", hooks[0])
	}
}

func TestHookRunner_RunWithOutput_Role(t *testing.T) {
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "role-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho \"role=$AZUD_ROLE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}

	runner := NewHookRunner(dir, 5*time.Second, nil)

	ctx := &HookContext{
		Service: "my-app",
		Role:    "web,workers",
	}

	out, err := runner.RunWithOutput(context.Background(), "role-hook", ctx)
	if err != nil {
		t.Fatalf("RunWithOutput should succeed, got: %v", err)
	}
	if !strings.Contains(out, "role=web,workers") {
		t.Errorf("output should contain role, got: %q", out)
	}
}
