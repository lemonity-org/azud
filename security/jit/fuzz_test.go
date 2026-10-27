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
			Restart:         "always",
			WantedBy:        "default.target",
		}
		out := quadlet.GenerateContainerFile(unit)
		for _, section := range []string{"[Unit]", "[Container]", "[Service]", "[Install]"} {
			if exactLineCount(out, section) != 1 {
				t.Fatalf("section injection for %q:\n%s", section, out)
			}
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "ExecStartPre=") {
				t.Fatalf("directive injection: %q\n%s", line, out)
			}
		}
	})
}
