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

// newAppContainerConfig creates a standard container configuration from the
// application config. The extra labels parameter allows callers to add
// deployment-specific labels (e.g., canary markers).
//
// The Podman HEALTHCHECK is configured with the liveness probe path
// (healthcheck.liveness_path, falling back to healthcheck.path). This
// probe runs continuously inside the container and determines if it is
// still functioning. The readiness probe is checked separately during
// deployment to gate proxy registration.
func newAppContainerConfig(cfg *config.Config, image, name string, extraLabels map[string]string) *podman.ContainerConfig {
	labels := map[string]string{
		"azud.managed": "true",
		"azud.service": cfg.Service,
	}
	for k, v := range extraLabels {
		labels[k] = v
	}

	containerCfg := &podman.ContainerConfig{
		Name:    name,
		Image:   image,
		Detach:  true,
		Restart: "unless-stopped",
		Network: "azud",
		// Register the service name as a network alias so DNS resolves
		// regardless of the actual container name. This is needed because
		// podman rename does not update aardvark-dns entries.
		NetworkAliases: []string{cfg.Service},
		Labels:         labels,
		Env:            make(map[string]string),
	}

	for key, value := range cfg.Env.Clear {
		containerCfg.Env[key] = value
	}

	containerCfg.SecretEnv = cfg.Env.Secret
	if len(containerCfg.SecretEnv) > 0 {
		containerCfg.EnvFile = config.RemoteSecretsPath(cfg)
	}
	containerCfg.Volumes = cfg.Volumes

	// Use liveness probe for Podman HEALTHCHECK (continuous container health)
	livenessCmd := LivenessCommand(cfg)
	if livenessCmd != "" {
		containerCfg.HealthCmd = livenessCmd
		containerCfg.HealthInterval = cfg.Proxy.Healthcheck.Interval
		containerCfg.HealthTimeout = cfg.Proxy.Healthcheck.Timeout
		containerCfg.HealthRetries = 3
		// Give the app time to start before health check failures count.
		// Uses the deploy timeout as the start period so the container
		// stays in "starting" state during the entire deployment window
		// and the readiness probe gates registration instead.
		if cfg.Deploy.DeployTimeout > 0 {
			containerCfg.HealthStartPeriod = cfg.Deploy.DeployTimeout.String()
		}
	}

	return containerCfg
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
// The function succeeds when EITHER the Podman health status is "healthy"
// OR the readiness probe responds successfully.
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
		// Check Podman HEALTHCHECK status (liveness)
		if livenessEnabled {
			result, err := podmanClient.Execute(host, "inspect", container, "--format", "'{{.State.Health.Status}}'")
			if err == nil && result.ExitCode == 0 {
				status := strings.Trim(result.Stdout, "'\n ")
				switch status {
				case "healthy":
					return nil
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
		if readinessPath != "" {
			if ok := readinessProbe(sshClient, host, readinessCandidates, readinessHelper); ok {
				return nil
			}
		}

		time.Sleep(checkInterval)
	}

	return fmt.Errorf("timeout waiting for container to become ready")
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
