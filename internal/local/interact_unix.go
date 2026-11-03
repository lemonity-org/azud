//go:build !windows

package local

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/term"
)

// Interact replaces this process with argv, for `app exec -t` and `accessory exec
// -t`. Podman then owns the operator's terminal, raw mode, resize and signals, with
// no PTY library and no background process group. It returns only on failure.
func Interact(argv []string) error {
	if len(argv) == 0 {
		return errors.New("no command to run")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("stdin is not a terminal: connect with ssh -t, or drop -t")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		return fmt.Errorf("failed to run %s: %w", argv[0], err)
	}
	return nil
}
