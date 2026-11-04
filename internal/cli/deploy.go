package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
)

var deployCmd = &cobra.Command{
	Use:         "deploy",
	Annotations: scopedServer,
	Short:       "Deploy the application",
	Long: `Deploy the application to all configured servers.

This command performs a zero-downtime deployment:
  1. Pulls the latest image on all servers
  2. Starts new containers
  3. Waits for health checks to pass
  4. Registers with the proxy
  5. Drains and removes old containers

Example:
  azud deploy                    # Deploy latest version
  azud deploy --version v1.2.3   # Deploy specific version
  azud deploy --skip-build       # Deploy without building (image already in registry)`,
	RunE: runDeploy,
}

var redeployCmd = &cobra.Command{
	Use:         "redeploy",
	Annotations: scopedServer,
	Short:       "Redeploy the application",
	Long: `Quickly redeploy the application without building.

This is useful when you want to restart the application with the same image,
or when the image has been pushed separately.

With an agent section, run it on the server. It re-applies the current release
through the agent's transaction, with no registry while the image is here. With
agent.source: image it uses the configuration on disk, which is how an edit to
deploy.yml gets applied. --host and --role do not apply.

Example:
  azud redeploy`,
	RunE: runRedeploy,
}

var rollbackCmd = &cobra.Command{
	Use:         "rollback [version | digest]",
	Annotations: scopedServer,
	Short:       "Rollback to a previous version",
	Long: `Rollback the application to a previous version.

With an agent section, run it on the server. It takes a digest or a stored
release, and with neither goes to the release before this one, so a second
rollback goes further back. A stored release needs no registry while its image is
here, and is not verified again. The agent then holds what the tag names until the
tag moves. --host does not apply.

Example:
  azud rollback v1.2.2           # Rollback to specific version
  azud rollback abc123           # Rollback to specific commit
  azud rollback                  # With an agent: to the release before this one
  azud rollback sha256:...       # With an agent: to this digest`,
	Args: rollbackArgs,
	RunE: runRollback,
}

var (
	deployVersion   string
	deploySkipPull  bool
	deploySkipBuild bool
	deployHost      string
	deployRole      string
	rollbackNoPull  bool
)

func init() {
	// Deploy flags
	deployCmd.Flags().StringVar(&deployVersion, "version", "", "Version/tag to deploy (default: latest)")
	deployCmd.Flags().BoolVar(&deploySkipPull, "skip-pull", false, "Skip pulling the image")
	deployCmd.Flags().BoolVar(&deploySkipBuild, "skip-build", false, "Skip building the image")
	deployCmd.Flags().StringVar(&deployHost, "host", "", "Deploy to specific host only")
	deployCmd.Flags().StringVar(&deployRole, "role", "", "Deploy to specific role only")

	// Redeploy flags
	redeployCmd.Flags().StringVar(&deployHost, "host", "", "Redeploy on specific host only")
	redeployCmd.Flags().StringVar(&deployRole, "role", "", "Redeploy on specific role only")

	// Rollback flags
	rollbackCmd.Flags().StringVar(&deployHost, "host", "", "Rollback on specific host only")
	rollbackCmd.Flags().BoolVar(&rollbackNoPull, "no-pull", false, "With an agent section, fail if the image is not on this server")

	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(redeployCmd)
	rootCmd.AddCommand(rollbackCmd)
}

func runDeploy(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	log.Header("Deploy / %s", cfg.Service)

	// An explicit version refers to an already tagged image. Building the
	// current checkout under a different generated tag would be misleading.
	if deployVersion != "" && !deploySkipBuild {
		log.Info("Explicit version %s selected; skipping local build", deployVersion)
	} else if !deploySkipBuild {
		log.Info("Building image...")
		if err := runBuild(cmd, args); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
	}

	// Run pre-connect hook
	hookCtx := newHookContext()
	hookCtx.Version = deployVersion
	hookCtx.Role = deployRole
	if err := newHookRunner().Run(cmd.Context(), "pre-connect", hookCtx); err != nil {
		return fmt.Errorf("pre-connect hook failed: %w", err)
	}

	// Create deployer
	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	// Build deploy options
	opts := &deploy.DeployOptions{
		Version:     deployVersion,
		SkipPull:    deploySkipPull,
		Destination: GetDestination(),
	}

	if deployHost != "" {
		opts.Hosts = []string{deployHost}
	}

	if deployRole != "" {
		opts.Roles = []string{deployRole}
	}

	// Run deployment
	return deployer.Deploy(cmd.Context(), opts)
}

func runRedeploy(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if cfg.Agent.Configured() {
		return runAgentRedeploy(cmd, log)
	}

	log.Header("Redeploy / %s", cfg.Service)

	// Run pre-connect hook
	hookCtx := newHookContext()
	hookCtx.Role = deployRole
	if err := newHookRunner().Run(cmd.Context(), "pre-connect", hookCtx); err != nil {
		return fmt.Errorf("pre-connect hook failed: %w", err)
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	opts := &deploy.DeployOptions{
		SkipPull:    true, // Don't pull, use existing image
		Destination: GetDestination(),
	}

	if deployHost != "" {
		opts.Hosts = []string{deployHost}
	}

	if deployRole != "" {
		opts.Roles = []string{deployRole}
	}

	return deployer.Redeploy(cmd.Context(), opts)
}

// rollbackArgs takes one version, and with an agent section also none. The configuration
// is read here because arguments are checked before it loads, so that with no agent
// section this fails as it always has, before a failing secrets_command could.
func rollbackArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 && configuresAgent() {
		return nil
	}
	return cobra.ExactArgs(1)(cmd, args)
}

// configuresAgent reports whether the configuration the command points at has an agent
// section, after its destination file is merged.
func configuresAgent() bool {
	path := GetConfigPath()
	if path == "" {
		return false
	}
	loaded, err := config.NewLoader(path, destination).WithoutSecrets().Load()
	return err == nil && loaded.Agent.Configured()
}

func runRollback(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if cfg.Agent.Configured() {
		return runAgentRollback(cmd, log, args)
	}
	if rollbackNoPull {
		return fmt.Errorf("--no-pull applies to a configuration with an agent section")
	}

	version := args[0]
	log.Header("Rolling back to %s", version)

	// Run pre-connect hook
	hookCtx := newHookContext()
	hookCtx.Version = version
	if deployHost != "" {
		hookCtx.Hosts = deployHost
	}
	if err := newHookRunner().Run(cmd.Context(), "pre-connect", hookCtx); err != nil {
		return fmt.Errorf("pre-connect hook failed: %w", err)
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	var hosts []string
	if deployHost != "" {
		hosts = []string{deployHost}
	}

	return deployer.Rollback(cmd.Context(), version, GetDestination(), hosts)
}
