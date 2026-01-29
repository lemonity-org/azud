package server

import (
	"fmt"
	"strings"

	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/ssh"
)

// Bootstrapper installs Podman and configures remote servers for deployment.
type Bootstrapper struct {
	sshClient *ssh.Client
	log       *output.Logger
}

func NewBootstrapper(sshClient *ssh.Client, log *output.Logger) *Bootstrapper {
	if log == nil {
		log = output.DefaultLogger
	}
	return &Bootstrapper{
		sshClient: sshClient,
		log:       log,
	}
}

// Bootstrap installs Podman, configures the network backend, and creates
// the azud network on the given host.
func (b *Bootstrapper) Bootstrap(host string) error {
	b.log.Host(host, "Starting bootstrap...")

	// Detect OS
	osInfo, err := b.detectOS(host)
	if err != nil {
		return fmt.Errorf("failed to detect OS: %w", err)
	}
	b.log.Host(host, "Detected OS: %s", osInfo.Name)

	// Check if Podman is already installed
	podmanInstalled, err := b.isPodmanInstalled(host)
	if err != nil {
		return fmt.Errorf("failed to check Podman: %w", err)
	}

	if podmanInstalled {
		b.log.HostSuccess(host, "Podman already installed")
	} else {
		// Install Podman
		b.log.Host(host, "Installing Podman...")
		if err := b.installPodman(host, osInfo); err != nil {
			return fmt.Errorf("failed to install Podman: %w", err)
		}
		b.log.HostSuccess(host, "Podman installed")
	}

	// Configure Podman (netavark network backend)
	b.log.Host(host, "Configuring Podman...")
	if err := b.configurePodman(host); err != nil {
		return fmt.Errorf("failed to configure Podman: %w", err)
	}

	// Create azud network if it doesn't exist
	b.log.Host(host, "Setting up azud network...")
	if err := b.createNetwork(host); err != nil {
		return fmt.Errorf("failed to create network: %w", err)
	}

	b.log.HostSuccess(host, "Bootstrap complete")
	return nil
}

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

type OSInfo struct {
	Name    string
	Version string
	ID      string
	Family  string // debian, rhel, etc.
}

func (b *Bootstrapper) detectOS(host string) (*OSInfo, error) {
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

func (b *Bootstrapper) isPodmanInstalled(host string) (bool, error) {
	result, err := b.sshClient.Execute(host, "podman --version 2>/dev/null && echo 'ok'")
	if err != nil {
		return false, nil // Podman not installed
	}
	return strings.Contains(result.Stdout, "ok"), nil
}

func (b *Bootstrapper) installPodman(host string, osInfo *OSInfo) error {
	var installCmd string

	switch osInfo.Family {
	case "debian":
		installCmd = b.getDebianPodmanInstall()
	case "rhel":
		installCmd = b.getRHELPodmanInstall()
	case "alpine":
		installCmd = b.getAlpinePodmanInstall()
	default:
		return fmt.Errorf("unsupported OS family: %s", osInfo.Family)
	}

	result, err := b.sshClient.Execute(host, installCmd)
	if err != nil {
		return fmt.Errorf("podman installation failed: %w", err)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("podman installation failed: %s", result.Stderr)
	}

	return nil
}

func (b *Bootstrapper) getDebianPodmanInstall() string {
	return `
set -e
apt-get update
apt-get install -y podman netavark aardvark-dns
`
}

func (b *Bootstrapper) getRHELPodmanInstall() string {
	return `
set -e
dnf install -y podman netavark aardvark-dns
`
}

func (b *Bootstrapper) getAlpinePodmanInstall() string {
	return `
set -e
apk add --update podman netavark aardvark-dns
`
}

func (b *Bootstrapper) configurePodman(host string) error {
	containersConf := `[network]
network_backend = "netavark"
`

	cmd := fmt.Sprintf(`mkdir -p /etc/containers && cat > /etc/containers/containers.conf << 'EOF'
%sEOF`, containersConf)

	result, err := b.sshClient.Execute(host, cmd)
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to write containers.conf: %s", result.Stderr)
	}

	return nil
}

func (b *Bootstrapper) createNetwork(host string) error {
	result, err := b.sshClient.Execute(host, "podman network inspect azud >/dev/null 2>&1 && echo 'exists'")
	if err != nil {
		return err
	}

	if strings.Contains(result.Stdout, "exists") {
		b.log.Debug("Network azud already exists on %s", host)
		return nil
	}

	result, err = b.sshClient.Execute(host, "podman network create --dns-enabled azud")
	if err != nil {
		return err
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("failed to create network: %s", result.Stderr)
	}

	return nil
}

func (b *Bootstrapper) CheckPodman(host string) (*PodmanStatus, error) {
	status := &PodmanStatus{Host: host}

	result, err := b.sshClient.Execute(host, "podman --version")
	if err != nil || result.ExitCode != 0 {
		return status, nil
	}
	status.Installed = true
	status.Version = strings.TrimSpace(result.Stdout)

	result, err = b.sshClient.Execute(host, "podman info --format '{{.Version.Version}}'")
	if err != nil || result.ExitCode != 0 {
		return status, nil
	}
	status.Running = true

	result, err = b.sshClient.Execute(host, "podman ps -q | wc -l")
	if err == nil && result.ExitCode == 0 {
		fmt.Sscanf(strings.TrimSpace(result.Stdout), "%d", &status.ContainerCount)
	}

	return status, nil
}

type PodmanStatus struct {
	Host           string
	Installed      bool
	Running        bool
	Version        string
	ContainerCount int
}

func (b *Bootstrapper) ExecuteOnAll(hosts []string, cmd string) []*ssh.Result {
	return b.sshClient.ExecuteParallel(hosts, cmd)
}
