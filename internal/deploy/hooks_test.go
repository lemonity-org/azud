package deploy

import (
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

func TestHookRunner_Run_NotFound(t *testing.T) {
	dir := t.TempDir()
	runner := NewHookRunner(dir, 5*time.Second, nil)

	err := runner.Run("nonexistent", nil)
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

	err := runner.Run("my-hook", nil)
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

	err := runner.Run("ok-hook", nil)
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

	err := runner.Run("fail-hook", nil)
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

	err := runner.Run("slow-hook", nil)
	if err == nil {
		t.Fatal("Run on slow hook with short timeout should return error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout, got: %v", err)
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

	out, err := runner.RunWithOutput("env-hook", ctx)
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
