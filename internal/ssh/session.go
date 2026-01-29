package ssh

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Connection represents an SSH connection to a single host
type Connection struct {
	host     string
	client   *ssh.Client
	lastUsed time.Time
	mu       sync.Mutex
}

// Result holds the result of a command execution
type Result struct {
	Host     string
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	Error    error
}

// Success returns true if the command executed successfully
func (r *Result) Success() bool {
	return r.ExitCode == 0 && r.Error == nil
}

// Output returns stdout if successful, stderr otherwise
func (r *Result) Output() string {
	if r.Stdout != "" {
		return r.Stdout
	}
	return r.Stderr
}

// Execute runs a command on the remote host
func (c *Connection) Execute(cmd string) (*Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastUsed = time.Now()

	session, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	start := time.Now()
	err = session.Run(cmd)
	duration := time.Since(start)

	result := &Result{
		Host:     c.host,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			result.ExitCode = exitErr.ExitStatus()
		} else {
			result.ExitCode = -1
			result.Error = err
		}
	}

	return result, nil
}

// ExecuteWithPty runs a command with a pseudo-terminal
func (c *Connection) ExecuteWithPty(cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastUsed = time.Now()

	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// Request pseudo-terminal
	modes := ssh.TerminalModes{
		ssh.ECHO:          0,     // disable echoing
		ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
		ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
	}

	if err := session.RequestPty("xterm", 80, 40, modes); err != nil {
		return fmt.Errorf("failed to request PTY: %w", err)
	}

	session.Stdin = stdin
	session.Stdout = stdout
	session.Stderr = stderr

	return session.Run(cmd)
}

// ExecuteStream runs a command and streams output to the provided writers
func (c *Connection) ExecuteStream(cmd string, stdout, stderr io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastUsed = time.Now()

	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	session.Stdout = stdout
	session.Stderr = stderr

	return session.Run(cmd)
}

// Upload copies a local file to the remote host using SCP
func (c *Connection) Upload(localPath, remotePath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastUsed = time.Now()

	// Read local file
	localFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer func() { _ = localFile.Close() }()

	stat, err := localFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat local file: %w", err)
	}

	// Create session
	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// Get remote directory and filename
	remoteDir := filepath.Dir(remotePath)
	remoteFile := filepath.Base(remotePath)

	// Start SCP in sink mode
	go func() {
		w, _ := session.StdinPipe()
		defer func() { _ = w.Close() }()

		// Send file info
		_, _ = fmt.Fprintf(w, "C%04o %d %s\n", stat.Mode().Perm(), stat.Size(), remoteFile)

		// Send file content
		_, _ = io.Copy(w, localFile)

		// Send end marker
		_, _ = fmt.Fprint(w, "\x00")
	}()

	// Run SCP command
	cmd := fmt.Sprintf("scp -t %s", remoteDir)
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("SCP failed: %w", err)
	}

	return nil
}

// UploadContent uploads content directly to a remote file
func (c *Connection) UploadContent(content []byte, remotePath string, mode os.FileMode) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastUsed = time.Now()

	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	remoteDir := filepath.Dir(remotePath)
	remoteFile := filepath.Base(remotePath)

	go func() {
		w, _ := session.StdinPipe()
		defer func() { _ = w.Close() }()

		_, _ = fmt.Fprintf(w, "C%04o %d %s\n", mode, len(content), remoteFile)
		_, _ = w.Write(content)
		_, _ = fmt.Fprint(w, "\x00")
	}()

	cmd := fmt.Sprintf("scp -t %s", remoteDir)
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("SCP failed: %w", err)
	}

	return nil
}

// Download copies a remote file to the local host
func (c *Connection) Download(remotePath, localPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastUsed = time.Now()

	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// Create local file
	localFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer func() { _ = localFile.Close() }()

	// Get remote file content via cat
	var stdout bytes.Buffer
	session.Stdout = &stdout

	cmd := fmt.Sprintf("cat %s", remotePath)
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("failed to read remote file: %w", err)
	}

	if _, err := localFile.Write(stdout.Bytes()); err != nil {
		return fmt.Errorf("failed to write local file: %w", err)
	}

	return nil
}

// Close closes the SSH connection
func (c *Connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// Host returns the host address
func (c *Connection) Host() string {
	return c.host
}

// LastUsed returns the time of last use
func (c *Connection) LastUsed() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastUsed
}

// IsAlive checks if the connection is still alive
func (c *Connection) IsAlive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client == nil {
		return false
	}

	// Send a keepalive request
	_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}
