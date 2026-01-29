package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/deploy"
	"github.com/adriancarayol/azud/internal/output"
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy the application",
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
  azud deploy --skip-push        # Deploy without pushing (image already in registry)`,
	RunE: runDeploy,
}

var redeployCmd = &cobra.Command{
	Use:   "redeploy",
	Short: "Redeploy the application",
	Long: `Quickly redeploy the application without building.

This is useful when you want to restart the application with the same image,
or when the image has been pushed separately.

Example:
  azud redeploy`,
	RunE: runRedeploy,
}

var rollbackCmd = &cobra.Command{
	Use:   "rollback [version]",
	Short: "Rollback to a previous version",
	Long: `Rollback the application to a previous version.

Example:
  azud rollback v1.2.2           # Rollback to specific version
  azud rollback abc123           # Rollback to specific commit`,
	Args: cobra.ExactArgs(1),
	RunE: runRollback,
}

var (
	deployVersion   string
	deploySkipPull  bool
	deploySkipBuild bool
	deployHost      string
	deployRole      string
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

	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(redeployCmd)
	rootCmd.AddCommand(rollbackCmd)
}

func runDeploy(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	log.Header("Azud Deploy")

	// Build if not skipped
	if !deploySkipBuild {
		log.Info("Building image...")
		if err := runBuild(cmd, args); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
	}

	// Create deployer
	sshClient := createSSHClient()
	defer sshClient.Close()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	// Build deploy options
	opts := &deploy.DeployOptions{
		Version:  deployVersion,
		SkipPull: deploySkipPull,
	}

	if deployHost != "" {
		opts.Hosts = []string{deployHost}
	}

	if deployRole != "" {
		opts.Roles = []string{deployRole}
	}

	// Run deployment
	return deployer.Deploy(opts)
}

func runRedeploy(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	log.Header("Azud Redeploy")

	sshClient := createSSHClient()
	defer sshClient.Close()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	opts := &deploy.DeployOptions{
		SkipPull: true, // Don't pull, use existing image
	}

	if deployHost != "" {
		opts.Hosts = []string{deployHost}
	}

	if deployRole != "" {
		opts.Roles = []string{deployRole}
	}

	return deployer.Redeploy(opts)
}

func runRollback(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	version := args[0]
	log.Header("Rolling back to %s", version)

	sshClient := createSSHClient()
	defer sshClient.Close()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	return deployer.Rollback(version)
}
