//go:build !windows

package ssh

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

func newLocalTestClient(t *testing.T, commandTimeout time.Duration) *Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	client := NewClient(&Config{LocalHost: "app-1", CommandTimeout: commandTimeout})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestLocalTransportRefusesOtherHosts(t *testing.T) {
	client := newLocalTestClient(t, 0)

	if _, err := client.Execute("app-2", "true"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("Execute on another host error = %v, want refusal", err)
	}
	if err := client.Upload("app-2", "/etc/hostname", "x"); err == nil {
		t.Fatal("Upload to another host succeeded, want refusal")
	}
}

func TestLocalExecuteMatchesSSHResultSemantics(t *testing.T) {
	client := newLocalTestClient(t, 0)

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
	client := newLocalTestClient(t, 0)

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
	client := newLocalTestClient(t, 0)

	result, err := client.ExecuteWithStdin("app-1", "cat", strings.NewReader("secret=value\n"))
	if err != nil || !result.Success() {
		t.Fatalf("ExecuteWithStdin = %+v, %v", result, err)
	}
	if result.Stdout != "secret=value\n" {
		t.Fatalf("stdout = %q, want stdin echoed", result.Stdout)
	}
}

func TestLocalExecuteTimesOut(t *testing.T) {
	client := newLocalTestClient(t, 200*time.Millisecond)

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
	client := NewClient(&Config{Context: ctx, LocalHost: "app-1"})
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
	client := newLocalTestClient(t, 0)

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

func TestLocalUploadAndDownloadResolveRelativePathsFromHome(t *testing.T) {
	client := newLocalTestClient(t, 0)
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

	conn, err := client.Connect("app-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.UploadContent([]byte("B=2\n"), "content.env", 0o600); err != nil {
		t.Fatalf("UploadContent: %v", err)
	}
	info, err = os.Stat(filepath.Join(home, "content.env"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("content file = %v, %v; want mode 600", info, err)
	}

	downloaded := filepath.Join(t.TempDir(), "downloaded.env")
	if err := client.Download("app-1", "content.env", downloaded); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, _ := os.ReadFile(downloaded); string(got) != "B=2\n" {
		t.Fatalf("downloaded = %q", got)
	}
}

func TestLocalLockExcludesConcurrentHolders(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock is not installed")
	}
	client := newLocalTestClient(t, 0)
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
