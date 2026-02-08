//go:build !windows

package deploy

import (
	"errors"
	"syscall"
)

const oNofollow = syscall.O_NOFOLLOW

func isSymlinkError(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
