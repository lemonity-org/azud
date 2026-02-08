package quadlet

import (
	"fmt"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/shell"
	"github.com/lemonity-org/azud/internal/ssh"
)

// QuadletDeployer writes systemd quadlet unit files to remote hosts and
// manages their lifecycle via systemctl.
type QuadletDeployer struct {
	ssh  *ssh.Client
	log  *output.Logger
	path string // e.g., /etc/containers/systemd/
	user bool
	sudo bool
}

func NewQuadletDeployer(sshClient *ssh.Client, log *output.Logger, path string, userMode bool) *QuadletDeployer {
	return NewQuadletDeployerWithOptions(sshClient, log, path, userMode, false)
}

func NewQuadletDeployerWithOptions(sshClient *ssh.Client, log *output.Logger, path string, userMode bool, useSudo bool) *QuadletDeployer {
	if log == nil {
		log = output.DefaultLogger
	}
	if path == "" {
		path = "/etc/containers/systemd/"
	}
	if userMode {
		useSudo = false
	}
	return &QuadletDeployer{
		ssh:  sshClient,
		log:  log,
		path: path,
		user: userMode,
		sudo: useSudo,
	}
}

// Deploy writes a quadlet file to the remote host and reloads systemd.
func (q *QuadletDeployer) Deploy(host, filename, content string) error {
	q.log.Host(host, "Deploying quadlet %s...", filename)

	filePath := q.path + filename
	var cmd string
	if q.sudo {
		cmd = fmt.Sprintf("%smkdir -p %s && %stee %s >/dev/null << 'QUADLET_EOF'\n%sQUADLET_EOF",
			q.sudoPrefix(), shell.Quote(q.path), q.sudoPrefix(), shell.Quote(filePath), content)
	} else {
		// Use shell.Quote for paths to prevent command injection.
		cmd = fmt.Sprintf("mkdir -p %s && cat > %s << 'QUADLET_EOF'\n%sQUADLET_EOF",
			shell.Quote(q.path), shell.Quote(filePath), content)
	}

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
	cmd := fmt.Sprintf("%srm -f %s", q.sudoPrefix(), shell.Quote(filePath))

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
	result, err := q.ssh.Execute(host, q.systemctlCmd("daemon-reload"))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("daemon-reload failed: %s", result.Stderr)
	}
	return nil
}

func (q *QuadletDeployer) Start(host, service string) error {
	result, err := q.ssh.Execute(host, q.systemctlCmd("start "+shell.Quote(service)))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to start %s: %s", service, result.Stderr)
	}
	return nil
}

func (q *QuadletDeployer) Stop(host, service string) error {
	result, err := q.ssh.Execute(host, q.systemctlCmd("stop "+shell.Quote(service)))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to stop %s: %s", service, result.Stderr)
	}
	return nil
}

func (q *QuadletDeployer) Enable(host, service string) error {
	result, err := q.ssh.Execute(host, q.systemctlCmd("enable "+shell.Quote(service)))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to enable %s: %s", service, result.Stderr)
	}
	return nil
}

func (q *QuadletDeployer) Status(host, service string) (string, error) {
	result, err := q.ssh.Execute(host, q.systemctlCmd(fmt.Sprintf("is-active %s 2>/dev/null || true", shell.Quote(service))))
	if err != nil {
		return "", err
	}
	return result.Stdout, nil
}

func (q *QuadletDeployer) Logs(host, service string, follow bool, lines int) (*ssh.Result, error) {
	cmd := fmt.Sprintf("journalctl -u %s --no-pager", shell.Quote(service))
	if q.user {
		cmd = "journalctl --user-unit " + shell.Quote(service) + " --no-pager"
	} else if q.sudo {
		cmd = q.sudoPrefix() + cmd
	}
	if follow {
		cmd += " -f"
	}
	if lines > 0 {
		cmd += fmt.Sprintf(" -n %d", lines)
	}
	return q.ssh.Execute(host, cmd)
}

func (q *QuadletDeployer) systemctlCmd(action string) string {
	base := "systemctl"
	if q.user {
		base = "systemctl --user"
	}
	if q.sudo {
		base = q.sudoPrefix() + base
	}
	return base + " " + action
}

func (q *QuadletDeployer) sudoPrefix() string {
	if q.sudo {
		return "sudo -n "
	}
	return ""
}
