package cli

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/docker"
	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/proxy"
)

var scaleCmd = &cobra.Command{
	Use:   "scale [role=count]",
	Short: "Scale application instances",
	Long: `Dynamically scale the number of container instances for a role.

This command adds or removes container instances without affecting running services.
New containers are health-checked before being added to the load balancer.
Existing containers continue serving traffic during scale operations.

Examples:
  azud scale web=3        # Scale web role to 3 instances per host
  azud scale web=2 job=1  # Scale multiple roles at once
  azud scale web=+1       # Add 1 more instance to web role
  azud scale web=-1       # Remove 1 instance from web role`,
	Args: cobra.MinimumNArgs(1),
	RunE: runScale,
}

var scaleStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current scale status",
	Long: `Display the current number of running instances for each role.

Example:
  azud scale status`,
	RunE: runScaleStatus,
}

var (
	scaleHost string
)

func init() {
	scaleCmd.Flags().StringVar(&scaleHost, "host", "", "Scale on specific host only")

	scaleCmd.AddCommand(scaleStatusCmd)
	rootCmd.AddCommand(scaleCmd)
}

func runScale(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	// Parse scale arguments
	scales := make(map[string]scaleOperation)
	for _, arg := range args {
		parts := strings.Split(arg, "=")
		if len(parts) != 2 {
			return fmt.Errorf("invalid scale argument: %s (expected role=count)", arg)
		}

		role := parts[0]
		if !cfg.HasRole(role) {
			return fmt.Errorf("role %s not found", role)
		}

		op, err := parseScaleOperation(parts[1])
		if err != nil {
			return fmt.Errorf("invalid scale count for %s: %w", role, err)
		}

		scales[role] = op
	}

	sshClient := createSSHClient()
	defer sshClient.Close()

	dockerClient := docker.NewClient(sshClient)
	containerManager := docker.NewContainerManager(dockerClient)
	imageManager := docker.NewImageManager(dockerClient)
	proxyManager := proxy.NewManager(sshClient, log)

	log.Header("Scaling Application")

	for role, op := range scales {
		hosts := cfg.GetRoleHosts(role)
		if scaleHost != "" {
			hosts = []string{scaleHost}
		}

		for _, host := range hosts {
			log.Host(host, "Scaling role %s", role)

			// Get current instance count
			currentCount, err := countRunningInstances(containerManager, host, role)
			if err != nil {
				log.HostError(host, "Failed to get current count: %v", err)
				continue
			}

			targetCount := op.calculateTarget(currentCount)
			if targetCount < 0 {
				targetCount = 0
			}

			log.Host(host, "Current: %d, Target: %d", currentCount, targetCount)

			if targetCount == currentCount {
				log.HostSuccess(host, "Already at target scale")
				continue
			}

			if targetCount > currentCount {
				// Scale up
				if err := scaleUp(containerManager, imageManager, proxyManager, host, role, currentCount, targetCount, log); err != nil {
					log.HostError(host, "Scale up failed: %v", err)
					continue
				}
			} else {
				// Scale down
				if err := scaleDown(containerManager, proxyManager, host, role, currentCount, targetCount, log); err != nil {
					log.HostError(host, "Scale down failed: %v", err)
					continue
				}
			}

			log.HostSuccess(host, "Scaled %s to %d instances", role, targetCount)
		}
	}

	log.Success("Scaling complete!")
	return nil
}

func runScaleStatus(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	sshClient := createSSHClient()
	defer sshClient.Close()

	dockerClient := docker.NewClient(sshClient)
	containerManager := docker.NewContainerManager(dockerClient)

	log.Header("Scale Status")

	var rows [][]string

	for role, roleConfig := range cfg.Servers {
		hosts := roleConfig.Hosts
		if scaleHost != "" {
			hosts = []string{scaleHost}
		}

		for _, host := range hosts {
			count, err := countRunningInstances(containerManager, host, role)
			status := fmt.Sprintf("%d", count)
			if err != nil {
				status = "error"
			}

			rows = append(rows, []string{role, host, status})
		}
	}

	log.Table([]string{"Role", "Host", "Instances"}, rows)

	return nil
}

type scaleOperation struct {
	isRelative bool
	value      int
}

func parseScaleOperation(s string) (scaleOperation, error) {
	op := scaleOperation{}

	if strings.HasPrefix(s, "+") {
		op.isRelative = true
		val, err := strconv.Atoi(s[1:])
		if err != nil {
			return op, err
		}
		op.value = val
	} else if strings.HasPrefix(s, "-") {
		op.isRelative = true
		val, err := strconv.Atoi(s[1:])
		if err != nil {
			return op, err
		}
		op.value = -val
	} else {
		val, err := strconv.Atoi(s)
		if err != nil {
			return op, err
		}
		op.value = val
	}

	return op, nil
}

func (op scaleOperation) calculateTarget(current int) int {
	if op.isRelative {
		return current + op.value
	}
	return op.value
}

func countRunningInstances(cm *docker.ContainerManager, host, role string) (int, error) {
	filters := map[string]string{
		"label": fmt.Sprintf("azud.service=%s", cfg.Service),
	}

	containers, err := cm.List(host, false, filters)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, c := range containers {
		// Check if container belongs to this role
		if strings.HasPrefix(c.Name, fmt.Sprintf("%s-%s", cfg.Service, role)) ||
			c.Name == cfg.Service {
			count++
		}
	}

	return count, nil
}

func scaleUp(cm *docker.ContainerManager, im *docker.ImageManager, pm *proxy.Manager, host, role string, from, to int, log *output.Logger) error {
	// Pull image first
	if err := im.Pull(host, cfg.Image); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	roleConfig := cfg.Servers[role]

	var wg sync.WaitGroup
	errors := make(chan error, to-from)

	for i := from; i < to; i++ {
		wg.Add(1)
		go func(instance int) {
			defer wg.Done()

			containerName := fmt.Sprintf("%s-%s-%d", cfg.Service, role, instance)
			log.Host(host, "Starting instance %s", containerName)

			// Build container config
			containerConfig := &docker.ContainerConfig{
				Name:    containerName,
				Image:   cfg.Image,
				Detach:  true,
				Restart: "unless-stopped",
				Network: "azud",
				Labels: map[string]string{
					"azud.managed":  "true",
					"azud.service":  cfg.Service,
					"azud.role":     role,
					"azud.instance": fmt.Sprintf("%d", instance),
				},
				Env: make(map[string]string),
			}

			// Add environment variables
			for key, value := range cfg.Env.Clear {
				containerConfig.Env[key] = value
			}
			for key, value := range roleConfig.Env {
				containerConfig.Env[key] = value
			}
			containerConfig.SecretEnv = cfg.Env.Secret

			// Add role-specific options (convert map to docker run flags)
			for opt, val := range roleConfig.Options {
				switch opt {
				case "memory":
					containerConfig.Memory = val
				case "cpus":
					containerConfig.CPUs = val
				default:
					containerConfig.Options = append(containerConfig.Options, fmt.Sprintf("--%s=%s", opt, val))
				}
			}

			// Add command if specified
			if roleConfig.Cmd != "" {
				containerConfig.Command = []string{"/bin/sh", "-c", roleConfig.Cmd}
			}

			// Add volumes
			containerConfig.Volumes = cfg.Volumes

			// Add health check
			if cfg.Proxy.Healthcheck.Path != "" {
				containerConfig.HealthCmd = fmt.Sprintf("curl -f http://localhost:%d%s || exit 1",
					cfg.Proxy.AppPort, cfg.Proxy.Healthcheck.Path)
				containerConfig.HealthInterval = cfg.Proxy.Healthcheck.Interval
				containerConfig.HealthTimeout = cfg.Proxy.Healthcheck.Timeout
				containerConfig.HealthRetries = 3
			}

			// Run the container
			_, err := cm.Run(host, containerConfig)
			if err != nil {
				errors <- fmt.Errorf("failed to start %s: %w", containerName, err)
				return
			}

			// Wait for health check if configured
			if cfg.Deploy.ReadinessDelay > 0 {
				time.Sleep(cfg.Deploy.ReadinessDelay)
			}

			if cfg.Proxy.Healthcheck.Path != "" {
				if err := cm.WaitHealthy(host, containerName, cfg.Deploy.DeployTimeout); err != nil {
					log.Warn("Health check failed for %s: %v", containerName, err)
					// Continue anyway, proxy will detect unhealthy upstream
				}
			}

			// Register with proxy
			if cfg.Proxy.Host != "" {
				upstream := fmt.Sprintf("%s:%d", containerName, cfg.Proxy.AppPort)
				if err := pm.AddUpstream(host, cfg.Proxy.Host, upstream); err != nil {
					log.Warn("Failed to register %s with proxy: %v", containerName, err)
				}
			}

			log.Host(host, "Instance %s started", containerName)
		}(i)
	}

	wg.Wait()
	close(errors)

	var errs []string
	for err := range errors {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("some instances failed: %s", strings.Join(errs, "; "))
	}

	return nil
}

func scaleDown(cm *docker.ContainerManager, pm *proxy.Manager, host, role string, from, to int, log *output.Logger) error {
	var wg sync.WaitGroup
	errors := make(chan error, from-to)

	for i := from - 1; i >= to; i-- {
		wg.Add(1)
		go func(instance int) {
			defer wg.Done()

			containerName := fmt.Sprintf("%s-%s-%d", cfg.Service, role, instance)
			log.Host(host, "Stopping instance %s", containerName)

			// Remove from proxy first
			if cfg.Proxy.Host != "" {
				upstream := fmt.Sprintf("%s:%d", containerName, cfg.Proxy.AppPort)
				if err := pm.RemoveUpstream(host, cfg.Proxy.Host, upstream); err != nil {
					log.Debug("Failed to remove %s from proxy: %v", containerName, err)
				}
			}

			// Wait for drain
			if cfg.Deploy.DrainTimeout > 0 {
				time.Sleep(cfg.Deploy.DrainTimeout)
			}

			// Stop and remove container
			if err := cm.Stop(host, containerName, 30); err != nil {
				if !strings.Contains(err.Error(), "No such container") {
					errors <- fmt.Errorf("failed to stop %s: %w", containerName, err)
					return
				}
			}

			if err := cm.Remove(host, containerName, true); err != nil {
				log.Debug("Failed to remove %s: %v", containerName, err)
			}

			log.Host(host, "Instance %s stopped", containerName)
		}(i)
	}

	wg.Wait()
	close(errors)

	var errs []string
	for err := range errors {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("some instances failed: %s", strings.Join(errs, "; "))
	}

	return nil
}
