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

	if cfg.Proxy.Healthcheck.Path != "" {
		containerCfg.HealthCmd = fmt.Sprintf("curl -f http://localhost:%d%s || exit 1",
			cfg.Proxy.AppPort, cfg.Proxy.Healthcheck.Path)
		containerCfg.HealthInterval = cfg.Proxy.Healthcheck.Interval
		containerCfg.HealthTimeout = cfg.Proxy.Healthcheck.Timeout
		containerCfg.HealthRetries = 3
	}

	return containerCfg
}

// waitForContainerHealthy polls a container's health status and also
// attempts a direct HTTP health check until the container becomes healthy,
// times out, or is reported unhealthy.
func waitForContainerHealthy(cfg *config.Config, podmanClient *podman.Client, sshClient *ssh.Client, host, container string) error {
	timeout := cfg.Deploy.DeployTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	deadline := time.Now().Add(timeout)
	checkInterval := 2 * time.Second

	for time.Now().Before(deadline) {
		result, err := podmanClient.Execute(host, "inspect", container, "--format", "'{{.State.Health.Status}}'")
		if err == nil && result.ExitCode == 0 {
			status := strings.Trim(result.Stdout, "'\n ")
			switch status {
			case "healthy":
				return nil
			case "unhealthy":
				return fmt.Errorf("container is unhealthy")
			}
		}

		healthPath := cfg.Proxy.Healthcheck.Path
		if healthPath != "" {
			checkCmd := fmt.Sprintf("podman exec %s curl -sf http://localhost:%d%s",
				container, cfg.Proxy.AppPort, healthPath)
			result, err := sshClient.Execute(host, checkCmd)
			if err == nil && result.ExitCode == 0 {
				return nil
			}
		}

		time.Sleep(checkInterval)
	}

	return fmt.Errorf("timeout waiting for container to become healthy")
}
