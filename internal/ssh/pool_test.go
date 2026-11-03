package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// floodCommand makes the test server write output until the client goes away.
const floodCommand = "flood"

// startCommandServer starts an SSH server whose commands run until release is
// closed or the client goes away, and returns a connection to it. started gets
// a value when a command begins.
func startCommandServer(t *testing.T, commandTimeout time.Duration) (conn *Connection, started <-chan struct{}, release chan struct{}) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{NoClientAuth: true}
	serverConfig.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	begun := make(chan struct{}, 1)
	release = make(chan struct{})
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			go serveCommands(raw, serverConfig, begun, release)
		}
	}()

	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Connection{
		host:           "test",
		client:         client,
		commandTimeout: commandTimeout,
		lastUsed:       time.Now(),
		sessions:       make(chan struct{}, 8),
	}, begun, release
}

func serveCommands(raw net.Conn, config *ssh.ServerConfig, begun chan<- struct{}, release <-chan struct{}) {
	server, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = channel.Close() }()
			for request := range channelRequests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				_ = request.Reply(true, nil)
				begun <- struct{}{}
				go ssh.DiscardRequests(channelRequests)
				var exec struct{ Command string }
				if err := ssh.Unmarshal(request.Payload, &exec); err == nil && exec.Command == floodCommand {
					for {
						if _, err := channel.Write(make([]byte, 1024)); err != nil {
							return
						}
					}
				}
				<-release
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		}()
	}
}

// backdate makes the connection look unused for far longer than the pool allows.
func backdate(conn *Connection) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.lastUsed = time.Now().Add(-time.Hour)
}

func TestIdleCleanupLeavesARunningCommandAlone(t *testing.T) {
	conn, started, release := startCommandServer(t, time.Minute)
	pool := NewPool()
	t.Cleanup(func() { _ = pool.CloseAll() })
	pool.Put("test", conn)

	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := conn.ExecuteLong("podman pull slow")
		done <- outcome{result, err}
	}()
	<-started

	// The command has outlasted the pool's limit, as a slow pull does.
	backdate(conn)
	pool.removeIdleConnections()
	if pool.Size() != 1 {
		t.Error("cleanup removed a connection with a command running")
	}
	close(release)

	got := <-done
	if got.err != nil || !got.result.Success() {
		t.Fatalf("the command did not survive cleanup: %v, %+v", got.err, got.result)
	}

	// Finishing counts as use: the next command must not lose the connection.
	pool.removeIdleConnections()
	if pool.Size() != 1 {
		t.Error("cleanup removed a connection whose command had just finished")
	}

	backdate(conn)
	pool.removeIdleConnections()
	if pool.Size() != 0 {
		t.Error("cleanup kept a connection that was idle")
	}
}

func TestSSHCommandThatTimesOutSaysSo(t *testing.T) {
	conn, _, release := startCommandServer(t, 100*time.Millisecond)
	t.Cleanup(func() {
		close(release)
		_ = conn.Close()
	})

	result, err := conn.Execute("sleep 100")
	if err != nil {
		t.Fatal(err)
	}
	if result.Success() || !strings.Contains(result.Stderr, "timed out") {
		t.Fatalf("Stderr = %q, Error = %v; want the timeout in Stderr", result.Stderr, result.Error)
	}
}

// lateWriteDetector is a slow writer that notes any write after the caller got
// it back. Being slow leaves a backlog of output for the session to deliver.
type lateWriteDetector struct {
	returned atomic.Bool
	late     atomic.Bool
}

func (w *lateWriteDetector) Write(p []byte) (int, error) {
	if w.returned.Load() {
		w.late.Store(true)
	}
	time.Sleep(time.Millisecond)
	return len(p), nil
}

func TestSSHOutputStopsReachingTheWriterAfterTimeout(t *testing.T) {
	conn, _, release := startCommandServer(t, 100*time.Millisecond)
	t.Cleanup(func() {
		close(release)
		_ = conn.Close()
	})

	var stdout lateWriteDetector
	err := conn.ExecuteStream(floodCommand, &stdout, io.Discard)
	stdout.returned.Store(true)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}

	time.Sleep(100 * time.Millisecond)
	if stdout.late.Load() {
		t.Error("the session wrote to the caller's writer after the command returned")
	}
}
