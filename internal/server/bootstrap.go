package server

import (
	"fmt"
	"strings"

	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/ssh"
)

// Bootstrapper handles server setup operations
type Bootstrapper struct {
	sshClient *ssh.Client
	log       *output.Logger
}

// NewBootstrapper creates a new server bootstrapper
func NewBootstrapper(sshClient *ssh.Client, log *output.Logger) *Bootstrapper {
	if log == nil {
		log = output.DefaultLogger
	}
	return &Bootstrapper{
		sshClient: sshClient,
		log:       log,
	}
}

// Bootstrap sets up a server with Docker and required components
func (b *Bootstrapper) Bootstrap(host string) error {
	b.log.Host(host, "Starting bootstrap...")

	// Detect OS
	osInfo, err := b.detectOS(host)
	if err != nil {
		return fmt.Errorf("failed to detect OS: %w", err)
	}
	b.log.Host(host, "Detected OS: %s", osInfo.Name)

	// Check if Docker is already installed
	dockerInstalled, err := b.isDockerInstalled(host)
	if err != nil {
		return fmt.Errorf("failed to check Docker: %w", err)
	}

	if dockerInstalled {
		b.log.HostSuccess(host, "Docker already installed")
	} else {
		// Install Docker
		b.log.Host(host, "Installing Docker...")
		if err := b.installDocker(host, osInfo); err != nil {
			return fmt.Errorf("failed to install Docker: %w", err)
		}
		b.log.HostSuccess(host, "Docker installed")
	}

	// Configure Docker daemon
	b.log.Host(host, "Configuring Docker daemon...")
	if err := b.configureDocker(host); err != nil {
		return fmt.Errorf("failed to configure Docker: %w", err)
	}

	// Create azud network if it doesn't exist
	b.log.Host(host, "Setting up azud network...")
	if err := b.createNetwork(host); err != nil {
		return fmt.Errorf("failed to create network: %w", err)
	}

	b.log.HostSuccess(host, "Bootstrap complete")
	return nil
}

// BootstrapAll bootstraps multiple servers in parallel
func (b *Bootstrapper) BootstrapAll(hosts []string) error {
	b.log.Header("Bootstrapping %d server(s)", len(hosts))

	results := make(chan error, len(hosts))

	for _, host := range hosts {
		go func(h string) {
			results <- b.Bootstrap(h)
		}(host)
	}

	var errors []string
	for range hosts {
		if err := <-results; err != nil {
			errors = append(errors, err.Error())
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("bootstrap failed on some hosts: %s", strings.Join(errors, "; "))
	}

	b.log.Success("All servers bootstrapped successfully")
	return nil
}

// OSInfo holds operating system information
type OSInfo struct {
	Name    string
	Version string
	ID      string
	Family  string // debian, rhel, etc.
}

// detectOS detects the operating system on the remote host
func (b *Bootstrapper) detectOS(host string) (*OSInfo, error) {
	// Try /etc/os-release first (most Linux distros)
	result, err := b.sshClient.Execute(host, "cat /etc/os-release 2>/dev/null || true")
	if err != nil {
		return nil, err
	}

	info := &OSInfo{}

	if result.Stdout != "" {
		lines := strings.Split(result.Stdout, "\n")
		for _, line := range lines {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := parts[0]
			value := strings.Trim(parts[1], "\"")

			switch key {
			case "ID":
				info.ID = value
			case "NAME":
				info.Name = value
			case "VERSION_ID":
				info.Version = value
			case "ID_LIKE":
				info.Family = value
			}
		}
	}

	// Determine family if not set
	if info.Family == "" {
		switch info.ID {
		case "ubuntu", "debian", "linuxmint", "pop":
			info.Family = "debian"
		case "centos", "rhel", "fedora", "rocky", "almalinux", "amazon":
			info.Family = "rhel"
		case "alpine":
			info.Family = "alpine"
		default:
			info.Family = info.ID
		}
	}

	if info.Name == "" {
		info.Name = "Unknown Linux"
	}

	return info, nil
}

// isDockerInstalled checks if Docker is installed and running
func (b *Bootstrapper) isDockerInstalled(host string) (bool, error) {
	result, err := b.sshClient.Execute(host, "docker --version 2>/dev/null && docker info >/dev/null 2>&1 && echo 'ok'")
	if err != nil {
		return false, nil // Docker not installed or not running
	}
	return strings.Contains(result.Stdout, "ok"), nil
}

// installDocker installs Docker on the remote host
func (b *Bootstrapper) installDocker(host string, osInfo *OSInfo) error {
	var installCmd string

	switch osInfo.Family {
	case "debian":
		installCmd = b.getDebianDockerInstall()
	case "rhel":
		installCmd = b.getRHELDockerInstall()
	case "alpine":
		installCmd = b.getAlpineDockerInstall()
	default:
		// Try the convenience script as fallback
		installCmd = b.getConvenienceScriptInstall()
	}

	result, err := b.sshClient.Execute(host, installCmd)
	if err != nil {
		return fmt.Errorf("docker installation failed: %w", err)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("docker installation failed: %s", result.Stderr)
	}

	// Start and enable Docker
	startCmd := "systemctl start docker && systemctl enable docker"
	result, err = b.sshClient.Execute(host, startCmd)
	if err != nil {
		return fmt.Errorf("failed to start Docker: %w", err)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to start Docker: %s", result.Stderr)
	}

	return nil
}

// getDebianDockerInstall returns the Docker installation command for Debian/Ubuntu
func (b *Bootstrapper) getDebianDockerInstall() string {
	return `
set -e
apt-get update
apt-get install -y ca-certificates curl gnupg
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/$(. /etc/os-release && echo "$ID")/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg
echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/$(. /etc/os-release && echo "$ID") \
  $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
  tee /etc/apt/sources.list.d/docker.list > /dev/null
apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
`
}

// getRHELDockerInstall returns the Docker installation command for RHEL/CentOS
func (b *Bootstrapper) getRHELDockerInstall() string {
	return `
set -e
yum install -y yum-utils
yum-config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
yum install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
`
}

// getAlpineDockerInstall returns the Docker installation command for Alpine
func (b *Bootstrapper) getAlpineDockerInstall() string {
	return `
set -e
apk add --update docker docker-cli-compose
rc-update add docker boot
`
}

// getConvenienceScriptInstall returns the convenience script installation
func (b *Bootstrapper) getConvenienceScriptInstall() string {
	return `
set -e
curl -fsSL https://get.docker.com | sh
`
}

// configureDocker configures the Docker daemon
func (b *Bootstrapper) configureDocker(host string) error {
	// Create daemon.json with optimized settings
	daemonConfig := `{
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "10m",
    "max-file": "3"
  },
  "storage-driver": "overlay2",
  "live-restore": true
}`

	// Write daemon.json
	cmd := fmt.Sprintf(`mkdir -p /etc/docker && cat > /etc/docker/daemon.json << 'EOF'
%s
EOF`, daemonConfig)

	result, err := b.sshClient.Execute(host, cmd)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to write daemon.json: %s", result.Stderr)
	}

	// Reload Docker
	result, err = b.sshClient.Execute(host, "systemctl reload docker 2>/dev/null || systemctl restart docker")
	if err != nil {
		return err
	}

	return nil
}

// createNetwork creates the azud Docker network
func (b *Bootstrapper) createNetwork(host string) error {
	// Check if network exists
	result, err := b.sshClient.Execute(host, "docker network inspect azud >/dev/null 2>&1 && echo 'exists'")
	if err != nil {
		return err
	}

	if strings.Contains(result.Stdout, "exists") {
		b.log.Debug("Network azud already exists on %s", host)
		return nil
	}

	// Create network
	result, err = b.sshClient.Execute(host, "docker network create azud")
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to create network: %s", result.Stderr)
	}

	return nil
}

// CheckDocker checks Docker status on a host
func (b *Bootstrapper) CheckDocker(host string) (*DockerStatus, error) {
	status := &DockerStatus{Host: host}

	// Check if Docker is installed
	result, err := b.sshClient.Execute(host, "docker --version")
	if err != nil || result.ExitCode != 0 {
		status.Installed = false
		return status, nil
	}
	status.Installed = true
	status.Version = strings.TrimSpace(result.Stdout)

	// Check if Docker is running
	result, err = b.sshClient.Execute(host, "docker info --format '{{.ServerVersion}}'")
	if err != nil || result.ExitCode != 0 {
		status.Running = false
		return status, nil
	}
	status.Running = true

	// Get container count
	result, err = b.sshClient.Execute(host, "docker ps -q | wc -l")
	if err == nil && result.ExitCode == 0 {
		fmt.Sscanf(strings.TrimSpace(result.Stdout), "%d", &status.ContainerCount)
	}

	return status, nil
}

// DockerStatus holds Docker status information
type DockerStatus struct {
	Host           string
	Installed      bool
	Running        bool
	Version        string
	ContainerCount int
}

// ExecuteOnAll executes a command on all hosts
func (b *Bootstrapper) ExecuteOnAll(hosts []string, cmd string) []*ssh.Result {
	return b.sshClient.ExecuteParallel(hosts, cmd)
}
