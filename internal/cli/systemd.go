package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
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

	hosts := getSystemdHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	if cfg.Podman.Rootless {
		for _, host := range hosts {
			if err := enableLinger(sshClient, host, cfg.SSH.User); err != nil {
				log.HostError(host, "Failed to enable linger: %v", err)
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

	if !systemdSkipApp {
		if err := ensureRemoteSecretsFile(sshClient, hosts, cfg.Env.Secret); err != nil {
			return err
		}
		appUnit := buildAppQuadletUnit(image)
		unitName := fmt.Sprintf("%s.container", cfg.Service)
		for _, host := range hosts {
			if err := appDeployer.Deploy(host, unitName, quadlet.GenerateContainerFile(appUnit)); err != nil {
				log.HostError(host, "Failed to deploy app unit: %v", err)
				hasErrors = true
				continue
			}
			if !systemdNoStart {
				if err := appDeployer.Start(host, cfg.Service); err != nil {
					log.HostError(host, "Failed to start app unit: %v", err)
					hasErrors = true
				}
			}
		}
	}

	if !systemdSkipProxy && len(cfg.Proxy.AllHosts()) > 0 {
		proxyUnit := buildProxyQuadletUnit()
		unitName := fmt.Sprintf("%s.container", proxy.CaddyContainerName)
		for _, host := range hosts {
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

func getSystemdHosts() []string {
	if systemdHost != "" {
		return []string{systemdHost}
	}
	if systemdRole != "" {
		return cfg.GetRoleHosts(systemdRole)
	}
	return cfg.GetAllHosts()
}

func resolveSystemdImage(log *output.Logger) string {
	image := cfg.Image
	if idx := strings.LastIndex(image, ":"); idx > 0 && !strings.Contains(image[idx:], "/") {
		return image
	}

	history := deploy.NewHistoryStore(".", cfg.Deploy.RetainHistory, log)
	if last, err := history.GetLastSuccessful(cfg.Service); err == nil && last.Version != "" {
		return fmt.Sprintf("%s:%s", image, last.Version)
	}

	return fmt.Sprintf("%s:latest", image)
}

func buildAppQuadletUnit(image string) *quadlet.ContainerUnit {
	labels := map[string]string{
		"azud.managed": "true",
		"azud.service": cfg.Service,
	}

	publishPorts := []string{}
	if cfg.UseHostPortUpstreams() {
		publishPorts = append(publishPorts, fmt.Sprintf("127.0.0.1::%d", cfg.Proxy.AppPort))
	}

	unit := &quadlet.ContainerUnit{
		Description:    fmt.Sprintf("Azud service %s", cfg.Service),
		After:          []string{"network-online.target"},
		Requires:       []string{"network-online.target"},
		Image:          image,
		ContainerName:  cfg.Service,
		Environment:    cfg.Env.Clear,
		PublishPort:    publishPorts,
		Volume:         cfg.Volumes,
		Network:        []string{"azud"},
		Label:          labels,
		Restart:        "always",
		TimeoutStopSec: cfg.Deploy.GetStopTimeout(),
		WantedBy:       "default.target",
	}

	if len(cfg.Env.Secret) > 0 {
		unit.EnvironmentFile = []string{config.RemoteSecretsPath(cfg)}
	}

	livenessCmd := deploy.LivenessCommand(cfg)
	if livenessCmd != "" {
		unit.HealthCmd = livenessCmd
		unit.HealthInterval = cfg.Proxy.Healthcheck.Interval
	}

	return unit
}

func buildProxyQuadletUnit() *quadlet.ContainerUnit {
	network := []string{"azud"}
	publishPorts := []string{
		fmt.Sprintf("%d:80", cfg.Proxy.EffectiveHTTPPort()),
		fmt.Sprintf("%d:443", cfg.Proxy.EffectiveHTTPSPort()),
		fmt.Sprintf("127.0.0.1:%d:%d", proxy.CaddyAdminPort, proxy.CaddyAdminPort),
	}
	if cfg.UseHostPortUpstreams() {
		network = []string{"host"}
		publishPorts = []string{}
	}

	unit := &quadlet.ContainerUnit{
		Description:    "Azud Caddy proxy",
		After:          []string{"network-online.target"},
		Requires:       []string{"network-online.target"},
		Image:          proxy.CaddyImage,
		ContainerName:  proxy.CaddyContainerName,
		Environment:    map[string]string{"CADDY_ADMIN": "127.0.0.1:2019"},
		PublishPort:    publishPorts,
		Volume:         []string{"caddy_data:/data", "caddy_config:/config"},
		Network:        network,
		Label:          map[string]string{"azud.managed": "true", "azud.type": "proxy"},
		Exec:           "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --watch",
		Restart:        "always",
		TimeoutStopSec: 30,
		WantedBy:       "default.target",
	}

	return unit
}

func needsAzudNetworkUnit(skipApp, skipProxy bool) bool {
	if cfg == nil {
		return !skipApp || !skipProxy
	}
	return !skipApp || (!skipProxy && !cfg.UseHostPortUpstreams())
}
