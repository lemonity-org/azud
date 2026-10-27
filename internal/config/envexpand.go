package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// maxExpandedEnvValue is the maximum length of a single environment value
// substituted into the configuration.
const maxExpandedEnvValue = 64 << 10 // 64 KiB

// envReferencePattern matches a braced reference on a single line. These are
// replaced with plain placeholder tokens before parsing because "{" and "}"
// are YAML flow indicators: hosts: [${HOST}] would not parse otherwise.
var envReferencePattern = regexp.MustCompile(`\$\{[^}\r\n]*\}`)

// protectEnvReferences replaces every braced environment reference in the
// configuration text with a unique alphanumeric token, so the YAML parser sees
// the file's own structure only. The returned placeholders restore the
// original reference text. Environment values are never placed into parsed
// bytes.
//
// A reference that contains a quote or a backslash, or follows a backslash, is
// left as written: YAML must still decode the quoting and escapes in it, which
// a token would hide. Plain flow collections, the reason for the tokens, never
// need such a reference.
func protectEnvReferences(data []byte) ([]byte, *envPlaceholders, error) {
	matches := envReferencePattern.FindAllIndex(data, -1)
	if len(matches) == 0 {
		return data, &envPlaceholders{}, nil
	}

	var nonce string
	for attempt := 0; ; attempt++ {
		if attempt == 3 {
			return nil, nil, fmt.Errorf("failed to generate a unique environment placeholder")
		}
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, fmt.Errorf("failed to generate environment placeholder: %w", err)
		}
		nonce = hex.EncodeToString(raw)
		if !bytes.Contains(data, []byte(nonce)) {
			break
		}
	}

	var out strings.Builder
	out.Grow(len(data) + len(matches)*len("azudenv"+nonce))
	pairs := make([]string, 0, len(matches)*2)
	last := 0
	for _, match := range matches {
		reference := data[match[0]:match[1]]
		if bytes.ContainsAny(reference, `'"\`) || (match[0] > 0 && data[match[0]-1] == '\\') {
			continue
		}
		token := fmt.Sprintf("azudenv%s%06dz", nonce, len(pairs)/2)
		out.Write(data[last:match[0]])
		out.WriteString(token)
		pairs = append(pairs, token, string(reference))
		last = match[1]
	}
	out.Write(data[last:])
	return []byte(out.String()), &envPlaceholders{nonce: nonce, replacer: strings.NewReplacer(pairs...)}, nil
}

// envPlaceholders maps the tokens written by protectEnvReferences back to the
// reference text they replaced.
type envPlaceholders struct {
	nonce    string
	replacer *strings.Replacer
}

// restore returns s with every token replaced by its original reference text.
// A token that YAML decoding altered can no longer be restored, so leftover
// token text is an error instead of part of a configuration value.
func (p *envPlaceholders) restore(s string) (string, error) {
	if p.nonce == "" {
		return s, nil
	}
	s = p.replacer.Replace(s)
	if strings.Contains(s, p.nonce) {
		return "", fmt.Errorf("malformed environment variable reference")
	}
	return s, nil
}

// envExpander substitutes environment references inside YAML scalar values.
type envExpander struct {
	placeholders *envPlaceholders
	total        int
}

// expandEnvInNode expands environment references in the scalar values of a
// parsed document. Mapping keys and comments are never expanded, and each
// substituted value stays a single scalar: an environment value cannot add
// keys, list items, or comments to the configuration.
func expandEnvInNode(node *yaml.Node, placeholders *envPlaceholders) error {
	e := &envExpander{placeholders: placeholders}
	return e.expandNode(node)
}

func (e *envExpander) expandNode(node *yaml.Node) error {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := e.expandNode(child); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			restored, err := e.placeholders.restore(key.Value)
			if err != nil {
				return fmt.Errorf("line %d: %w", key.Line, err)
			}
			key.Value = restored
			if containsEnvReference(key.Value) {
				return fmt.Errorf("line %d: environment variable references are not supported in configuration keys (%q)", key.Line, key.Value)
			}
			if err := e.expandNode(node.Content[i+1]); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		value, err := e.placeholders.restore(node.Value)
		if err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		expanded, err := e.expandScalar(value)
		if err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		node.Value = expanded
		if expanded != value && node.Style&(yaml.TaggedStyle|yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
			// Let the decoder resolve the substituted text of a plain scalar
			// as it would have been written, so app_port: ${PORT} still
			// decodes as an int and ssl: ${SSL} as a bool.
			node.Value = strings.Trim(expanded, " \t")
			node.Tag = ""
		}
	case yaml.AliasNode:
		// The anchored node is expanded where it is defined.
	}
	return nil
}

func (e *envExpander) expandScalar(s string) (string, error) {
	var expandErr error
	out := os.Expand(s, func(key string) string {
		switch {
		case key == "$":
			// "$$" escapes a literal dollar sign.
			return "$"
		case key == "HOME":
			// $HOME in deployment configuration refers to the remote SSH
			// user's home, not the workstation running Azud.
			return "$HOME"
		case !isEnvReferenceName(key):
			return "${" + key + "}" // leave unexpanded
		}
		value := os.Getenv(key)
		if expandErr == nil {
			expandErr = validateExpandedEnvValue(key, value)
		}
		if expandErr == nil {
			e.total += len(value)
			if e.total > maxConfigFileSize {
				expandErr = fmt.Errorf("expanded environment variables exceed maximum total size (%d bytes)", maxConfigFileSize)
			}
		}
		return value
	})
	if expandErr != nil {
		return "", expandErr
	}
	return out, nil
}

// isEnvReferenceName reports whether key looks like an intentional config
// reference: uppercase letters, digits, and underscores (e.g., APP_NAME, PORT).
func isEnvReferenceName(key string) bool {
	if key == "" {
		return false
	}
	for _, c := range key {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// containsEnvReference reports whether s contains a reference that would be
// expanded from the environment.
func containsEnvReference(s string) bool {
	found := false
	os.Expand(s, func(key string) string {
		if isEnvReferenceName(key) && key != "HOME" {
			found = true
		}
		return ""
	})
	return found
}

// validateExpandedEnvValue rejects environment values that cannot be a single
// configuration value. Errors name the variable but never include the value,
// which may be a secret.
func validateExpandedEnvValue(key, value string) error {
	if len(value) > maxExpandedEnvValue {
		return fmt.Errorf("environment variable %s exceeds maximum length (%d bytes)", key, maxExpandedEnvValue)
	}
	for _, r := range value {
		if r == '\t' {
			continue
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return fmt.Errorf("environment variable %s contains a newline or control character; expanded values must be a single line", key)
		}
	}
	return nil
}
