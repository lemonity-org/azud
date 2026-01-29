package docker

import (
	"fmt"
	"strings"

	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/ssh"
)

// Client executes Docker commands on remote hosts via SSH
type Client struct {
	ssh *ssh.Client
}

// NewClient creates a new Docker client
func NewClient(sshClient *ssh.Client) *Client {
	return &Client{
		ssh: sshClient,
	}
}

// Execute runs a docker command on the specified host
func (c *Client) Execute(host string, args ...string) (*ssh.Result, error) {
	cmd := "docker " + strings.Join(args, " ")
	return c.ssh.Execute(host, cmd)
}

// ExecuteAll runs a docker command on multiple hosts in parallel
func (c *Client) ExecuteAll(hosts []string, args ...string) []*ssh.Result {
	cmd := "docker " + strings.Join(args, " ")
	return c.ssh.ExecuteParallel(hosts, cmd)
}

// ContainerConfig holds configuration for running a container
type ContainerConfig struct {
	// Container name
	Name string

	// Image to run
	Image string

	// Command to execute (optional)
	Command []string

	// Environment variables
	Env map[string]string

	// Secret environment variables (from secrets file)
	SecretEnv []string

	// Port mappings (host:container or ip:host:container)
	Ports []string

	// Volume mounts (host:container or host:container:options)
	Volumes []string

	// Labels
	Labels map[string]string

	// Network to connect to
	Network string

	// Additional networks to connect to
	Networks []string

	// Resource limits
	Memory string // e.g., "512m"
	CPUs   string // e.g., "0.5"

	// Restart policy
	Restart string // no, always, unless-stopped, on-failure[:max-retries]

	// Run in detached mode
	Detach bool

	// Remove container when it exits
	Remove bool

	// Pull image before running
	Pull bool

	// Healthcheck configuration
	HealthCmd      string
	HealthInterval string
	HealthTimeout  string
	HealthRetries  int

	// Additional docker run options
	Options []string
}

// BuildRunCommand builds a docker run command from the configuration
func (c *ContainerConfig) BuildRunCommand() string {
	var args []string
	args = append(args, "run")

	if c.Detach {
		args = append(args, "-d")
	}

	if c.Remove {
		args = append(args, "--rm")
	}

	if c.Name != "" {
		args = append(args, "--name", c.Name)
	}

	// Environment variables
	for key, value := range c.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
	}

	// Secret environment variables (resolved from secrets file)
	for _, key := range c.SecretEnv {
		if value, ok := config.GetSecret(key); ok && value != "" {
			args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
		} else {
			// Fallback: pass just the key, expecting env to be set on server
			args = append(args, "-e", key)
		}
	}

	// Port mappings
	for _, port := range c.Ports {
		args = append(args, "-p", port)
	}

	// Volume mounts
	for _, vol := range c.Volumes {
		args = append(args, "-v", vol)
	}

	// Labels
	for key, value := range c.Labels {
		args = append(args, "-l", fmt.Sprintf("%s=%s", key, value))
	}

	// Network
	if c.Network != "" {
		args = append(args, "--network", c.Network)
	}

	// Resource limits
	if c.Memory != "" {
		args = append(args, "--memory", c.Memory)
	}
	if c.CPUs != "" {
		args = append(args, "--cpus", c.CPUs)
	}

	// Restart policy
	if c.Restart != "" {
		args = append(args, "--restart", c.Restart)
	}

	// Healthcheck
	if c.HealthCmd != "" {
		// Quote the health command to prevent shell interpretation
		quotedCmd := fmt.Sprintf("'%s'", strings.ReplaceAll(c.HealthCmd, "'", "'\\''"))
		args = append(args, "--health-cmd", quotedCmd)
		if c.HealthInterval != "" {
			args = append(args, "--health-interval", c.HealthInterval)
		}
		if c.HealthTimeout != "" {
			args = append(args, "--health-timeout", c.HealthTimeout)
		}
		if c.HealthRetries > 0 {
			args = append(args, "--health-retries", fmt.Sprintf("%d", c.HealthRetries))
		}
	}

	// Additional options
	args = append(args, c.Options...)

	// Image
	args = append(args, c.Image)

	// Command
	if len(c.Command) > 0 {
		args = append(args, c.Command...)
	}

	return "docker " + strings.Join(args, " ")
}

// ExecConfig holds configuration for executing a command in a container
type ExecConfig struct {
	// Container name or ID
	Container string

	// Command to execute
	Command []string

	// Interactive mode (keep STDIN open)
	Interactive bool

	// Allocate a pseudo-TTY
	TTY bool

	// Run as user
	User string

	// Working directory
	WorkDir string

	// Environment variables
	Env map[string]string

	// Detached mode
	Detach bool
}

// BuildExecCommand builds a docker exec command from the configuration
func (c *ExecConfig) BuildExecCommand() string {
	var args []string
	args = append(args, "exec")

	if c.Interactive {
		args = append(args, "-i")
	}

	if c.TTY {
		args = append(args, "-t")
	}

	if c.Detach {
		args = append(args, "-d")
	}

	if c.User != "" {
		args = append(args, "-u", c.User)
	}

	if c.WorkDir != "" {
		args = append(args, "-w", c.WorkDir)
	}

	for key, value := range c.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
	}

	args = append(args, c.Container)
	args = append(args, c.Command...)

	return "docker " + strings.Join(args, " ")
}

// LogsConfig holds configuration for viewing container logs
type LogsConfig struct {
	// Container name or ID
	Container string

	// Follow log output
	Follow bool

	// Number of lines to show from the end
	Tail string

	// Show timestamps
	Timestamps bool

	// Show logs since timestamp or relative time
	Since string

	// Show logs until timestamp or relative time
	Until string
}

// BuildLogsCommand builds a docker logs command from the configuration
func (c *LogsConfig) BuildLogsCommand() string {
	var args []string
	args = append(args, "logs")

	if c.Follow {
		args = append(args, "-f")
	}

	if c.Tail != "" {
		args = append(args, "--tail", c.Tail)
	}

	if c.Timestamps {
		args = append(args, "-t")
	}

	if c.Since != "" {
		args = append(args, "--since", c.Since)
	}

	if c.Until != "" {
		args = append(args, "--until", c.Until)
	}

	args = append(args, c.Container)

	return "docker " + strings.Join(args, " ")
}

// Info represents Docker system info
type Info struct {
	ServerVersion  string
	ContainersTotal int
	ContainersRunning int
	ContainersPaused int
	ContainersStopped int
	Images         int
	Driver         string
	MemoryTotal    string
	CPUs           int
}

// GetInfo retrieves Docker system information from a host
func (c *Client) GetInfo(host string) (*Info, error) {
	format := `{{.ServerVersion}}|{{.Containers}}|{{.ContainersRunning}}|{{.ContainersPaused}}|{{.ContainersStopped}}|{{.Images}}|{{.Driver}}|{{.MemTotal}}|{{.NCPU}}`
	result, err := c.Execute(host, "info", "--format", fmt.Sprintf("'%s'", format))
	if err != nil {
		return nil, err
	}

	if result.ExitCode != 0 {
		return nil, fmt.Errorf("docker info failed: %s", result.Stderr)
	}

	info := &Info{}
	output := strings.Trim(result.Stdout, "'\n")
	parts := strings.Split(output, "|")
	if len(parts) >= 9 {
		info.ServerVersion = parts[0]
		fmt.Sscanf(parts[1], "%d", &info.ContainersTotal)
		fmt.Sscanf(parts[2], "%d", &info.ContainersRunning)
		fmt.Sscanf(parts[3], "%d", &info.ContainersPaused)
		fmt.Sscanf(parts[4], "%d", &info.ContainersStopped)
		fmt.Sscanf(parts[5], "%d", &info.Images)
		info.Driver = parts[6]
		info.MemoryTotal = parts[7]
		fmt.Sscanf(parts[8], "%d", &info.CPUs)
	}

	return info, nil
}

// Version returns the Docker version on a host
func (c *Client) Version(host string) (string, error) {
	result, err := c.Execute(host, "version", "--format", "'{{.Server.Version}}'")
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("docker version failed: %s", result.Stderr)
	}

	return strings.Trim(result.Stdout, "'\n"), nil
}
