package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/shell"
	"github.com/lemonity-org/azud/internal/ssh"
)

// syncBuildContext archives the local build context and streams it to the
// remote builder host over SSH. Returns the remote directory path where
// the context was extracted.
func syncBuildContext(sshClient *ssh.Client, host, localContext string) (string, error) {
	log := output.DefaultLogger

	if localContext == "" {
		localContext = "."
	}

	absContext, err := filepath.Abs(localContext)
	if err != nil {
		return "", fmt.Errorf("failed to resolve build context path: %w", err)
	}

	info, err := os.Stat(absContext)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("build context directory does not exist: %s", absContext)
	}

	remotePath := fmt.Sprintf("/tmp/azud-build-%s-%d", cfg.Service, time.Now().UnixNano())

	log.Info("Syncing build context to %s:%s...", host, remotePath)

	// Create remote directory
	mkdirCmd := fmt.Sprintf("mkdir -p %s", shell.Quote(remotePath))
	if result, err := sshClient.Execute(host, mkdirCmd); err != nil {
		return "", fmt.Errorf("failed to create remote build directory: %w", err)
	} else if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to create remote build directory: %s", result.Stderr)
	}

	// Read ignore patterns
	patterns := readContainerIgnore(absContext)

	// Stream tar.gz archive to remote via SSH pipe
	pr, pw := io.Pipe()
	archiveErr := make(chan error, 1)
	go func() {
		defer pw.Close()
		archiveErr <- createContextArchive(pw, absContext, patterns)
	}()

	extractCmd := fmt.Sprintf("tar xzf - -C %s", shell.Quote(remotePath))
	result, err := sshClient.ExecuteWithStdin(host, extractCmd, pr)
	if err != nil {
		return "", fmt.Errorf("failed to extract build context on remote: %w", err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to extract build context on remote: %s", result.Stderr)
	}

	// Check if archive creation had errors
	if err := <-archiveErr; err != nil {
		return "", fmt.Errorf("failed to create build context archive: %w", err)
	}

	log.Success("Build context synced")
	return remotePath, nil
}

// cleanupBuildContext removes the remote build context directory.
func cleanupBuildContext(sshClient *ssh.Client, host, remotePath string) {
	if remotePath == "" {
		return
	}
	// Safety check: only remove paths under /tmp/azud-build-
	if !strings.HasPrefix(remotePath, "/tmp/azud-build-") {
		return
	}
	log := output.DefaultLogger
	cmd := fmt.Sprintf("rm -rf %s", shell.Quote(remotePath))
	if _, err := sshClient.Execute(host, cmd); err != nil {
		log.Warn("Failed to clean up remote build context: %v", err)
	}
}

// createContextArchive creates a tar.gz archive of the build context,
// excluding files that match the container ignore patterns.
func createContextArchive(w io.Writer, contextDir string, patterns []ignorePattern) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.WalkDir(contextDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(contextDir, path)
		if err != nil {
			return err
		}

		// Skip root directory
		if relPath == "." {
			return nil
		}

		if shouldIgnore(relPath, d.IsDir(), patterns) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		// Only include regular files and directories
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		// Use relative path in the archive
		header.Name = relPath
		if d.IsDir() {
			header.Name += "/"
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			f.Close()
			if copyErr != nil {
				return copyErr
			}
		}

		return nil
	})
}

// ignorePattern represents a parsed .dockerignore / .containerignore line.
type ignorePattern struct {
	pattern string
	negate  bool
}

// readContainerIgnore reads ignore patterns from .containerignore or
// .dockerignore in the context directory.
func readContainerIgnore(contextDir string) []ignorePattern {
	for _, name := range []string{".containerignore", ".dockerignore"} {
		path := filepath.Join(contextDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return parseIgnorePatterns(string(data))
	}
	// Default: always exclude .git
	return []ignorePattern{{pattern: ".git"}}
}

// parseIgnorePatterns parses a .dockerignore file into patterns.
func parseIgnorePatterns(content string) []ignorePattern {
	var patterns []ignorePattern
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		if negate {
			line = strings.TrimPrefix(line, "!")
		}
		line = strings.TrimPrefix(line, "/")
		line = strings.TrimSuffix(line, "/")
		patterns = append(patterns, ignorePattern{pattern: line, negate: negate})
	}
	return patterns
}

// shouldIgnore checks if a path should be excluded based on the ignore
// patterns. Later patterns override earlier ones (last match wins),
// including negation patterns.
func shouldIgnore(relPath string, isDir bool, patterns []ignorePattern) bool {
	ignored := false
	for _, p := range patterns {
		if matchIgnorePattern(relPath, p.pattern) {
			ignored = !p.negate
		}
	}
	return ignored
}

// matchIgnorePattern checks if a relative path matches a .dockerignore pattern.
func matchIgnorePattern(relPath, pattern string) bool {
	// Handle **/ prefix (match in any directory depth)
	if strings.HasPrefix(pattern, "**/") {
		suffix := strings.TrimPrefix(pattern, "**/")
		if matchSimple(relPath, suffix) {
			return true
		}
		parts := strings.Split(relPath, string(filepath.Separator))
		for i := range parts {
			sub := strings.Join(parts[i:], string(filepath.Separator))
			if matchSimple(sub, suffix) {
				return true
			}
		}
		return false
	}

	// Handle /** suffix (match everything under)
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return relPath == prefix || strings.HasPrefix(relPath, prefix+string(filepath.Separator))
	}

	// Direct match
	if matchSimple(relPath, pattern) {
		return true
	}

	// Match if the pattern matches a parent directory of the path
	parts := strings.Split(relPath, string(filepath.Separator))
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], string(filepath.Separator))
		if matchSimple(parent, pattern) {
			return true
		}
	}

	return false
}

// matchSimple performs a filepath.Match-style glob match.
func matchSimple(name, pattern string) bool {
	matched, _ := filepath.Match(pattern, name)
	return matched
}
