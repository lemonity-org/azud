package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestRenderHelpUsesTechnicalCommandIndex(t *testing.T) {
	root := &cobra.Command{
		Use:   "azud",
		Short: "Deployment control",
		Long:  "Deployment control for Podman hosts.",
	}
	root.AddCommand(
		&cobra.Command{Use: "deploy", Short: "Deploy the application", Run: func(*cobra.Command, []string) {}},
		&cobra.Command{Use: "proxy", Short: "Manage the reverse proxy", Run: func(*cobra.Command, []string) {}},
		&cobra.Command{Use: "version", Short: "Print build metadata", Run: func(*cobra.Command, []string) {}},
	)
	root.PersistentFlags().Bool("verbose", false, "Enable verbose output")

	var output bytes.Buffer
	root.SetOut(&output)
	renderHelp(root, nil)

	got := output.String()
	for _, want := range []string{
		"AZUD / COMMAND INDEX\n",
		helpRule,
		"\nUSAGE\n  azud <command> [options]",
		"\nDEPLOY\n  deploy",
		"\nOPERATE\n  proxy",
		"\nREFERENCE\n  version",
		"\nGLOBAL OPTIONS\n",
		"\nHELP\n  azud <command> --help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("help output must remain safe for docs and pipes: %q", got)
	}
}

func TestRenderUsageWritesToStderr(t *testing.T) {
	command := &cobra.Command{Use: "deploy [flags]"}
	command.SetUsageFunc(renderUsage)

	var output bytes.Buffer
	command.SetErr(&output)
	if err := renderUsage(command); err != nil {
		t.Fatal(err)
	}

	if got := output.String(); !strings.HasPrefix(got, "USAGE / DEPLOY\n") {
		t.Fatalf("usage output = %q", got)
	}
}

func TestSplitEmbeddedExamples(t *testing.T) {
	description, examples := splitEmbeddedExamples("Deploy the application.\n\nExample:\n  azud deploy\n  azud deploy --version v2")
	if description != "Deploy the application." {
		t.Fatalf("description = %q", description)
	}
	if examples != "azud deploy\nazud deploy --version v2" {
		t.Fatalf("examples = %q", examples)
	}
}

func TestNarrowHelpStacksFlagsAndWrapsDescriptions(t *testing.T) {
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("configuration-file", "", "Read deployment configuration from this file before connecting to hosts")

	var output bytes.Buffer
	writeStackedFlags(&output, flags, 30)

	got := output.String()
	if !strings.Contains(got, "    --configuration-file string\n") {
		t.Fatalf("narrow flags do not preserve the option signature:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n")[1:] {
		if len(line) > 30 {
			t.Fatalf("description line exceeds narrow width: %q", line)
		}
	}
}

func TestNarrowHelpStacksCommandsAndExampleComments(t *testing.T) {
	root := &cobra.Command{Use: "azud"}
	root.AddCommand(&cobra.Command{
		Use:   "deploy",
		Short: "Deploy the configured application to every selected host",
		Run:   func(*cobra.Command, []string) {},
	})

	var output bytes.Buffer
	writeCommandIndex(&output, root, 30)
	writeExamples(&output, "azud deploy --version v2 # deploy an exact application version", 30)

	got := output.String()
	for _, want := range []string{
		"  deploy\n",
		"    Deploy the configured\n",
		"  azud deploy --version v2\n",
		"    # deploy an exact\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("narrow help does not contain %q:\n%s", want, got)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.TrimRight(line, " \t") != line {
			t.Fatalf("narrow help contains trailing whitespace: %q", line)
		}
	}
}

func TestNarrowHelpUsesHangingListIndent(t *testing.T) {
	var output bytes.Buffer
	writeHelpText(&output, "  1. Pulls the latest image on every configured server", 30, "")

	const want = "" +
		"  1. Pulls the latest image on\n" +
		"     every configured server\n"
	if got := output.String(); got != want {
		t.Fatalf("narrow list wrapping = %q, want %q", got, want)
	}
}
