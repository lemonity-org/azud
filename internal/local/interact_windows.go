package local

import "errors"

// Interact is for commands on a server, which Azud does not run on Windows.
func Interact(argv []string) error {
	return errors.New("interactive commands are not supported on Windows")
}
