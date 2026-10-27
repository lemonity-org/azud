//go:build security

package jit

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
)

// Control: hook discovery must reject traversal and refuse to follow symlinks.
func TestControl_HooksRejectTraversalAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(dir, "CANARY")
	outside := filepath.Join(dir, "outside.sh")
	writeFile(t, outside, "#!/bin/sh\ntouch "+canary+"\n", 0o755)
	if err := os.Symlink(outside, filepath.Join(hooks, "pre-deploy")); err != nil {
		t.Fatal(err)
	}
	logger := output.NewLogger(io.Discard, io.Discard, false)
	runner := deploy.NewHookRunner(hooks, time.Second, logger)

	if err := runner.Run(context.Background(), "pre-deploy", nil); err != nil {
		t.Fatalf("symlinked hook should be skipped without error: %v", err)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("symlinked hook executed its target")
	}

	err := runner.Run(context.Background(), "../outside.sh", nil)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("traversal hook executed")
	}
}
