package local

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
	"strings"
	"time"

	shellquote "github.com/lemonity-org/azud/internal/shell"
)

// Shell runs the commands Azud would send over SSH on this machine instead, so
// `azud agent` deploys the server it runs on with the same deployer, Podman, and
// proxy code as a pushed deploy.
//
// Commands run through /bin/sh from the user's home directory, matching what sshd
// does for a non-interactive session, so relative paths and ${HOME} expansions
// resolve the same way in both modes.
type Shell struct {
	opts Options
}

// Options configures a Shell.
type Options struct {
	// Host is the one host this Shell serves. Any other is refused, so a local run
	// can never execute commands meant for a different server.
	Host string
	// Context cancels active commands. Defaults to context.Background when unset.
	Context context.Context
	// User is the user commands run as, which is the user running this process.
	User string
	// CommandTimeout limits a command. Zero means no limit.
	CommandTimeout time.Duration
	// LongCommandTimeout limits a command run with ExecuteLong, when it is longer
	// than CommandTimeout.
	LongCommandTimeout time.Duration
}

// New returns a Shell.
func New(opts Options) *Shell {
	return &Shell{opts: opts}
}

// shellPath is the POSIX shell that runs commands.
const shellPath = "/bin/sh"

// waitDelay bounds how long a command's output pipes may stay open after it exits
// or is signaled. A background process it left behind can hold them for as long as
// it runs.
var waitDelay = 10 * time.Second

var errNoTerminal = errors.New("the local transport does not allocate terminals")

// check refuses a host this Shell does not serve, and a system without a POSIX shell.
func (c *Shell) check(host string) error {
	if host != c.opts.Host {
		return fmt.Errorf("local transport serves %s only; refusing to run commands for %s", c.opts.Host, host)
	}
	if runtime.GOOS == "windows" {
		return fmt.Errorf("the local transport needs a POSIX shell and is not supported on Windows")
	}
	return nil
}

// Execute runs a command and returns its result.
func (c *Shell) Execute(host, cmd string) (*Result, error) {
	if err := c.check(host); err != nil {
		return nil, err
	}
	return c.run(cmd, nil, c.opts.CommandTimeout)
}

// ExecuteLong runs a command that can outlast the command timeout, such as an
// image pull.
func (c *Shell) ExecuteLong(host, cmd string) (*Result, error) {
	if err := c.check(host); err != nil {
		return nil, err
	}
	return c.run(cmd, nil, LongLimit(c.opts.CommandTimeout, c.opts.LongCommandTimeout))
}

// ExecuteWithStdin runs a command with the given stdin.
func (c *Shell) ExecuteWithStdin(host, cmd string, stdin io.Reader) (*Result, error) {
	if err := c.check(host); err != nil {
		return nil, err
	}
	return c.run(cmd, stdin, c.opts.CommandTimeout)
}

// ExecuteStream runs a command and streams its output to the given writers.
func (c *Shell) ExecuteStream(host, cmd string, stdout, stderr io.Writer) error {
	if err := c.check(host); err != nil {
		return err
	}
	return c.runCommand(cmd, nil, stdout, stderr, c.opts.CommandTimeout)
}

// ExecuteIO runs a command with live stdin, stdout and stderr. A command that wants
// a terminal goes through Interact instead.
func (c *Shell) ExecuteIO(host, cmd string, stdin io.Reader, stdout, stderr io.Writer, tty bool) error {
	if err := c.check(host); err != nil {
		return err
	}
	if tty {
		return errNoTerminal
	}
	return c.runCommand(cmd, stdin, stdout, stderr, c.opts.CommandTimeout)
}

// ExecuteParallel runs a command for each host.
func (c *Shell) ExecuteParallel(hosts []string, cmd string) []*Result {
	return RunParallel(hosts, cmd, c.Execute)
}

// ExecuteLongParallel is ExecuteParallel for commands that can outlast the command
// timeout.
func (c *Shell) ExecuteLongParallel(hosts []string, cmd string) []*Result {
	return RunParallel(hosts, cmd, c.ExecuteLong)
}

// Upload copies a file to remotePath on this machine.
func (c *Shell) Upload(host, localFilePath, remotePath string) error {
	if err := c.check(host); err != nil {
		return err
	}
	return c.upload(localFilePath, remotePath)
}

// WithRemoteLock holds an exclusive flock on lockFile while fn runs.
func (c *Shell) WithRemoteLock(host, lockFile string, timeout time.Duration, fn func() error) error {
	if err := c.check(host); err != nil {
		return err
	}
	return c.lock(lockFile, timeout, fn)
}

// User returns the user commands run as.
func (c *Shell) User() string {
	return c.opts.User
}

// Close does nothing: a Shell holds no connection.
func (c *Shell) Close() error {
	return nil
}

func (c *Shell) run(cmd string, stdin io.Reader, timeout time.Duration) (*Result, error) {
	var stdout, stderr bytes.Buffer
	start := time.Now()
	err := c.runCommand(cmd, stdin, &stdout, &stderr, timeout)
	result := &Result{
		Host:     c.opts.Host,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.SetError(err)
		}
	}

	return result, nil
}

// runCommand runs cmd to completion, honoring the connection context and timeout
// the same way an SSH session does. A zero timeout means no limit.
func (c *Shell) runCommand(cmd string, stdin io.Reader, stdout, stderr io.Writer, timeout time.Duration) error {
	ctx := c.opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	command, err := shellCommand(ctx, cmd)
	if err != nil {
		return err
	}
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr

	if err := command.Run(); err != nil {
		// The command exited 0 and left a process holding its output pipes.
		// That is its success, not a failure to run it.
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) && timeout > 0 {
				return fmt.Errorf("command timed out after %s: %w", timeout, ctxErr)
			}
			return fmt.Errorf("command canceled: %w", ctxErr)
		}
		return err
	}
	return nil
}

// shellCommand prepares cmd to run through the local shell from the user's
// home directory. Canceling ctx signals the command's whole process group.
func shellCommand(ctx context.Context, cmd string) (*exec.Cmd, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve home directory: %w", err)
	}
	command := exec.CommandContext(ctx, shellPath, "-c", cmd)
	command.Dir = home
	command.WaitDelay = waitDelay
	configureProcess(command)
	return command, nil
}

// resolvePath resolves a remote path the way scp and cat do over SSH: relative
// paths are relative to the user's home directory.
func resolvePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve home directory: %w", err)
	}
	return filepath.Join(home, path), nil
}

func (c *Shell) upload(localFilePath, remotePath string) error {
	source, err := os.Open(localFilePath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer func() { _ = source.Close() }()

	stat, err := source.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat local file: %w", err)
	}
	return c.writeFile(remotePath, source, stat.Mode().Perm())
}

// writeFile writes content to remotePath with mode, like the scp sink
// Upload and UploadContent use: the parent directory must already exist.
func (c *Shell) writeFile(remotePath string, content io.Reader, mode os.FileMode) error {
	path, err := resolvePath(remotePath)
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

// lock runs flock(1) as a local process, so a local deploy and a pushed deploy to
// the same host exclude each other.
func (c *Shell) lock(lockFile string, timeout time.Duration, fn func() error) error {
	base := c.opts.Context
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	command, err := shellCommand(ctx, LockCommand(lockFile, timeout))
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

// LockCommand builds the shell command WithRemoteLock runs: it prints LOCKED
// once flock holds lockFile and keeps holding it until stdin closes.
func LockCommand(lockFile string, timeout time.Duration) string {
	dir := filepath.Dir(lockFile)
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	// Quote paths safely. A leading ${HOME}/ (non-root users) is preserved
	// unquoted so the shell expands it, while the remainder is single-quoted
	// and thus immune to injection. All other paths are fully single-quoted.
	quotedDir := quoteRemotePath(dir)
	quotedLockFile := quoteRemotePath(lockFile)
	return fmt.Sprintf("mkdir -p %s && flock -x -w %d %s sh -c 'echo LOCKED; cat'", // safe: paths are quoteRemotePath output and timeout is numeric
		quotedDir, secs, quotedLockFile)
}

// quoteRemotePath quotes a remote path for safe use in a shell command. A
// leading ${HOME}/ is preserved unquoted so the shell expands it (non-root
// users), while the rest of the path is single-quoted and therefore immune to
// injection. All other paths are fully single-quoted.
func quoteRemotePath(path string) string {
	const homePrefix = "${HOME}/"
	if strings.HasPrefix(path, homePrefix) {
		return `"${HOME}/"` + shellquote.Quote(strings.TrimPrefix(path, homePrefix))
	}
	return shellquote.Quote(path)
}
