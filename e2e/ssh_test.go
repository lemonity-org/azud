//go:build e2e

package e2e

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
)

// startSSH runs a loopback SSH server inside the test process, which executes each
// request through /bin/sh as the test's user. Commands other than the agent still
// reach a server through the SSH client, so `azud cron boot` runs here against real
// Podman with no sshd to install. It goes when those commands run locally.
func (h *harness) startSSH() error {
	clientPublic, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	authorized, err := ssh.NewPublicKey(clientPublic)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(clientKey, "azud-e2e")
	if err != nil {
		return err
	}
	h.sshKey = filepath.Join(h.workDir, "ssh-key")
	if err := os.WriteFile(h.sshKey, pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}

	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		return err
	}
	config := &ssh.ServerConfig{
		// Anyone who gets in runs commands as this process, which may be root, and
		// the listener is open to every local user.
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, errors.New("unknown key")
			}
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	h.sshPort = listener.Addr().(*net.TCPAddr).Port
	h.stopSSH = func() { _ = listener.Close() }
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSSH(conn, config)
		}
	}()
	return nil
}

func serveSSH(conn net.Conn, config *ssh.ServerConfig) {
	defer func() { _ = conn.Close() }()
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		if channel.ChannelType() != "session" {
			_ = channel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		go serveSSHSession(channel)
	}
}

func serveSSHSession(channel ssh.NewChannel) {
	session, requests, err := channel.Accept()
	if err != nil {
		return
	}
	defer func() { _ = session.Close() }()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		status := runSSHCommand(payload.Command, session)
		_, _ = session.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

// runSSHCommand runs command with the session as its streams. Stdin goes through a
// pipe so a client that never closes it cannot hold a finished command open.
func runSSHCommand(command string, session ssh.Channel) uint32 {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = os.Environ()
	cmd.Stdout = session
	cmd.Stderr = session.Stderr()
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(session.Stderr(), "stdin: %v\n", err)
		return 255
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(session.Stderr(), "start: %v\n", err)
		return 255
	}
	go func() {
		_, _ = io.Copy(stdin, session)
		_ = stdin.Close()
	}()
	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return uint32(exit.ExitCode())
	}
	return 255
}
