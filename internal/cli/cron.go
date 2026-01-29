package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/docker"
	"github.com/adriancarayol/azud/internal/output"
)

var cronCmd = &cobra.Command{
	Use:   "cron",
	Short: "Manage cron jobs",
	Long: `Commands for managing scheduled cron jobs.

Cron jobs are defined in your deploy.yml and run on a schedule.
They use the same Docker image as your application.

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
	Use:   "logs [name]",
	Short: "View cron job logs",
	Args:  cobra.ExactArgs(1),
	Long: `View logs from a cron job container.

Example:
  azud cron logs db_backup
  azud cron logs db_backup -f`,
	RunE: runCronLogs,
}

var cronRunCmd = &cobra.Command{
	Use:   "run [name]",
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
	defer sshClient.Close()

	dockerClient := docker.NewClient(sshClient)
	containerManager := docker.NewContainerManager(dockerClient)
	imageManager := docker.NewImageManager(dockerClient)

	log.Header("Starting Cron Jobs")

	for _, name := range cronNames {
		cronConfig := cfg.Cron[name]
		hosts := getCronHosts(name)

		for _, host := range hosts {
			containerName := getCronContainerName(name)

			// Check if already running
			running, _ := containerManager.IsRunning(host, containerName)
			if running {
				log.HostSuccess(host, "Cron %s already running", name)
				continue
			}

			log.Host(host, "Starting cron job %s...", name)

			// Pull the image first
			if err := imageManager.Pull(host, cfg.Image); err != nil {
				log.HostError(host, "Failed to pull image: %v", err)
				continue
			}

			// Build the cron container config
			containerConfig := buildCronContainerConfig(name, cronConfig)

			// Run the container
			_, err := containerManager.Run(host, containerConfig)
			if err != nil {
				log.HostError(host, "Failed to start cron %s: %v", name, err)
				continue
			}

			log.HostSuccess(host, "Cron %s started", name)
		}
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
	defer sshClient.Close()

	dockerClient := docker.NewClient(sshClient)
	containerManager := docker.NewContainerManager(dockerClient)

	log.Header("Stopping Cron Jobs")

	var wg sync.WaitGroup
	errors := make(chan error, len(cronNames)*10) // Buffer for errors

	for _, name := range cronNames {
		hosts := getCronHosts(name)

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
					log.Debug("Failed to remove container: %v", err)
				}

				log.HostSuccess(h, "Cron %s stopped", n)
			}(name, host)
		}
	}

	wg.Wait()
	close(errors)

	return nil
}

func runCronLogs(cmd *cobra.Command, args []string) error {
	name := args[0]

	if !cfg.HasCron(name) {
		return fmt.Errorf("cron job %s not found", name)
	}

	hosts := getCronHosts(name)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured for cron job %s", name)
	}

	host := hosts[0]
	if cronHost != "" {
		host = cronHost
	}

	sshClient := createSSHClient()
	defer sshClient.Close()

	dockerClient := docker.NewClient(sshClient)
	containerManager := docker.NewContainerManager(dockerClient)

	containerName := getCronContainerName(name)

	logsConfig := &docker.LogsConfig{
		Container: containerName,
		Follow:    cronFollow,
		Tail:      cronTail,
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
		return fmt.Errorf("no hosts configured for cron job %s", name)
	}

	host := hosts[0]
	if cronHost != "" {
		host = cronHost
	}

	sshClient := createSSHClient()
	defer sshClient.Close()

	dockerClient := docker.NewClient(sshClient)
	containerManager := docker.NewContainerManager(dockerClient)

	log.Header("Running Cron Job: %s", name)
	log.Host(host, "Executing: %s", cronConfig.Command)

	// Execute the command in a one-off container
	runContainerName := fmt.Sprintf("%s-cron-%s-run", cfg.Service, name)

	containerConfig := &docker.ContainerConfig{
		Name:    runContainerName,
		Image:   cfg.Image,
		Detach:  false,
		Remove:  true,
		Network: "azud",
		Command: []string{"/bin/sh", "-c", cronConfig.Command},
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

	// Add volumes
	containerConfig.Volumes = cfg.Volumes

	// Run the container
	_, err := containerManager.Run(host, containerConfig)
	if err != nil {
		return fmt.Errorf("cron job failed: %w", err)
	}

	log.Success("Cron job %s completed", name)
	return nil
}

func runCronList(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if len(cfg.Cron) == 0 {
		log.Info("No cron jobs configured")
		return nil
	}

	sshClient := createSSHClient()
	defer sshClient.Close()

	dockerClient := docker.NewClient(sshClient)
	containerManager := docker.NewContainerManager(dockerClient)

	log.Header("Cron Jobs")

	var rows [][]string

	for name, cronConfig := range cfg.Cron {
		hosts := getCronHosts(name)
		containerName := getCronContainerName(name)

		for _, host := range hosts {
			status := "stopped"
			running, err := containerManager.IsRunning(host, containerName)
			if err == nil && running {
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

	return nil
}

func getCronContainerName(name string) string {
	return fmt.Sprintf("%s-cron-%s", cfg.Service, name)
}

func getCronHosts(name string) []string {
	if cronHost != "" {
		return []string{cronHost}
	}
	return cfg.GetCronHosts(name)
}

func buildCronContainerConfig(name string, cronConfig config.CronConfig) *docker.ContainerConfig {
	containerName := getCronContainerName(name)

	// Build the cron command using supercronic or a shell-based approach
	// We use a shell wrapper to run the command on schedule
	cronCommand := buildCronCommand(cronConfig)

	containerConfig := &docker.ContainerConfig{
		Name:    containerName,
		Image:   cfg.Image,
		Detach:  true,
		Restart: "unless-stopped",
		Network: "azud",
		Command: []string{"/bin/sh", "-c", cronCommand},
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

	// Add volumes from app config
	containerConfig.Volumes = cfg.Volumes

	return containerConfig
}

func buildCronCommand(cronConfig config.CronConfig) string {
	// Create a crontab entry and run it using crond or a shell loop
	// This approach works without needing supercronic installed

	lockPrefix := ""
	lockSuffix := ""
	if cronConfig.Lock {
		// Use flock for locking if available
		lockPrefix = "flock -n /tmp/azud_cron.lock "
		lockSuffix = " || echo 'Skipped: lock held'"
	}

	timeoutPrefix := ""
	if cronConfig.Timeout != "" {
		timeoutPrefix = fmt.Sprintf("timeout %s ", cronConfig.Timeout)
	}

	logRedirect := ""
	if cronConfig.LogPath != "" {
		logRedirect = fmt.Sprintf(" >> %s 2>&1", cronConfig.LogPath)
	}

	// Build the cron entry
	cronEntry := fmt.Sprintf("%s %s%s%s%s",
		cronConfig.Schedule,
		lockPrefix,
		timeoutPrefix,
		cronConfig.Command,
		lockSuffix,
	)

	// Create a script that sets up crond with the crontab
	script := fmt.Sprintf(`
echo '%s%s' > /tmp/crontab
crontab /tmp/crontab
exec crond -f -l 2
`, cronEntry, logRedirect)

	return script
}

func truncateCommand(cmd string, maxLen int) string {
	if len(cmd) <= maxLen {
		return cmd
	}
	return cmd[:maxLen-3] + "..."
}
