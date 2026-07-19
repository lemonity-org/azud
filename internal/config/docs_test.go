package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fencedYAMLAfter(t *testing.T, document, marker string) string {
	t.Helper()
	markerIndex := strings.Index(document, marker)
	if markerIndex < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	remainder := document[markerIndex:]
	start := strings.Index(remainder, "```yaml\n")
	if start < 0 {
		t.Fatalf("YAML fence after %q not found", marker)
	}
	remainder = remainder[start+len("```yaml\n"):]
	end := strings.Index(remainder, "```")
	if end < 0 {
		t.Fatalf("closing fence after %q not found", marker)
	}
	lines := strings.Split(remainder[:end], "\n")
	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		spaces := len(line) - len(strings.TrimLeft(line, " "))
		if indent < 0 || spaces < indent {
			indent = spaces
		}
	}
	if indent > 0 {
		for i, line := range lines {
			if len(line) >= indent {
				lines[i] = line[indent:]
			}
		}
	}
	return strings.Join(lines, "\n")
}

func TestQuickStartConfigurationsLoadAsWritten(t *testing.T) {
	tests := []struct {
		file   string
		marker string
	}{
		{file: filepath.Join("..", "..", "README.md"), marker: "2. **Configure**"},
		{file: filepath.Join("..", "..", "docs", "GETTING_STARTED.md"), marker: "## 3) Configure a minimal deploy"},
	}
	for _, tt := range tests {
		t.Run(filepath.Base(tt.file), func(t *testing.T) {
			document, err := os.ReadFile(tt.file)
			if err != nil {
				t.Fatal(err)
			}
			yamlText := fencedYAMLAfter(t, string(document), tt.marker)
			configPath := filepath.Join(t.TempDir(), "deploy.yml")
			if err := os.WriteFile(configPath, []byte(yamlText), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewLoader(configPath, "").Load(); err != nil {
				t.Fatalf("quick-start config does not load as written: %v\n%s", err, yamlText)
			}
		})
	}
}
