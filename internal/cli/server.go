package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/server"
	"github.com/adriancarayol/azud/internal/ssh"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage deployment servers",
	Long:  `Commands for managing and interacting with deployment servers.`,
}

var serverBootstrapCmd = &cobra.Command{
	Use:   "bootstrap [hosts...]",
	Short: "Bootstrap servers with Podman",
	Long: `Install Podman and configure servers for deployment.

If no hosts are specified, all hosts from the configuration will be bootstrapped.

Example:
  azud server bootstrap                    # Bootstrap all configured hosts
  azud server bootstrap 192.168.1.1        # Bootstrap specific host
  azud server bootstrap host1 host2        # Bootstrap multiple hosts`,
	RunE: runServerBootstrap,
}

var serverExecCmd = &cobra.Command{
	Use:   "exec [command]",
	Short: "Execute command on servers",
	Long: `Execute a command on one or more servers.

Example:
  azud server exec "podman ps"                  # Run on all servers
  azud server exec --host 192.168.1.1 "uptime"  # Run on specific host
  azud server exec --role web "podman ps"       # Run on servers with role`,
	Args: cobra.MinimumNArgs(1),
	RunE: runServerExec,
}

var (
	serverExecHost string
	serverExecRole string
)

func init() {
	// Add server subcommands
	serverCmd.AddCommand(serverBootstrapCmd)
	serverCmd.AddCommand(serverExecCmd)

	// Exec flags
	serverExecCmd.Flags().StringVar(&serverExecHost, "host", "", "Specific host to execute on")
	serverExecCmd.Flags().StringVar(&serverExecRole, "role", "", "Execute on hosts with this role")

	// Add to root
	rootCmd.AddCommand(serverCmd)
}

func runServerBootstrap(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)

	var hosts []string
	if len(args) > 0 {
		hosts = args
	} else {
		hosts = cfg.GetAllHosts()
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts configured")
		}
	}

	output.Info("Bootstrapping %d server(s)...", len(hosts))

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	bootstrapper := server.NewBootstrapper(sshClient, output.DefaultLogger, cfg.Podman.NetworkBackend)
	if err := bootstrapper.BootstrapAll(hosts); err != nil {
		return err
	}

	if cfg.Podman.Rootless {
		for _, host := range hosts {
			if err := enableLinger(sshClient, host, cfg.SSH.User); err != nil {
				output.DefaultLogger.HostError(host, "Failed to enable linger: %v", err)
			}
		}
	}

	return nil
}

func runServerExec(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)

	command := strings.Join(args, " ")

	var hosts []string
	if serverExecHost != "" {
		hosts = []string{serverExecHost}
	} else if serverExecRole != "" {
		hosts = cfg.GetRoleHosts(serverExecRole)
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts found for role: %s", serverExecRole)
		}
	} else {
		hosts = cfg.GetAllHosts()
		if len(hosts) == 0 {
			return fmt.Errorf("no hosts configured")
		}
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	output.Info("Executing on %d host(s): %s", len(hosts), command)
	results := sshClient.ExecuteParallel(hosts, command)

	var hasErrors bool
	for _, result := range results {
		if result.Success() {
			output.DefaultLogger.HostSuccess(result.Host, "")
			if result.Stdout != "" {
				output.DefaultLogger.Output(result.Stdout)
			}
		} else {
			hasErrors = true
			output.DefaultLogger.HostError(result.Host, "exit code %d", result.ExitCode)
			if result.Stderr != "" {
				output.DefaultLogger.Output(result.Stderr)
			}
			if result.Error != nil {
				output.DefaultLogger.Output(result.Error.Error())
			}
		}
	}

	if hasErrors {
		return fmt.Errorf("command failed on one or more hosts")
	}

	return nil
}

func createSSHClient() *ssh.Client {
	sshConfig := &ssh.Config{
		User:                  cfg.SSH.User,
		Port:                  cfg.SSH.Port,
		Keys:                  cfg.SSH.Keys,
		KnownHostsFile:        cfg.SSH.KnownHostsFile,
		ConnectTimeout:        cfg.SSH.ConnectTimeout,
		InsecureIgnoreHostKey: cfg.SSH.InsecureIgnoreHostKey,
	}

	// Add proxy configuration if present
	if cfg.SSH.Proxy.Host != "" {
		sshConfig.Proxy = &ssh.ProxyConfig{
			Host: cfg.SSH.Proxy.Host,
			User: cfg.SSH.Proxy.User,
		}
	}

	return ssh.NewClient(sshConfig)
}
