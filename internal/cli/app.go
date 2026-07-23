package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
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
	appExecCmd.Flags().StringVar(&appRole, "role", "", "Specific role (default: web)")
	appExecCmd.Flags().BoolVarP(&appInteractive, "interactive", "i", false, "Keep STDIN open")
	appExecCmd.Flags().BoolVarP(&appTTY, "tty", "t", false, "Allocate a pseudo-TTY")

	// Start/Stop/Restart flags
	appStartCmd.Flags().StringVar(&appHost, "host", "", "Specific host")
	appStartCmd.Flags().StringVar(&appRole, "role", "", "Specific role")
	appStopCmd.Flags().StringVar(&appHost, "host", "", "Specific host")
	appStopCmd.Flags().StringVar(&appRole, "role", "", "Specific role")
	appRestartCmd.Flags().StringVar(&appHost, "host", "", "Specific host")
	appRestartCmd.Flags().StringVar(&appRole, "role", "", "Specific role")

	// Details flags
	appDetailsCmd.Flags().StringVar(&appHost, "host", "", "Specific host")
	appDetailsCmd.Flags().StringVar(&appRole, "role", "", "Specific role")

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
	hosts := getSingleRoleAppHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no matching host configured for role %s", defaultAppRole())
	}
	host := hosts[0]
	if len(hosts) > 1 {
		log.Warn("Multiple hosts, showing logs from %s", host)
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	role := appRole
	if role == "" {
		role = "web"
	}
	logsConfig := &podman.LogsConfig{
		Container: deploy.RoleContainerName(cfg, role),
		Follow:    appFollow,
		Tail:      appTail,
	}

	if appFollow {
		if err := containerManager.LogsStream(host, logsConfig, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("failed to follow logs: %w", err)
		}
		return nil
	}
	result, err := containerManager.Logs(host, logsConfig)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	fmt.Print(result.Stdout)
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("logs exited with code %d", result.ExitCode)
	}

	return nil
}

func runAppExec(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)

	if len(args) == 0 {
		return fmt.Errorf("no command specified")
	}

	// Get target host
	hosts := getSingleRoleAppHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no matching host configured for role %s", defaultAppRole())
	}
	host := hosts[0]

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	execConfig := &podman.ExecConfig{
		Container:   deploy.RoleContainerName(cfg, defaultAppRole()),
		Command:     args,
		Interactive: appInteractive,
		TTY:         appTTY,
	}
	if appInteractive || appTTY {
		if err := containerManager.ExecInteractive(host, execConfig, os.Stdin, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("interactive exec failed: %w", err)
		}
		return nil
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
	defer func() { _ = sshClient.Close() }()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	hosts := getAppHosts()
	return deployer.StartRoles(hosts, selectedAppRoles())
}

func runAppStop(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	hosts := getAppHosts()
	return deployer.StopRoles(hosts, selectedAppRoles())
}

func runAppRestart(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	deployer := deploy.NewDeployer(cfg, sshClient, log)

	hosts := getAppHosts()
	return deployer.RestartRoles(hosts, selectedAppRoles())
}

func runAppDetails(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := getAppHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	log.Header("Application / %s", cfg.Service)

	var rows [][]string
	var detailErrors []string
	roles := selectedAppRoles()
	if len(roles) == 0 {
		roles = cfg.GetRoles()
	}
	for _, role := range roles {
		containerName := deploy.RoleContainerName(cfg, role)
		for _, host := range cfg.GetRoleHosts(role) {
			if appHost != "" && host != appHost {
				continue
			}
			running, err := containerManager.IsRunning(host, containerName)
			if err != nil {
				rows = append(rows, []string{role, host, "error", err.Error()})
				detailErrors = append(detailErrors, fmt.Sprintf("%s/%s: %v", host, role, err))
				continue
			}

			status := "stopped"
			if running {
				status = "running"

				// Get stats
				stats, err := containerManager.Stats(host, containerName)
				if err == nil {
					rows = append(rows, []string{role, host, status, stats})
					continue
				}
				detailErrors = append(detailErrors, fmt.Sprintf("%s/%s stats: %v", host, role, err))
			}

			rows = append(rows, []string{role, host, status, "-"})
		}
	}

	log.Table([]string{"Role", "Host", "Status", "Stats"}, rows)

	// Show image info
	log.Println("")
	log.Println("Image: %s", cfg.Image)
	proxyHosts := cfg.Proxy.AllHosts()
	if len(proxyHosts) > 0 {
		log.Println("Proxy: %s (port %d)", strings.Join(proxyHosts, ", "), cfg.Proxy.AppPort)
	} else {
		log.Println("Proxy: (not configured)")
	}

	if len(detailErrors) > 0 {
		return fmt.Errorf("application details failed: %s", strings.Join(detailErrors, "; "))
	}
	return nil
}

func getAppHosts() []string {
	if appHost != "" {
		candidateHosts := cfg.GetAllHosts()
		if appRole != "" {
			candidateHosts = cfg.GetRoleHosts(appRole)
		}
		if containsString(candidateHosts, appHost) {
			return []string{appHost}
		}
		return nil
	}
	if appRole != "" {
		return cfg.GetRoleHosts(appRole)
	}
	return cfg.GetAllHosts()
}

func defaultAppRole() string {
	if appRole != "" {
		return appRole
	}
	return "web"
}

func selectedAppRoles() []string {
	if appRole == "" {
		return nil
	}
	return []string{appRole}
}

func getSingleRoleAppHosts() []string {
	hosts := cfg.GetRoleHosts(defaultAppRole())
	if appHost == "" {
		return hosts
	}
	if containsString(hosts, appHost) {
		return []string{appHost}
	}
	return nil
}

var accessoryCmd = &cobra.Command{
	Use:   "accessory",
	Short: "Manage accessories",
	Long:  `Commands for managing accessories (databases, caches, etc.).`,
}

var accessoryBootCmd = &cobra.Command{
	Use:   "boot <name>",
	Short: "Start an accessory",
	Long: `Start an accessory container.

Example:
  azud accessory boot mysql
  azud accessory boot redis`,
	Args: cobra.ExactArgs(1),
	RunE: runAccessoryBoot,
}

var accessoryStopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop an accessory",
	Args:  cobra.ExactArgs(1),
	RunE:  runAccessoryStop,
}

var accessoryLogsCmd = &cobra.Command{
	Use:   "logs <name>",
	Short: "View accessory logs",
	Args:  cobra.ExactArgs(1),
	RunE:  runAccessoryLogs,
}

var accessoryExecCmd = &cobra.Command{
	Use:   "exec <name> -- command",
	Short: "Execute command in accessory",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runAccessoryExec,
}

var accessoryRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Stop and remove an accessory container",
	Long: `Stop and remove an accessory container from the remote host.

This will stop the running container and remove it. Use --yes to skip
the confirmation prompt.

Example:
  azud accessory remove mysql
  azud accessory remove redis --yes`,
	Args: cobra.ExactArgs(1),
	RunE: runAccessoryRemove,
}

var (
	accessoryRemoveYes bool
	accessoryHost      string
)

func init() {
	accessoryLogsCmd.Flags().BoolVarP(&appFollow, "follow", "f", false, "Follow logs")
	accessoryLogsCmd.Flags().StringVar(&appTail, "tail", "100", "Number of lines")
	accessoryBootCmd.Flags().StringVar(&accessoryHost, "host", "", "Specific configured host")
	accessoryStopCmd.Flags().StringVar(&accessoryHost, "host", "", "Specific configured host")
	accessoryLogsCmd.Flags().StringVar(&accessoryHost, "host", "", "Specific configured host")
	accessoryExecCmd.Flags().StringVar(&accessoryHost, "host", "", "Specific configured host")
	accessoryRemoveCmd.Flags().StringVar(&accessoryHost, "host", "", "Specific configured host")

	accessoryRemoveCmd.Flags().BoolVar(&accessoryRemoveYes, "yes", false, "Skip confirmation prompt")

	accessoryCmd.AddCommand(accessoryBootCmd)
	accessoryCmd.AddCommand(accessoryStopCmd)
	accessoryCmd.AddCommand(accessoryLogsCmd)
	accessoryCmd.AddCommand(accessoryExecCmd)
	accessoryCmd.AddCommand(accessoryRemoveCmd)

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

	hosts, err := selectedAccessoryHosts(name, accessory, false)
	if err != nil {
		return err
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	log.Info("Starting accessory %s on %s...", name, strings.Join(hosts, ", "))

	return deployAccessoriesOnHost(sshClient, log, accessoryHost, name)
}

func runAccessoryStop(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	name := args[0]

	accessory, ok := cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %s not found", name)
	}

	hosts, err := selectedAccessoryHosts(name, accessory, false)
	if err != nil {
		return err
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	containerName := fmt.Sprintf("%s-%s", cfg.Service, name)

	log.Info("Stopping accessory %s...", name)

	var stopErrors []string
	for _, host := range hosts {
		if err := containerManager.Stop(host, containerName, 30); err != nil {
			stopErrors = append(stopErrors, fmt.Sprintf("%s: %v", host, err))
			continue
		}
		log.HostSuccess(host, "Accessory %s stopped", name)
	}
	if len(stopErrors) > 0 {
		return fmt.Errorf("failed to stop accessory %s: %s", name, strings.Join(stopErrors, "; "))
	}
	return nil
}

func runAccessoryRemove(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	name := args[0]

	accessory, ok := cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %s not found", name)
	}

	hosts, err := selectedAccessoryHosts(name, accessory, false)
	if err != nil {
		return err
	}

	containerName := fmt.Sprintf("%s-%s", cfg.Service, name)

	if !accessoryRemoveYes {
		writer := cmd.OutOrStdout()
		_, _ = fmt.Fprintln(writer, "REMOVE / ACCESSORY")
		_, _ = fmt.Fprintln(writer, "------------------")
		_, _ = fmt.Fprintf(writer, "  NAME      %s\n", name)
		_, _ = fmt.Fprintf(writer, "  CONTAINER %s\n", containerName)
		_, _ = fmt.Fprintf(writer, "  HOSTS     %s\n", strings.Join(hosts, ", "))
		_, _ = fmt.Fprint(writer, "  CONFIRM   Remove? [y/N] ")

		var answer string
		if _, err := fmt.Scanln(&answer); err != nil {
			log.Info("Aborted")
			return nil
		}
		if strings.ToLower(strings.TrimSpace(answer)) != "y" {
			log.Info("Aborted")
			return nil
		}
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	var removeErrors []string
	for _, host := range hosts {
		log.Host(host, "Stopping accessory %s...", name)
		if err := containerManager.Stop(host, containerName, 30); err != nil && !strings.Contains(err.Error(), "No such container") {
			removeErrors = append(removeErrors, fmt.Sprintf("%s stop: %v", host, err))
			continue
		}

		log.Host(host, "Removing accessory %s...", name)
		if err := containerManager.Remove(host, containerName, true); err != nil {
			removeErrors = append(removeErrors, fmt.Sprintf("%s remove: %v", host, err))
			continue
		}
		log.HostSuccess(host, "Accessory %s removed", name)
	}
	if len(removeErrors) > 0 {
		return fmt.Errorf("failed to remove accessory %s: %s", name, strings.Join(removeErrors, "; "))
	}
	return nil
}

func runAccessoryLogs(cmd *cobra.Command, args []string) error {
	name := args[0]

	accessory, ok := cfg.Accessories[name]
	if !ok {
		return fmt.Errorf("accessory %s not found", name)
	}

	hosts, err := selectedAccessoryHosts(name, accessory, true)
	if err != nil {
		return err
	}
	host := hosts[0]

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	containerName := fmt.Sprintf("%s-%s", cfg.Service, name)

	logsConfig := &podman.LogsConfig{
		Container: containerName,
		Follow:    appFollow,
		Tail:      appTail,
	}
	if appFollow {
		if err := containerManager.LogsStream(host, logsConfig, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("failed to follow accessory logs: %w", err)
		}
		return nil
	}

	result, err := containerManager.Logs(host, logsConfig)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	fmt.Print(result.Stdout)
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("accessory logs exited with status %d", result.ExitCode)
	}
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

	hosts, err := selectedAccessoryHosts(name, accessory, true)
	if err != nil {
		return err
	}
	host := hosts[0]

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	containerName := fmt.Sprintf("%s-%s", cfg.Service, name)

	execConfig := &podman.ExecConfig{
		Container:   containerName,
		Command:     cmdArgs,
		Interactive: appInteractive,
		TTY:         appTTY,
	}
	if appInteractive || appTTY {
		if err := containerManager.ExecInteractive(host, execConfig, os.Stdin, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("interactive accessory exec failed: %w", err)
		}
		return nil
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
		return fmt.Errorf("accessory exec exited with status %d", result.ExitCode)
	}

	return nil
}

func selectedAccessoryHosts(name string, accessory config.AccessoryConfig, requireSingle bool) ([]string, error) {
	hosts := accessoryHosts(accessory)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no host configured for accessory %s", name)
	}
	if accessoryHost != "" {
		for _, host := range hosts {
			if host == accessoryHost {
				return []string{host}, nil
			}
		}
		return nil, fmt.Errorf("host %s is not configured for accessory %s", accessoryHost, name)
	}
	if requireSingle && len(hosts) > 1 {
		return nil, fmt.Errorf("accessory %s has multiple hosts; select one with --host", name)
	}
	return hosts, nil
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
	for _, role := range cfg.GetRoles() {
		rc := cfg.Servers[role]
		log.Println("  %s: %s", role, strings.Join(rc.Hosts, ", "))
	}
	log.Println("")

	proxyHosts := cfg.Proxy.AllHosts()
	if len(proxyHosts) > 0 {
		log.Println("Proxy:")
		log.Println("  Hosts: %s", strings.Join(proxyHosts, ", "))
		log.Println("  SSL: %v", cfg.Proxy.SSL)
		log.Println("  App Port: %d", cfg.Proxy.AppPort)
		log.Println("")
	}

	if len(cfg.Accessories) > 0 {
		log.Println("Accessories:")
		for _, name := range cfg.GetAccessoryNames() {
			acc := cfg.Accessories[name]
			log.Println("  %s: %s", name, acc.Image)
		}
		log.Println("")
	}

	if len(cfg.Cron) > 0 {
		log.Println("Cron Jobs:")
		for _, name := range cfg.GetCronNames() {
			cron := cfg.Cron[name]
			log.Println("  %s: %s (%s)", name, cron.Schedule, cron.Command)
		}
	}

	return nil
}
