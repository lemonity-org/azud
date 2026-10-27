// Package secretsfile writes local Azud secrets files.
//
// Every local write of a secrets file goes through WriteFile: the content is
// written to a private temporary file in the same directory and renamed over
// the target. The final path is never opened for writing, so a symbolic link
// or hard link planted at that path cannot redirect the write, and the result
// is always mode 0600 even when the previous file was readable by others.
package secretsfile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const (
	// FileMode is the mode of every secrets file Azud writes.
	FileMode os.FileMode = 0o600
	// DirMode is the mode of directories Azud creates for secrets.
	DirMode os.FileMode = 0o700
	// MaxValueBytes bounds a secret value so that each KEY=VALUE line stays
	// below the 64 KiB line limit of Azud's secrets file readers and of
	// podman run --env-file, which reads the remote copy.
	MaxValueBytes = 60 * 1024
	// TempPattern names the temporary file WriteFile creates next to the
	// secrets file. An interrupted write can leave one behind.
	TempPattern = ".secrets-*.tmp"

	azudDirName = ".azud"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Result describes the file that WriteFile replaced.
type Result struct {
	// Created is true when no file existed at the path before the write.
	Created bool
	// PreviousMode holds the permission bits of the replaced file.
	PreviousMode os.FileMode
}

// Tightened reports whether the write replaced a file that group or other
// users could access.
func (r Result) Tightened() bool {
	return !r.Created && insecurePerm(r.PreviousMode)
}

// ValidateKey reports whether key is a portable environment variable name.
func ValidateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("invalid secret name %q: must match [A-Za-z_][A-Za-z0-9_]*", key)
	}
	return nil
}

// ValidateValue rejects values that would break the one-entry-per-line file
// format and create or hide other entries, or that are too long for a line to
// be read back.
func ValidateValue(key, value string) error {
	if strings.ContainsAny(value, "\n\r\x00") {
		return fmt.Errorf("secret value for %s must not contain newlines, carriage returns or NUL bytes; encode multi-line values, for example as base64", key)
	}
	if len(value) > MaxValueBytes {
		return fmt.Errorf("secret value for %s exceeds %d KiB, the longest value a secrets file line can hold", key, MaxValueBytes/1024)
	}
	return nil
}

// Format renders secrets as KEY=VALUE lines, sorted by key, after header.
//
// Only structural problems are rejected, so existing hand-written keys that
// are not strict identifiers can still be rewritten or deleted.
func Format(header string, secrets map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(secrets))
	for key := range secrets {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var content strings.Builder
	content.WriteString(header)
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		switch {
		case trimmed == "":
			return nil, fmt.Errorf("invalid secret name %q: must not be empty", key)
		case strings.HasPrefix(trimmed, "#"):
			return nil, fmt.Errorf("invalid secret name %q: must not start with #", key)
		case strings.Contains(key, "="):
			return nil, fmt.Errorf("invalid secret name %q: must not contain =", key)
		case strings.ContainsAny(key, "\n\r\x00"):
			return nil, fmt.Errorf("invalid secret name %q: must not contain newlines, carriage returns or NUL bytes", key)
		}
		if err := ValidateValue(key, secrets[key]); err != nil {
			return nil, err
		}
		content.WriteString(key)
		content.WriteByte('=')
		content.WriteString(secrets[key])
		content.WriteByte('\n')
	}
	return []byte(content.String()), nil
}

// InsecureMode reports whether group or other users can access the file.
// Windows does not expose POSIX permission bits, so it always reports false.
func InsecureMode(info os.FileInfo) bool {
	return insecurePerm(info.Mode().Perm())
}

func insecurePerm(perm os.FileMode) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	return perm&0o077 != 0
}

// WriteFile atomically replaces path with data using mode 0600.
//
// Missing parent directories are created with mode 0700. An existing .azud
// parent directory is tightened to 0700; other existing directories are left
// unchanged because a custom secrets_path may live in a shared directory.
// WriteFile refuses to replace a symbolic link or a non-regular file.
func WriteFile(path string, data []byte) (Result, error) {
	var result Result

	dir := filepath.Dir(path)
	if err := EnsureDir(dir); err != nil {
		return result, err
	}

	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		result.Created = true
	case err != nil:
		return result, fmt.Errorf("failed to inspect secrets file: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return result, fmt.Errorf("refusing to write secrets file %s: it is a symbolic link; set secrets_path to the link target instead", path)
	case !info.Mode().IsRegular():
		return result, fmt.Errorf("refusing to write secrets file %s: not a regular file", path)
	default:
		result.PreviousMode = info.Mode().Perm()
	}

	tmpFile, err := os.CreateTemp(dir, TempPattern)
	if err != nil {
		return result, fmt.Errorf("failed to create secrets temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmpFile.Chmod(FileMode); err != nil {
		_ = tmpFile.Close()
		return result, fmt.Errorf("failed to secure secrets temp file: %w", err)
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return result, fmt.Errorf("failed to write secrets file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return result, fmt.Errorf("failed to sync secrets file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return result, fmt.Errorf("failed to close secrets file: %w", err)
	}
	// Rename replaces the directory entry itself. A link swapped in after the
	// Lstat above is replaced rather than followed, and hard-linked copies of
	// the old file keep their content.
	if err := os.Rename(tmpPath, path); err != nil {
		return result, fmt.Errorf("failed to commit secrets file: %w", err)
	}
	committed = true
	syncDir(dir)

	return result, nil
}

// Tighten removes group and other permissions from an existing regular file
// without following a symbolic link. It reports whether the mode changed.
func Tighten(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("refusing to change %s: not a regular file", path)
	}
	if !InsecureMode(info) {
		return false, nil
	}
	if err := f.Chmod(FileMode); err != nil {
		return false, err
	}
	return true, nil
}

// AppendFile appends data to a project file such as .gitignore, creating it
// with perm when missing. It refuses symbolic links and non-regular files so
// a planted link cannot redirect the write outside the project.
func AppendFile(path string, data []byte, perm os.FileMode) error {
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("refusing to write %s: it is a symbolic link", path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("refusing to write %s: not a regular file", path)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|oNoFollow, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// EnsureDir creates dir and its missing parents with mode 0700. An existing
// .azud directory is tightened to 0700; any other existing directory is left
// unchanged.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("failed to create secrets directory: %w", err)
	}
	return securePrivateDir(dir)
}

// securePrivateDir tightens the Azud-owned .azud directory to 0700. It does
// not follow a symbolic link and never touches any other directory.
func securePrivateDir(dir string) error {
	if runtime.GOOS == "windows" || filepath.Base(dir) != azudDirName {
		return nil
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("failed to inspect secrets directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return fmt.Errorf("failed to secure secrets directory: %w", err)
	}
	return nil
}

// syncDir flushes the rename to disk where the platform supports it.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
