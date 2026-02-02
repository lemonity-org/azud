package shell

import (
	"testing"
)

func TestQuote(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "''",
		},
		{
			name:     "simple alphanumeric",
			input:    "simple",
			expected: "simple",
		},
		{
			name:     "with hyphen",
			input:    "my-service",
			expected: "my-service",
		},
		{
			name:     "with underscore",
			input:    "my_service",
			expected: "my_service",
		},
		{
			name:     "with dot",
			input:    "v1.2.3",
			expected: "v1.2.3",
		},
		{
			name:     "file path",
			input:    "/var/lib/azud/config.json",
			expected: "/var/lib/azud/config.json",
		},
		{
			name:     "path with spaces",
			input:    "/path/to/file with spaces",
			expected: "'/path/to/file with spaces'",
		},
		{
			name:     "single quote",
			input:    "it's",
			expected: "'it'\\''s'",
		},
		{
			name:     "double quote",
			input:    `say "hello"`,
			expected: `'say "hello"'`,
		},
		{
			name:     "command substitution dollar",
			input:    "$(whoami)",
			expected: "'$(whoami)'",
		},
		{
			name:     "command substitution backtick",
			input:    "`id`",
			expected: "'`id`'",
		},
		{
			name:     "variable expansion",
			input:    "$HOME/secrets",
			expected: "'$HOME/secrets'",
		},
		{
			name:     "semicolon injection",
			input:    "file; rm -rf /",
			expected: "'file; rm -rf /'",
		},
		{
			name:     "pipe injection",
			input:    "file | cat /etc/passwd",
			expected: "'file | cat /etc/passwd'",
		},
		{
			name:     "ampersand",
			input:    "cmd &",
			expected: "'cmd &'",
		},
		{
			name:     "newline",
			input:    "line1\nline2",
			expected: "'line1\nline2'",
		},
		{
			name:     "tab",
			input:    "col1\tcol2",
			expected: "'col1\tcol2'",
		},
		{
			name:     "backslash",
			input:    "path\\to\\file",
			expected: "'path\\to\\file'",
		},
		{
			name:     "mixed quotes",
			input:    `"it's complicated"`,
			expected: "'\"it'\\''s complicated\"'",
		},
		{
			name:     "registry with port",
			input:    "ghcr.io:443",
			expected: "ghcr.io:443",
		},
		{
			name:     "image reference",
			input:    "ghcr.io/user/app:v1.0.0",
			expected: "ghcr.io/user/app:v1.0.0",
		},
		{
			name:     "email address",
			input:    "user@example.com",
			expected: "user@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Quote(tt.input)
			if result != tt.expected {
				t.Errorf("Quote(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestQuoteAll(t *testing.T) {
	input := []string{"cat", "/path/with spaces", "$(whoami)"}
	expected := []string{"cat", "'/path/with spaces'", "'$(whoami)'"}

	result := QuoteAll(input)

	if len(result) != len(expected) {
		t.Fatalf("QuoteAll length = %d, want %d", len(result), len(expected))
	}

	for i := range result {
		if result[i] != expected[i] {
			t.Errorf("QuoteAll[%d] = %q, want %q", i, result[i], expected[i])
		}
	}
}

func TestJoin(t *testing.T) {
	result := Join("cat", "/path/with spaces", "file.txt")
	expected := "cat '/path/with spaces' file.txt"

	if result != expected {
		t.Errorf("Join = %q, want %q", result, expected)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"", false},
		{"simple", true},
		{"my-service", true},
		{"my_service", true},
		{"v1.2.3", true},
		{"/var/lib/file", true},
		{"ghcr.io:443", true},
		{"user@host", true},
		{"path with spaces", false},
		{"$(whoami)", false},
		{"`id`", false},
		{"$HOME", false},
		{"file;rm", false},
		{"a|b", false},
		{"a&b", false},
		{"'quoted'", false},
		{`"quoted"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := Validate(tt.input)
			if result != tt.expected {
				t.Errorf("Validate(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"", false},
		{"myapp", true},
		{"my-app", true},
		{"my_app", true},
		{"MyApp", true},
		{"my.app", true},
		{"app123", true},
		{"123app", false},         // must start with letter
		{"-app", false},           // must start with letter
		{"_app", false},           // must start with letter
		{"my app", false},         // no spaces
		{"my/app", false},         // no slashes
		{"my:app", false},         // no colons
		{"app@host", false},       // no at signs
		{string(make([]byte, 64)), false}, // too long
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ValidateName(tt.input)
			if result != tt.expected {
				t.Errorf("ValidateName(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}
