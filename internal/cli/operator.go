package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/state"
)

var syncCmd = &cobra.Command{
	Use:         "sync",
	Annotations: scopedServer,
	Short:       "Check the tag now, or move to a digest",
	Long: `Run one check now, the same check as a tick of the agent, under the same locks.

--retry tries a failed release again. --digest moves to that digest or stored
release instead of what the tag names, and the agent then holds what the tag names
until the tag moves. A stored release is not verified again, since it was verified
when it was stored. An unknown digest is verified unless --no-verify.

Run it on the server.

Example:
  azud sync                               # Check the tag now
  azud sync --retry                       # Try a failed release again
  azud sync --digest sha256:...           # Move to this release`,
	Args: cobra.NoArgs,
	RunE: runSync,
}

var (
	syncRetry    bool
	syncDigest   string
	syncNoVerify bool
)

func init() {
	syncCmd.Flags().BoolVar(&syncRetry, "retry", false, "Try a failed release again")
	syncCmd.Flags().StringVar(&syncDigest, "digest", "", "Move to this digest or stored release instead of what the tag names")
	syncCmd.Flags().BoolVar(&syncNoVerify, "no-verify", false, "With --digest, do not verify a digest this server has not stored")
	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	switch {
	case !cfg.Agent.Configured():
		return fmt.Errorf("azud sync does what the agent does, and this configuration has no agent section")
	case syncNoVerify && syncDigest == "":
		return fmt.Errorf("--no-verify requires --digest")
	case syncRetry && syncDigest != "":
		return fmt.Errorf("--retry tries what the tag names, and --digest moves away from it, so use one of them")
	}
	if err := checkAgentCanRun(cfg); err != nil {
		return err
	}

	log.Header("Sync / %s", cfg.Service)
	a, closeAgent, err := newAgent(cmd)
	if err != nil {
		return err
	}
	defer closeAgent()

	if syncDigest != "" {
		outcome, digest, err := a.Move(cmd.Context(), syncDigest, agent.MoveOptions{NoVerify: syncNoVerify})
		return reportMove(log, a, outcome, digest, err)
	}

	outcome, digest, err := a.Reconcile(cmd.Context(), syncRetry)
	switch outcome {
	case agent.Unchanged:
		log.Success("%s is deployed (%s)", a.Ref, digest)
	case agent.Adopted:
		log.Success("%s is already running; recorded it as deployed", a.Image(digest))
	case agent.Deployed:
		log.Success("Deployed %s", a.Image(digest))
	case agent.SkippedFailed:
		log.Warn("%s names %s, which is held until the tag moves. To try it again, run `azud sync --retry`", a.Ref, digest)
	case agent.Recovered:
		log.Warn("Undid the failed deploy of %s. To try it again, run `azud sync --retry`", a.Image(digest))
	case agent.Waiting:
		return fmt.Errorf("undoing the failed deploy of %s failed and is waiting to retry; see azud status", a.Image(digest))
	}
	return err
}

func runAgentRollback(cmd *cobra.Command, log *output.Logger, args []string) error {
	if deployHost != "" {
		return fmt.Errorf("--host applies to a pushed rollback. With an agent section, run azud rollback on the server")
	}
	if err := checkAgentCanRun(cfg); err != nil {
		return err
	}

	log.Header("Rollback / %s", cfg.Service)
	a, closeAgent, err := newAgent(cmd)
	if err != nil {
		return err
	}
	defer closeAgent()

	opts := agent.MoveOptions{NoPull: rollbackNoPull, Back: true}
	if len(args) == 0 {
		outcome, digest, err := a.Rollback(cmd.Context(), opts)
		return reportMove(log, a, outcome, digest, err)
	}
	outcome, digest, err := a.Move(cmd.Context(), args[0], opts)
	return reportMove(log, a, outcome, digest, err)
}

func runAgentRedeploy(cmd *cobra.Command, log *output.Logger) error {
	if deployHost != "" || deployRole != "" {
		return fmt.Errorf("--host and --role apply to a pushed redeploy. With an agent section, run azud redeploy on the server")
	}
	if err := checkAgentCanRun(cfg); err != nil {
		return err
	}

	log.Header("Redeploy / %s", cfg.Service)
	a, closeAgent, err := newAgent(cmd)
	if err != nil {
		return err
	}
	defer closeAgent()

	outcome, digest, err := a.Redeploy(cmd.Context())
	return reportMove(log, a, outcome, digest, err)
}

// reportMove says what an operator's move did, and what the agent now holds.
func reportMove(log *output.Logger, a *agent.Agent, outcome agent.Outcome, digest string, err error) error {
	if err != nil {
		return err
	}
	if outcome == agent.Deployed {
		log.Success("Deployed %s", a.Image(digest))
	}
	if st, err := a.State.Load(); err == nil && st.FailedDigest != "" {
		log.Info("The agent holds %s until %s moves", st.FailedDigest, a.Ref)
	}
	return nil
}

// recordIntent changes what an operator stopped, before the command acts on it. With no
// agent section there is nothing to tell, so it does nothing.
func recordIntent(edit func(*agent.Intent)) error {
	if !cfg.Agent.Configured() {
		return nil
	}
	store, err := localStore()
	if err != nil {
		return err
	}
	return store.UpdateIntent(edit)
}

// recordStopped records a stop. These commands still reach their server over SSH, and the
// record stays on this machine, so it says when the agent is unlikely to be here.
func recordStopped(edit func(*agent.Intent)) error {
	if !cfg.Agent.Configured() {
		return nil
	}
	store, err := localStore()
	if err != nil {
		return err
	}
	if st, err := store.Load(); err == nil && st.DeployedDigest == "" {
		output.DefaultLogger.WarnStderr("Nothing is deployed according to this machine's agent state. An agent on another server does not see what this records, so it would start this again when it restarts: run this on that server")
	}
	return store.UpdateIntent(edit)
}

func localStore() (*agent.Store, error) {
	stateDir, err := state.LocalDir()
	if err != nil {
		return nil, err
	}
	return agent.NewStore(stateDir, cfg.Service), nil
}

// recordRolesStopped records that the roles this command selected are stopped.
func recordRolesStopped() error {
	return recordStopped(func(intent *agent.Intent) {
		intent.Roles = agent.WithStopped(intent.Roles, appIntentRoles()...)
	})
}

// recordRolesStarted records that the roles this command selected are running.
func recordRolesStarted() error {
	return recordIntent(func(intent *agent.Intent) {
		intent.Roles = agent.WithStarted(intent.Roles, appIntentRoles()...)
	})
}

// recordAccessoryStopped records that an accessory is stopped.
func recordAccessoryStopped(name string) error {
	return recordStopped(func(intent *agent.Intent) {
		intent.Accessories = agent.WithStopped(intent.Accessories, name)
	})
}

// recordAccessoryStarted records that an accessory is no longer stopped.
func recordAccessoryStarted(name string) error {
	return recordIntent(func(intent *agent.Intent) {
		intent.Accessories = agent.WithStarted(intent.Accessories, name)
	})
}

// appIntentRoles returns the roles an app command acts on: all of them if it names none,
// and none if it names a role this release does not have, which the command then refuses.
func appIntentRoles() []string {
	switch {
	case appRole == "":
		return cfg.GetRoles()
	case cfg.HasRole(appRole):
		return []string{appRole}
	}
	return nil
}
