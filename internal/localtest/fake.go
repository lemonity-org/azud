// Package localtest provides an in-process local.Runner for tests.
package localtest

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/lemonity-org/azud/internal/local"
)

// Fake is a local.Runner that answers commands from Handle instead of running them.
type Fake struct {
	// Handle answers a command. stdin is empty unless the caller gave one. A nil
	// Handle answers every command with success and no output.
	Handle func(cmd string, stdin io.Reader) (stdout, stderr string, exitCode int)
	// Lock runs fn holding lockFile. A nil Lock runs fn straight away.
	Lock func(lockFile string, timeout time.Duration, fn func() error) error
	// UserName is what User returns.
	UserName string

	mu       sync.Mutex
	commands []string
}

var _ local.Runner = (*Fake)(nil)

// Commands returns every command run so far, in order.
func (f *Fake) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *Fake) run(host, cmd string, stdin io.Reader) *local.Result {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()

	result := &local.Result{Host: host}
	if f.Handle == nil {
		return result
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	result.Stdout, result.Stderr, result.ExitCode = f.Handle(cmd, stdin)
	return result
}

func (f *Fake) Execute(host, cmd string) (*local.Result, error) {
	return f.run(host, cmd, nil), nil
}

func (f *Fake) ExecuteLong(host, cmd string) (*local.Result, error) {
	return f.run(host, cmd, nil), nil
}

func (f *Fake) ExecuteWithStdin(host, cmd string, stdin io.Reader) (*local.Result, error) {
	return f.run(host, cmd, stdin), nil
}

func (f *Fake) ExecuteStream(host, cmd string, stdout, stderr io.Writer) error {
	return f.ExecuteIO(host, cmd, nil, stdout, stderr, false)
}

func (f *Fake) ExecuteIO(host, cmd string, stdin io.Reader, stdout, stderr io.Writer, _ bool) error {
	result := f.run(host, cmd, stdin)
	if _, err := io.WriteString(stdout, result.Stdout); err != nil {
		return err
	}
	if _, err := io.WriteString(stderr, result.Stderr); err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("exit status %d", result.ExitCode)
	}
	return nil
}

func (f *Fake) ExecuteParallel(hosts []string, cmd string) []*local.Result {
	return local.RunParallel(hosts, cmd, f.Execute)
}

func (f *Fake) ExecuteLongParallel(hosts []string, cmd string) []*local.Result {
	return local.RunParallel(hosts, cmd, f.ExecuteLong)
}

func (f *Fake) Upload(host, localPath, remotePath string) error {
	result := f.run(host, fmt.Sprintf("upload %s %s", localPath, remotePath), nil)
	if result.ExitCode != 0 {
		return fmt.Errorf("upload failed: %s", result.Stderr)
	}
	return nil
}

func (f *Fake) WithRemoteLock(_, lockFile string, timeout time.Duration, fn func() error) error {
	if f.Lock == nil {
		return fn()
	}
	return f.Lock(lockFile, timeout, fn)
}

func (f *Fake) User() string {
	return f.UserName
}

func (f *Fake) Close() error {
	return nil
}
