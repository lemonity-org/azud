//go:build !windows

package ssh

import (
	"io"
	"os"
	"os/signal"
	"syscall"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func preparePTY(stdin io.Reader, session *gossh.Session) (width, height int, startResize func() func(), cleanup func()) {
	width, height = 80, 40
	cleanup = func() {}
	fdProvider, ok := stdin.(interface{ Fd() uintptr })
	if !ok {
		return width, height, func() func() { return func() {} }, cleanup
	}
	fd := int(fdProvider.Fd())
	if !term.IsTerminal(fd) {
		return width, height, func() func() { return func() {} }, cleanup
	}
	if currentWidth, currentHeight, err := term.GetSize(fd); err == nil {
		width, height = currentWidth, currentHeight
	}
	if oldState, err := term.MakeRaw(fd); err == nil {
		cleanup = func() { _ = term.Restore(fd, oldState) }
	}

	startResize = func() func() {
		signals := make(chan os.Signal, 1)
		done := make(chan struct{})
		signal.Notify(signals, syscall.SIGWINCH)
		resize := func() {
			newWidth, newHeight, err := term.GetSize(fd)
			if err == nil {
				_ = session.WindowChange(newHeight, newWidth)
			}
		}
		resize()
		go func() {
			for {
				select {
				case <-signals:
					resize()
				case <-done:
					return
				}
			}
		}()
		return func() {
			signal.Stop(signals)
			close(done)
		}
	}
	return width, height, startResize, cleanup
}
