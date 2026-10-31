package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/quadlet"
)

var systemdCmd = &cobra.Command{
	Use:   "systemd",
	Short: "Manage systemd/quadlet units for Azud",
}

var systemdEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Install quadlet units",
	Long: `Generate quadlet unit files for the app and proxy.

This improves reboot reliability, especially for rootless Podman.
Units are installed into quadlet paths and can be started immediately.`,
	RunE: runSystemdEnable,
}

var (
	systemdHost      string
	systemdRole      string
	systemdNoStart   bool
	systemdSkipApp   bool
	systemdSkipProxy bool
)

func init() {
	systemdEnableCmd.Flags().StringVar(&systemdHost, "host", "", "Target a specific host")
	systemdEnableCmd.Flags().StringVar(&systemdRole, "role", "", "Target hosts for a role")
	systemdEnableCmd.Flags().BoolVar(&systemdNoStart, "no-start", false, "Only enable units (do not start)")
	systemdEnableCmd.Flags().BoolVar(&systemdSkipApp, "skip-app", false, "Skip app unit")
	systemdEnableCmd.Flags().BoolVar(&systemdSkipProxy, "skip-proxy", false, "Skip proxy unit")

	systemdCmd.AddCommand(systemdEnableCmd)
	rootCmd.AddCommand(systemdCmd)
}

func runSystemdEnable(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger
	hasErrors := false

	targets, err := getSystemdTargets()
	if err != nil {
		return err
	}
	hosts := systemdTargetHosts(targets)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	if cfg.Podman.Rootless {
		for _, host := range hosts {
			if err := enableLinger(sshClient, host, cfg.SSH.User); err != nil {
				log.HostError(host, "Failed to enable linger: %v", err)
				hasErrors = true
			}
		}
	}

	appUseSudo := !cfg.Podman.Rootless && cfg.SSH.User != "root"
	appDeployer := quadlet.NewQuadletDeployerWithOptions(
		sshClient,
		log,
		cfg.Podman.QuadletPath,
		cfg.Podman.Rootless,
		appUseSudo,
	)

	proxyRootless := cfg.Podman.Rootless && !cfg.Proxy.Rootful
	proxyPath := cfg.Podman.QuadletPath
	if cfg.Proxy.Rootful {
		proxyPath = "/etc/containers/systemd/"
	}
	proxyUseSudo := !proxyRootless && cfg.SSH.User != "root"
	proxyDeployer := quadlet.NewQuadletDeployerWithOptions(
		sshClient,
		log,
		proxyPath,
		proxyRootless,
		proxyUseSudo,
	)

	// Ensure azud network quadlet exists only when app units need it, or when
	// proxy units run in bridge mode and need azud DNS/network access.
	needsAzudNetwork := needsAzudNetworkUnit(systemdSkipApp, systemdSkipProxy)
	if needsAzudNetwork {
		networkUnitName := "azud.network"
		networkServiceName := "azud-network"
		netFile := quadlet.GenerateNetworkFile("azud", true)
		for _, host := range hosts {
			if err := appDeployer.Deploy(host, networkUnitName, netFile); err != nil {
				log.HostError(host, "Failed to deploy network unit: %v", err)
				hasErrors = true
				continue
			}
			if !systemdNoStart {
				if err := appDeployer.Start(host, networkServiceName); err != nil {
					log.HostError(host, "Failed to start network unit: %v", err)
					hasErrors = true
				}
			}
		}
	}

	image := resolveSystemdImage(log)
	appContainers := podman.NewContainerManager(podman.NewClient(sshClient))

	if !systemdSkipApp {
		if err := ensureRemoteSecretsFile(sshClient, hosts, cfg.Env.Secret); err != nil {
			return err
		}
		for _, target := range targets {
			appUnit := buildAppQuadletUnit(image, target.Role)
			serviceName := deploy.RoleContainerName(cfg, target.Role)
			if cfg.UseHostPortUpstreams() && deploy.IsProxyRole(target.Role) {
				hostPort, err := appContainers.HostPort(target.Host, serviceName, cfg.Proxy.AppPort)
				if err != nil {
					log.HostError(target.Host, "Failed to preserve mixed-mode port for %s: %v", target.Role, err)
					hasErrors = true
					continue
				}
				pinQuadletHostPort(appUnit, hostPort, cfg.Proxy.AppPort)
			}
			if err := appUnit.Validate(); err != nil {
				return fmt.Errorf("invalid %s app quadlet unit: %w", target.Role, err)
			}
			unitName := fmt.Sprintf("%s.container", serviceName)
			if err := appDeployer.Deploy(target.Host, unitName, quadlet.GenerateContainerFile(appUnit)); err != nil {
				log.HostError(target.Host, "Failed to deploy %s app unit: %v", target.Role, err)
				hasErrors = true
				continue
			}
			if !systemdNoStart {
				if err := appDeployer.Start(target.Host, serviceName); err != nil {
					log.HostError(target.Host, "Failed to start %s app unit: %v", target.Role, err)
					hasErrors = true
				}
			}
		}
	}

	if !systemdSkipProxy && (systemdRole == "" || systemdRole == "web") && len(cfg.Proxy.AllHosts()) > 0 {
		proxyUnit := buildProxyQuadletUnit()
		if err := proxyUnit.Validate(); err != nil {
			return fmt.Errorf("invalid proxy quadlet unit: %w", err)
		}
		unitName := fmt.Sprintf("%s.container", proxy.CaddyContainerName)
		proxyManager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())
		for _, host := range cfg.GetRoleHosts("web") {
			if systemdHost != "" && host != systemdHost {
				continue
			}
			if err := proxyDeployer.Deploy(host, unitName, quadlet.GenerateContainerFile(proxyUnit)); err != nil {
				log.HostError(host, "Failed to deploy proxy unit: %v", err)
				hasErrors = true
				continue
			}
			if err := enableProxyUnit(proxyDeployer, proxyManager, host, !systemdNoStart); err != nil {
				log.HostError(host, "%v", err)
				hasErrors = true
			}
		}
	}

	if hasErrors {
		return fmt.Errorf("one or more systemd operations failed")
	}

	log.Success("systemd units installed")
	return nil
}

// enableProxyUnit migrates a host's proxy to the freshly deployed unit.
// `systemctl start` does nothing for a unit that is already running, so a
// started migration stops the previous unit and any imperative container
// first; Caddy's autosave is then normalized to the private admin socket
// before the new unit resumes it. Without start, only the autosave is
// prepared and the running proxy is left untouched.
func enableProxyUnit(deployer *quadlet.QuadletDeployer, manager *proxy.Manager, host string, start bool) error {
	if !start {
		if err := manager.PrepareQuadletStart(host, false); err != nil {
			return fmt.Errorf("failed to prepare proxy state for its unit: %w", err)
		}
		return nil
	}
	if err := deployer.Stop(host, proxy.CaddyContainerName); err != nil {
		return fmt.Errorf("failed to stop previous proxy unit: %w", err)
	}
	if err := manager.PrepareQuadletStart(host, true); err != nil {
		// The proxy was stopped above. Start the installed unit anyway, as the
		// next host boot would, rather than leave the host without a proxy.
		if startErr := deployer.Start(host, proxy.CaddyContainerName); startErr != nil {
			return fmt.Errorf("failed to prepare proxy state for its unit: %v (starting the unit anyway also failed: %v)", err, startErr)
		}
		return fmt.Errorf("failed to prepare proxy state for its unit; started the unit with its existing autosave: %w", err)
	}
	if err := deployer.Start(host, proxy.CaddyContainerName); err != nil {
		return fmt.Errorf("failed to start proxy unit: %w", err)
	}
	return nil
}

type systemdTarget struct {
	Host string
	Role string
}

func getSystemdTargets() ([]systemdTarget, error) {
	roles := cfg.GetRoles()
	sort.Strings(roles)
	if systemdRole != "" {
		if !cfg.HasRole(systemdRole) {
			return nil, fmt.Errorf("unknown role %q", systemdRole)
		}
		roles = []string{systemdRole}
	}
	var targets []systemdTarget
	for _, role := range roles {
		for _, host := range cfg.GetRoleHosts(role) {
			if systemdHost != "" && host != systemdHost {
				continue
			}
			targets = append(targets, systemdTarget{Host: host, Role: role})
		}
	}
	if systemdHost != "" && len(targets) == 0 {
		return nil, fmt.Errorf("host %q is not configured for the selected role(s)", systemdHost)
	}
	return targets, nil
}

func systemdTargetHosts(targets []systemdTarget) []string {
	seen := make(map[string]struct{}, len(targets))
	var hosts []string
	for _, target := range targets {
		if _, ok := seen[target.Host]; ok {
			continue
		}
		seen[target.Host] = struct{}{}
		hosts = append(hosts, target.Host)
	}
	return hosts
}

func resolveSystemdImage(log *output.Logger) string {
	image := cfg.Image
	if idx := strings.LastIndex(image, ":"); idx > 0 && !strings.Contains(image[idx:], "/") {
		return image
	}

	history := deploy.NewDurableHistoryStore(cfg.Deploy.RetainHistory, log)
	if last, err := history.GetLastSuccessful(cfg.Service); err == nil && last.Version != "" {
		return deploy.ImageAtVersion(image, last.Version)
	}

	return fmt.Sprintf("%s:latest", image)
}

func systemdSecretsPath(path string, rootless bool, user string) string {
	home := "%h"
	if !rootless {
		if user == "" || user == "root" {
			home = "/root"
		} else {
			home = "/home/" + user
		}
	}
	for _, prefix := range []string{"${HOME}", "$HOME", "~"} {
		if path == prefix {
			return home
		}
		if strings.HasPrefix(path, prefix+"/") {
			return home + strings.TrimPrefix(path, prefix)
		}
	}
	return path
}

func buildAppQuadletUnit(image, role string) *quadlet.ContainerUnit {
	containerCfg := deploy.NewAppContainerConfig(cfg, image, deploy.RoleContainerName(cfg, role), role, nil)
	after, requires := quadletNetworkOnlineDependencies(cfg.Podman.Rootless)

	unit := &quadlet.ContainerUnit{
		Description:   fmt.Sprintf("Azud service %s (%s role)", cfg.Service, role),
		After:         after,
		Requires:      requires,
		Image:         image,
		ContainerName: containerCfg.Name,
		Environment:   containerCfg.Env,
		PublishPort:   containerCfg.Ports,
		Volume:        containerCfg.Volumes,
		// Refer to the Quadlet filename so systemd orders the container after
		// the generated network unit. NetworkName=azud preserves the runtime
		// network name used by imperative deployments.
		Network:        []string{"azud.network"},
		Label:          containerCfg.Labels,
		Restart:        "always",
		TimeoutStopSec: containerCfg.StopTimeout + int(config.StopTimeoutOverhead/time.Second),
		WantedBy:       "default.target",
	}

	if containerCfg.EnvFile != "" {
		unit.EnvironmentFile = []string{systemdSecretsPath(containerCfg.EnvFile, cfg.Podman.Rootless, cfg.SSH.User)}
	}
	unit.HealthCmd = containerCfg.HealthCmd
	unit.HealthInterval = containerCfg.HealthInterval
	if roleCfg, ok := cfg.Servers[role]; ok {
		unit.Exec = roleCfg.Cmd
		if memory := roleCfg.Options["memory"]; memory != "" {
			unit.PodmanArgs = append(unit.PodmanArgs, "--memory="+memory)
		}
		if cpus := roleCfg.Options["cpus"]; cpus != "" {
			unit.PodmanArgs = append(unit.PodmanArgs, "--cpus="+cpus)
		}
		if containerCfg.User != "" {
			unit.PodmanArgs = append(unit.PodmanArgs, "--user="+containerCfg.User)
		}
		if containerCfg.ReadOnly {
			unit.PodmanArgs = append(unit.PodmanArgs, "--read-only")
		}
		for _, capability := range containerCfg.CapDrop {
			unit.PodmanArgs = append(unit.PodmanArgs, "--cap-drop="+capability)
		}
		if containerCfg.NoNewPrivileges {
			unit.PodmanArgs = append(unit.PodmanArgs, "--security-opt=no-new-privileges")
		}
		for _, mount := range containerCfg.Tmpfs {
			unit.PodmanArgs = append(unit.PodmanArgs, "--tmpfs="+mount)
		}
		if containerCfg.NoHealthcheck {
			unit.PodmanArgs = append(unit.PodmanArgs, "--no-healthcheck")
		}
		if containerCfg.StopTimeout > 0 {
			unit.PodmanArgs = append(unit.PodmanArgs, fmt.Sprintf("--stop-timeout=%d", containerCfg.StopTimeout))
		}
	}

	return unit
}

// buildProxyQuadletUnit renders the same container definition Azud boots
// imperatively. The unit no longer mounts Azud's state directory: before the
// unit starts, `systemd enable` normalizes Caddy's autosave to the private
// admin socket or seeds it from the protected persisted copy, and Caddy
// resumes from that autosave.
func buildProxyQuadletUnit() *quadlet.ContainerUnit {
	proxyRootless := cfg.Podman.Rootless && !cfg.Proxy.Rootful
	after, requires := quadletNetworkOnlineDependencies(proxyRootless)
	containerCfg := proxy.NewProxyContainerConfig(cfg.Proxy.EffectiveHTTPPort(), cfg.Proxy.EffectiveHTTPSPort(), cfg.UseHostPortUpstreams())
	// Refer to the Quadlet filename so systemd orders the proxy after the
	// generated network unit.
	network := []string{"azud.network"}
	if containerCfg.Network == "host" {
		network = []string{"host"}
	}

	unit := &quadlet.ContainerUnit{
		Description:    "Azud Caddy proxy",
		After:          after,
		Requires:       requires,
		Image:          containerCfg.Image,
		ContainerName:  containerCfg.Name,
		Environment:    containerCfg.Env,
		PublishPort:    containerCfg.Ports,
		Volume:         containerCfg.Volumes,
		Network:        network,
		Label:          containerCfg.Labels,
		Exec:           strings.Join(containerCfg.Command, " "),
		PodmanArgs:     proxyQuadletPodmanArgs(containerCfg),
		Restart:        "always",
		TimeoutStopSec: 30,
		WantedBy:       "default.target",
	}

	return unit
}

// proxyQuadletPodmanArgs renders the proxy's isolation settings as Quadlet
// PodmanArgs so the unit and imperative boots cannot drift apart.
func proxyQuadletPodmanArgs(containerCfg *podman.ContainerConfig) []string {
	var args []string
	if containerCfg.ReadOnly {
		args = append(args, "--read-only")
	}
	for _, capability := range containerCfg.CapDrop {
		args = append(args, "--cap-drop="+capability)
	}
	for _, capability := range containerCfg.CapAdd {
		args = append(args, "--cap-add="+capability)
	}
	if containerCfg.NoNewPrivileges {
		args = append(args, "--security-opt=no-new-privileges")
	}
	for _, mount := range containerCfg.Tmpfs {
		args = append(args, "--tmpfs="+mount)
	}
	return args
}

// network-online.target belongs to the system manager and is not generally
// available in a user's systemd manager. Quadlet already derives the ordering
// for Network=*.network; keep the host-online dependency only for rootful
// units, where the target is valid and useful for externally published ports.
func quadletNetworkOnlineDependencies(rootless bool) (after, requires []string) {
	if rootless {
		return nil, nil
	}
	return []string{"network-online.target"}, []string{"network-online.target"}
}

func pinQuadletHostPort(unit *quadlet.ContainerUnit, hostPort, containerPort int) {
	unit.PublishPort = []string{fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort)}
}

func needsAzudNetworkUnit(skipApp, skipProxy bool) bool {
	if cfg == nil {
		return !skipApp || !skipProxy
	}
	return !skipApp || (!skipProxy && !cfg.UseHostPortUpstreams())
}
