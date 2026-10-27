package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeLocalBuildIgnoreFile writes the ignore file passed to local podman
// builds with --ignorefile and returns its path and a cleanup function.
//
// --ignorefile replaces the ignore file podman would otherwise read; it does
// not merge with it. The user's effective ignore file is therefore copied
// verbatim and Azud's mandatory excludes are appended last, where a user
// negation cannot re-include them. The file lives outside the build context.
func writeLocalBuildIgnoreFile(contextDir, dockerfile string) (string, func(), error) {
	if contextDir == "" {
		contextDir = "."
	}
	excludes, err := mandatoryLocalBuildExcludes(contextDir)
	if err != nil {
		return "", nil, err
	}
	userContent, err := effectiveContainerIgnore(contextDir, dockerfile)
	if err != nil {
		return "", nil, err
	}

	var content strings.Builder
	content.Write(userContent)
	if len(userContent) > 0 && !strings.HasSuffix(string(userContent), "\n") {
		content.WriteByte('\n')
	}
	content.WriteString("# Added by Azud: local secrets never enter the build context\n")
	for _, pattern := range excludes {
		content.WriteString(pattern)
		content.WriteByte('\n')
	}

	dir, err := os.MkdirTemp("", "azud-ignore-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create build ignore directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "containerignore")
	if err := os.WriteFile(path, []byte(content.String()), 0600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to write build ignore file: %w", err)
	}
	return path, cleanup, nil
}

// effectiveContainerIgnore returns the ignore file content podman would use
// for this build, following its precedence (verified with podman 6.1):
// <containerfile>.dockerignore, <containerfile>.containerignore, then
// <context>/.containerignore and <context>/.dockerignore. A relative
// containerfile is resolved against the context directory, as podman does
// for this lookup.
func effectiveContainerIgnore(contextDir, dockerfile string) ([]byte, error) {
	if dockerfile == "" {
		dockerfile = "Dockerfile"
		if _, err := os.Stat(filepath.Join(contextDir, "Containerfile")); err == nil {
			dockerfile = "Containerfile"
		}
	}
	if !filepath.IsAbs(dockerfile) {
		dockerfile = filepath.Join(contextDir, dockerfile)
	}

	candidates := []string{
		dockerfile + ".dockerignore",
		dockerfile + ".containerignore",
		filepath.Join(contextDir, ".containerignore"),
		filepath.Join(contextDir, ".dockerignore"),
	}
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", candidate, err)
		}
		return data, nil
	}
	return nil, nil
}

// mandatoryLocalBuildExcludes returns the patterns every local build must
// exclude: .azud/secrets at any depth (projects nested below the context, as
// in monorepos), the configured secrets file when it is inside the build
// context, and leftover temporary files of the secrets writer next to either.
// These match the excludes of remote builds, except that podman matches
// paths, so a hard link to a secrets file under another name is not caught.
func mandatoryLocalBuildExcludes(contextDir string) ([]string, error) {
	excludes := []string{".azud/secrets", "**/.azud/secrets", ".azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp"}

	absContext, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve build context path: %w", err)
	}
	absSecrets, err := filepath.Abs(getSecretsFilePath())
	if err != nil {
		return nil, fmt.Errorf("failed to resolve secrets path: %w", err)
	}
	rel, err := filepath.Rel(absContext, absSecrets)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// The secrets file is outside the context and cannot be sent.
		return excludes, nil
	}

	rel = filepath.ToSlash(rel)
	if rel == ".azud/secrets" {
		return excludes, nil
	}
	pattern, err := escapeIgnorePattern(rel)
	if err != nil {
		return nil, err
	}
	excludes = append(excludes, pattern)

	tmpPattern := ".secrets-*.tmp"
	if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
		escapedDir, err := escapeIgnorePattern(dir)
		if err != nil {
			return nil, err
		}
		tmpPattern = escapedDir + "/" + tmpPattern
	}
	if tmpPattern != ".azud/.secrets-*.tmp" {
		excludes = append(excludes, tmpPattern)
	}
	return excludes, nil
}

// escapeIgnorePattern turns a literal context-relative path into an ignore
// pattern that matches only that path.
func escapeIgnorePattern(path string) (string, error) {
	if strings.ContainsAny(path, "\n\r") || strings.TrimSpace(path) != path {
		return "", fmt.Errorf("secrets_path %q cannot be excluded from the build context; rename it or move it outside the context", path)
	}
	var escaped strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`\*?[`, r) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}
	pattern := escaped.String()
	if strings.HasPrefix(pattern, "!") || strings.HasPrefix(pattern, "#") {
		pattern = `\` + pattern
	}
	return pattern, nil
}
