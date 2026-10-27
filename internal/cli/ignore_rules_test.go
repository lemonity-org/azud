package cli

import "testing"

func TestIgnoreRuleSatisfiedForAzudSecrets(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "empty", content: "", want: false},
		{name: "comment only", content: "# .azud/secrets are documented elsewhere\nnode_modules/\n", want: false},
		{name: "near miss suffix", content: ".azud/secrets-old\n", want: false},
		{name: "near miss example", content: ".azud/secrets.example\n", want: false},
		{name: "directory only", content: ".azud/secrets/\n", want: false},
		{name: "nested path", content: "sub/.azud/secrets\n", want: false},
		{name: "negation only", content: "!.azud/secrets\n", want: false},
		{name: "leading whitespace", content: "  .azud/secrets\n", want: false},
		{name: "exact", content: ".azud/secrets\n", want: true},
		{name: "anchored", content: "/.azud/secrets\n", want: true},
		{name: "any depth", content: "**/.azud/secrets\n", want: true},
		{name: "whole directory", content: ".azud/\n", want: true},
		{name: "directory contents", content: "/.azud/*\n", want: true},
		{name: "CRLF", content: "node_modules/\r\n.azud/secrets\r\n", want: true},
		{name: "trailing spaces", content: ".azud/secrets  \n", want: true},
		{name: "no trailing newline", content: "node_modules/\n.azud/secrets", want: true},
		{name: "later negation", content: ".azud/secrets\n!.azud/secrets\n", want: false},
		{name: "later glob negation", content: ".azud/\n!.azud/secret*\n", want: false},
		{name: "later catch-all negation", content: ".azud/secrets\n!*\n", want: false},
		{name: "rule after negation", content: "!.azud/secrets\n.azud/secrets\n", want: true},
		{name: "unrelated negation", content: ".azud/\n!.azud/hooks\n!.azud/secrets.example\n", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ignoreRuleSatisfied(tt.content, azudSecretsIgnoreRule); got != tt.want {
				t.Fatalf("ignoreRuleSatisfied(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestIgnoreRuleSatisfiedForSecretsTempFiles(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "empty", content: "", want: false},
		{name: "secrets file only", content: ".azud/secrets\n", want: false},
		{name: "exact", content: ".azud/.secrets-*.tmp\n", want: true},
		{name: "any depth", content: "**/.azud/.secrets-*.tmp\n", want: true},
		{name: "whole directory", content: ".azud/\n", want: true},
		{name: "later negation", content: ".azud/\n!.azud/.secrets-*.tmp\n", want: false},
		{name: "later extension negation", content: ".azud/.secrets-*.tmp\n!*.tmp\n", want: false},
		{name: "unrelated negation", content: ".azud/\n!.azud/hooks\n", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ignoreRuleSatisfied(tt.content, azudSecretsTempIgnoreRule); got != tt.want {
				t.Fatalf("ignoreRuleSatisfied(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestIgnoreRuleSatisfiedRespectsNegationWhenAsked(t *testing.T) {
	rule := ignoreRule{line: ".git/", target: ".git", equivalents: []string{".git", ".git/"}, respectNegation: true}
	if !ignoreRuleSatisfied("!.git\n", rule) {
		t.Fatal("a deliberate negation of a hygiene rule must not be overridden")
	}
	if ignoreRuleSatisfied("!.github/\n", rule) {
		t.Fatal("an unrelated negation must not satisfy the rule")
	}
}

func TestIgnoreAppendBlockStartsOnNewLine(t *testing.T) {
	if got := ignoreAppendBlock([]byte("node_modules/"), "# c", []string{"a", "b"}); got != "\n# c\na\nb\n" {
		t.Fatalf("block = %q", got)
	}
	if got := ignoreAppendBlock(nil, "# c", []string{"a"}); got != "# c\na\n" {
		t.Fatalf("block = %q", got)
	}
}
