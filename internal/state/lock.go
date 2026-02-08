package state

import (
	"fmt"
	"os"
	"path/filepath"
)

// FileLock represents an exclusive file lock.
type FileLock struct {
	file *os.File
	path string
}

// AcquireFileLock acquires an exclusive file lock on the given path.
// Returns a FileLock that must be released by calling Release().
func AcquireFileLock(path string) (*FileLock, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create lock directory: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}

	// Try to acquire exclusive lock (non-blocking)
	if err := lockFileExclusive(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("failed to acquire lock (another process may be holding it): %w", err)
	}

	return &FileLock{file: file, path: path}, nil
}

// Release releases the file lock.
func (l *FileLock) Release() error {
	if l.file == nil {
		return nil
	}

	if err := unlockFile(l.file); err != nil {
		_ = l.file.Close()
		return fmt.Errorf("failed to release lock: %w", err)
	}

	return l.file.Close()
}

// WithFileLock acquires a file lock, runs fn, then releases the lock.
func WithFileLock(path string, fn func() error) error {
	lock, err := AcquireFileLock(path)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	return fn()
}
