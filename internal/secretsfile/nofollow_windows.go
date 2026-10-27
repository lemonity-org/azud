//go:build windows

package secretsfile

// Windows does not support O_NOFOLLOW; callers Lstat the path first.
const oNoFollow = 0
