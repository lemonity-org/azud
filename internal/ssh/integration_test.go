package ssh

import (
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// integrationClient connects to the real sshd of the CI integration jobs.
func integrationClient(t *testing.T, commandTimeout time.Duration) (*Client, string) {
	t.Helper()
	if os.Getenv("AZUD_INTEGRATION") == "" {
		t.Skip("set AZUD_INTEGRATION to run against a real sshd")
	}
	port, err := strconv.Atoi(os.Getenv("AZUD_INTEGRATION_PORT"))
	if err != nil {
		t.Fatalf("AZUD_INTEGRATION_PORT: %v", err)
	}
	host := os.Getenv("AZUD_INTEGRATION_HOST")
	client := NewClient(&Config{
		User:                  os.Getenv("AZUD_INTEGRATION_USER"),
		Port:                  port,
		Keys:                  []string{os.Getenv("AZUD_INTEGRATION_KEY")},
		CommandTimeout:        commandTimeout,
		InsecureIgnoreHostKey: true,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client, host
}

func TestSSHIntegrationTimeoutLeavesTheConnectionUsable(t *testing.T) {
	client, host := integrationClient(t, time.Second)

	start := time.Now()
	result, err := client.Execute(host, "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if result.Success() || !strings.Contains(result.Stderr, "timed out") {
		t.Fatalf("Stderr = %q, Error = %v; want the timeout", result.Stderr, result.Error)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the timeout took %s to return", elapsed)
	}

	// A later command on the same connection still works.
	result, err = client.Execute(host, "echo still here")
	if err != nil || !result.Success() || !strings.Contains(result.Stdout, "still here") {
		t.Fatalf("next command = %+v, %v", result, err)
	}
}

func TestSSHIntegrationOutputStopsReachingTheWriterAfterTimeout(t *testing.T) {
	client, host := integrationClient(t, time.Second)

	var stdout lateWriteDetector
	err := client.ExecuteStream(host, "yes", &stdout, io.Discard)
	stdout.returned.Store(true)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	time.Sleep(500 * time.Millisecond)
	if stdout.late.Load() {
		t.Error("the session wrote to the caller's writer after the command returned")
	}
}

func TestSSHIntegrationIdleCleanupLeavesARunningCommandAlone(t *testing.T) {
	client, host := integrationClient(t, time.Minute)
	conn, err := client.Connect(host)
	if err != nil {
		t.Fatal(err)
	}

	var finished atomic.Bool
	done := make(chan *Result, 1)
	go func() {
		result, err := conn.ExecuteLong("sleep 3; echo finished")
		if err != nil {
			t.Error(err)
		}
		finished.Store(true)
		done <- result
	}()

	// Backdate while the command runs, as a pull that outlasts the pool's limit.
	time.Sleep(time.Second)
	backdate(conn)
	client.pool.removeIdleConnections()
	if client.pool.Size() != 1 || finished.Load() {
		t.Fatalf("pool size = %d, finished = %v", client.pool.Size(), finished.Load())
	}

	result := <-done
	if result == nil || !result.Success() || !strings.Contains(result.Stdout, "finished") {
		t.Fatalf("the command did not survive cleanup: %+v", result)
	}
}
