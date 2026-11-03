//go:build !windows

package podman

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/local"
)

func TestWaitRunning(t *testing.T) {
	tests := []struct {
		name          string
		before, after string
		wantErr       string
	}{
		{name: "stable", before: "echo 'true 0 0'", after: "echo 'true 0 0'"},
		{name: "exited", before: "echo 'true 0 0'", after: "echo 'false 0 3'", wantErr: "exited shortly after start (exit code: 3)"},
		// A real crash loop shows as running about 1% of the time.
		{name: "running between crashes", before: "echo 'false 2 1'", after: "echo 'true 43 1'", wantErr: "restarted 41 time(s) (last exit code: 1)"},
		{name: "stopped between crashes", before: "echo 'false 2 1'", after: "echo 'false 43 1'", wantErr: "restarted 41 time(s)"},
		{name: "recovered long ago", before: "echo 'true 3 1'", after: "echo 'true 3 1'"},
		{name: "gone", before: "exit 125", after: "exit 125", wantErr: "exit code: unknown"},
		{name: "unreadable", before: "echo nonsense", after: "echo nonsense", wantErr: "unexpected inspect output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := t.TempDir()
			podman := filepath.Join(dir, "podman")
			script := fmt.Sprintf("#!/bin/sh\nif [ -e %[1]s/seen ]; then\n%[3]s\nexit\nfi\n: > %[1]s/seen\n%[2]s\n", dir, tt.before, tt.after)
			if err := os.WriteFile(podman, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			sshClient := local.New(local.Options{Host: "app-1"})
			defer func() { _ = sshClient.Close() }()
			containers := NewContainerManager(NewClientWithCommand(sshClient, podman))

			err := containers.WaitRunning("app-1", "app-worker", time.Millisecond)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("WaitRunning = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("WaitRunning = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
