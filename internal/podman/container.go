package podman

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/lemonity-org/azud/internal/ssh"
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

// SharedNetwork is the network Azud's containers reach each other on.
const SharedNetwork = "azud"

// ContainerManager handles container lifecycle operations via Podman.
type ContainerManager struct {
	client *Client
}

func NewContainerManager(client *Client) *ContainerManager {
	return &ContainerManager{client: client}
}

func (m *ContainerManager) Run(host string, config *ContainerConfig) (string, error) {
	return m.run(host, config, nil)
}

// RunWithStdin starts a foreground container with stdin attached without
// exposing the streamed content in the remote command line.
func (m *ContainerManager) RunWithStdin(host string, config *ContainerConfig, stdin io.Reader) (string, error) {
	copy := *config
	copy.Stdin = true
	return m.run(host, &copy, stdin)
}

func (m *ContainerManager) run(host string, config *ContainerConfig, stdin io.Reader) (string, error) {
	if err := ValidateOptions(config.Options); err != nil {
		return "", err
	}
	if config.Network == SharedNetwork {
		if err := m.ensureSharedNetwork(host); err != nil {
			return "", err
		}
	}
	cmd := config.BuildRunCommand()
	cmd = m.client.RewriteCommand(cmd)
	var result *ssh.Result
	var err error
	if stdin == nil {
		result, err = m.client.ssh.Execute(host, cmd)
	} else {
		result, err = m.client.ssh.ExecuteWithStdin(host, cmd, stdin)
	}
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to run container: %s", result.Stderr)
	}
	return strings.TrimSpace(result.Stdout), nil
}

// Create validates and creates a container without starting it.
func (m *ContainerManager) Create(host string, config *ContainerConfig) (string, error) {
	if err := ValidateOptions(config.Options); err != nil {
		return "", err
	}
	cmd := config.BuildCreateCommand()
	cmd = m.client.RewriteCommand(cmd)
	result, err := m.client.ssh.Execute(host, cmd)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to create container: %s", result.Stderr)
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
	cmd = m.client.RewriteCommand(cmd)
	return m.client.ssh.Execute(host, cmd)
}

func (m *ContainerManager) Logs(host string, config *LogsConfig) (*ssh.Result, error) {
	cmd := config.BuildLogsCommand()
	cmd = m.client.RewriteCommand(cmd)
	return m.client.ssh.Execute(host, cmd)
}

func (m *ContainerManager) LogsStream(host string, config *LogsConfig, stdout, stderr io.Writer) error {
	cmd := m.client.RewriteCommand(config.BuildLogsCommand())
	return m.client.ssh.ExecuteStream(host, cmd, stdout, stderr)
}

func (m *ContainerManager) ExecInteractive(host string, config *ExecConfig, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := m.client.RewriteCommand(config.BuildExecCommand())
	return m.client.ssh.ExecuteIO(host, cmd, stdin, stdout, stderr, config.TTY)
}

func (m *ContainerManager) List(host string, all bool, filters map[string]string) ([]Container, error) {
	args := []string{"ps", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.Status}}|{{.State}}|{{.Ports}}|{{json .Labels}}"}
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

		parts := strings.SplitN(line, "|", 7)
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
		if len(parts) > 6 && parts[6] != "" {
			if err := json.Unmarshal([]byte(parts[6]), &container.Labels); err != nil {
				return nil, fmt.Errorf("failed to parse labels for container %s: %w", container.Name, err)
			}
		}

		containers = append(containers, container)
	}

	return containers, nil
}

func (m *ContainerManager) Inspect(host, container string) (string, error) {
	result, err := m.client.Execute(host, "container", "inspect", container)
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("failed to inspect container: %s", result.Stderr)
	}

	return result.Stdout, nil
}

// IsNoSuchContainer reports whether err is a container runtime's "container
// does not exist" error. Podman prints it in lowercase ("no container with
// name or ID ... found: no such container") and Docker capitalizes it, so the
// match is case-insensitive.
func IsNoSuchContainer(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such container") ||
		strings.Contains(message, "no container with name or id")
}

func (m *ContainerManager) Exists(host, container string) (bool, error) {
	result, err := m.client.Execute(host, "container", "inspect", container, "--format", "{{.Id}}")
	if err != nil {
		return false, err
	}

	return result.ExitCode == 0, nil
}

func (m *ContainerManager) IsRunning(host, container string) (bool, error) {
	result, err := m.client.Execute(host, "container", "inspect", container, "--format", "{{.State.Running}}")
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
		result, err := m.client.Execute(host, "container", "inspect", container, "--format", "{{.State.Health.Status}}")
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

// HasHealthcheck checks whether the container has a Podman HEALTHCHECK defined.
func (m *ContainerManager) HasHealthcheck(host, container string) (bool, error) {
	result, err := m.client.Execute(host, "container", "inspect", container, "--format", "{{len .Config.Healthcheck.Test}}")
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	count := strings.TrimSpace(result.Stdout)
	return count != "" && count != "0", nil
}

// ensureSharedNetwork creates the shared network if this Podman store lacks it.
// Only `azud setup` and `azud systemd enable` used to create it, so a server set
// up any other way failed its first deploy with "network not found".
func (m *ContainerManager) ensureSharedNetwork(host string) error {
	result, err := m.client.Execute(host, "network", "create", "--ignore", SharedNetwork)
	if err != nil {
		return fmt.Errorf("failed to create network %s: %w", SharedNetwork, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to create network %s: %s", SharedNetwork, result.Stderr)
	}
	return nil
}

// WaitRunning waits for a brief stabilization period and then verifies the
// container is still running and was not restarted during it. This catches
// containers that crash on start (e.g., bad config, missing env vars). Roles and
// accessories run with a restart policy, so a crashing container comes straight
// back, and looking only at whether it is running can catch it between two
// crashes. Restarts from before the period do not count: an accessory that is
// already running may have crashed and recovered long ago.
func (m *ContainerManager) WaitRunning(host, container string, stabilize time.Duration) error {
	if stabilize <= 0 {
		stabilize = 5 * time.Second
	}
	before, err := m.runState(host, container)
	if err != nil {
		return fmt.Errorf("failed to check container status: %w", err)
	}
	time.Sleep(stabilize)

	after, err := m.runState(host, container)
	if err != nil {
		return fmt.Errorf("failed to check container status: %w", err)
	}
	if after == nil {
		return fmt.Errorf("container exited shortly after start (exit code: unknown)")
	}
	if before != nil && after.restarts > before.restarts {
		return fmt.Errorf("container crashed shortly after start and was restarted %d time(s) (last exit code: %s)", after.restarts-before.restarts, after.exitCode)
	}
	if !after.running {
		return fmt.Errorf("container exited shortly after start (exit code: %s)", after.exitCode)
	}
	return nil
}

// containerRunState is what WaitRunning reads from an inspect.
type containerRunState struct {
	running  bool
	restarts int
	exitCode string
}

// runState returns nil if the container cannot be inspected, such as when it no
// longer exists.
func (m *ContainerManager) runState(host, container string) (*containerRunState, error) {
	result, err := m.client.Execute(host, "container", "inspect", container, "--format", "{{.State.Running}} {{.RestartCount}} {{.State.ExitCode}}")
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, nil
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) != 3 {
		return nil, fmt.Errorf("unexpected inspect output %q", result.Stdout)
	}
	restarts, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("unexpected inspect output %q", result.Stdout)
	}
	return &containerRunState{running: fields[0] == "true", restarts: restarts, exitCode: fields[2]}, nil
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
	result, err := m.client.Execute(host, "cp", src, fmt.Sprintf("%s:%s", container, dest)) // safe: Execute quotes each argv element
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to copy to container: %s", result.Stderr)
	}

	return nil
}

func (m *ContainerManager) CopyFrom(host, container, src, dest string) error {
	result, err := m.client.Execute(host, "cp", fmt.Sprintf("%s:%s", container, src), dest) // safe: Execute quotes each argv element
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

// HostPort resolves the published host port for containerPort/tcp.
func (m *ContainerManager) HostPort(host, container string, containerPort int) (int, error) {
	if containerPort <= 0 {
		return 0, fmt.Errorf("invalid container port: %d", containerPort)
	}
	portSpec := fmt.Sprintf("%d/tcp", containerPort)
	result, err := m.client.Execute(host, "port", container, portSpec)
	if err != nil {
		return 0, err
	}
	if result.ExitCode != 0 {
		return 0, fmt.Errorf("failed to inspect container port: %s", result.Stderr)
	}

	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_, port, parseErr := parseHostPort(line)
		if parseErr == nil {
			return port, nil
		}
	}

	return 0, fmt.Errorf("no published host port found for %s", portSpec)
}

func parseHostPort(value string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(value)
	if err != nil {
		sep := strings.LastIndex(value, ":")
		if sep < 0 || sep == len(value)-1 {
			return "", 0, err
		}
		host = value[:sep]
		portStr = value[sep+1:]
	}
	port, convErr := strconv.Atoi(portStr)
	if convErr != nil {
		return "", 0, convErr
	}
	return host, port, nil
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
