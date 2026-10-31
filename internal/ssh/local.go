package ssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// The local transport runs the commands Azud would send over SSH on this
// machine instead. `azud agent` uses it to deploy the server it runs on with
// the same deployer, Podman, and proxy code as a pushed deploy.
//
// Commands run through /bin/sh from the user's home directory, matching what
// sshd does for a non-interactive session, so relative paths and ${HOME}
// expansions resolve the same way in both modes.

// localShell is the POSIX shell that runs local commands.
const localShell = "/bin/sh"

// localWaitDelay bounds how long a canceled local command may keep its output
// pipes open after its process group has been signaled.
const localWaitDelay = 10 * time.Second

var errLocalPTY = errors.New("the local transport does not allocate terminals")

// newLocalConnection returns a connection that runs commands on this machine.
// It serves only localHost: any other host is refused, so a local run can
// never execute commands meant for a different server.
func newLocalConnection(localHost, host string, ctx context.Context, commandTimeout time.Duration) (*Connection, error) {
	if host != localHost {
		return nil, fmt.Errorf("local transport serves %s only; refusing to run commands for %s", localHost, host)
	}
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("the local transport needs a POSIX shell and is not supported on Windows")
	}
	return &Connection{
		host:           host,
		lastUsed:       time.Now(),
		commandTimeout: commandTimeout,
		context:        ctx,
		local:          true,
	}, nil
}

func (c *Connection) localExecute(cmd string, stdin io.Reader) (*Result, error) {
	var stdout, stderr bytes.Buffer
	start := time.Now()
	err := c.runLocal(cmd, stdin, &stdout, &stderr)
	result := &Result{
		Host:     c.host,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			result.Error = err
		}
	}

	return result, nil
}

// runLocal runs cmd to completion, honoring the connection context and the
// command timeout the same way runWithTimeout does for SSH sessions.
func (c *Connection) runLocal(cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	ctx := c.context
	if ctx == nil {
		ctx = context.Background()
	}
	var cancel context.CancelFunc
	if c.commandTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.commandTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	command, err := localCommand(ctx, cmd)
	if err != nil {
		return err
	}
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr

	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) && c.commandTimeout > 0 {
				return fmt.Errorf("command timed out after %s: %w", c.commandTimeout, ctxErr)
			}
			return fmt.Errorf("command canceled: %w", ctxErr)
		}
		return err
	}
	return nil
}

// localCommand prepares cmd to run through the local shell from the user's
// home directory. Canceling ctx signals the command's whole process group.
func localCommand(ctx context.Context, cmd string) (*exec.Cmd, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve home directory: %w", err)
	}
	command := exec.CommandContext(ctx, localShell, "-c", cmd)
	command.Dir = home
	command.WaitDelay = localWaitDelay
	configureLocalProcess(command)
	return command, nil
}

// localPath resolves a remote path the way scp and cat do over SSH: relative
// paths are relative to the user's home directory.
func localPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve home directory: %w", err)
	}
	return filepath.Join(home, path), nil
}

func (c *Connection) localUpload(localFilePath, remotePath string) error {
	source, err := os.Open(localFilePath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer func() { _ = source.Close() }()

	stat, err := source.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat local file: %w", err)
	}
	return c.localWriteFile(remotePath, source, stat.Mode().Perm())
}

// localWriteFile writes content to remotePath with mode, like the scp sink
// Upload and UploadContent use: the parent directory must already exist.
func (c *Connection) localWriteFile(remotePath string, content io.Reader, mode os.FileMode) error {
	path, err := localPath(remotePath)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", remotePath, err)
	}
	if _, err := io.Copy(file, content); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to write %s: %w", remotePath, err)
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to set mode on %s: %w", remotePath, err)
	}
	return file.Close()
}

func (c *Connection) localDownload(remotePath, localFilePath string) error {
	path, err := localPath(remotePath)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read remote file: %w", err)
	}
	if err := os.WriteFile(localFilePath, content, 0o666); err != nil {
		return fmt.Errorf("failed to write local file: %w", err)
	}
	return nil
}

// localLock runs the same flock command as WithRemoteLock as a local process,
// so a local deploy and a pushed deploy to the same host exclude each other.
func (c *Connection) localLock(lockFile string, timeout time.Duration, fn func() error) error {
	base := c.context
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	command, err := localCommand(ctx, lockCommand(lockFile, timeout))
	if err != nil {
		return err
	}
	stdinPipe, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get lock stdin: %w", err)
	}
	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get lock stdout: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr

	if err := command.Start(); err != nil {
		return fmt.Errorf("failed to start lock command: %w", err)
	}

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
			_ = command.Wait()
			if detail := bytes.TrimSpace(stderr.Bytes()); len(detail) > 0 {
				err = fmt.Errorf("%w: %s", err, detail)
			}
			return fmt.Errorf("failed to acquire local lock %s: %w", lockFile, err)
		}
	case <-time.After(timeout + 5*time.Second):
		_ = stdinPipe.Close()
		cancel()
		_ = command.Wait()
		return fmt.Errorf("timed out acquiring local lock %s", lockFile)
	case <-base.Done():
		_ = stdinPipe.Close()
		cancel()
		_ = command.Wait()
		return fmt.Errorf("local lock canceled: %w", base.Err())
	}

	fnErr := fn()

	// Closing stdin ends cat, which releases the flock.
	_ = stdinPipe.Close()
	_ = command.Wait()

	return fnErr
}
