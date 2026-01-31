package deploy

import (
	"fmt"
	"strings"
	"time"

	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/podman"
	"github.com/adriancarayol/azud/internal/ssh"
)

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
		Labels:  labels,
		Env:     make(map[string]string),
	}

	for key, value := range cfg.Env.Clear {
		containerCfg.Env[key] = value
	}

	containerCfg.SecretEnv = cfg.Env.Secret
	containerCfg.Volumes = cfg.Volumes

	// Use liveness probe for Podman HEALTHCHECK (continuous container health)
	livenessPath := cfg.Proxy.Healthcheck.GetLivenessPath()
	if livenessPath != "" {
		containerCfg.HealthCmd = fmt.Sprintf("curl -f http://localhost:%d%s || exit 1",
			cfg.Proxy.AppPort, livenessPath)
		containerCfg.HealthInterval = cfg.Proxy.Healthcheck.Interval
		containerCfg.HealthTimeout = cfg.Proxy.Healthcheck.Timeout
		containerCfg.HealthRetries = 3
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

	for time.Now().Before(deadline) {
		// Check Podman HEALTHCHECK status (liveness)
		result, err := podmanClient.Execute(host, "inspect", container, "--format", "'{{.State.Health.Status}}'")
		if err == nil && result.ExitCode == 0 {
			status := strings.Trim(result.Stdout, "'\n ")
			switch status {
			case "healthy":
				return nil
			case "unhealthy":
				return fmt.Errorf("container liveness check failed (unhealthy)")
			}
		}

		// Check readiness probe (can the container accept traffic?)
		if readinessPath != "" {
			checkCmd := fmt.Sprintf("podman exec %s curl -sf http://localhost:%d%s",
				container, cfg.Proxy.AppPort, readinessPath)
			result, err := sshClient.Execute(host, checkCmd)
			if err == nil && result.ExitCode == 0 {
				return nil
			}
		}

		time.Sleep(checkInterval)
	}

	return fmt.Errorf("timeout waiting for container to become ready")
}
