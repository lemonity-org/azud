package quadlet

import (
	"fmt"

	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/ssh"
)

// QuadletDeployer writes systemd quadlet unit files to remote hosts and
// manages their lifecycle via systemctl.
type QuadletDeployer struct {
	ssh  *ssh.Client
	log  *output.Logger
	path string // e.g., /etc/containers/systemd/
}

func NewQuadletDeployer(sshClient *ssh.Client, log *output.Logger, path string) *QuadletDeployer {
	if log == nil {
		log = output.DefaultLogger
	}
	if path == "" {
		path = "/etc/containers/systemd/"
	}
	return &QuadletDeployer{
		ssh:  sshClient,
		log:  log,
		path: path,
	}
}

// Deploy writes a quadlet file to the remote host and reloads systemd.
func (q *QuadletDeployer) Deploy(host, filename, content string) error {
	q.log.Host(host, "Deploying quadlet %s...", filename)

	filePath := q.path + filename
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s << 'QUADLET_EOF'\n%sQUADLET_EOF", q.path, filePath, content)

	result, err := q.ssh.Execute(host, cmd)
	if err != nil {
		return fmt.Errorf("failed to write quadlet file: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to write quadlet file: %s", result.Stderr)
	}

	if err := q.Reload(host); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}

	q.log.HostSuccess(host, "Quadlet %s deployed", filename)
	return nil
}

func (q *QuadletDeployer) Remove(host, filename string) error {
	q.log.Host(host, "Removing quadlet %s...", filename)

	filePath := q.path + filename
	cmd := fmt.Sprintf("rm -f %s", filePath)

	result, err := q.ssh.Execute(host, cmd)
	if err != nil {
		return fmt.Errorf("failed to remove quadlet file: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to remove quadlet file: %s", result.Stderr)
	}

	if err := q.Reload(host); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}

	q.log.HostSuccess(host, "Quadlet %s removed", filename)
	return nil
}

func (q *QuadletDeployer) Reload(host string) error {
	result, err := q.ssh.Execute(host, "systemctl daemon-reload")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("daemon-reload failed: %s", result.Stderr)
	}
	return nil
}

func (q *QuadletDeployer) Start(host, service string) error {
	result, err := q.ssh.Execute(host, fmt.Sprintf("systemctl start %s", service))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to start %s: %s", service, result.Stderr)
	}
	return nil
}

func (q *QuadletDeployer) Stop(host, service string) error {
	result, err := q.ssh.Execute(host, fmt.Sprintf("systemctl stop %s", service))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to stop %s: %s", service, result.Stderr)
	}
	return nil
}

func (q *QuadletDeployer) Enable(host, service string) error {
	result, err := q.ssh.Execute(host, fmt.Sprintf("systemctl enable %s", service))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to enable %s: %s", service, result.Stderr)
	}
	return nil
}

func (q *QuadletDeployer) Status(host, service string) (string, error) {
	result, err := q.ssh.Execute(host, fmt.Sprintf("systemctl is-active %s 2>/dev/null || true", service))
	if err != nil {
		return "", err
	}
	return result.Stdout, nil
}

func (q *QuadletDeployer) Logs(host, service string, follow bool, lines int) (*ssh.Result, error) {
	cmd := fmt.Sprintf("journalctl -u %s --no-pager", service)
	if follow {
		cmd += " -f"
	}
	if lines > 0 {
		cmd += fmt.Sprintf(" -n %d", lines)
	}
	return q.ssh.Execute(host, cmd)
}
