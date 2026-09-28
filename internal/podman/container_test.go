package podman

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseHostPort(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{
			name:     "ipv4",
			input:    "127.0.0.1:8080",
			wantHost: "127.0.0.1",
			wantPort: 8080,
		},
		{
			name:     "ipv6",
			input:    "[::1]:8443",
			wantHost: "::1",
			wantPort: 8443,
		},
		{
			name:     "wildcard",
			input:    "0.0.0.0:49123",
			wantHost: "0.0.0.0",
			wantPort: 49123,
		},
		{
			name:    "missing separator",
			input:   "8080",
			wantErr: true,
		},
		{
			name:    "invalid port",
			input:   "127.0.0.1:not-a-port",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := parseHostPort(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q", tt.input)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.wantHost {
				t.Fatalf("unexpected host: want %q got %q", tt.wantHost, host)
			}
			if port != tt.wantPort {
				t.Fatalf("unexpected port: want %d got %d", tt.wantPort, port)
			}
		})
	}
}

// A bare `podman inspect NAME` also matches images, volumes and networks, so
// an image called like the service made a first deploy believe an old
// container existed. Lookups by name must say what they inspect
// (`container inspect`, `image inspect`, ...). This scans every non-test Go
// file in the module, whatever the host variable is called, and shell
// command strings that run podman inspect directly.
func TestInspectsAreTyped(t *testing.T) {
	root := filepath.Join("..", "..")
	var scanned int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if untypedInspect(line) {
				t.Errorf("%s:%d: use `container inspect` (or `image inspect`), not a bare inspect: %s",
					path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning sources: %v", err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d Go files from %s; the walk is not covering the module", scanned, root)
	}
}

var (
	// Execute(host, "inspect", ...), ExecuteAll(hosts, "inspect", ...), ...:
	// "inspect" as the first podman argument, whatever the host variable is.
	untypedInspectCall = regexp.MustCompile(`\.Execute\w*\(\s*[^,()]+,\s*"inspect"`)
	// A shell command string running podman inspect directly.
	untypedInspectShell = regexp.MustCompile(`podman\s+inspect\b`)
)

func untypedInspect(line string) bool {
	return untypedInspectCall.MatchString(line) || untypedInspectShell.MatchString(line)
}

func TestUntypedInspectPatterns(t *testing.T) {
	for _, line := range []string{
		`result, err := m.client.Execute(host, "inspect", container)`,
		`r, err := c.Execute(target, "inspect", name, "--format", "{{.Id}}")`,
		`results := m.client.ExecuteAll(hosts, "inspect", container)`,
		`cmd := fmt.Sprintf("podman inspect --format '{{.Id}}' %s", name)`,
	} {
		if !untypedInspect(line) {
			t.Errorf("not flagged: %s", line)
		}
	}
	for _, line := range []string{
		`result, err := m.client.Execute(host, "container", "inspect", container)`,
		`result, err := m.client.Execute(host, "image", "inspect", image)`,
		`cmd := "podman container inspect --format '{{.Id}}' app"`,
		`return fmt.Errorf("inspect %s before stop: %w", name, err)`,
	} {
		if untypedInspect(line) {
			t.Errorf("flagged a typed inspect: %s", line)
		}
	}
}
