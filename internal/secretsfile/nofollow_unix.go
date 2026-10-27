//go:build !windows

package secretsfile

import "syscall"

const oNoFollow = syscall.O_NOFOLLOW
