package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/proxy"
)

var (
	proxyReconcileCheck  bool
	proxyReconcileRepair bool
)

var proxyReconcileCmd = &cobra.Command{
	Use:         "reconcile",
	Annotations: map[string]string{writesWithFlagAnnotation: "repair", readsSecretsAnnotation: "true"},
	Short:       "Check or repair the service proxy route",
	Args:        cobra.NoArgs,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if proxyReconcileCheck == proxyReconcileRepair {
			return fmt.Errorf("exactly one of --check or --repair is required")
		}
		return nil
	},
	RunE: runProxyReconcile,
}

func init() {
	proxyReconcileCmd.Flags().BoolVar(&proxyReconcileCheck, "check", false, "Check route state without changing it")
	proxyReconcileCmd.Flags().BoolVar(&proxyReconcileRepair, "repair", false, "Repair route drift")
	proxyReconcileCmd.Flags().StringVar(&proxyHost, "host", "", "Specific host to reconcile")
	proxyCmd.AddCommand(proxyReconcileCmd)
}

func runProxyReconcile(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	hosts := getProxyRouteHosts(proxyHost)
	if len(hosts) == 0 {
		return fmt.Errorf("no matching web hosts configured")
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()
	cm := podman.NewContainerManager(podman.NewClient(sshClient))
	manager := proxy.NewManagerWithOptions(sshClient, output.DefaultLogger, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())
	// Only a repair boots the proxy. A check compares routes, and loads no secrets
	// for a certificate.
	var proxyConfig *proxy.ProxyConfig
	if proxyReconcileRepair {
		proxyConfig = buildProxyConfig(output.DefaultLogger)
		manager.SetProxyConfig(proxyConfig)
	}
	var failures []string
	for _, host := range hosts {
		upstreams, err := desiredProxyUpstreams(cm, host)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", host, err))
			output.DefaultLogger.HostError(host, "%v", err)
			continue
		}
		if proxyReconcileRepair {
			if err := manager.Boot(host, proxyConfig); err != nil {
				failures = append(failures, fmt.Sprintf("%s: boot: %v", host, err))
				continue
			}
		}
		desired := deploy.BuildProxyServiceConfig(cfg, upstreams)
		status, err := manager.ReconcileService(host, desired, proxyReconcileRepair)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", host, err))
			output.DefaultLogger.HostError(host, "%v", err)
			continue
		}
		if proxyReconcileCheck && status != proxy.ReconcileInSync {
			output.DefaultLogger.HostError(host, "route drift: %s", status)
			failures = append(failures, fmt.Sprintf("%s: %s", host, status))
		} else if proxyReconcileRepair && status != proxy.ReconcileInSync {
			output.DefaultLogger.HostSuccess(host, "repaired (%s)", status)
		} else {
			output.DefaultLogger.HostSuccess(host, "in-sync")
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("proxy reconciliation failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func desiredProxyUpstreams(cm *podman.ContainerManager, host string) ([]string, error) {
	containers, err := cm.List(host, true, map[string]string{"label": "azud.managed=true"})
	if err != nil {
		return nil, err
	}
	names := selectProxyContainers(containers, cfg.Service, deploy.RoleContainerName(cfg, "web"))
	result := make([]string, 0, len(names))
	for _, name := range names {
		if cfg.UseHostPortUpstreams() {
			port, err := cm.HostPort(host, name, cfg.Proxy.AppPort)
			if err != nil {
				return nil, err
			}
			result = append(result, fmt.Sprintf("127.0.0.1:%d", port))
		} else {
			result = append(result, fmt.Sprintf("%s:%d", name, cfg.Proxy.AppPort))
		}
	}
	return result, nil
}

func getProxyRouteHosts(specificHost string) []string {
	configured := cfg.GetRoleHosts("web")
	seen := make(map[string]struct{}, len(configured))
	hosts := make([]string, 0, len(configured))
	for _, host := range configured {
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	if specificHost == "" {
		return hosts
	}
	if _, ok := seen[specificHost]; ok {
		return []string{specificHost}
	}
	return nil
}

func selectProxyContainers(containers []podman.Container, service, stable string) []string {
	var names []string
	for _, c := range containers {
		if c.Labels["azud.managed"] != "true" || c.Labels["azud.service"] != service || (c.Labels["azud.role"] != "web" && c.Labels["azud.role"] != "") {
			continue
		}
		if c.Name == stable {
			names = append(names, c.Name)
		}
	}
	return names
}
