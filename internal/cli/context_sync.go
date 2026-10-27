package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/secretsfile"
	"github.com/lemonity-org/azud/internal/shell"
	"github.com/lemonity-org/azud/internal/ssh"
)

// syncBuildContext archives the local build context and streams it to the
// remote builder host over SSH. Returns the remote directory path where
// the context was extracted.
func syncBuildContext(sshClient *ssh.Client, host, localContext string) (_ string, retErr error) {
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

	// Create a private, unpredictable remote directory. mktemp -d creates it
	// with mode 0700, so other users on a shared builder cannot read the
	// context.
	result, err := sshClient.Execute(host, remoteBuildDirCommand(cfg.Service))
	if err != nil {
		return "", fmt.Errorf("failed to create remote build directory: %w", err)
	} else if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to create remote build directory: %s", result.Stderr)
	}
	remotePath, err := parseRemoteBuildDir(cfg.Service, result.Stdout)
	if err != nil {
		return "", err
	}

	// Remove the directory on every failure from here on; the caller only
	// schedules cleanup after a successful sync.
	defer func() {
		if retErr != nil {
			cleanupBuildContext(sshClient, host, remotePath)
		}
	}()

	log.Info("Syncing build context to %s:%s...", host, remotePath)

	// Read ignore patterns
	patterns := readContainerIgnore(absContext)
	excluded := secretContextExclusions(absContext)
	for _, rel := range excluded.paths {
		log.Debug("Excluding secrets file %s from build context", rel)
	}

	// Stream tar.gz archive to remote via SSH pipe
	pr, pw := io.Pipe()
	archiveErr := make(chan error, 1)
	go func() {
		if err := createContextArchive(pw, absContext, patterns, excluded); err != nil {
			_ = pw.CloseWithError(err)
			archiveErr <- err
			return
		}
		archiveErr <- pw.Close()
	}()

	extractCmd := fmt.Sprintf("tar xzf - -C %s", shell.Quote(remotePath))
	result, err = sshClient.ExecuteWithStdin(host, extractCmd, pr)
	_ = pr.Close()
	archiveBuildErr := <-archiveErr
	if err != nil {
		return "", fmt.Errorf("failed to extract build context on remote: %w", err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to extract build context on remote: %s", result.Stderr)
	}

	// Check if archive creation had errors
	if archiveBuildErr != nil {
		return "", fmt.Errorf("failed to create build context archive: %w", archiveBuildErr)
	}

	log.Success("Build context synced")
	return remotePath, nil
}

// remoteBuildDirPrefix is the only location remote build contexts are
// created in and removed from.
const remoteBuildDirPrefix = "/tmp/azud-build-"

// remoteBuildDirRegex matches a single directory directly under /tmp. It has
// no slash after the prefix, so a value cannot escape with "..".
var remoteBuildDirRegex = regexp.MustCompile(`^/tmp/azud-build-[A-Za-z0-9_.-]+$`)

// remoteBuildDirCommand creates a private remote build directory with mktemp
// and prints its path. The template keeps the azud-build-<service>. prefix so
// stale directories remain recognisable.
func remoteBuildDirCommand(service string) string {
	template := remoteBuildDirPrefix + service + ".XXXXXXXXXX"
	return fmt.Sprintf(`umask 077 && d=$(mktemp -d %s) && chmod 700 "$d" && test -d "$d" && test ! -L "$d" && printf '%%s\n' "$d"`, shell.Quote(template))
}

// parseRemoteBuildDir extracts the directory created by remoteBuildDirCommand.
// The last non-empty line is used so login banners cannot be mistaken for the
// path, and the result must match the expected template.
func parseRemoteBuildDir(service, stdout string) (string, error) {
	var remotePath string
	for _, line := range strings.Split(stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			remotePath = line
		}
	}
	if !remoteBuildDirRegex.MatchString(remotePath) || !strings.HasPrefix(remotePath, remoteBuildDirPrefix+service+".") {
		return "", fmt.Errorf("failed to create remote build directory: unexpected path %q", remotePath)
	}
	return remotePath, nil
}

// cleanupBuildContext removes the remote build context directory.
func cleanupBuildContext(sshClient *ssh.Client, host, remotePath string) {
	// Safety check: only remove a single directory under /tmp/azud-build-
	if !remoteBuildDirRegex.MatchString(remotePath) {
		return
	}
	log := output.DefaultLogger
	cmd := fmt.Sprintf("rm -rf -- %s", shell.Quote(remotePath))
	result, err := sshClient.Execute(host, cmd)
	if err != nil {
		log.Warn("Failed to clean up remote build context: %v", err)
		return
	}
	if !result.Success() {
		log.Warn("Failed to clean up remote build context %s: %s", remotePath, strings.TrimSpace(result.Stderr))
	}
}

// contextExclusions lists local secrets files that are never archived,
// regardless of ignore patterns or negations.
type contextExclusions struct {
	// paths are context-relative, slash-separated paths.
	paths []string
	// files identify the same secrets files by inode so hard links and
	// aliased paths are excluded too.
	files []os.FileInfo
	// tempDirs are the context-relative directories of the secrets files,
	// where the secrets writer may leave .secrets-*.tmp copies behind.
	tempDirs []string
}

// matchesPath reports whether relSlash is an excluded path, lies below one, or
// is a leftover temporary file of the secrets writer.
func (e contextExclusions) matchesPath(relSlash string) bool {
	for _, excluded := range e.paths {
		if relSlash == excluded || strings.HasPrefix(relSlash, excluded+"/") {
			return true
		}
	}
	if slices.Contains(e.tempDirs, path.Dir(relSlash)) {
		if matched, _ := path.Match(secretsfile.TempPattern, path.Base(relSlash)); matched {
			return true
		}
	}
	return false
}

// matchesFile reports whether info refers to an excluded secrets file.
func (e contextExclusions) matchesFile(info os.FileInfo) bool {
	for _, secret := range e.files {
		if os.SameFile(info, secret) {
			return true
		}
	}
	return false
}

// secretContextExclusions resolves the configured local secrets file paths
// against the build context. Paths are compared exactly rather than as ignore
// patterns, so file names containing glob characters are still excluded.
// Relative secrets paths are resolved against the working directory, which is
// how the config loader reads them.
func secretContextExclusions(absContext string) contextExclusions {
	var candidates []string
	if cfg != nil && cfg.SecretsPath != "" {
		candidates = append(candidates, cfg.SecretsPath)
	}
	candidates = append(candidates, getSecretsFilePath(), filepath.Join(".azud", "secrets"))

	contexts := []string{absContext}
	if real, err := filepath.EvalSymlinks(absContext); err == nil && real != absContext {
		contexts = append(contexts, real)
	}

	var excluded contextExclusions
	seenPaths := make(map[string]bool)
	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		variants := []string{abs}
		if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
			variants = append(variants, real)
		}
		if info, err := os.Stat(abs); err == nil && info.Mode().IsRegular() {
			excluded.files = append(excluded.files, info)
		}
		for _, contextDir := range contexts {
			for _, variant := range variants {
				rel, err := filepath.Rel(contextDir, variant)
				if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					continue
				}
				rel = filepath.ToSlash(rel)
				if !seenPaths[rel] {
					seenPaths[rel] = true
					excluded.paths = append(excluded.paths, rel)
				}
				if dir := path.Dir(rel); !slices.Contains(excluded.tempDirs, dir) {
					excluded.tempDirs = append(excluded.tempDirs, dir)
				}
			}
		}
	}
	return excluded
}

// createContextArchive creates a tar.gz archive of the build context,
// excluding the given secrets files and files that match the container
// ignore patterns.
func createContextArchive(w io.Writer, contextDir string, patterns []ignorePattern, excluded contextExclusions) (err error) {
	gw := gzip.NewWriter(w)
	defer func() {
		if closeErr := gw.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("failed to close gzip writer: %w", closeErr)
		}
	}()

	tw := tar.NewWriter(gw)
	defer func() {
		if closeErr := tw.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("failed to close tar writer: %w", closeErr)
		}
	}()

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

		// Secrets files are excluded before ignore patterns are evaluated, so
		// a negation pattern cannot re-include them.
		if excluded.matchesPath(filepath.ToSlash(relPath)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if shouldIgnore(relPath, d.IsDir(), patterns) {
			// Continue walking ignored directories so later negation patterns can
			// re-include descendants, matching container-ignore semantics.
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.Mode().IsRegular() && excluded.matchesFile(info) {
			return nil
		}

		isSymlink := info.Mode()&os.ModeSymlink != 0
		// Preserve symlinks as links (do not follow them), matching local
		// Podman/Docker context behavior.
		if !info.Mode().IsRegular() && !info.IsDir() && !isSymlink {
			return nil
		}

		linkTarget := ""
		if isSymlink {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(linkTarget) {
				return fmt.Errorf("symlink %s points outside the build context", relPath)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(path), linkTarget))
			resolvedRel, relErr := filepath.Rel(contextDir, resolved)
			if relErr != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("symlink %s points outside the build context", relPath)
			}
		}
		header, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return err
		}
		// Use relative path in the archive
		header.Name = filepath.ToSlash(relPath)
		header.Linkname = filepath.ToSlash(header.Linkname)
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
			if closeErr := f.Close(); closeErr != nil && copyErr == nil {
				copyErr = fmt.Errorf("failed to close %s: %w", path, closeErr)
			}
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
		return withMandatoryContextExcludes(parseIgnorePatterns(string(data)))
	}
	// Default: always exclude .git
	return withMandatoryContextExcludes([]ignorePattern{{pattern: ".git"}})
}

func withMandatoryContextExcludes(patterns []ignorePattern) []ignorePattern {
	// These are appended last so user negation patterns cannot re-include
	// sensitive Azud-local files in remote build contexts. The **/ form also
	// covers projects nested below the context directory (monorepos), and the
	// .secrets-*.tmp patterns cover copies left behind by an interrupted
	// secrets write.
	return append(patterns,
		ignorePattern{pattern: ".azud/secrets"},
		ignorePattern{pattern: "**/.azud/secrets"},
		ignorePattern{pattern: ".azud/.secrets-*.tmp"},
		ignorePattern{pattern: "**/.azud/.secrets-*.tmp"},
	)
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
	relPath = filepath.ToSlash(relPath)
	pattern = filepath.ToSlash(pattern)
	separator := "/"
	// A slashless pattern applies to a file or directory with that basename
	// at any depth (for example *.env matches config/private.env).
	if !strings.Contains(pattern, separator) {
		for _, component := range strings.Split(relPath, separator) {
			if matchSimple(component, pattern) {
				return true
			}
		}
	}

	// Handle **/ prefix (match in any directory depth)
	if strings.HasPrefix(pattern, "**/") {
		suffix := strings.TrimPrefix(pattern, "**/")
		if matchSimple(relPath, suffix) {
			return true
		}
		parts := strings.Split(relPath, separator)
		for i := range parts {
			sub := strings.Join(parts[i:], separator)
			if matchSimple(sub, suffix) {
				return true
			}
		}
		return false
	}

	// Handle /** suffix (match everything under)
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return relPath == prefix || strings.HasPrefix(relPath, prefix+separator)
	}

	// Direct match
	if matchSimple(relPath, pattern) {
		return true
	}

	// Match if the pattern matches a parent directory of the path
	parts := strings.Split(relPath, separator)
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], separator)
		if matchSimple(parent, pattern) {
			return true
		}
	}

	return false
}

// matchSimple performs a filepath.Match-style glob match.
func matchSimple(name, pattern string) bool {
	matched, _ := path.Match(pattern, name)
	return matched
}
