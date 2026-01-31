package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/deploy"
	"github.com/adriancarayol/azud/internal/output"
)

var canaryCmd = &cobra.Command{
	Use:   "canary",
	Short: "Manage canary deployments",
	Long: `Commands for managing canary deployments.

Canary deployments allow you to gradually roll out changes by first
deploying to a small percentage of traffic, monitoring, and then
promoting or rolling back.

Example workflow:
  azud canary deploy --version abc123    # Deploy canary at 10% traffic
  azud canary status                     # Check canary health
  azud canary weight 25                  # Increase to 25%
  azud canary promote                    # Promote to 100%
  # OR
  azud canary rollback                   # Rollback if issues found`,
}

var canaryDeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Start a canary deployment",
	Long: `Deploy a new version as a canary with initial traffic percentage.

The canary will receive a small percentage of traffic initially.
Monitor its performance, then use 'canary promote' or 'canary rollback'.

Example:
  azud canary deploy --version abc123             # Deploy with default 10%
  azud canary deploy --version abc123 --weight 5  # Deploy with 5%`,
	RunE: runCanaryDeploy,
}

var canaryPromoteCmd = &cobra.Command{
	Use:   "promote",
	Short: "Promote canary to production",
	Long: `Promote the current canary deployment to full production traffic.

This will:
  1. Route 100% traffic to the canary
  2. Drain connections from the old stable
  3. Remove the old stable container
  4. Rename canary to become the new stable

Example:
  azud canary promote`,
	RunE: runCanaryPromote,
}

var canaryRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Rollback canary deployment",
	Long: `Rollback the current canary deployment and restore full traffic to stable.

This will:
  1. Remove canary from load balancer
  2. Restore 100% traffic to stable
  3. Remove the canary container

Example:
  azud canary rollback`,
	RunE: runCanaryRollback,
}

var canaryStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show canary deployment status",
	Long: `Display the current status of the canary deployment.

Shows:
  - Deployment status (running, none, etc.)
  - Stable and canary versions
  - Current traffic weight
  - Deployment duration

Example:
  azud canary status`,
	RunE: runCanaryStatus,
}

var canaryWeightCmd = &cobra.Command{
	Use:   "weight [PERCENTAGE]",
	Short: "Adjust canary traffic weight",
	Long: `Adjust the percentage of traffic routed to the canary.

Example:
  azud canary weight 25   # Set canary to 25% traffic
  azud canary weight 50   # Increase to 50%`,
	Args: cobra.ExactArgs(1),
	RunE: runCanaryWeight,
}

var (
	canaryVersion       string
	canaryInitialWeight int
	canarySkipPull      bool
	canarySkipHealth    bool
)

func init() {
	// Deploy flags
	canaryDeployCmd.Flags().StringVar(&canaryVersion, "version", "", "Version/tag to deploy as canary (required)")
	canaryDeployCmd.Flags().IntVar(&canaryInitialWeight, "weight", 0, "Initial traffic percentage (default: from config or 10)")
	canaryDeployCmd.Flags().BoolVar(&canarySkipPull, "skip-pull", false, "Skip image pull")
	canaryDeployCmd.Flags().BoolVar(&canarySkipHealth, "skip-health", false, "Skip health check")
	_ = canaryDeployCmd.MarkFlagRequired("version")

	// Add subcommands
	canaryCmd.AddCommand(canaryDeployCmd)
	canaryCmd.AddCommand(canaryPromoteCmd)
	canaryCmd.AddCommand(canaryRollbackCmd)
	canaryCmd.AddCommand(canaryStatusCmd)
	canaryCmd.AddCommand(canaryWeightCmd)

	rootCmd.AddCommand(canaryCmd)
}

// Global canary deployer instance (to maintain state across commands)
var canaryDeployer *deploy.CanaryDeployer

func getCanaryDeployer() *deploy.CanaryDeployer {
	if canaryDeployer == nil {
		sshClient := createSSHClient()
		canaryDeployer = deploy.NewCanaryDeployer(cfg, sshClient, output.DefaultLogger)
	}
	return canaryDeployer
}

func runCanaryDeploy(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if !cfg.Deploy.Canary.Enabled {
		log.Warn("Canary deployments are not enabled in configuration")
		log.Info("Add 'deploy.canary.enabled: true' to your config to enable")
	}

	deployer := getCanaryDeployer()

	opts := &deploy.CanaryDeployOptions{
		Version:         canaryVersion,
		InitialWeight:   canaryInitialWeight,
		SkipPull:        canarySkipPull,
		SkipHealthCheck: canarySkipHealth,
		Destination:     GetDestination(),
	}

	return deployer.Deploy(opts)
}

func runCanaryPromote(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)

	deployer := getCanaryDeployer()
	return deployer.Promote()
}

func runCanaryRollback(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)

	deployer := getCanaryDeployer()
	return deployer.Rollback()
}

func runCanaryStatus(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	deployer := getCanaryDeployer()
	state := deployer.Status()

	log.Header("Canary Deployment Status")

	if state.Status == deploy.CanaryStatusNone {
		log.Info("No canary deployment in progress")
		return nil
	}

	log.StatusBadge("Status:", string(state.Status))
	log.Info("Version:         %s %s %s",
		output.Lavender.Bold(state.StableVersion),
		output.Pink.Sprint("→"),
		output.Mint.Bold(state.CanaryVersion))
	log.TrafficBar(state.CurrentWeight,
		fmt.Sprintf("canary (%s)", state.CanaryVersion),
		fmt.Sprintf("stable (%s)", state.StableVersion))

	duration := time.Since(state.StartedAt).Truncate(time.Second)
	log.Info("Duration:        %s (started %s)", duration, state.StartedAt.Format("15:04:05"))
	log.Info("Hosts:           %d", len(state.Hosts))
	for _, host := range state.Hosts {
		log.Host(host, "")
	}

	return nil
}

func runCanaryWeight(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)

	weight, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid weight: %s", args[0])
	}

	if weight < 0 || weight > 100 {
		return fmt.Errorf("weight must be between 0 and 100")
	}

	deployer := getCanaryDeployer()
	return deployer.SetWeight(weight)
}
