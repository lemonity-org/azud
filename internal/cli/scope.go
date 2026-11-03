package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/state"
)

// Every command says which configuration it wants, and the root pre-run produces it,
// so handlers read the global cfg and a new command gets the right one by declaring
// nothing. A command takes the scope of its nearest annotated parent. With none it
// is scopeEffective, the strictest and most common.
const (
	scopeAnnotation = "azud.scope"

	// scopeEffective holds what is running. A configuration with no agent section
	// has no agent state to read, so it holds the file as written.
	scopeEffective = "effective"
	// scopeServer holds the server's configuration as written.
	scopeServer = "server"
	// scopeRepo holds the repository's configuration, and reads no agent state.
	scopeRepo = "repo"
	// scopeEither holds the configuration the command was pointed at, as written.
	scopeEither = "either"
	// scopeNone loads nothing.
	scopeNone = "none"
)

// serverWithFlagAnnotation names a flag that puts a command in scopeServer, for an
// effective command that does not read what is running when the flag is set.
const serverWithFlagAnnotation = "azud.server-with-flag"

// What an effective command needs of the release. Commands that declare nothing are
// releaseWhole, the strictest.
const (
	releaseAnnotation = "azud.release"

	// releaseWhole writes from cfg: it needs a release, and refuses while a journal
	// is open.
	releaseWhole = "whole"
	// releaseRunning runs what the release specifies and writes nothing that stays:
	// it needs a release, and runs while a journal is open.
	releaseRunning = "running"
	// releaseSpec writes from cfg, and needs a release only for the spec a bundle
	// carries. With images that spec is in this server's configuration.
	releaseSpec = "spec"
	// releaseNone reads, or acts on containers by name, so it runs at any time.
	releaseNone = "none"
)

// writesWithFlagAnnotation names a bool flag without which a command is releaseNone
// and reads no secrets, for a command that only reads unless it is set.
const writesWithFlagAnnotation = "azud.writes-with-flag"

// anyState annotates an effective command that reads, or acts on containers by name.
var anyState = map[string]string{releaseAnnotation: releaseNone}

// Annotations a command's scope comes from, for commands that declare one.
var (
	scopedNone   = map[string]string{scopeAnnotation: scopeNone}
	scopedServer = map[string]string{scopeAnnotation: scopeServer}
	scopedRepo   = map[string]string{scopeAnnotation: scopeRepo}
	scopedEither = map[string]string{scopeAnnotation: scopeEither}
)

// commandScope returns cmd's scope. Commands Cobra generates load nothing: shell
// completion must never execute secrets_command from the current directory's
// configuration.
func commandScope(cmd *cobra.Command) string {
	if flag := cmd.Annotations[serverWithFlagAnnotation]; flag != "" && cmd.Flags().Changed(flag) {
		return scopeServer
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return scopeNone
		}
		if scope := c.Annotations[scopeAnnotation]; scope != "" {
			return scope
		}
	}
	return scopeEffective
}

// commandRelease returns what an effective command needs of the release.
func commandRelease(cmd *cobra.Command) string {
	if commandOnlyReads(cmd) {
		return releaseNone
	}
	if need := cmd.Annotations[releaseAnnotation]; need != "" {
		return need
	}
	return releaseWhole
}

// commandOnlyReads reports whether a flag that makes cmd write is unset.
func commandOnlyReads(cmd *cobra.Command) bool {
	flag := cmd.Annotations[writesWithFlagAnnotation]
	if flag == "" {
		return false
	}
	set, err := cmd.Flags().GetBool(flag)
	return err == nil && !set
}

// effectiveConfig returns what an effective command reads on a server with an agent.
// A command that needs a release fails if what is running cannot be read. One that
// does not must keep working in an incident, so it warns and uses the file as written.
func effectiveConfig(cmd *cobra.Command, server *config.Config) (*config.Config, error) {
	need := commandRelease(cmd)
	effective, err := readEffective(server)
	if err != nil {
		if need != releaseNone {
			return nil, fmt.Errorf("cannot read what is running: %w", err)
		}
		output.DefaultLogger.WarnStderr("Cannot read what is running, so this uses the configuration as written: %v", err)
		return server, nil
	}
	if pinned := deployCommandConfig(server, effective); pinned != nil {
		return pinned, nil
	}
	if err := checkRelease(cmd, need, server, effective); err != nil {
		return nil, err
	}
	if need != releaseNone {
		warnConfigEdited(effective)
	}
	return effective.Config, nil
}

// warnConfigEdited says when an image release's configuration is not the file on
// disk, because an edit there looks applied and is not: the command reads the release.
func warnConfigEdited(effective *agent.Effective) {
	rec := effective.Record
	if rec == nil || rec.Source != agent.SourceImage {
		return
	}
	files, err := agent.ReadConfigFiles(GetConfigPath(), GetDestination())
	if err != nil || rec.HasFiles(files) {
		return
	}
	output.DefaultLogger.WarnStderr("The configuration on disk changed after release %s was applied. This uses the release's; an edit applies with the next release", effective.Config.Image)
}

func readEffective(server *config.Config) (*agent.Effective, error) {
	stateDir, err := state.LocalDir()
	if err != nil {
		return nil, err
	}
	return agent.LoadEffective(server, stateDir)
}

// deployCommandConfig returns the configuration for a command that agent.deploy_command
// runs for the release it is deploying, or nil for any other command. The journal open
// around the script is its own, and the release it deploys is the one it means. A
// deploy_command deploys images, so its configuration is on disk.
func deployCommandConfig(server *config.Config, effective *agent.Effective) *config.Config {
	digest := os.Getenv("AZUD_AGENT_DIGEST")
	if server.Agent.DeployCommand == "" || digest == "" || effective.Journal == nil || effective.Journal.Digest != digest {
		return nil
	}
	pinned := *server
	pinned.Image = config.RepositoryOf(server.Image) + "@" + digest
	return &pinned
}

// checkRelease refuses a command that cannot run in the state the server is in, and
// names what to do. A command that needs nothing says when nothing is deployed, on
// stderr, because its stdout may be a container's logs.
func checkRelease(cmd *cobra.Command, need string, server *config.Config, effective *agent.Effective) error {
	if effective.Settling && (need == releaseWhole || need == releaseSpec) {
		return fmt.Errorf("a deploy of %s has not finished, so this server may be running parts of two releases and `%s` would write from one of them. The agent finishes or undoes it on its next check. To do that now, run `azud agent run --once`, then run this again (`azud agent status` shows where it stands)",
			effective.Journal.Digest, cmd.CommandPath())
	}
	if effective.Deployed {
		if effective.Settling {
			output.DefaultLogger.WarnStderr("A deploy of %s has not finished; this uses the last whole release, %s", effective.Journal.Digest, effective.Config.Image)
		}
		return nil
	}
	switch {
	case need == releaseNone:
		output.DefaultLogger.WarnStderr("Nothing is deployed on this server yet")
		return nil
	case need == releaseSpec && !server.Agent.Bundles():
		return nil
	case need == releaseSpec:
		return fmt.Errorf("nothing is deployed on this server yet, so `%s` has no release to read the accessory from. To boot it before the first deploy, run `azud accessory boot <name> --release <tag>`", cmd.CommandPath())
	}
	return fmt.Errorf("nothing is deployed on this server yet, so `%s` has no release to run. Move the tag `%s` to a signed release: the agent deploys it on its next check, or run `azud agent run --once` to check now", cmd.CommandPath(), server.Agent.Track)
}
