//go:build !windows

package podman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/local"
)

// A pull outlasts the command timeout through both ways of pulling, while the
// same slow command through Execute does not.
func TestPullsGetTheLongTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	slow := filepath.Join(t.TempDir(), "slow-podman")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sshClient := local.New(local.Options{Host: "app-1", CommandTimeout: 100 * time.Millisecond, LongCommandTimeout: time.Minute})
	defer func() { _ = sshClient.Close() }()
	client := NewClientWithCommand(sshClient, slow)
	images := NewImageManager(client)

	if err := images.Pull("app-1", "ghcr.io/acme/app:v1"); err != nil {
		t.Errorf("Pull = %v", err)
	}
	if errs := images.PullAll([]string{"app-1"}, "ghcr.io/acme/app:v1"); len(errs) != 0 {
		t.Errorf("PullAll = %v", errs)
	}
	if result, err := client.Execute("app-1", "inspect", "x"); err != nil || result.Success() {
		t.Errorf("Execute = %+v, %v; want the command timeout", result, err)
	}
}

func TestAPullThatTimesOutSaysSo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	slow := filepath.Join(t.TempDir(), "slow-podman")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sshClient := local.New(local.Options{Host: "app-1", CommandTimeout: 100 * time.Millisecond})
	defer func() { _ = sshClient.Close() }()
	images := NewImageManager(NewClientWithCommand(sshClient, slow))

	err := images.PullAll([]string{"app-1"}, "ghcr.io/acme/app:v1")["app-1"]
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("PullAll error = %v, want the reason", err)
	}
}
