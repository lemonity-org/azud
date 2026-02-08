package ssh

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lemonity-org/azud/internal/shell"
	"golang.org/x/crypto/ssh"
)

// Connection represents an SSH connection to a single host
type Connection struct {
	host           string
	client         *ssh.Client
	proxyClient    *ssh.Client // bastion/proxy connection, closed with client
	lastUsed       time.Time
	commandTimeout time.Duration
	mu             sync.Mutex
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
	err = c.runWithTimeout(session, cmd)
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

// ExecuteWithStdin runs a command on the remote host with provided stdin.
func (c *Connection) ExecuteWithStdin(cmd string, stdin io.Reader) (*Result, error) {
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
	session.Stdin = stdin

	start := time.Now()
	err = c.runWithTimeout(session, cmd)
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

	return c.runWithTimeout(session, cmd)
}

// runWithTimeout executes a command on the session with an optional timeout.
// If commandTimeout is zero, it falls back to session.Run (no timeout).
func (c *Connection) runWithTimeout(session *ssh.Session, cmd string) error {
	if c.commandTimeout <= 0 {
		return session.Run(cmd)
	}

	if err := session.Start(cmd); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(c.commandTimeout):
		_ = session.Close()
		return fmt.Errorf("command timed out after %s", c.commandTimeout)
	}
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

		// Send file info - remoteFile is sanitized to prevent protocol injection
		safeRemoteFile := sanitizeSCPFilename(remoteFile)
		_, _ = fmt.Fprintf(w, "C%04o %d %s\n", stat.Mode().Perm(), stat.Size(), safeRemoteFile)

		// Send file content
		_, _ = io.Copy(w, localFile)

		// Send end marker
		_, _ = fmt.Fprint(w, "\x00")
	}()

	// Run SCP command
	cmd := fmt.Sprintf("scp -t %s", shell.Quote(remoteDir))
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

		// Sanitize filename to prevent SCP protocol injection
		safeRemoteFile := sanitizeSCPFilename(remoteFile)
		_, _ = fmt.Fprintf(w, "C%04o %d %s\n", mode, len(content), safeRemoteFile)
		_, _ = w.Write(content)
		_, _ = fmt.Fprint(w, "\x00")
	}()

	cmd := fmt.Sprintf("scp -t %s", shell.Quote(remoteDir))
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

	cmd := fmt.Sprintf("cat %s", shell.Quote(remotePath))
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("failed to read remote file: %w", err)
	}

	if _, err := localFile.Write(stdout.Bytes()); err != nil {
		return fmt.Errorf("failed to write local file: %w", err)
	}

	return nil
}

// WithRemoteLock acquires an exclusive flock on the remote host for the
// duration of fn. It opens a dedicated SSH session that runs:
//
//	mkdir -p <dir> && flock -x -w <secs> <lockFile> sh -c 'echo LOCKED; cat'
//
// The session waits for "LOCKED\n" on stdout to confirm acquisition, then
// calls fn (which may use c.Execute() etc. through separate sessions). When
// fn returns, closing stdin causes cat to exit, which releases the flock.
func (c *Connection) WithRemoteLock(lockFile string, timeout time.Duration, fn func() error) error {
	// Create a dedicated session — bypass c.mu so the lock session can
	// coexist with command sessions opened by fn().
	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create lock session: %w", err)
	}
	defer func() { _ = session.Close() }()

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get lock session stdin: %w", err)
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get lock session stdout: %w", err)
	}

	dir := filepath.Dir(lockFile)
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	// Quote paths safely. Paths containing ${HOME} (non-root users) must use
	// double quotes to allow variable expansion. All other paths use
	// shell.Quote() (single-quote based) which is immune to injection.
	quotedDir := shell.Quote(dir)
	quotedLockFile := shell.Quote(lockFile)
	if strings.Contains(lockFile, "${") {
		quotedDir = fmt.Sprintf(`"%s"`, dir)
		quotedLockFile = fmt.Sprintf(`"%s"`, lockFile)
	}
	cmd := fmt.Sprintf("mkdir -p %s && flock -x -w %d %s sh -c 'echo LOCKED; cat'",
		quotedDir, secs, quotedLockFile)

	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("failed to start lock command: %w", err)
	}

	// Wait for "LOCKED" confirmation with a timeout.
	locked := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		if scanner.Scan() && scanner.Text() == "LOCKED" {
			locked <- nil
		} else if err := scanner.Err(); err != nil {
			locked <- fmt.Errorf("lock stdout error: %w", err)
		} else {
			locked <- fmt.Errorf("lock command exited without confirming acquisition")
		}
	}()

	select {
	case err := <-locked:
		if err != nil {
			_ = stdinPipe.Close()
			_ = session.Wait()
			return fmt.Errorf("failed to acquire remote lock %s: %w", lockFile, err)
		}
	case <-time.After(timeout + 5*time.Second):
		_ = stdinPipe.Close()
		_ = session.Wait()
		return fmt.Errorf("timed out acquiring remote lock %s", lockFile)
	}

	// Lock acquired — run the callback.
	fnErr := fn()

	// Release the lock by closing stdin (cat exits → flock releases).
	_ = stdinPipe.Close()
	_ = session.Wait()

	return fnErr
}

// Close closes the SSH connection and any underlying proxy connection.
func (c *Connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var err error
	if c.client != nil {
		err = c.client.Close()
	}
	if c.proxyClient != nil {
		_ = c.proxyClient.Close()
		c.proxyClient = nil
	}
	return err
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

// sanitizeSCPFilename removes or replaces characters that could be used for
// SCP protocol injection (newlines, control characters). The SCP protocol
// uses newline-delimited commands, so a filename containing \n could inject
// additional protocol commands.
func sanitizeSCPFilename(name string) string {
	var result []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		// Replace control characters (0x00-0x1F and 0x7F) with underscore
		if c < 0x20 || c == 0x7F {
			result = append(result, '_')
		} else {
			result = append(result, c)
		}
	}
	return string(result)
}
