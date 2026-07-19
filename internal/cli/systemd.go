package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

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
		unitName := fmt.Sprintf("%s.container", proxy.CaddyContainerName)
		for _, host := range cfg.GetRoleHosts("web") {
			if systemdHost != "" && host != systemdHost {
				continue
			}
			if err := proxyDeployer.Deploy(host, unitName, quadlet.GenerateContainerFile(proxyUnit)); err != nil {
				log.HostError(host, "Failed to deploy proxy unit: %v", err)
				hasErrors = true
				continue
			}
			if !systemdNoStart {
				if err := proxyDeployer.Start(host, proxy.CaddyContainerName); err != nil {
					log.HostError(host, "Failed to start proxy unit: %v", err)
					hasErrors = true
				}
			}
		}
	}

	if hasErrors {
		return fmt.Errorf("one or more systemd operations failed")
	}

	log.Success("systemd units installed")
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
		return fmt.Sprintf("%s:%s", image, last.Version)
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
		TimeoutStopSec: cfg.Deploy.GetStopTimeout(),
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
	}

	return unit
}

func buildProxyQuadletUnit() *quadlet.ContainerUnit {
	proxyRootless := cfg.Podman.Rootless && !cfg.Proxy.Rootful
	after, requires := quadletNetworkOnlineDependencies(proxyRootless)
	network := []string{"azud.network"}
	publishPorts := []string{
		fmt.Sprintf("%d:80", cfg.Proxy.EffectiveHTTPPort()),
		fmt.Sprintf("%d:443", cfg.Proxy.EffectiveHTTPSPort()),
		fmt.Sprintf("127.0.0.1:%d:%d", proxy.CaddyAdminPort, proxy.CaddyAdminPort),
	}
	if cfg.UseHostPortUpstreams() {
		network = []string{"host"}
		publishPorts = []string{}
	}
	adminListen := "0.0.0.0:2019" // safe: container-only; Quadlet publishes the admin port to host loopback
	if cfg.UseHostPortUpstreams() {
		adminListen = "127.0.0.1:2019"
	}
	stateDir := "%h/.local/share/azud"
	if cfg.SSH.User == "" || cfg.SSH.User == "root" {
		stateDir = "/var/lib/azud"
	} else if !proxyRootless {
		stateDir = "/home/" + cfg.SSH.User + "/.local/share/azud"
	}

	unit := &quadlet.ContainerUnit{
		Description:    "Azud Caddy proxy",
		After:          after,
		Requires:       requires,
		Image:          proxy.CaddyImage,
		ContainerName:  proxy.CaddyContainerName,
		Environment:    map[string]string{"CADDY_ADMIN": adminListen},
		PublishPort:    publishPorts,
		Volume:         []string{"caddy_data:/data", "caddy_config:/config", stateDir + ":/azud-state:ro,Z"},
		Network:        network,
		Label:          map[string]string{"azud.managed": "true", "azud.type": "proxy"},
		Exec:           fmt.Sprintf("/bin/sh -c 'if [ -s /azud-state/%s ]; then exec caddy run --config /azud-state/%s --watch; else exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --watch; fi'", proxy.CaddyConfigFileName, proxy.CaddyConfigFileName),
		Restart:        "always",
		TimeoutStopSec: 30,
		WantedBy:       "default.target",
	}

	return unit
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
