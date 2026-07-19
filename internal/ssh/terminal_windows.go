//go:build windows

package ssh

import (
	"io"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func preparePTY(stdin io.Reader, _ *gossh.Session) (width, height int, startResize func() func(), cleanup func()) {
	width, height = 80, 40
	startResize = func() func() { return func() {} }
	cleanup = func() {}
	fdProvider, ok := stdin.(interface{ Fd() uintptr })
	if !ok {
		return
	}
	fd := int(fdProvider.Fd())
	if !term.IsTerminal(fd) {
		return
	}
	if currentWidth, currentHeight, err := term.GetSize(fd); err == nil {
		width, height = currentWidth, currentHeight
	}
	if oldState, err := term.MakeRaw(fd); err == nil {
		cleanup = func() { _ = term.Restore(fd, oldState) }
	}
	return
}
