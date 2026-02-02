package podman

import (
	"fmt"
	"strings"

	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/shell"
	"github.com/adriancarayol/azud/internal/ssh"
)

// Client executes Podman commands on remote hosts via SSH.
type Client struct {
	ssh *ssh.Client
}

func NewClient(sshClient *ssh.Client) *Client {
	return &Client{
		ssh: sshClient,
	}
}

func (c *Client) Execute(host string, args ...string) (*ssh.Result, error) {
	cmd := "podman " + strings.Join(shell.QuoteAll(args), " ")
	return c.ssh.Execute(host, cmd)
}

func (c *Client) ExecuteAll(hosts []string, args ...string) []*ssh.Result {
	cmd := "podman " + strings.Join(shell.QuoteAll(args), " ")
	return c.ssh.ExecuteParallel(hosts, cmd)
}

// ContainerConfig holds configuration for running a container.
type ContainerConfig struct {
	Name      string
	Image     string
	Command   []string
	Env       map[string]string
	SecretEnv []string // Secret env var names (resolved from secrets file)
	EnvFile   string   // Path to env file on remote host
	// EnvFileOptional controls whether a missing env file is tolerated.
	// When true, run command falls back to no env file if it's missing.
	EnvFileOptional bool
	Ports           []string // host:container or ip:host:container
	Volumes         []string // host:container or host:container:options
	Labels          map[string]string
	Network         string
	NetworkAliases  []string
	Networks        []string
	Memory          string // e.g., "512m"
	CPUs            string // e.g., "0.5"
	Restart         string // no, always, unless-stopped, on-failure[:max-retries]
	Detach          bool
	Remove          bool
	Pull            bool

	// Healthcheck
	HealthCmd         string
	HealthInterval    string
	HealthTimeout     string
	HealthRetries     int
	HealthStartPeriod string

	Options []string // Additional run options
}

func (c *ContainerConfig) BuildRunCommand() string {
	args := []string{"run"}

	if c.Detach {
		args = append(args, "-d")
	}

	if c.Remove {
		args = append(args, "--rm")
	}

	if c.Name != "" {
		args = append(args, "--name", shell.Quote(c.Name))
	}

	for key, value := range c.Env {
		args = append(args, "-e", shell.Quote(fmt.Sprintf("%s=%s", key, value)))
	}

	if c.EnvFile == "" {
		for _, key := range c.SecretEnv {
			if value, ok := config.GetSecret(key); ok && value != "" {
				args = append(args, "-e", shell.Quote(fmt.Sprintf("%s=%s", key, value)))
			} else {
				args = append(args, "-e", shell.Quote(key))
			}
		}
	}

	for _, port := range c.Ports {
		args = append(args, "-p", shell.Quote(port))
	}

	for _, vol := range c.Volumes {
		args = append(args, "-v", shell.Quote(vol))
	}

	for key, value := range c.Labels {
		args = append(args, "-l", shell.Quote(fmt.Sprintf("%s=%s", key, value)))
	}

	if c.Network != "" {
		args = append(args, "--network", shell.Quote(c.Network))
	}

	for _, alias := range c.NetworkAliases {
		args = append(args, "--network-alias", shell.Quote(alias))
	}

	if c.Memory != "" {
		args = append(args, "--memory", shell.Quote(c.Memory))
	}
	if c.CPUs != "" {
		args = append(args, "--cpus", shell.Quote(c.CPUs))
	}

	if c.Restart != "" {
		args = append(args, "--restart", shell.Quote(c.Restart))
	}

	if c.HealthCmd != "" {
		args = append(args, "--health-cmd", shell.Quote(c.HealthCmd))
		if c.HealthInterval != "" {
			args = append(args, "--health-interval", shell.Quote(c.HealthInterval))
		}
		if c.HealthTimeout != "" {
			args = append(args, "--health-timeout", shell.Quote(c.HealthTimeout))
		}
		if c.HealthRetries > 0 {
			args = append(args, "--health-retries", fmt.Sprintf("%d", c.HealthRetries))
		}
		if c.HealthStartPeriod != "" {
			args = append(args, "--health-start-period", shell.Quote(c.HealthStartPeriod))
		}
	}

	args = append(args, c.Options...)

	preImageArgs := make([]string, len(args))
	copy(preImageArgs, args)

	if len(c.Command) > 0 {
		args = append(args, shell.Quote(c.Image))
		args = append(args, shell.QuoteAll(c.Command)...)
	} else {
		args = append(args, shell.Quote(c.Image))
	}

	baseCmd := "podman " + strings.Join(args, " ")
	if c.EnvFile == "" {
		return baseCmd
	}

	// EnvFile may contain $HOME for shell expansion; use double quotes
	// to allow variable expansion while protecting against spaces.
	quotedEnvFile := fmt.Sprintf("\"%s\"", c.EnvFile)
	withEnvArgs := append(preImageArgs, "--env-file", quotedEnvFile, shell.Quote(c.Image))
	if len(c.Command) > 0 {
		withEnvArgs = append(withEnvArgs, shell.QuoteAll(c.Command)...)
	}
	withEnvCmd := "podman " + strings.Join(withEnvArgs, " ")

	if !c.EnvFileOptional {
		return withEnvCmd
	}

	// Prefer env-file if present; otherwise run without it.
	return fmt.Sprintf("if [ -f %s ]; then %s; else %s; fi", quotedEnvFile, withEnvCmd, baseCmd)
}

// ExecConfig holds configuration for executing a command in a container.
type ExecConfig struct {
	Container   string
	Command     []string
	Interactive bool
	TTY         bool
	User        string
	WorkDir     string
	Env         map[string]string
	Detach      bool
}

func (c *ExecConfig) BuildExecCommand() string {
	args := []string{"exec"}

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
		args = append(args, "-u", shell.Quote(c.User))
	}

	if c.WorkDir != "" {
		args = append(args, "-w", shell.Quote(c.WorkDir))
	}

	for key, value := range c.Env {
		args = append(args, "-e", shell.Quote(fmt.Sprintf("%s=%s", key, value)))
	}

	args = append(args, shell.Quote(c.Container))
	args = append(args, shell.QuoteAll(c.Command)...)

	return "podman " + strings.Join(args, " ")
}

// LogsConfig holds configuration for viewing container logs.
type LogsConfig struct {
	Container  string
	Follow     bool
	Tail       string
	Timestamps bool
	Since      string
	Until      string
}

func (c *LogsConfig) BuildLogsCommand() string {
	args := []string{"logs"}

	if c.Follow {
		args = append(args, "-f")
	}

	if c.Tail != "" {
		args = append(args, "--tail", shell.Quote(c.Tail))
	}

	if c.Timestamps {
		args = append(args, "-t")
	}

	if c.Since != "" {
		args = append(args, "--since", shell.Quote(c.Since))
	}

	if c.Until != "" {
		args = append(args, "--until", shell.Quote(c.Until))
	}

	args = append(args, shell.Quote(c.Container))

	return "podman " + strings.Join(args, " ")
}

type Info struct {
	ServerVersion     string
	ContainersTotal   int
	ContainersRunning int
	ContainersPaused  int
	ContainersStopped int
	Images            int
	Driver            string
	MemoryTotal       string
	CPUs              int
}

func (c *Client) GetInfo(host string) (*Info, error) {
	format := `{{.ServerVersion}}|{{.Containers}}|{{.ContainersRunning}}|{{.ContainersPaused}}|{{.ContainersStopped}}|{{.Images}}|{{.Driver}}|{{.MemTotal}}|{{.NCPU}}`
	result, err := c.Execute(host, "info", "--format", format)
	if err != nil {
		return nil, err
	}

	if result.ExitCode != 0 {
		return nil, fmt.Errorf("podman info failed: %s", result.Stderr)
	}

	info := &Info{}
	output := strings.Trim(result.Stdout, "'\n")
	parts := strings.Split(output, "|")
	if len(parts) >= 9 {
		info.ServerVersion = parts[0]
		_, _ = fmt.Sscanf(parts[1], "%d", &info.ContainersTotal)
		_, _ = fmt.Sscanf(parts[2], "%d", &info.ContainersRunning)
		_, _ = fmt.Sscanf(parts[3], "%d", &info.ContainersPaused)
		_, _ = fmt.Sscanf(parts[4], "%d", &info.ContainersStopped)
		_, _ = fmt.Sscanf(parts[5], "%d", &info.Images)
		info.Driver = parts[6]
		info.MemoryTotal = parts[7]
		_, _ = fmt.Sscanf(parts[8], "%d", &info.CPUs)
	}

	return info, nil
}

func (c *Client) Version(host string) (string, error) {
	result, err := c.Execute(host, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", err
	}

	if result.ExitCode != 0 {
		return "", fmt.Errorf("podman version failed: %s", result.Stderr)
	}

	return strings.Trim(result.Stdout, "'\n"), nil
}
