// Package local runs commands on this machine. It replaces internal/ssh as the
// transport below the CLI: Azud 2 never opens a connection to a server.
package local

import (
	"io"
	"strings"
	"sync"
	"time"
)

// Runner is what podman, proxy, deploy, quadlet and server depend on. Commands are
// shell strings. Shell runs them here, and ssh.Client runs them on another host
// until push mode goes.
type Runner interface {
	// Execute runs a command and returns its result. A command that exits nonzero
	// is a result, not an error.
	Execute(host, cmd string) (*Result, error)
	// ExecuteLong is Execute for a command that can outlast the command timeout,
	// such as an image pull.
	ExecuteLong(host, cmd string) (*Result, error)
	ExecuteWithStdin(host, cmd string, stdin io.Reader) (*Result, error)
	ExecuteStream(host, cmd string, stdout, stderr io.Writer) error
	ExecuteIO(host, cmd string, stdin io.Reader, stdout, stderr io.Writer, tty bool) error
	ExecuteParallel(hosts []string, cmd string) []*Result
	ExecuteLongParallel(hosts []string, cmd string) []*Result
	Upload(host, localPath, remotePath string) error
	// WithRemoteLock holds an exclusive flock through flock(1) while fn runs, so a
	// deploy_command script and a v1 binary exclude it on the same inode.
	WithRemoteLock(host, lockFile string, timeout time.Duration, fn func() error) error
	// User returns the user commands run as.
	User() string
	Close() error
}

// Result holds the result of a command.
type Result struct {
	Host     string
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	Error    error
}

// Success returns true if the command executed successfully.
func (r *Result) Success() bool {
	return r.ExitCode == 0 && r.Error == nil
}

// SetError records that the command could not run or finish. Most callers report
// only Stderr, so it carries the reason too, after any output the command wrote:
// the last lines of an interrupted pull do not say why it stopped.
func (r *Result) SetError(err error) {
	r.ExitCode = -1
	r.Error = err
	r.Stderr = strings.TrimLeft(strings.TrimRight(r.Stderr, "\n")+"\n"+err.Error(), "\n")
}

// Output returns stdout if successful, stderr otherwise.
func (r *Result) Output() string {
	if r.Stdout != "" {
		return r.Stdout
	}
	return r.Stderr
}

// LongLimit is the limit for ExecuteLong. It never shortens the command timeout,
// and no limit stays no limit.
func LongLimit(commandTimeout, longCommandTimeout time.Duration) time.Duration {
	if commandTimeout <= 0 {
		return commandTimeout
	}
	return max(commandTimeout, longCommandTimeout)
}

// RunParallel runs execute for each host at once and returns the results in host
// order. A failure to run the command becomes a result, so one host cannot hide
// the others.
func RunParallel(hosts []string, cmd string, execute func(host, cmd string) (*Result, error)) []*Result {
	results := make([]*Result, len(hosts))
	var wg sync.WaitGroup

	for i, host := range hosts {
		wg.Add(1)
		go func(idx int, h string) {
			defer wg.Done()
			result, err := execute(h, cmd)
			if err != nil {
				results[idx] = &Result{
					Host:     h,
					ExitCode: -1,
					Stderr:   err.Error(),
					Error:    err,
				}
				return
			}
			result.Host = h
			results[idx] = result
		}(i, host)
	}

	wg.Wait()
	return results
}
