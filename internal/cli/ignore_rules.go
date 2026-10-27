package cli

import (
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
)

// ignoreRule is an entry Azud wants active in a .gitignore or container
// ignore file.
type ignoreRule struct {
	// line is appended when the rule is not satisfied.
	line string
	// target is the context-relative path the rule protects.
	target string
	// equivalents are exact patterns that already exclude target.
	equivalents []string
	// respectNegation treats a user's negation of target as a deliberate
	// choice instead of a gap. It is false for rules that protect secrets.
	respectNegation bool
}

// azudSecretsTempIgnoreRule keeps copies of the secrets file that an
// interrupted write leaves behind out of git. The target is the glob itself,
// so any negation that could match a temporary file is detected.
var azudSecretsTempIgnoreRule = ignoreRule{
	line:   ".azud/.secrets-*.tmp",
	target: ".azud/.secrets-*.tmp",
	equivalents: []string{
		".azud/.secrets-*.tmp", "/.azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp",
		".azud", "/.azud", ".azud/", "/.azud/",
		".azud/*", "/.azud/*", ".azud/**", "/.azud/**",
	},
}

// azudSecretsIgnoreRule keeps the default secrets file out of git and out of
// build contexts.
var azudSecretsIgnoreRule = ignoreRule{
	line:   ".azud/secrets",
	target: ".azud/secrets",
	equivalents: []string{
		".azud/secrets", "/.azud/secrets", "**/.azud/secrets",
		".azud", "/.azud", ".azud/", "/.azud/",
		".azud/*", "/.azud/*", ".azud/**", "/.azud/**",
	},
}

// ignoreRuleSatisfied reports whether content already has an active rule
// equivalent to rule. Only exact, uncommented lines count, and the last
// relevant line wins, as in git and container ignore files. Comments,
// near-miss paths such as .azud/secrets.bak, directory-only variants such as
// .azud/secrets/, and lines with leading whitespace never count, so any doubt
// leads to appending a rule, which is always harmless.
func ignoreRuleSatisfied(content string, rule ignoreRule) bool {
	satisfied := false
	for _, raw := range strings.Split(content, "\n") {
		// Git ignores trailing spaces and CRLF files are common; leading
		// whitespace is significant to git, so it is left in place.
		line := strings.TrimRight(strings.TrimSuffix(raw, "\r"), " ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		pattern := strings.TrimPrefix(line, "!")
		switch {
		case !negate && slices.Contains(rule.equivalents, pattern):
			satisfied = true
		case negate && ignorePatternMayMatch(pattern, rule.target):
			satisfied = rule.respectNegation
		}
	}
	return satisfied
}

// ignorePatternMayMatch reports whether pattern could match target or one of
// its parent directories. It errs towards true for patterns it cannot
// evaluate precisely.
func ignorePatternMayMatch(pattern, target string) bool {
	p := strings.Trim(pattern, "/")
	for strings.HasPrefix(p, "**/") {
		p = strings.TrimPrefix(p, "**/")
	}
	if p == "" || p == "**" || strings.Contains(p, "**") {
		return true
	}
	parts := strings.Split(target, "/")
	for i := range parts {
		candidates := []string{strings.Join(parts[:i+1], "/")}
		if !strings.Contains(p, "/") {
			candidates = append(candidates, parts[i])
		}
		for _, candidate := range candidates {
			matched, err := path.Match(p, candidate)
			if err != nil || matched {
				return true
			}
		}
	}
	return false
}

// readIgnoreFile returns the content of an ignore file Azud may append to. A
// missing file reads as empty. Symbolic links are refused: git does not read
// a symlinked in-tree .gitignore, and appending through one would write
// outside the project.
func readIgnoreFile(name string) ([]byte, error) {
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to update %s: it is a symbolic link; replace it with a regular file", name)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to update %s: not a regular file", name)
	}
	return os.ReadFile(name)
}

// missingIgnoreLines returns the lines of rules that content does not satisfy.
func missingIgnoreLines(content []byte, rules ...ignoreRule) []string {
	var missing []string
	for _, rule := range rules {
		if !ignoreRuleSatisfied(string(content), rule) {
			missing = append(missing, rule.line)
		}
	}
	return missing
}

// ignoreAppendBlock renders lines under a comment, starting on a new line.
func ignoreAppendBlock(content []byte, comment string, lines []string) string {
	var block strings.Builder
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		block.WriteByte('\n')
	}
	block.WriteString(comment)
	block.WriteByte('\n')
	for _, line := range lines {
		block.WriteString(line)
		block.WriteByte('\n')
	}
	return block.String()
}
