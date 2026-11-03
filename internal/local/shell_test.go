//go:build !windows

package local

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestShell(t *testing.T, commandTimeout time.Duration) *Shell {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	client := New(Options{Host: "app-1", CommandTimeout: commandTimeout})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestLocalTransportRefusesOtherHosts(t *testing.T) {
	client := newTestShell(t, 0)

	if _, err := client.Execute("app-2", "true"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("Execute on another host error = %v, want refusal", err)
	}
	if err := client.Upload("app-2", "/etc/hostname", "x"); err == nil {
		t.Fatal("Upload to another host succeeded, want refusal")
	}
}

func TestLocalExecuteMatchesSSHResultSemantics(t *testing.T) {
	client := newTestShell(t, 0)

	result, err := client.Execute("app-1", `printf out; printf err >&2; exit 3`)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Host != "app-1" || result.Stdout != "out" || result.Stderr != "err" || result.ExitCode != 3 || result.Error != nil {
		t.Fatalf("result = %+v, want stdout/stderr captured and exit code 3 without transport error", result)
	}
	if result.Success() {
		t.Fatal("Success() = true for exit code 3")
	}
}

func TestLocalExecuteRunsFromHomeDirectory(t *testing.T) {
	client := newTestShell(t, 0)

	result, err := client.Execute("app-1", `pwd -P; printf '%s\n' "${HOME}"`)
	if err != nil || !result.Success() {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	home, _ := filepath.EvalSymlinks(os.Getenv("HOME"))
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 2 || lines[0] != home {
		t.Fatalf("working directory = %q, want home %q", result.Stdout, home)
	}
}

func TestLocalExecuteWithStdin(t *testing.T) {
	client := newTestShell(t, 0)

	result, err := client.ExecuteWithStdin("app-1", "cat", strings.NewReader("secret=value\n"))
	if err != nil || !result.Success() {
		t.Fatalf("ExecuteWithStdin = %+v, %v", result, err)
	}
	if result.Stdout != "secret=value\n" {
		t.Fatalf("stdout = %q, want stdin echoed", result.Stdout)
	}
}

func TestLocalExecuteTimesOut(t *testing.T) {
	client := newTestShell(t, 200*time.Millisecond)

	start := time.Now()
	result, err := client.Execute("app-1", "sleep 30")
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.ExitCode != -1 || result.Error == nil || !strings.Contains(result.Error.Error(), "timed out") {
		t.Fatalf("result = %+v, want a timeout transport error", result)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("timeout took %s; the process group was not signaled", elapsed)
	}
}

func TestLocalExecuteHonorsCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	client := New(Options{Context: ctx, Host: "app-1"})
	defer func() { _ = client.Close() }()

	time.AfterFunc(100*time.Millisecond, cancel)
	result, err := client.Execute("app-1", "sleep 30")
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Error == nil || !strings.Contains(result.Error.Error(), "canceled") {
		t.Fatalf("result = %+v, want a cancellation error", result)
	}
}

func TestLocalExecuteStream(t *testing.T) {
	client := newTestShell(t, 0)

	var stdout, stderr bytes.Buffer
	if err := client.ExecuteStream("app-1", "echo one; echo two >&2", &stdout, &stderr); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if stdout.String() != "one\n" || stderr.String() != "two\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if err := client.ExecuteIO("app-1", "true", nil, &stdout, &stderr, true); err == nil {
		t.Fatal("ExecuteIO with a terminal succeeded, want the local transport to refuse")
	}
}

func TestUploadResolvesRelativePathsFromHome(t *testing.T) {
	client := newTestShell(t, 0)
	home := os.Getenv("HOME")

	source := filepath.Join(t.TempDir(), "source.env")
	if err := os.WriteFile(source, []byte("A=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := client.Upload("app-1", source, "uploaded.env"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, "uploaded.env"))
	if err != nil {
		t.Fatalf("uploaded file missing from home: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("uploaded mode = %o, want 640", info.Mode().Perm())
	}
}

func TestLocalLockExcludesConcurrentHolders(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock is not installed")
	}
	client := newTestShell(t, 0)
	lockFile := "${HOME}/.local/share/azud-locks/svc.deploy.lock"

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- client.WithRemoteLock("app-1", lockFile, 5*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// A second holder must time out while the first one holds the lock.
	err := client.WithRemoteLock("app-1", lockFile, time.Second, func() error {
		t.Error("second holder acquired a held lock")
		return nil
	})
	if err == nil {
		t.Fatal("second WithRemoteLock succeeded while the lock was held")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first holder: %v", err)
	}

	ran := false
	if err := client.WithRemoteLock("app-1", lockFile, 5*time.Second, func() error {
		ran = true
		return nil
	}); err != nil || !ran {
		t.Fatalf("lock after release = %v, ran=%v", err, ran)
	}
}

func TestLocalTransportErrorsReachStderr(t *testing.T) {
	client := newTestShell(t, 200*time.Millisecond)

	silent, err := client.Execute("app-1", "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(silent.Stderr, "timed out") {
		t.Fatalf("stderr = %q; callers that report only stderr would print nothing", silent.Stderr)
	}

	pulling, err := client.Execute("app-1", "echo Trying to pull >&2; sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pulling.Stderr, "Trying to pull\n") || !strings.Contains(pulling.Stderr, "timed out") {
		t.Fatalf("stderr = %q, want the command's output and the reason", pulling.Stderr)
	}
}

func TestLocalCommandLeavingAProcessBehindKeepsItsResult(t *testing.T) {
	original := waitDelay
	waitDelay = 100 * time.Millisecond
	t.Cleanup(func() { waitDelay = original })
	client := newTestShell(t, 0)

	// The background sleep inherits the command's stdout and stderr.
	result, err := client.Execute("app-1", "sleep 5 & echo started")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success() || result.Stdout != "started\n" {
		t.Fatalf("result = %+v; the command exited 0", result)
	}

	failed, err := client.Execute("app-1", "sleep 5 & exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if failed.ExitCode != 3 || failed.Error != nil {
		t.Fatalf("result = %+v, want its exit code", failed)
	}

	var stdout, stderr bytes.Buffer
	if err := client.ExecuteStream("app-1", "sleep 5 & echo started", &stdout, &stderr); err != nil {
		t.Fatalf("ExecuteStream = %v; the command exited 0", err)
	}
}

func TestExecuteLongGetsTheLongerLimit(t *testing.T) {
	const slow = "sleep 1"
	for _, test := range []struct {
		name          string
		command, long time.Duration
		finishes      bool
	}{
		{name: "the long limit applies", command: 100 * time.Millisecond, long: time.Minute, finishes: true},
		{name: "a short long limit still times out", command: 100 * time.Millisecond, long: 200 * time.Millisecond},
		{name: "unset falls back to the command timeout", command: 100 * time.Millisecond},
		{name: "a longer command timeout is not shortened", command: time.Minute, long: 100 * time.Millisecond, finishes: true},
		{name: "no limit stays no limit", long: 100 * time.Millisecond, finishes: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := New(Options{Host: "app-1", CommandTimeout: test.command, LongCommandTimeout: test.long})
			defer func() { _ = client.Close() }()

			result, err := client.ExecuteLong("app-1", slow)
			if err != nil {
				t.Fatal(err)
			}
			if result.Success() != test.finishes {
				t.Fatalf("result = %+v, want finished: %v", result, test.finishes)
			}
			parallel := client.ExecuteLongParallel([]string{"app-1"}, slow)
			if parallel[0].Success() != test.finishes {
				t.Fatalf("parallel result = %+v, want finished: %v", parallel[0], test.finishes)
			}
		})
	}
}

func TestOnlyExecuteLongGetsTheLongerLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := New(Options{Host: "app-1", CommandTimeout: 100 * time.Millisecond, LongCommandTimeout: time.Minute})
	defer func() { _ = client.Close() }()

	result, err := client.Execute("app-1", "sleep 1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Success() {
		t.Fatal("Execute outlasted the command timeout")
	}
}
