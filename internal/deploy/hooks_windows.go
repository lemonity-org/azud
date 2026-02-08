//go:build windows

package deploy

// Windows does not support O_NOFOLLOW; set to 0 (no-op flag).
const oNofollow = 0

func isSymlinkError(_ error) bool {
	return false
}
