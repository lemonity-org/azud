package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/ssh"
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
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)
	imageManager := podman.NewImageManager(podmanClient)
	proxyManager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())

	log.Header("Scaling Application")

	roles := make([]string, 0, len(scales))
	for role := range scales {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var operationErrors []string
	for _, role := range roles {
		op := scales[role]
		hosts := cfg.GetRoleHosts(role)
		if scaleHost != "" {
			if !containsString(hosts, scaleHost) {
				operationErrors = append(operationErrors, fmt.Sprintf("%s/%s: host is not configured for role", scaleHost, role))
				continue
			}
			hosts = []string{scaleHost}
		}

		for _, host := range hosts {
			log.Host(host, "Scaling role %s", role)

			if err := ensureRemoteSecretsFile(sshClient, []string{host}, cfg.Env.Secret); err != nil {
				log.HostError(host, "Missing secrets: %v", err)
				operationErrors = append(operationErrors, fmt.Sprintf("%s/%s: missing secrets: %v", host, role, err))
				continue
			}

			// Get current instance count
			currentCount, err := countRunningInstances(containerManager, host, role)
			if err != nil {
				log.HostError(host, "Failed to get current count: %v", err)
				operationErrors = append(operationErrors, fmt.Sprintf("%s/%s: count: %v", host, role, err))
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
				if err := scaleUp(containerManager, imageManager, proxyManager, podmanClient, sshClient, host, role, currentCount, targetCount, log); err != nil {
					log.HostError(host, "Scale up failed: %v", err)
					operationErrors = append(operationErrors, fmt.Sprintf("%s/%s: scale up: %v", host, role, err))
					continue
				}
			} else {
				// Scale down
				if err := scaleDown(containerManager, proxyManager, host, role, currentCount, targetCount, log); err != nil {
					log.HostError(host, "Scale down failed: %v", err)
					operationErrors = append(operationErrors, fmt.Sprintf("%s/%s: scale down: %v", host, role, err))
					continue
				}
			}

			log.HostSuccess(host, "Scaled %s to %d instances", role, targetCount)
		}
	}

	if len(operationErrors) > 0 {
		return fmt.Errorf("scaling failed: %s", strings.Join(operationErrors, "; "))
	}
	log.Success("Scaling complete!")
	return nil
}

func runScaleStatus(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	podmanClient := podman.NewClient(sshClient)
	containerManager := podman.NewContainerManager(podmanClient)

	log.Header("Scale Status")

	var rows [][]string
	var statusErrors []string

	matchedHost := scaleHost == ""
	for _, role := range cfg.GetRoles() {
		roleConfig := cfg.Servers[role]
		hosts := roleConfig.Hosts
		if scaleHost != "" {
			if !containsString(hosts, scaleHost) {
				continue
			}
			matchedHost = true
			hosts = []string{scaleHost}
		}

		for _, host := range hosts {
			count, err := countRunningInstances(containerManager, host, role)
			status := fmt.Sprintf("%d", count)
			if err != nil {
				status = "error"
				statusErrors = append(statusErrors, fmt.Sprintf("%s/%s: %v", host, role, err))
			}

			rows = append(rows, []string{role, host, status})
		}
	}
	if !matchedHost {
		return fmt.Errorf("host %s is not configured for any role", scaleHost)
	}

	log.Table([]string{"Role", "Host", "Instances"}, rows)
	if len(statusErrors) > 0 {
		return fmt.Errorf("scale status failed: %s", strings.Join(statusErrors, "; "))
	}
	return nil
}

type scaleOperation struct {
	isRelative bool
	value      int
}

func parseScaleOperation(s string) (scaleOperation, error) {
	isRelative := strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-")

	val, err := strconv.Atoi(s)
	if err != nil {
		return scaleOperation{}, err
	}

	return scaleOperation{isRelative: isRelative, value: val}, nil
}

func (op scaleOperation) calculateTarget(current int) int {
	if op.isRelative {
		return current + op.value
	}
	return op.value
}

type roleInstance struct {
	Name   string
	Index  int
	Stable bool
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// listRoleInstances enumerates instances by their exact managed service and
// role labels. Names are then validated against the documented stable name or
// the indexed scale name, preventing prefix collisions with other services.
func listRoleInstances(cm *podman.ContainerManager, host, role string) ([]roleInstance, error) {
	containers, err := cm.List(host, false, map[string]string{
		"label": fmt.Sprintf("azud.service=%s", cfg.Service),
	})
	if err != nil {
		return nil, err
	}

	stableName := deploy.RoleContainerName(cfg, role)
	prefix := stableName + "-"
	instances := make([]roleInstance, 0, len(containers))
	for _, container := range containers {
		if container.Labels["azud.service"] != cfg.Service || container.Labels["azud.role"] != role {
			continue
		}
		if container.Name == stableName {
			instances = append(instances, roleInstance{Name: container.Name, Index: -1, Stable: true})
			continue
		}
		if !strings.HasPrefix(container.Name, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(container.Name, prefix))
		if err != nil || index < 0 {
			continue
		}
		instances = append(instances, roleInstance{Name: container.Name, Index: index})
	}

	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Stable != instances[j].Stable {
			return instances[i].Stable
		}
		return instances[i].Index < instances[j].Index
	})
	return instances, nil
}

func countRunningInstances(cm *podman.ContainerManager, host, role string) (int, error) {
	instances, err := listRoleInstances(cm, host, role)
	if err != nil {
		return 0, err
	}
	return len(instances), nil
}

func scaleUp(cm *podman.ContainerManager, im *podman.ImageManager, pm *proxy.Manager, podmanClient *podman.Client, sshClient *ssh.Client, host, role string, from, to int, log *output.Logger) error {
	// Pull image first
	if err := im.Pull(host, cfg.Image); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	proxyHost := cfg.Proxy.PrimaryHost()
	instances, err := listRoleInstances(cm, host, role)
	if err != nil {
		return fmt.Errorf("failed to enumerate instances: %w", err)
	}
	used := make(map[int]struct{})
	for _, instance := range instances {
		if !instance.Stable {
			used[instance.Index] = struct{}{}
		}
	}

	created := make([]string, 0, to-from)
	registered := make(map[string]string)
	cleanup := func() error {
		var cleanupErrors []string
		for i := len(created) - 1; i >= 0; i-- {
			name := created[i]
			if upstream, routed := registered[name]; routed {
				if err := pm.RemoveUpstream(host, proxyHost, upstream); err != nil {
					// Keep a still-routed container alive. Removing it here would turn
					// a failed scale-up into an avoidable bad gateway.
					cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove route for %s: %v", name, err))
					continue
				}
			}
			if err := cm.Remove(host, name, true); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove container %s: %v", name, err))
			}
		}
		if len(cleanupErrors) > 0 {
			return fmt.Errorf("cleanup incomplete: %s", strings.Join(cleanupErrors, "; "))
		}
		return nil
	}
	failWithCleanup := func(cause error) error {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return fmt.Errorf("%w (%v)", cause, cleanupErr)
		}
		return cause
	}

	for len(instances)+len(created) < to {
		index := 0
		for {
			if _, exists := used[index]; !exists {
				break
			}
			index++
		}
		used[index] = struct{}{}
		containerName := fmt.Sprintf("%s-%d", deploy.RoleContainerName(cfg, role), index)
		log.Host(host, "Starting instance %s", containerName)
		containerConfig := deploy.NewAppContainerConfig(cfg, cfg.Image, containerName, role, map[string]string{
			"azud.instance": strconv.Itoa(index),
		})
		if _, err := cm.Run(host, containerConfig); err != nil {
			return failWithCleanup(fmt.Errorf("failed to start %s: %w", containerName, err))
		}
		created = append(created, containerName)

		if deploy.IsProxyRole(role) {
			if cfg.Proxy.Healthcheck.GetReadinessPath() != "" {
				if err := deploy.WaitForContainerReady(cfg, podmanClient, sshClient, host, containerName); err != nil {
					return failWithCleanup(fmt.Errorf("instance %s is not ready: %w", containerName, err))
				}
			}
			if proxyHost == "" {
				return failWithCleanup(fmt.Errorf("proxy host is required to scale web role"))
			}
			upstream, err := scaleUpstreamForContainer(cm, host, containerName)
			if err != nil {
				return failWithCleanup(fmt.Errorf("failed to resolve upstream for %s: %w", containerName, err))
			}
			if err := pm.AddUpstream(host, proxyHost, upstream); err != nil {
				return failWithCleanup(fmt.Errorf("failed to register %s with proxy: %w", containerName, err))
			}
			registered[containerName] = upstream
		} else if err := cm.WaitRunning(host, containerName, cfg.Deploy.ReadinessDelay); err != nil {
			return failWithCleanup(fmt.Errorf("instance %s failed startup check: %w", containerName, err))
		}
		log.Host(host, "Instance %s started", containerName)
	}

	return nil
}

func scaleDown(cm *podman.ContainerManager, pm *proxy.Manager, host, role string, from, to int, log *output.Logger) error {
	proxyHost := cfg.Proxy.PrimaryHost()
	instances, err := listRoleInstances(cm, host, role)
	if err != nil {
		return fmt.Errorf("failed to enumerate instances: %w", err)
	}
	if to >= len(instances) {
		return nil
	}

	// Remove indexed instances from highest to lowest, keeping the stable
	// container until it is the final instance selected for scale-to-zero.
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Stable != instances[j].Stable {
			return !instances[i].Stable
		}
		return instances[i].Index > instances[j].Index
	})
	for _, instance := range instances[:len(instances)-to] {
		containerName := instance.Name
		log.Host(host, "Stopping instance %s", containerName)
		if deploy.IsProxyRole(role) {
			if proxyHost == "" {
				return fmt.Errorf("proxy host is required to scale down web role")
			}
			upstream, err := scaleUpstreamForContainer(cm, host, containerName)
			if err != nil {
				return fmt.Errorf("failed to resolve upstream for %s: %w", containerName, err)
			}
			if err := pm.RemoveUpstream(host, proxyHost, upstream); err != nil {
				return fmt.Errorf("refusing to stop still-routed instance %s: %w", containerName, err)
			}
			if cfg.Deploy.DrainTimeout > 0 {
				if err := pm.DrainUpstream(host, upstream, cfg.Deploy.DrainTimeout); err != nil {
					return fmt.Errorf("failed to drain %s: %w", containerName, err)
				}
			}
		}
		if err := cm.Remove(host, containerName, true); err != nil {
			return fmt.Errorf("failed to remove %s: %w", containerName, err)
		}
		log.Host(host, "Instance %s stopped", containerName)
	}

	return nil
}

func scaleUpstreamForContainer(cm *podman.ContainerManager, host, container string) (string, error) {
	if !cfg.UseHostPortUpstreams() {
		return fmt.Sprintf("%s:%d", container, cfg.Proxy.AppPort), nil
	}
	port, err := cm.HostPort(host, container, cfg.Proxy.AppPort)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("127.0.0.1:%d", port), nil
}
