// Package shell provides utilities for safely building shell commands.
package shell

import (
	"regexp"
	"strings"
)

// safePattern matches strings that don't need quoting in POSIX shell.
// Only allows alphanumeric, underscore, hyphen, dot, forward slash, and colon.
var safePattern = regexp.MustCompile(`^[a-zA-Z0-9_.\-/:@]+$`)

// Quote returns a shell-safe quoted version of the input string.
// It uses single quotes and escapes any embedded single quotes.
// This is safe for use in sh/bash command strings.
//
// Examples:
//
//	Quote("simple")           -> "simple"
//	Quote("path with spaces") -> "'path with spaces'"
//	Quote("it's")             -> "'it'\\''s'"
//	Quote("$(whoami)")        -> "'$(whoami)'"
func Quote(s string) string {
	if s == "" {
		return "''"
	}

	// If the string contains only safe characters, no quoting needed
	if safePattern.MatchString(s) {
		return s
	}

	// Use single quotes and escape embedded single quotes
	// 'foo'\''bar' -> foo'bar (shell concatenates adjacent strings)
	escaped := strings.ReplaceAll(s, "'", "'\\''")
	return "'" + escaped + "'"
}

// QuoteAll quotes each string in the slice and returns a new slice.
func QuoteAll(args []string) []string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = Quote(arg)
	}
	return quoted
}

// Join quotes each argument and joins them with spaces.
// Useful for building command strings.
//
// Example:
//
//	Join("cat", "/path/to/file with spaces") -> "cat '/path/to/file with spaces'"
func Join(args ...string) string {
	return strings.Join(QuoteAll(args), " ")
}

// Validate checks if a string is safe for use as an identifier in shell contexts.
// Returns true if the string contains only safe characters.
// Use this to validate user-provided names before using them unquoted.
func Validate(s string) bool {
	if s == "" {
		return false
	}
	return safePattern.MatchString(s)
}

// ValidateName checks if a string is a valid identifier name.
// More restrictive than Validate - must start with letter, only alphanumeric/underscore/hyphen/dot.
var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.\-]*$`)

func ValidateName(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	return namePattern.MatchString(s)
}
