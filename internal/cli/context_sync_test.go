package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseIgnorePatterns(t *testing.T) {
	content := `
# comment
_build/
/deps/
*.md
!README.md
.git
node_modules
`
	patterns := parseIgnorePatterns(content)

	expected := []struct {
		pattern string
		negate  bool
	}{
		{"_build", false},
		{"deps", false},
		{"*.md", false},
		{"README.md", true},
		{".git", false},
		{"node_modules", false},
	}

	if len(patterns) != len(expected) {
		t.Fatalf("expected %d patterns, got %d", len(expected), len(patterns))
	}

	for i, p := range patterns {
		if p.pattern != expected[i].pattern {
			t.Errorf("pattern[%d]: expected %q, got %q", i, expected[i].pattern, p.pattern)
		}
		if p.negate != expected[i].negate {
			t.Errorf("pattern[%d]: expected negate=%v, got %v", i, expected[i].negate, p.negate)
		}
	}
}

func TestShouldIgnore(t *testing.T) {
	patterns := parseIgnorePatterns(`
_build/
deps/
*.md
!README.md
.git
node_modules
**/*.log
`)

	tests := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		{"_build", true, true},
		{"_build/lib/app", false, true},
		{"deps", true, true},
		{"deps/phoenix/mix.exs", false, true},
		{"CHANGELOG.md", false, true},
		{"README.md", false, false}, // negated
		{".git", true, true},
		{".git/HEAD", false, true},
		{"node_modules", true, true},
		{"node_modules/foo/index.js", false, true},
		{"lib/app.ex", false, false},
		{"config/runtime.exs", false, false},
		{"Dockerfile", false, false},
		{"some/deep/path/debug.log", false, true}, // **/
		{"app.log", false, true},                  // **/
	}

	for _, tt := range tests {
		got := shouldIgnore(tt.path, tt.isDir, patterns)
		if got != tt.ignored {
			t.Errorf("shouldIgnore(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.ignored)
		}
	}
}

func TestReadContainerIgnoreAlwaysExcludesAzudSecrets(t *testing.T) {
	dir := t.TempDir()
	ignore := []byte("*.log\n!.azud/secrets\n")
	if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), ignore, 0644); err != nil {
		t.Fatalf("write .dockerignore: %v", err)
	}

	patterns := readContainerIgnore(dir)

	tests := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		{".azud/secrets", false, true},
		{".azud/secrets/api", false, true},
		{".azud/hooks/post-deploy", false, false},
		{"debug.log", false, true},
	}

	for _, tt := range tests {
		if got := shouldIgnore(tt.path, tt.isDir, patterns); got != tt.ignored {
			t.Errorf("shouldIgnore(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.ignored)
		}
	}
}

func TestMatchIgnorePattern(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		match   bool
	}{
		// Simple patterns
		{"_build", "_build", true},
		{"deps", "deps", true},
		{"lib", "deps", false},

		// Glob patterns
		{"README.md", "*.md", true},
		{"CHANGELOG.md", "*.md", true},
		{"config/private.env", "*.env", true},
		{"lib/app.ex", "*.md", false},

		// Nested path matches parent
		{"_build/lib/app.beam", "_build", true},
		{".git/HEAD", ".git", true},

		// ** prefix
		{"deep/path/debug.log", "**/*.log", true},
		{"app.log", "**/*.log", true},

		// ** suffix
		{"vendor/foo/bar.go", "vendor/**", true},
		{"vendor", "vendor/**", true},

		// No false matches
		{"lib/app.ex", "deps", false},
		{"config/dev.exs", "_build", false},
	}

	for _, tt := range tests {
		got := matchIgnorePattern(tt.path, tt.pattern)
		if got != tt.match {
			t.Errorf("matchIgnorePattern(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.match)
		}
	}
}

func archiveHeaders(t *testing.T, dir string, patterns []ignorePattern) map[string]*tar.Header {
	t.Helper()
	var archive bytes.Buffer
	if err := createContextArchive(&archive, dir, patterns); err != nil {
		t.Fatalf("createContextArchive: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	headers := make(map[string]*tar.Header)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		copy := *header
		headers[header.Name] = &copy
	}
	return headers
}

func TestCreateContextArchiveIgnoreNegationAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ignored", "keep"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"app.txt":                  "app",
		"config/private.env":       "secret",
		"ignored/drop.txt":         "drop",
		"ignored/keep/include.txt": "include",
	} {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("app.txt", filepath.Join(dir, "app-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	patterns := parseIgnorePatterns("*.env\nignored\n!ignored/keep/include.txt\n")
	headers := archiveHeaders(t, dir, patterns)
	if _, ok := headers["config/private.env"]; ok {
		t.Fatal("nested *.env file was included")
	}
	if _, ok := headers["ignored/drop.txt"]; ok {
		t.Fatal("ignored descendant was included")
	}
	if _, ok := headers["ignored/keep/include.txt"]; !ok {
		t.Fatal("negated descendant under ignored directory was not included")
	}
	link := headers["app-link"]
	if link == nil || link.Typeflag != tar.TypeSymlink || link.Linkname != "app.txt" {
		t.Fatalf("safe symlink header = %#v", link)
	}
}

func TestCreateContextArchiveRejectsEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("../outside-secret", filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var archive bytes.Buffer
	err := createContextArchive(&archive, dir, nil)
	if err == nil || !strings.Contains(err.Error(), "outside the build context") {
		t.Fatalf("expected escaping symlink error, got %v", err)
	}
}
