//go:build !windows

package podman

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/ssh"
)

// fakePodmanLog runs a fake podman that appends its arguments to the returned
// file, and fails `network create` when told to.
func fakePodmanLog(t *testing.T, networkCreateFails bool) (*ContainerManager, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	fail := ""
	if networkCreateFails {
		fail = `[ "$1 $2" = "network create" ] && { echo "no space left" >&2; exit 1; }`
	}
	podman := filepath.Join(dir, "podman")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %s\n%s\necho abc123\n", calls, fail)
	if err := os.WriteFile(podman, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sshClient := ssh.NewClient(&ssh.Config{LocalHost: "app-1"})
	t.Cleanup(func() { _ = sshClient.Close() })
	return NewContainerManager(NewClientWithCommand(sshClient, podman)), calls
}

func TestRunCreatesTheSharedNetworkFirst(t *testing.T) {
	containers, calls := fakePodmanLog(t, false)

	if _, err := containers.Run("app-1", &ContainerConfig{Name: "app", Image: "app:v1", Detach: true, Network: SharedNetwork}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 || lines[0] != "network create --ignore azud" || !strings.HasPrefix(lines[1], "run ") {
		t.Fatalf("podman calls = %q", lines)
	}
}

// A stop_first role creates its container rather than running it.
func TestCreateCreatesTheSharedNetworkFirst(t *testing.T) {
	containers, calls := fakePodmanLog(t, false)

	if _, err := containers.Create("app-1", &ContainerConfig{Name: "app", Image: "app:v1", Network: SharedNetwork}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 || lines[0] != "network create --ignore azud" || !strings.HasPrefix(lines[1], "create ") {
		t.Fatalf("podman calls = %q", lines)
	}
}

func TestRunLeavesOtherNetworksAlone(t *testing.T) {
	for _, network := range []string{"", "host", "other"} {
		t.Run("network="+network, func(t *testing.T) {
			containers, calls := fakePodmanLog(t, false)

			if _, err := containers.Run("app-1", &ContainerConfig{Name: "app", Image: "app:v1", Detach: true, Network: network}); err != nil {
				t.Fatal(err)
			}

			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got), "network create") {
				t.Fatalf("podman calls = %q", got)
			}
		})
	}
}

func TestRunReportsAFailureToCreateTheNetwork(t *testing.T) {
	containers, calls := fakePodmanLog(t, true)

	_, err := containers.Run("app-1", &ContainerConfig{Name: "app", Image: "app:v1", Detach: true, Network: SharedNetwork})
	if err == nil || !strings.Contains(err.Error(), "failed to create network azud: no space left") {
		t.Fatalf("Run = %v", err)
	}
	got, readErr := os.ReadFile(calls)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(got), "run ") {
		t.Fatalf("the container still started: %q", got)
	}
}
