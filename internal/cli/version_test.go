package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/pkg/version"
)

func TestVersionShortIsStableAndUnstyled(t *testing.T) {
	originalVersion := version.Version
	t.Cleanup(func() {
		version.Version = originalVersion
	})

	version.Version = "v9.8.7"
	var output bytes.Buffer
	command := newVersionCommand()
	command.SetOut(&output)
	command.SetArgs([]string{"--short"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}

	if got, want := output.String(), "v9.8.7\n"; got != want {
		t.Fatalf("version --short = %q, want %q", got, want)
	}
	if strings.Contains(output.String(), "\x1b") {
		t.Fatalf("version --short contains ANSI: %q", output.String())
	}
}

func TestVersionHumanViewKeepsCompatibleFirstLine(t *testing.T) {
	originalVersion := version.Version
	originalCommit := version.Commit
	originalBuildDate := version.BuildDate
	t.Cleanup(func() {
		version.Version = originalVersion
		version.Commit = originalCommit
		version.BuildDate = originalBuildDate
	})

	version.Version = "v9.8.7"
	version.Commit = "abc1234"
	version.BuildDate = "2026-07-23T12:00:00Z"
	var output bytes.Buffer
	command := newVersionCommand()
	command.SetOut(&output)
	command.SetArgs([]string{})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if lines[0] != "Azud v9.8.7" {
		t.Fatalf("first version line = %q", lines[0])
	}
	for _, label := range []string{"COMMIT", "BUILT", "GO", "TARGET"} {
		if !strings.Contains(output.String(), label) {
			t.Fatalf("version output lacks %s: %q", label, output.String())
		}
	}
}

func TestVersionShortDoesNotLeakAcrossExecutions(t *testing.T) {
	originalVersion := version.Version
	t.Cleanup(func() {
		version.Version = originalVersion
	})
	version.Version = "v9.8.7"

	command := newVersionCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--short"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}

	output.Reset()
	command.SetArgs([]string{})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.HasPrefix(got, "Azud v9.8.7\n") {
		t.Fatalf("--short leaked into the next execution: %q", got)
	}
}
