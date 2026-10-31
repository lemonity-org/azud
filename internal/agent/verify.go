package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// maxVerifyOutputLines bounds how much verify_command output an error repeats.
const maxVerifyOutputLines = 20

// CommandVerifier runs the configured verify_command for an image. The image
// is passed in the environment, never interpolated into the command, so a
// reference cannot inject shell syntax.
type CommandVerifier struct {
	// Command runs through /bin/sh.
	Command string
	// Timeout bounds one run. Zero means no limit.
	Timeout time.Duration
	// Env adds variables to the agent's environment, such as AZUD_SERVICE.
	Env []string
}

// Verify runs the command with AZUD_AGENT_IMAGE set to image and returns an
// error carrying the command's last output lines if it fails.
func (v *CommandVerifier) Verify(ctx context.Context, image string) error {
	if v.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, v.Timeout)
		defer cancel()
	}

	digest := ""
	if at := strings.LastIndex(image, "@"); at >= 0 {
		digest = image[at+1:]
	}

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", v.Command)
	cmd.Env = append(append(os.Environ(), v.Env...),
		"AZUD_AGENT_IMAGE="+image,
		"AZUD_AGENT_DIGEST="+digest,
	)
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", v.Timeout)
	}
	if tail := lastLines(out.String(), maxVerifyOutputLines); tail != "" {
		return fmt.Errorf("%w\n%s", err, tail)
	}
	return err
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
