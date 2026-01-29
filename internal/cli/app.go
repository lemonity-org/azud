package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/deploy"
	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/podman"
)

var appCmd = &cobra.Command{
	Use:   "app",
	Short: "Manage the application",
	Long:  `Commands for managing the deployed application.`,
}

var appLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View application logs",
	Long: `View logs from the application containers.

Example:
  azud app logs              # View recent logs
  azud app logs -f           # Follow logs
  azud app logs --tail 100   # Last 100 lines
  azud app logs --host x.x.x # Logs from specific host`,
	RunE: runAppLogs,
}

var appExecCmd = &cobra.Command{
	Use:   "exec [flags] -- command",
	Short: "Execute command in application container",
	Long: `Execute a command inside the application container.

Example:
  azud app exec -- ls -la
  azud app exec -it -- /bin/sh
  azud app exec -- bin/rails console`,
	RunE: runAppExec,
}

var appStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the application",
	Long: `Start the application containers on all servers.

Example:
  azud app start`,
	RunE: runAppStart,
}

var appStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the application",
	Long: `Stop the application containers on all servers.

Example:
  azud app stop`,
	RunE: runAppStop,
}

var appRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the application",
	Long: `Restart the application containers on all servers.

Example:
  azud app restart`,
	RunE: runAppRestart,
}

var appDetailsCmd = &cobra.Command{
	Use:   "details",
	Short: "Show application details",
	Long: `Show detailed information about the application containers.

Example:
  azud app details`,
	RunE: runAppDetails,
}

var (
	appHost        string
	appRole        string
	appFollow      bool
	appTail        string
	appInteractive bool
	appTTY         bool
)

func init() {
	// Logs flags
	appLogsCmd.Flags().StringVar(&appHost, "host", "", "Specific host")
	appLogsCmd.Flags().StringVar(&appRole, "role", "", "Specific role")
	appLogsCmd.Flags().BoolVarP(&appFollow, "follow", "f", false, "Follow log output")
	appLogsCmd.Flags().StringVar(&appTail, "tail", "100", "Number of lines")

	// Exec flags
	appExecCmd.Flags().StringVar(&appHost, "host", "", "Specific host")
	appExecCmd.Flags().BoolVarP(&appInteractive, "interactive", "i", false, "Keep STDIN open")
	appExecCmd.Flags().BoolVarP(&appTTY, "tty", "t", false, "Allocate a pseudo-TTY")

	// Start/Stop/Restart flags
	appStartCmd.Flags().StringVar(&appHost, "host", "", "Specific host")
	appStopCmd.Flags().StringVar(&appHost, "host", "", "Specific host")
	appRestartCmd.Flags().StringVar(&appHost, "host", "", "Specific host")

	// Details flags
	appDetailsCmd.Flags().StringVar(&appHost, "host", "", "Specific host")

	// Add subcommands
	appCmd.AddCommand(appLogsCmd)
	appCmd.AddCommand(appExecCmd)
	appCmd.AddCommand(appStartCmd)
	appCmd.AddCommand(appStopCmd)
	appCmd.AddCommand(appRestartCmd)
	appCmd.AddCommand(appDetailsCmd)

	rootCmd.AddCommand(appCmd)
}

func runAppLogs(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	// Get target host
	host := appHost
	if host == "" {
		hosts := getAppHosts()
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts configured")
		}
		host = hosts[0]
		if len(hosts) > 1 {
			log.Warn("Multiple hosts, showing logs from %s", host)
		}
	}

	sshClient := createSSHClient()
	defer sshClient.Close()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	logsConfig := &podman.LogsConfig{
		Container: cfg.Service,
		Follow:    appFollow,
		Tail:      appTail,
	}

	result, err := containerManager.Logs(host, logsConfig)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	fmt.Print(result.Stdout)
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	return nil
}

func runAppExec(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)

	if len(args) == 0 {
		return fmt.Errorf("no command specified")
	}

	// Get target host
	host := appHost
	if host == "" {
		hosts := getAppHosts()
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts configured")
		}
		host = hosts[0]
	}

	sshClient := createSSHClient()
	defer sshClient.Close()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	execConfig := &podman.ExecConfig{
		Container:   cfg.Service,
		Command:     args,
		Interactive: appInteractive,
		TTY:         appTTY,
	}

	result, err := containerManager.Exec(host, execConfig)
	if err != nil {
		return fmt.Errorf("exec failed: %w", err)
	}

	fmt.Print(result.Stdout)
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("command exited with code %d", result.ExitCode)
	}

	return nil
}

func runAppStart(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	sshClient := createSSHClient()
	defer sshClient.Close()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	hosts := getAppHosts()
	return deployer.Start(hosts)
}

func runAppStop(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	sshClient := createSSHClient()
	defer sshClient.Close()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	hosts := getAppHosts()
	return deployer.Stop(hosts)
}

func runAppRestart(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	sshClient := createSSHClient()
	defer sshClient.Close()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	hosts := getAppHosts()
	return deployer.Restart(hosts)
}

func runAppDetails(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := getAppHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	sshClient := createSSHClient()
	defer sshClient.Close()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	log.Header("Application Details: %s", cfg.Service)

	var rows [][]string
	for _, host := range hosts {
		running, err := containerManager.IsRunning(host, cfg.Service)
		if err != nil {
			rows = append(rows, []string{host, "error", err.Error()})
			continue
		}

		status := "stopped"
		if running {
			status = "running"

			// Get stats
			stats, err := containerManager.Stats(host, cfg.Service)
			if err == nil {
				rows = append(rows, []string{host, status, stats})
				continue
			}
		}

		rows = append(rows, []string{host, status, "-"})
	}

	log.Table([]string{"Host", "Status", "Stats"}, rows)

	// Show image info
	log.Println("")
	log.Println("Image: %s", cfg.Image)
	log.Println("Proxy: %s (port %d)", cfg.Proxy.Host, cfg.Proxy.AppPort)

	return nil
}

func getAppHosts() []string {
	if appHost != "" {
		return []string{appHost}
	}
	if appRole != "" {
		return cfg.GetRoleHosts(appRole)
	}
	return cfg.GetAllHosts()
}

var accessoryCmd = &cobra.Command{
	Use:   "accessory",
	Short: "Manage accessories",
	Long:  `Commands for managing accessories (databases, caches, etc.).`,
}

var accessoryBootCmd = &cobra.Command{
	Use:   "boot [name]",
	Short: "Start an accessory",
	Long: `Start an accessory container.

Example:
  azud accessory boot mysql
  azud accessory boot redis`,
	Args: cobra.ExactArgs(1),
	RunE: runAccessoryBoot,
}

var accessoryStopCmd = &cobra.Command{
	Use:   "stop [name]",
	Short: "Stop an accessory",
	Args:  cobra.ExactArgs(1),
	RunE:  runAccessoryStop,
}

var accessoryLogsCmd = &cobra.Command{
	Use:   "logs [name]",
	Short: "View accessory logs",
	Args:  cobra.ExactArgs(1),
	RunE:  runAccessoryLogs,
}

var accessoryExecCmd = &cobra.Command{
	Use:   "exec [name] -- command",
	Short: "Execute command in accessory",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runAccessoryExec,
}

func init() {
	accessoryLogsCmd.Flags().BoolVarP(&appFollow, "follow", "f", false, "Follow logs")
	accessoryLogsCmd.Flags().StringVar(&appTail, "tail", "100", "Number of lines")

	accessoryCmd.AddCommand(accessoryBootCmd)
	accessoryCmd.AddCommand(accessoryStopCmd)
	accessoryCmd.AddCommand(accessoryLogsCmd)
	accessoryCmd.AddCommand(accessoryExecCmd)

	rootCmd.AddCommand(accessoryCmd)
}

func runAccessoryBoot(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	name := args[0]

	accessory, ok := cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %s not found", name)
	}

	host := accessory.PrimaryHost()
	if host == "" {
		return fmt.Errorf("no host configured for accessory %s", name)
	}

	sshClient := createSSHClient()
	defer sshClient.Close()

	log.Info("Starting accessory %s on %s...", name, host)

	return deployAccessories(sshClient, log)
}

func runAccessoryStop(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	name := args[0]

	accessory, ok := cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %s not found", name)
	}

	host := accessory.PrimaryHost()

	sshClient := createSSHClient()
	defer sshClient.Close()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	containerName := fmt.Sprintf("%s-%s", cfg.Service, name)

	log.Info("Stopping accessory %s...", name)

	if err := containerManager.Stop(host, containerName, 30); err != nil {
		return fmt.Errorf("failed to stop accessory: %w", err)
	}

	log.Success("Accessory %s stopped", name)
	return nil
}

func runAccessoryLogs(cmd *cobra.Command, args []string) error {
	name := args[0]

	accessory, ok := cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %s not found", name)
	}

	host := accessory.PrimaryHost()

	sshClient := createSSHClient()
	defer sshClient.Close()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	containerName := fmt.Sprintf("%s-%s", cfg.Service, name)

	logsConfig := &podman.LogsConfig{
		Container: containerName,
		Follow:    appFollow,
		Tail:      appTail,
	}

	result, err := containerManager.Logs(host, logsConfig)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	fmt.Print(result.Stdout)
	return nil
}

func runAccessoryExec(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Find -- separator
	cmdArgs := args[1:]
	for i, arg := range cmdArgs {
		if arg == "--" {
			cmdArgs = cmdArgs[i+1:]
			break
		}
	}

	if len(cmdArgs) == 0 {
		return fmt.Errorf("no command specified")
	}

	accessory, ok := cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %s not found", name)
	}

	host := accessory.PrimaryHost()

	sshClient := createSSHClient()
	defer sshClient.Close()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	containerName := fmt.Sprintf("%s-%s", cfg.Service, name)

	execConfig := &podman.ExecConfig{
		Container:   containerName,
		Command:     cmdArgs,
		Interactive: appInteractive,
		TTY:         appTTY,
	}

	result, err := containerManager.Exec(host, execConfig)
	if err != nil {
		return fmt.Errorf("exec failed: %w", err)
	}

	fmt.Print(result.Stdout)
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	return nil
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show resolved configuration",
	Long: `Display the resolved configuration after merging destination config.

Example:
  azud config
  azud config -d staging`,
	RunE: runConfig,
}

func init() {
	rootCmd.AddCommand(configCmd)
}

func runConfig(cmd *cobra.Command, args []string) error {
	log := output.DefaultLogger

	log.Header("Configuration")

	log.Println("Service: %s", cfg.Service)
	log.Println("Image: %s", cfg.Image)
	log.Println("")

	log.Println("Servers:")
	for role, rc := range cfg.Servers {
		log.Println("  %s: %s", role, strings.Join(rc.Hosts, ", "))
	}
	log.Println("")

	if cfg.Proxy.Host != "" {
		log.Println("Proxy:")
		log.Println("  Host: %s", cfg.Proxy.Host)
		log.Println("  SSL: %v", cfg.Proxy.SSL)
		log.Println("  App Port: %d", cfg.Proxy.AppPort)
		log.Println("")
	}

	if len(cfg.Accessories) > 0 {
		log.Println("Accessories:")
		for name, acc := range cfg.Accessories {
			log.Println("  %s: %s", name, acc.Image)
		}
		log.Println("")
	}

	if len(cfg.Cron) > 0 {
		log.Println("Cron Jobs:")
		for name, cron := range cfg.Cron {
			log.Println("  %s: %s (%s)", name, cron.Schedule, cron.Command)
		}
	}

	return nil
}
