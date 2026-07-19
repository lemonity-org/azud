package deploy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/ssh"
)

// shellMetacharacters is the set of characters that indicate a command
// string contains shell operators and must be wrapped in sh -c.
const shellMetacharacters = "&|;><$`\"'\\"

// parseCommandArgs splits a command string into arguments suitable for
// ContainerConfig.Command. Simple commands are split on whitespace.
// Commands containing shell operators are wrapped in sh -c with proper
// single-quote escaping so they survive the remote SSH shell.
func parseCommandArgs(cmd string) []string {
	if strings.ContainsAny(cmd, shellMetacharacters) {
		return []string{"sh", "-c", cmd}
	}
	return strings.Fields(cmd)
}

// ParseCommandArgs exposes Azud's command-preserving parser to other workload
// types such as accessories.
func ParseCommandArgs(cmd string) []string {
	return parseCommandArgs(cmd)
}

// newPreDeployContainerConfig creates a minimal one-off container configuration
// for running a pre-deploy command (e.g., database migrations) from the new
// image. The container is created with --rm and runs in the foreground.
func newPreDeployContainerConfig(cfg *config.Config, image, name string) *podman.ContainerConfig {
	containerCfg := &podman.ContainerConfig{
		Name:    name,
		Image:   image,
		Remove:  true,
		Network: "azud",
		Labels: map[string]string{
			"azud.managed": "true",
			"azud.service": cfg.Service,
		},
		Env: make(map[string]string),
	}

	for key, value := range cfg.Env.Clear {
		containerCfg.Env[key] = value
	}

	containerCfg.SecretEnv = cfg.Env.Secret
	if len(containerCfg.SecretEnv) > 0 {
		containerCfg.EnvFile = config.RemoteSecretsPath(cfg)
	}

	return containerCfg
}

// RoleContainerName returns the stable container name for a service role. The
// web role retains the historical service name; every other role gets its own
// name so multiple roles can coexist on one host.
func RoleContainerName(cfg *config.Config, role string) string {
	if role == "" || role == "web" {
		return cfg.Service
	}
	return fmt.Sprintf("%s-%s", cfg.Service, role)
}

// IsProxyRole reports whether a role serves HTTP traffic through Caddy.
func IsProxyRole(role string) bool {
	return role == "" || role == "web"
}

// NewAppContainerConfig creates a standard role-aware application container
// configuration. The extra labels parameter allows callers to add
// deployment-specific labels (for example canary or scale markers).
//
// The Podman HEALTHCHECK is configured with the liveness probe path
// (healthcheck.liveness_path, falling back to healthcheck.path). This
// probe runs continuously inside the container and determines if it is
// still functioning. The readiness probe is checked separately during
// deployment to gate proxy registration.
func NewAppContainerConfig(cfg *config.Config, image, name, role string, extraLabels map[string]string) *podman.ContainerConfig {
	labels := make(map[string]string)
	roleConfig, hasRole := cfg.Servers[role]
	if hasRole {
		for key, value := range roleConfig.Labels {
			labels[key] = value
		}
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	// Managed labels are applied last so configuration cannot spoof ownership.
	labels["azud.managed"] = "true"
	labels["azud.service"] = cfg.Service
	if role != "" {
		labels["azud.role"] = role
	}

	aliases := []string{RoleContainerName(cfg, role)}
	containerCfg := &podman.ContainerConfig{
		Name:    name,
		Image:   image,
		Detach:  true,
		Restart: "unless-stopped",
		Network: "azud",
		// Register the stable role name as a network alias so DNS continues to
		// resolve while a temporary deployment container is renamed.
		NetworkAliases: aliases,
		Labels:         labels,
		Env:            make(map[string]string),
	}
	if IsProxyRole(role) && cfg.UseHostPortUpstreams() {
		containerCfg.Ports = append(containerCfg.Ports, fmt.Sprintf("127.0.0.1::%d", cfg.Proxy.AppPort))
	}

	for key, value := range cfg.Env.Clear {
		containerCfg.Env[key] = value
	}
	if hasRole {
		for key, value := range roleConfig.Env {
			containerCfg.Env[key] = value
		}
		containerCfg.Memory = roleConfig.Options["memory"]
		containerCfg.CPUs = roleConfig.Options["cpus"]
		if roleConfig.Cmd != "" {
			containerCfg.Command = parseCommandArgs(roleConfig.Cmd)
		}
	}

	containerCfg.SecretEnv = cfg.Env.Secret
	if len(containerCfg.SecretEnv) > 0 {
		containerCfg.EnvFile = config.RemoteSecretsPath(cfg)
	}
	containerCfg.Volumes = cfg.Volumes

	// HTTP liveness/readiness settings only belong to the proxy-serving role.
	if !IsProxyRole(role) {
		return containerCfg
	}

	// Use liveness probe for Podman HEALTHCHECK (continuous container health)
	livenessCmd := LivenessCommand(cfg)
	if livenessCmd != "" {
		containerCfg.HealthCmd = livenessCmd
		containerCfg.HealthInterval = cfg.Proxy.Healthcheck.Interval
		containerCfg.HealthTimeout = cfg.Proxy.Healthcheck.Timeout
		containerCfg.HealthRetries = 3
		if cfg.Deploy.DeployTimeout > 0 {
			containerCfg.HealthStartPeriod = cfg.Deploy.DeployTimeout.String()
		}
	}

	return containerCfg
}

// newAppContainerConfig retains an internal shorthand for web-only callers.
func newAppContainerConfig(cfg *config.Config, image, name string, extraLabels map[string]string) *podman.ContainerConfig {
	return NewAppContainerConfig(cfg, image, name, "web", extraLabels)
}

// waitForContainerHealthy polls a container's health status and also
// attempts a direct HTTP readiness check until the container is ready to
// accept traffic, times out, or is reported unhealthy.
//
// Two probes are involved:
//   - Liveness probe (Podman HEALTHCHECK): checks if the container process
//     is alive. A status of "unhealthy" from Podman is a hard failure.
//   - Readiness probe (direct HTTP check): checks if the container can
//     accept traffic. This gates proxy registration during deployment.
//
// When a readiness path is configured, only that readiness probe can admit
// the container to traffic. Liveness remains an independent hard-failure
// signal and cannot make a not-yet-ready container pass.
func waitForContainerHealthy(cfg *config.Config, podmanClient *podman.Client, sshClient *ssh.Client, host, container string) error {
	timeout := cfg.Deploy.DeployTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	deadline := time.Now().Add(timeout)
	checkInterval := 2 * time.Second

	// Use readiness path for deployment checks
	readinessPath := cfg.Proxy.Healthcheck.GetReadinessPath()
	readinessCandidates := BuildHTTPCheckExecCandidates(container, cfg.Proxy.AppPort, readinessPath)
	readinessHelper := BuildHTTPCheckHelperCommand(container, cfg.Proxy.AppPort, readinessPath, cfg.Proxy.Healthcheck.HelperImage, cfg.Proxy.Healthcheck.HelperPull)
	livenessEnabled := LivenessCommand(cfg) != ""

	for time.Now().Before(deadline) {
		livenessHealthy := !livenessEnabled
		// Check Podman HEALTHCHECK status (liveness)
		if livenessEnabled {
			result, err := podmanClient.Execute(host, "inspect", container, "--format", "'{{.State.Health.Status}}'")
			if err == nil && result.ExitCode == 0 {
				status := strings.Trim(result.Stdout, "'\n ")
				switch status {
				case "healthy":
					livenessHealthy = true
				case "unhealthy":
					unsupported := healthcheckUnsupported(podmanClient, host, container)
					if !unsupported {
						return fmt.Errorf("container liveness check failed (unhealthy)")
					}
					if readinessPath == "" {
						return fmt.Errorf("container liveness check failed and readiness path is not configured")
					}
				}
			}
		}

		// Check readiness probe (can the container accept traffic?)
		readinessHealthy := false
		if readinessPath != "" {
			readinessHealthy = readinessProbe(sshClient, host, readinessCandidates, readinessHelper)
		}
		if probeAdmitsTraffic(readinessPath != "", readinessHealthy, livenessHealthy) {
			return nil
		}

		time.Sleep(checkInterval)
	}

	return fmt.Errorf("timeout waiting for container to become ready")
}

// probeAdmitsTraffic centralizes the deployment gate. Once readiness is
// configured, liveness alone can never admit a container to the proxy.
func probeAdmitsTraffic(readinessConfigured, readinessHealthy, livenessHealthy bool) bool {
	if readinessConfigured {
		return readinessHealthy
	}
	return livenessHealthy
}

// WaitForContainerReady exposes readiness checks for callers outside deploy.
func WaitForContainerReady(cfg *config.Config, podmanClient *podman.Client, sshClient *ssh.Client, host, container string) error {
	return waitForContainerHealthy(cfg, podmanClient, sshClient, host, container)
}

func readinessProbe(sshClient *ssh.Client, host string, candidates []string, helperCmd string) bool {
	unsupported := true

	for _, cmd := range candidates {
		result, err := sshClient.Execute(host, cmd)
		if err == nil && result.ExitCode == 0 {
			return true
		}

		if err != nil || !commandNotFound(result) {
			unsupported = false
		}
	}

	if (len(candidates) == 0 || unsupported) && helperCmd != "" {
		result, err := sshClient.Execute(host, helperCmd)
		if err == nil && result.ExitCode == 0 {
			return true
		}
	}

	return false
}

func healthcheckUnsupported(podmanClient *podman.Client, host, container string) bool {
	result, err := podmanClient.Execute(host, "inspect", container, "--format", "'{{json .State.Health}}'")
	if err != nil || result.ExitCode != 0 {
		return false
	}

	raw := strings.Trim(strings.TrimSpace(result.Stdout), "'")
	if raw == "" || raw == "null" {
		return false
	}

	var state struct {
		Log []struct {
			ExitCode int    `json:"ExitCode"`
			Output   string `json:"Output"`
		} `json:"Log"`
	}

	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return false
	}
	if len(state.Log) == 0 {
		return false
	}

	last := state.Log[len(state.Log)-1]
	if last.ExitCode == 126 || last.ExitCode == 127 {
		return true
	}

	return outputIndicatesCommandNotFound(last.Output)
}

func outputIndicatesCommandNotFound(output string) bool {
	msg := strings.ToLower(output)
	if strings.Contains(msg, "not found") {
		return true
	}
	if strings.Contains(msg, "executable file not found") {
		return true
	}
	if strings.Contains(msg, "no such file or directory") {
		return true
	}
	return false
}

func commandNotFound(result *ssh.Result) bool {
	if result == nil {
		return false
	}

	if result.ExitCode != 126 && result.ExitCode != 127 {
		return false
	}

	return outputIndicatesCommandNotFound(result.Stdout + result.Stderr)
}
