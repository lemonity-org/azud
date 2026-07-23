package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/state"
)

var (
	proxyReconcileCheck  bool
	proxyReconcileRepair bool
)

var proxyReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Check or repair the service proxy route",
	Args:  cobra.NoArgs,
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
	canary, err := readCanaryState()
	if err != nil {
		return err
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()
	cm := podman.NewContainerManager(podman.NewClient(sshClient))
	manager := proxy.NewManagerWithOptions(sshClient, output.DefaultLogger, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())
	proxyConfig := buildProxyConfig(output.DefaultLogger)
	manager.SetProxyConfig(proxyConfig)
	var failures []string
	for _, host := range hosts {
		upstreams, weights, err := desiredProxyUpstreams(cm, host, canary)
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
		desired := deploy.BuildProxyServiceConfig(cfg, upstreams, weights)
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

func readCanaryState() (*deploy.CanaryState, error) {
	dir, err := state.LocalDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "canary", fmt.Sprintf("%s.json", cfg.Service)))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read canary state: %w", err)
	}
	var result deploy.CanaryState
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse canary state: %w", err)
	}
	if result.Service != "" && result.Service != cfg.Service {
		return nil, fmt.Errorf("canary state belongs to service %s", result.Service)
	}
	return &result, nil
}

func desiredProxyUpstreams(cm *podman.ContainerManager, host string, canary *deploy.CanaryState) ([]string, []proxy.UpstreamWeight, error) {
	containers, err := cm.List(host, true, map[string]string{"label": "azud.managed=true"})
	if err != nil {
		return nil, nil, err
	}
	stable := deploy.RoleContainerName(cfg, "web")
	if canary != nil && canary.StableContainer != "" && (len(canary.Hosts) == 0 || containsString(canary.Hosts, host)) {
		stable = canary.StableContainer
	}
	if name := unmanagedCanaryState(containers, cfg.Service, stable, canary, host); name != "" {
		return nil, nil, fmt.Errorf("managed canary container %s exists without matching running canary state; refusing to guess traffic weights", name)
	}
	allowedCanary := ""
	if canary != nil && (len(canary.Hosts) == 0 || containsString(canary.Hosts, host)) {
		switch canary.Status {
		case deploy.CanaryStatusDeploying, deploy.CanaryStatusPromoting, deploy.CanaryStatusRollingBack:
			return nil, nil, fmt.Errorf("refusing reconciliation during canary transition %s", canary.Status)
		case deploy.CanaryStatusRunning:
			if canary.CurrentWeight < 0 || canary.CurrentWeight > 100 {
				return nil, nil, fmt.Errorf("invalid persisted canary weight %d", canary.CurrentWeight)
			}
			allowedCanary = canary.CanaryContainer
			if allowedCanary == "" {
				return nil, nil, fmt.Errorf("running canary state has no canary container")
			}
		}
	}
	names := selectProxyContainers(containers, cfg.Service, stable, allowedCanary)
	if err := validateCanaryContainers(names, stable, allowedCanary); err != nil {
		return nil, nil, err
	}
	dials := make(map[string]string, len(names))
	for _, name := range names {
		if cfg.UseHostPortUpstreams() {
			port, err := cm.HostPort(host, name, cfg.Proxy.AppPort)
			if err != nil {
				return nil, nil, err
			}
			dials[name] = fmt.Sprintf("127.0.0.1:%d", port)
		} else {
			dials[name] = fmt.Sprintf("%s:%d", name, cfg.Proxy.AppPort)
		}
	}
	if allowedCanary != "" {
		return nil, []proxy.UpstreamWeight{{Dial: dials[stable], Weight: 100 - canary.CurrentWeight}, {Dial: dials[allowedCanary], Weight: canary.CurrentWeight}}, nil
	}
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, dials[name])
	}
	return result, nil, nil
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

func unmanagedCanaryState(containers []podman.Container, service, stable string, canary *deploy.CanaryState, host string) string {
	stateCoversHost := canary != nil && (len(canary.Hosts) == 0 || containsString(canary.Hosts, host)) && canary.Status == deploy.CanaryStatusRunning
	for _, container := range containers {
		if container.Labels["azud.managed"] != "true" || container.Labels["azud.service"] != service || (container.Labels["azud.role"] != "web" && container.Labels["azud.role"] != "") {
			continue
		}
		// A promoted canary is renamed to the stable container name, but Podman
		// retains its azud.canary label until the next regular deployment.
		if container.Name == stable {
			continue
		}
		isCanary := container.Labels["azud.canary"] == "true" || container.Name == service+"-canary"
		if isCanary && (!stateCoversHost || canary.CanaryContainer != container.Name) {
			return container.Name
		}
	}
	return ""
}

func validateCanaryContainers(names []string, stable, canary string) error {
	if canary == "" {
		return nil
	}
	if !containsString(names, stable) || !containsString(names, canary) {
		return fmt.Errorf("running canary state requires containers %s and %s", stable, canary)
	}
	for _, name := range names {
		if name != stable && name != canary {
			return fmt.Errorf("refusing reconciliation with scaled container %s during a running canary", name)
		}
	}
	return nil
}

func selectProxyContainers(containers []podman.Container, service, stable, canary string) []string {
	var names []string
	prefix := stable + "-"
	for _, c := range containers {
		if c.Labels["azud.managed"] != "true" || c.Labels["azud.service"] != service || (c.Labels["azud.role"] != "web" && c.Labels["azud.role"] != "") {
			continue
		}
		valid := c.Name == stable || (canary != "" && c.Name == canary)
		if !valid && strings.HasPrefix(c.Name, prefix) {
			i, err := strconv.Atoi(strings.TrimPrefix(c.Name, prefix))
			valid = err == nil && i >= 0 && c.Labels["azud.instance"] == strconv.Itoa(i)
		}
		if valid {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	return names
}
