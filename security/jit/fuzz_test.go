//go:build security

package jit

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/quadlet"
	"github.com/lemonity-org/azud/internal/shell"
)

func FuzzShellQuotePreservesData(f *testing.F) {
	for _, seed := range []string{
		"simple", "a b", "it's", "$(touch /tmp/x)", "`id`", "a;b", "a\nb", "\x1b[31m", "é🙂",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 256 || strings.ContainsRune(s, 0) {
			t.Skip()
		}
		cmd := exec.Command("sh", "-c", "printf '%s' "+shell.Quote(s))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("sh failed for %q: %v", s, err)
		}
		if string(out) != s {
			t.Fatalf("round trip mismatch:\nwant %q\ngot  %q\nquoted %q", s, out, shell.Quote(s))
		}
	})
}

func FuzzQuadletContainerFileLineInjection(f *testing.F) {
	for _, seed := range []string{
		"x", "x\n[Service]\nExecStartPre=/bin/sh -c id", "x\r\n[Unit]\nAfter=evil", "%h%t", "a\"b\\c",
		"x\\", "x \\ ", "x\u2028y\u0085z",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload string) {
		if len(payload) > 1024 {
			t.Skip()
		}
		unit := &quadlet.ContainerUnit{
			Description:     "d" + payload,
			After:           []string{"a" + payload},
			Requires:        []string{"r" + payload},
			Image:           "i" + payload,
			ContainerName:   "n" + payload,
			Environment:     map[string]string{"K" + payload: "V" + payload},
			EnvironmentFile: []string{"f" + payload},
			PublishPort:     []string{"p" + payload},
			Volume:          []string{"v" + payload},
			Network:         []string{"n" + payload},
			Label:           map[string]string{"L" + payload: "V" + payload},
			HealthCmd:       "h" + payload,
			HealthInterval:  "i" + payload,
			Exec:            "e" + payload,
			PodmanArgs:      []string{"--arg=" + payload},
			Restart:         "r" + payload,
			WantedBy:        "w" + payload,
		}
		out := quadlet.GenerateContainerFile(unit)
		sections := []string{"[Unit]", "[Container]", "[Service]", "[Install]"}
		for _, section := range sections {
			if exactLineCount(out, section) != 1 {
				t.Fatalf("section injection for %q:\n%s", section, out)
			}
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "ExecStartPre=") {
				t.Fatalf("directive injection: %q\n%s", line, out)
			}
		}
		// One directive line per field (15 container fields plus Restart and
		// WantedBy), and no line may continue onto the next one.
		assertQuadletLines(t, out, sections, 17)

		assertQuadletLines(t, quadlet.GenerateNetworkFile("n"+payload, true), []string{"[Network]"}, 2)
		assertQuadletLines(t, quadlet.GenerateVolumeFile("v"+payload), []string{"[Volume]"}, 1)
	})
}

// assertQuadletLines checks that out has exactly want directive lines besides
// its section headers, and that no line ends in a continuation backslash.
func assertQuadletLines(t *testing.T, out string, sections []string, want int) {
	t.Helper()
	directives := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, "\\") {
			t.Fatalf("line continuation: %q\n%s", line, out)
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		header := false
		for _, section := range sections {
			if trimmed == section {
				header = true
			}
		}
		if !header {
			directives++
		}
	}
	if directives != want {
		t.Fatalf("got %d directive lines, want %d:\n%s", directives, want, out)
	}
}
