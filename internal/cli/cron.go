package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/shell"
	"github.com/lemonity-org/azud/internal/state"
)

var cronCmd = &cobra.Command{
	Use:   "cron",
	Short: "Manage cron jobs",
	Long: `Commands for managing scheduled cron jobs.

Cron jobs are defined in your deploy.yml and run on a schedule.
They use the same container image as your application.

Example configuration in deploy.yml:
  cron:
    db_backup:
      schedule: "0 2 * * *"
      command: "bin/rails db:backup"
      lock: true

    cleanup:
      schedule: "0 4 * * 0"
      command: "bin/cleanup_old_files"
      timeout: "1h"`,
}

var cronBootCmd = &cobra.Command{
	Use:   "boot [name]",
	Short: "Start cron jobs",
	Long: `Start the cron scheduler for specified jobs.

If no name is provided, starts all cron jobs.

Example:
  azud cron boot           # Start all cron jobs
  azud cron boot db_backup # Start specific job`,
	RunE: runCronBoot,
}

var cronStopCmd = &cobra.Command{
	Use:   "stop [name]",
	Short: "Stop cron jobs",
	Long: `Stop the cron scheduler for specified jobs.

If no name is provided, stops all cron jobs.

Example:
  azud cron stop           # Stop all cron jobs
  azud cron stop db_backup # Stop specific job`,
	RunE: runCronStop,
}

var cronLogsCmd = &cobra.Command{
	Use:   "logs <name>",
	Short: "View cron job logs",
	Args:  cobra.ExactArgs(1),
	Long: `View logs from a cron job container.

Example:
  azud cron logs db_backup
  azud cron logs db_backup -f`,
	RunE: runCronLogs,
}

var cronRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Run a cron job immediately",
	Args:  cobra.ExactArgs(1),
	Long: `Execute a cron job immediately without waiting for its schedule.

Example:
  azud cron run db_backup`,
	RunE: runCronRun,
}

var cronListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all cron jobs",
	Long: `List all configured cron jobs and their status.

Example:
  azud cron list`,
	RunE: runCronList,
}

var (
	cronFollow bool
	cronTail   string
	cronHost   string
)

func init() {
	cronLogsCmd.Flags().BoolVarP(&cronFollow, "follow", "f", false, "Follow log output")
	cronLogsCmd.Flags().StringVar(&cronTail, "tail", "100", "Number of lines")

	cronBootCmd.Flags().StringVar(&cronHost, "host", "", "Specific host")
	cronStopCmd.Flags().StringVar(&cronHost, "host", "", "Specific host")
	cronLogsCmd.Flags().StringVar(&cronHost, "host", "", "Specific host")
	cronRunCmd.Flags().StringVar(&cronHost, "host", "", "Specific host")

	cronCmd.AddCommand(cronBootCmd)
	cronCmd.AddCommand(cronStopCmd)
	cronCmd.AddCommand(cronLogsCmd)
	cronCmd.AddCommand(cronRunCmd)
	cronCmd.AddCommand(cronListCmd)

	rootCmd.AddCommand(cronCmd)
}

func runCronBoot(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if len(cfg.Cron) == 0 {
		return fmt.Errorf("no cron jobs configured")
	}

	// Determine which cron jobs to start
	var cronNames []string
	if len(args) > 0 {
		name := args[0]
		if !cfg.HasCron(name) {
			return fmt.Errorf("cron job %s not found", name)
		}
		cronNames = []string{name}
	} else {
		cronNames = cfg.GetCronNames()
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)
	imageManager := podman.NewImageManager(podmanClient)

	log.Header("Starting Cron Jobs")
	var bootErrors []string

	for _, name := range cronNames {
		cronConfig := cfg.Cron[name]
		hosts := getCronHosts(name)
		if len(hosts) == 0 {
			bootErrors = append(bootErrors, fmt.Sprintf("%s: selected host %q is not configured for this cron job", name, cronHost))
			continue
		}

		if err := ensureRemoteSecretsFile(sshClient, hosts, cfg.Env.Secret); err != nil {
			bootErrors = append(bootErrors, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		for _, host := range hosts {
			containerName := getCronContainerName(name)

			// Check if already running
			running, err := containerManager.IsRunning(host, containerName)
			if err != nil {
				bootErrors = append(bootErrors, fmt.Sprintf("%s@%s inspect: %v", name, host, err))
				continue
			}
			if running {
				log.HostSuccess(host, "Cron %s already running", name)
				continue
			}

			log.Host(host, "Starting cron job %s...", name)

			// Pull the image first
			if err := imageManager.Pull(host, cfg.Image); err != nil {
				log.HostError(host, "Failed to pull image: %v", err)
				bootErrors = append(bootErrors, fmt.Sprintf("%s@%s pull: %v", name, host, err))
				continue
			}
			if err := validateCronImageRuntime(containerManager, host, name, cronConfig, true); err != nil {
				log.HostError(host, "Unsupported cron image runtime: %v", err)
				bootErrors = append(bootErrors, fmt.Sprintf("%s@%s runtime: %v", name, host, err))
				continue
			}

			// Build the cron container config
			containerConfig := buildCronContainerConfig(name, cronConfig)

			// Run the container
			_, err = containerManager.Run(host, containerConfig)
			if err != nil {
				log.HostError(host, "Failed to start cron %s: %v", name, err)
				bootErrors = append(bootErrors, fmt.Sprintf("%s@%s start: %v", name, host, err))
				continue
			}

			log.HostSuccess(host, "Cron %s started", name)
		}
	}

	if len(bootErrors) > 0 {
		return fmt.Errorf("cron boot failed: %s", strings.Join(bootErrors, "; "))
	}
	return nil
}

func runCronStop(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	// Determine which cron jobs to stop
	var cronNames []string
	if len(args) > 0 {
		name := args[0]
		if !cfg.HasCron(name) {
			return fmt.Errorf("cron job %s not found", name)
		}
		cronNames = []string{name}
	} else {
		cronNames = cfg.GetCronNames()
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	log.Header("Stopping Cron Jobs")

	var wg sync.WaitGroup
	errors := make(chan error, len(cronNames)*10) // Buffer for errors

	for _, name := range cronNames {
		hosts := getCronHosts(name)
		if len(hosts) == 0 {
			return fmt.Errorf("selected host %q is not configured for cron job %s", cronHost, name)
		}

		for _, host := range hosts {
			wg.Add(1)
			go func(n, h string) {
				defer wg.Done()

				containerName := getCronContainerName(n)

				if err := containerManager.Stop(h, containerName, 30); err != nil {
					if !strings.Contains(err.Error(), "No such container") {
						log.HostError(h, "Failed to stop cron %s: %v", n, err)
						errors <- fmt.Errorf("%s@%s: %w", n, h, err)
					}
					return
				}

				if err := containerManager.Remove(h, containerName, true); err != nil {
					log.HostError(h, "Failed to remove cron %s: %v", n, err)
					errors <- fmt.Errorf("%s@%s remove: %w", n, h, err)
					return
				}

				log.HostSuccess(h, "Cron %s stopped", n)
			}(name, host)
		}
	}

	wg.Wait()
	close(errors)

	var stopErrors []string
	for err := range errors {
		stopErrors = append(stopErrors, err.Error())
	}
	if len(stopErrors) > 0 {
		return fmt.Errorf("cron stop failed: %s", strings.Join(stopErrors, "; "))
	}
	return nil
}

func runCronLogs(cmd *cobra.Command, args []string) error {
	name := args[0]

	if !cfg.HasCron(name) {
		return fmt.Errorf("cron job %s not found", name)
	}

	hosts := getCronHosts(name)
	if len(hosts) == 0 {
		return fmt.Errorf("no matching host configured for cron job %s (selected %q)", name, cronHost)
	}
	if len(hosts) > 1 && cronHost == "" {
		return fmt.Errorf("cron job %s has multiple hosts; select one with --host", name)
	}

	host := hosts[0]

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	containerName := getCronContainerName(name)

	logsConfig := &podman.LogsConfig{
		Container: containerName,
		Follow:    cronFollow,
		Tail:      cronTail,
	}

	if cronFollow {
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
		return fmt.Errorf("cron logs failed with exit code %d", result.ExitCode)
	}

	return nil
}

func runCronRun(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	name := args[0]

	if !cfg.HasCron(name) {
		return fmt.Errorf("cron job %s not found", name)
	}

	cronConfig := cfg.Cron[name]
	hosts := getCronHosts(name)
	if len(hosts) == 0 {
		return fmt.Errorf("no matching host configured for cron job %s (selected %q)", name, cronHost)
	}
	if len(hosts) > 1 && cronHost == "" {
		return fmt.Errorf("cron job %s has multiple hosts; select one with --host", name)
	}

	host := hosts[0]

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)
	imageManager := podman.NewImageManager(podmanClient)
	if err := ensureRemoteSecretsFile(sshClient, []string{host}, cfg.Env.Secret); err != nil {
		return err
	}
	if err := imageManager.Pull(host, cfg.Image); err != nil {
		return fmt.Errorf("failed to pull cron image: %w", err)
	}
	if err := validateCronImageRuntime(containerManager, host, name, cronConfig, false); err != nil {
		return err
	}

	log.Header("Cron / run / %s", name)
	log.Host(host, "Executing: %s", cronConfig.Command)

	// Build container config
	runContainerName := fmt.Sprintf("%s-cron-%s-run", cfg.Service, name)
	containerConfig := buildCronRunContainerConfig(name, runContainerName, cronConfig)

	// If locking is enabled, acquire host-level lock before running
	if cronConfig.Lock {
		lockFile := cronLockFile(name)
		lockTimeout := 5 * time.Minute
		if cronConfig.Timeout != "" {
			if d, err := time.ParseDuration(cronConfig.Timeout); err == nil {
				lockTimeout = d + time.Minute // Add buffer for lock acquisition
			}
		}

		log.Host(host, "Acquiring lock %s...", lockFile)
		err := sshClient.WithRemoteLock(host, lockFile, lockTimeout, func() error {
			_, runErr := containerManager.Run(host, containerConfig)
			return runErr
		})
		if err != nil {
			return fmt.Errorf("cron job failed: %w", err)
		}
	} else {
		// No locking needed, run directly
		_, err := containerManager.Run(host, containerConfig)
		if err != nil {
			return fmt.Errorf("cron job failed: %w", err)
		}
	}

	log.Success("Cron job %s completed", name)
	return nil
}

// buildCronRunContainerConfig builds the container config for a manual cron run.
func buildCronRunContainerConfig(name, containerName string, cronConfig config.CronConfig) *podman.ContainerConfig {
	command := cronConfig.Command
	if cronConfig.Timeout != "" {
		command = fmt.Sprintf("timeout %s sh -c %s", shell.Quote(cronConfig.Timeout), shell.Quote(cronConfig.Command))
	}
	containerConfig := &podman.ContainerConfig{
		Name:       containerName,
		Image:      cfg.Image,
		Detach:     false,
		Remove:     true,
		Network:    "azud",
		Entrypoint: "/bin/sh",
		Command:    []string{"-c", command},
		Labels: map[string]string{
			"azud.managed":  "true",
			"azud.service":  cfg.Service,
			"azud.cron":     name,
			"azud.cron.run": "manual",
		},
		Env: make(map[string]string),
	}

	// Add environment variables
	for key, value := range cfg.Env.Clear {
		containerConfig.Env[key] = value
	}
	for key, value := range cronConfig.Env {
		containerConfig.Env[key] = value
	}
	containerConfig.SecretEnv = cfg.Env.Secret
	if len(containerConfig.SecretEnv) > 0 {
		containerConfig.EnvFile = config.RemoteSecretsPath(cfg)
	}

	// Add volumes
	containerConfig.Volumes = cfg.Volumes

	return containerConfig
}

func runCronList(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if len(cfg.Cron) == 0 {
		log.Info("No cron jobs configured")
		return nil
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	log.Header("Cron Jobs")

	var rows [][]string
	var statusErrors []string

	for _, name := range cfg.GetCronNames() {
		cronConfig := cfg.Cron[name]
		hosts := getCronHosts(name)
		containerName := getCronContainerName(name)

		for _, host := range hosts {
			status := "stopped"
			running, err := containerManager.IsRunning(host, containerName)
			if err != nil {
				status = "error"
				statusErrors = append(statusErrors, fmt.Sprintf("%s@%s: %v", name, host, err))
			} else if running {
				status = "running"
			}

			rows = append(rows, []string{
				name,
				cronConfig.Schedule,
				truncateCommand(cronConfig.Command, 40),
				host,
				status,
			})
		}
	}

	log.Table([]string{"Name", "Schedule", "Command", "Host", "Status"}, rows)
	if len(statusErrors) > 0 {
		return fmt.Errorf("cron status failed: %s", strings.Join(statusErrors, "; "))
	}
	return nil
}

func getCronContainerName(name string) string {
	return fmt.Sprintf("%s-cron-%s", cfg.Service, name)
}

// cronLockFile returns the path to the host-level lock file for a cron job.
// This lock file is shared between the scheduled container and manual runs.
func cronLockFile(name string) string {
	return state.LockFile(cfg.SSH.User, fmt.Sprintf("%s-cron-%s", cfg.Service, name))
}

// cronLockFileInContainer returns the lock file path as seen from inside
// the cron container. The host state directory is mounted at /var/lib/azud.
func cronLockFileInContainer(name string) string {
	return fmt.Sprintf("/var/lib/azud/%s-cron-%s.lock", cfg.Service, name)
}

func getCronHosts(name string) []string {
	hosts := cfg.GetCronHosts(name)
	if cronHost == "" {
		return hosts
	}
	for _, host := range hosts {
		if host == cronHost {
			return []string{host}
		}
	}
	return nil
}

func validateCronImageRuntime(containerManager *podman.ContainerManager, host, name string, cronConfig config.CronConfig, scheduled bool) error {
	required := []string{"/bin/sh"}
	if scheduled {
		required = append(required, "crontab", "crond")
		if cronConfig.Lock {
			required = append(required, "flock")
		}
	}
	if cronConfig.Timeout != "" {
		required = append(required, "timeout")
	}
	checks := make([]string, 0, len(required))
	for _, command := range required {
		if strings.Contains(command, "/") {
			checks = append(checks, fmt.Sprintf("test -x %s", shell.Quote(command)))
		} else {
			checks = append(checks, fmt.Sprintf("command -v %s >/dev/null 2>&1", shell.Quote(command)))
		}
	}
	checkConfig := &podman.ContainerConfig{
		Name:       fmt.Sprintf("%s-cron-%s-runtime-check-%d", cfg.Service, name, time.Now().UnixNano()),
		Image:      cfg.Image,
		Remove:     true,
		Entrypoint: "/bin/sh",
		Command:    []string{"-c", strings.Join(checks, " && ")},
	}
	if _, err := containerManager.Run(host, checkConfig); err != nil {
		return fmt.Errorf("image %s must provide %s: %w", cfg.Image, strings.Join(required, ", "), err)
	}
	return nil
}

func buildCronContainerConfig(name string, cronConfig config.CronConfig) *podman.ContainerConfig {
	containerName := getCronContainerName(name)

	// Build the cron command using supercronic or a shell-based approach
	// We use a shell wrapper to run the command on schedule
	cronCommand := buildCronCommand(name, cronConfig)

	containerConfig := &podman.ContainerConfig{
		Name:       containerName,
		Image:      cfg.Image,
		Detach:     true,
		Restart:    "unless-stopped",
		Network:    "azud",
		Entrypoint: "/bin/sh",
		Command:    []string{"-c", cronCommand},
		Labels: map[string]string{
			"azud.managed":       "true",
			"azud.service":       cfg.Service,
			"azud.cron":          name,
			"azud.cron.schedule": cronConfig.Schedule,
		},
		Env: make(map[string]string),
	}

	// Add environment variables from app config
	for key, value := range cfg.Env.Clear {
		containerConfig.Env[key] = value
	}

	// Add cron-specific environment variables
	for key, value := range cronConfig.Env {
		containerConfig.Env[key] = value
	}

	// Add secret environment variable names
	containerConfig.SecretEnv = cfg.Env.Secret
	if len(containerConfig.SecretEnv) > 0 {
		containerConfig.EnvFile = config.RemoteSecretsPath(cfg)
	}

	// Add volumes from app config
	containerConfig.Volumes = append([]string{}, cfg.Volumes...)

	// If locking is enabled, mount the host state directory for coordination
	// This allows the container's flock to coordinate with host-level locks
	// Note: For non-root users, state.Dir returns ${HOME}/... which is expanded
	// by the shell on the remote host when running podman. Inside the container,
	// we mount to a fixed path (/var/lib/azud) to avoid ${HOME} mismatch issues.
	if cronConfig.Lock {
		stateDir := state.Dir(cfg.SSH.User)
		// Mount to /var/lib/azud inside container for consistent path access
		containerConfig.Volumes = append(containerConfig.Volumes,
			fmt.Sprintf("%s:/var/lib/azud:rw", stateDir))
	}

	return containerConfig
}

func buildCronCommand(name string, cronConfig config.CronConfig) string {
	// Create a crontab entry and run it using crond or a shell loop
	// This approach works without needing supercronic installed

	lockPrefix := ""
	lockSuffix := ""
	if cronConfig.Lock {
		// Use flock with the in-container lock file path for cross-container coordination
		// The host state directory is mounted at /var/lib/azud inside the container
		lockFile := cronLockFileInContainer(name)
		lockPrefix = fmt.Sprintf("flock -n %s ", lockFile)
		lockSuffix = " || echo 'Skipped: lock held'"
	}

	timeoutPrefix := ""
	if cronConfig.Timeout != "" {
		timeoutPrefix = fmt.Sprintf("timeout %s ", cronConfig.Timeout)
	}

	logRedirect := ""
	if cronConfig.LogPath != "" {
		logRedirect = fmt.Sprintf(" >> %s 2>&1", shell.Quote(cronConfig.LogPath))
	}

	// Build the cron entry
	cronEntry := fmt.Sprintf("%s %s%s%s%s",
		cronConfig.Schedule,
		lockPrefix,
		timeoutPrefix,
		cronConfig.Command,
		lockSuffix,
	)

	// Create a script that sets up crond with the crontab.
	// Use shell.Quote for the echo argument to prevent breakage when
	// cron commands or log paths contain single quotes or shell metacharacters.
	script := fmt.Sprintf(`
mkdir -p /var/lib/azud 2>/dev/null || true
echo %s > /tmp/crontab
crontab /tmp/crontab
exec crond -f -l 2
`, shell.Quote(cronEntry+logRedirect))

	return script
}

func truncateCommand(cmd string, maxLen int) string {
	if len(cmd) <= maxLen {
		return cmd
	}
	return cmd[:maxLen-3] + "..."
}
