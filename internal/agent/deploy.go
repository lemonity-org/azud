package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// deployStopGrace is how long a deploy_command gets to exit after SIGTERM
// before it is killed, so its own cleanup and rollback can run.
const deployStopGrace = time.Minute

// maxDeployOutputLines bounds how much deploy_command output an error repeats.
const maxDeployOutputLines = 40

// CommandDeployer runs the configured deploy_command for a verified image,
// in place of Azud's own deploy. As with verify_command, the image reaches
// the command only through the environment.
type CommandDeployer struct {
	// Command runs through /bin/sh.
	Command string
	// Timeout bounds one run. Zero means no limit.
	Timeout time.Duration
	// Env adds variables to the agent's environment, such as AZUD_SERVICE.
	Env []string
	// Output receives the command's stdout and stderr as they are written,
	// so a long deploy is visible in the agent's log. Nil discards it.
	Output io.Writer
}

// Deploy runs the command with AZUD_AGENT_IMAGE set to image. It returns an
// error carrying the command's last output lines if the command fails or
// runs out of time.
func (d *CommandDeployer) Deploy(ctx context.Context, image string) error {
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}

	digest := ""
	if at := strings.LastIndex(image, "@"); at >= 0 {
		digest = image[at+1:]
	}

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", d.Command)
	cmd.Env = append(append(os.Environ(), d.Env...),
		"AZUD_AGENT_IMAGE="+image,
		"AZUD_AGENT_DIGEST="+digest,
	)
	// A deploy that runs out of time is asked to stop first: killing it
	// outright would skip the rollback a deploy script usually traps.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = deployStopGrace

	tail := &tailBuffer{max: 64 << 10}
	var out io.Writer = tail
	if d.Output != nil {
		out = io.MultiWriter(d.Output, tail)
	}
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", d.Timeout)
	}
	if lines := lastLines(tail.String(), maxDeployOutputLines); lines != "" {
		return fmt.Errorf("%w\n%s", err, lines)
	}
	return err
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
