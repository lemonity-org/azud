package podman

import (
	"fmt"
	"strings"
	"time"

	"github.com/adriancarayol/azud/internal/ssh"
)

type Container struct {
	ID      string
	Name    string
	Image   string
	Status  string
	State   string
	Created time.Time
	Ports   []string
	Labels  map[string]string
}

// ContainerManager handles container lifecycle operations via Podman.
type ContainerManager struct {
	client *Client
}

func NewContainerManager(client *Client) *ContainerManager {
	return &ContainerManager{client: client}
}

func (m *ContainerManager) Run(host string, config *ContainerConfig) (string, error) {
	cmd := config.BuildRunCommand()
	result, err := m.client.ssh.Execute(host, cmd)
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to run container: %s", result.Stderr)
	}

	return strings.TrimSpace(result.Stdout), nil
}

func (m *ContainerManager) Start(host, container string) error {
	result, err := m.client.Execute(host, "start", container)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to start container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) Stop(host, container string, timeout int) error {
	args := []string{"stop"}
	if timeout > 0 {
		args = append(args, "-t", fmt.Sprintf("%d", timeout))
	}
	args = append(args, container)

	result, err := m.client.Execute(host, args...)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to stop container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) Remove(host, container string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, container)

	result, err := m.client.Execute(host, args...)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to remove container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) Restart(host, container string, timeout int) error {
	args := []string{"restart"}
	if timeout > 0 {
		args = append(args, "-t", fmt.Sprintf("%d", timeout))
	}
	args = append(args, container)

	result, err := m.client.Execute(host, args...)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to restart container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) Kill(host, container, signal string) error {
	args := []string{"kill"}
	if signal != "" {
		args = append(args, "-s", signal)
	}
	args = append(args, container)

	result, err := m.client.Execute(host, args...)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to kill container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) Exec(host string, config *ExecConfig) (*ssh.Result, error) {
	cmd := config.BuildExecCommand()
	return m.client.ssh.Execute(host, cmd)
}

func (m *ContainerManager) Logs(host string, config *LogsConfig) (*ssh.Result, error) {
	cmd := config.BuildLogsCommand()
	return m.client.ssh.Execute(host, cmd)
}

func (m *ContainerManager) List(host string, all bool, filters map[string]string) ([]Container, error) {
	args := []string{"ps", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.Status}}|{{.State}}|{{.Ports}}"}
	if all {
		args = append(args, "-a")
	}

	for key, value := range filters {
		args = append(args, "-f", fmt.Sprintf("%s=%s", key, value))
	}

	result, err := m.client.Execute(host, args...)
	if err != nil {
		return nil, err
	}

	if result.ExitCode != 0 {
		return nil, fmt.Errorf("failed to list containers: %s", result.Stderr)
	}

	var containers []Container
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for _, line := range lines {
		line = strings.Trim(line, "'")
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}

		container := Container{
			ID:     parts[0],
			Name:   parts[1],
			Image:  parts[2],
			Status: parts[3],
			State:  parts[4],
		}

		if len(parts) > 5 && parts[5] != "" {
			container.Ports = strings.Split(parts[5], ", ")
		}

		containers = append(containers, container)
	}

	return containers, nil
}

func (m *ContainerManager) Inspect(host, container string) (string, error) {
	result, err := m.client.Execute(host, "inspect", container)
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to inspect container: %s", result.Stderr)
	}

	return result.Stdout, nil
}

func (m *ContainerManager) Exists(host, container string) (bool, error) {
	result, err := m.client.Execute(host, "inspect", container, "--format", "{{.Id}}")
	if err != nil {
		return false, err
	}

	return result.ExitCode == 0, nil
}

func (m *ContainerManager) IsRunning(host, container string) (bool, error) {
	result, err := m.client.Execute(host, "inspect", container, "--format", "{{.State.Running}}")
	if err != nil {
		return false, err
	}

	if result.ExitCode != 0 {
		return false, nil
	}

	return strings.Contains(result.Stdout, "true"), nil
}

func (m *ContainerManager) WaitHealthy(host, container string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		result, err := m.client.Execute(host, "inspect", container, "--format", "{{.State.Health.Status}}")
		if err != nil {
			return err
		}

		status := strings.Trim(result.Stdout, "'\n")
		switch status {
		case "healthy":
			return nil
		case "unhealthy":
			return fmt.Errorf("container is unhealthy")
		}

		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("timeout waiting for container to become healthy")
}

func (m *ContainerManager) Rename(host, oldName, newName string) error {
	result, err := m.client.Execute(host, "rename", oldName, newName)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to rename container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) CopyTo(host, container, src, dest string) error {
	result, err := m.client.Execute(host, "cp", src, fmt.Sprintf("%s:%s", container, dest))
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to copy to container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) CopyFrom(host, container, src, dest string) error {
	result, err := m.client.Execute(host, "cp", fmt.Sprintf("%s:%s", container, src), dest)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to copy from container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) Prune(host string) (int, error) {
	result, err := m.client.Execute(host, "container", "prune", "-f")
	if err != nil {
		return 0, err
	}

	if result.ExitCode != 0 {
		return 0, fmt.Errorf("failed to prune containers: %s", result.Stderr)
	}

	lines := strings.Split(result.Stdout, "\n")
	count := 0
	for _, line := range lines {
		if len(line) == 64 || len(line) == 12 {
			count++
		}
	}

	return count, nil
}

func (m *ContainerManager) Stats(host, container string) (string, error) {
	result, err := m.client.Execute(host, "stats", container, "--no-stream", "--format",
		"CPU: {{.CPUPerc}} | Memory: {{.MemUsage}} | Net: {{.NetIO}} | Block: {{.BlockIO}}")
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to get stats: %s", result.Stderr)
	}

	return strings.Trim(result.Stdout, "'\n"), nil
}

func (m *ContainerManager) ConnectNetwork(host, container, network string) error {
	result, err := m.client.Execute(host, "network", "connect", network, container)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to connect to network: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) DisconnectNetwork(host, container, network string) error {
	result, err := m.client.Execute(host, "network", "disconnect", network, container)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to disconnect from network: %s", result.Stderr)
	}

	return nil
}
