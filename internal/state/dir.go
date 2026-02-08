// Package state provides utilities for managing Azud state directories.
package state

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// RootStateDir is the state directory for root users
	RootStateDir = "/var/lib/azud"

	// UserStateDirName is the directory name under user home
	UserStateDirName = ".local/share/azud"
)

// Dir returns the appropriate state directory path for the given SSH user.
// For root users, it returns /var/lib/azud.
// For non-root users, it returns a path using ${HOME} which expands correctly
// even inside double quotes in shell commands.
//
// IMPORTANT: Paths from Dir() contain shell variables and should NOT be passed
// through shell.Quote(). Instead, use them in double-quoted contexts:
//
//	fmt.Sprintf("mkdir -p \"%s\"", state.Dir(user))
//
// Or use the DirQuoted() helper for a safely quoted version.
func Dir(user string) string {
	if user == "" || user == "root" {
		return RootStateDir
	}
	// Use ${HOME} syntax which expands inside double quotes
	return "${HOME}/" + UserStateDirName
}

// DirQuoted returns the state directory path in a form safe for shell commands.
// For root users, it returns /var/lib/azud (no quoting needed).
// For non-root users, it returns "${HOME}/.local/share/azud" (double-quoted
// to allow variable expansion while protecting against spaces).
func DirQuoted(user string) string {
	if user == "" || user == "root" {
		return RootStateDir
	}
	return "\"${HOME}/" + UserStateDirName + "\""
}

// LocalDir returns the state directory path for the current local user.
// This is used for local state files (e.g., canary state).
func LocalDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	// Check if we're running as root
	if os.Getuid() == 0 {
		return RootStateDir, nil
	}

	return filepath.Join(home, UserStateDirName), nil
}

// EnsureLocalDir creates the local state directory if it doesn't exist.
func EnsureLocalDir() (string, error) {
	dir, err := LocalDir()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("failed to create state directory: %w", err)
	}

	return dir, nil
}

// LockFile returns the path to a lock file for the given name.
// The returned path may contain ${HOME} for non-root users.
func LockFile(user, name string) string {
	return Dir(user) + "/" + name + ".lock"
}

// LockFileQuoted returns the lock file path in a shell-safe quoted form.
func LockFileQuoted(user, name string) string {
	if user == "" || user == "root" {
		return RootStateDir + "/" + name + ".lock"
	}
	return "\"${HOME}/" + UserStateDirName + "/" + name + ".lock\""
}

// ConfigFile returns the path to a config file for the given name.
// The returned path may contain ${HOME} for non-root users.
func ConfigFile(user, name string) string {
	return Dir(user) + "/" + name
}

// ConfigFileQuoted returns the config file path in a shell-safe quoted form.
func ConfigFileQuoted(user, name string) string {
	if user == "" || user == "root" {
		return RootStateDir + "/" + name
	}
	return "\"${HOME}/" + UserStateDirName + "/" + name + "\""
}

