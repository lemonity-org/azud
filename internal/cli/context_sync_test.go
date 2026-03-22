package cli

import (
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
		{"app.log", false, true},                   // **/
	}

	for _, tt := range tests {
		got := shouldIgnore(tt.path, tt.isDir, patterns)
		if got != tt.ignored {
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
