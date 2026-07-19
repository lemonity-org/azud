package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/ssh"
	"github.com/lemonity-org/azud/internal/state"
)

// CanaryStatus represents the current state of a canary deployment
type CanaryStatus string

const (
	CanaryStatusNone        CanaryStatus = "none"
	CanaryStatusDeploying   CanaryStatus = "deploying"
	CanaryStatusRunning     CanaryStatus = "running"
	CanaryStatusPromoting   CanaryStatus = "promoting"
	CanaryStatusRollingBack CanaryStatus = "rolling_back"
)

// CanaryState holds the current state of a canary deployment
type CanaryState struct {
	Service         string       `json:"service,omitempty"`
	Status          CanaryStatus `json:"status"`
	StableVersion   string       `json:"stable_version"`
	CanaryVersion   string       `json:"canary_version"`
	CurrentWeight   int          `json:"current_weight"`
	TargetWeight    int          `json:"target_weight"`
	StartedAt       time.Time    `json:"started_at"`
	LastUpdated     time.Time    `json:"last_updated"`
	Hosts           []string     `json:"hosts"`
	CanaryContainer string       `json:"canary_container"`
	StableContainer string       `json:"stable_container"`
}

// CanaryDeployer manages weighted traffic-shifting deployments where a new
// version receives a fraction of traffic before full promotion.
type CanaryDeployer struct {
	cfg        *config.Config
	sshClient  *ssh.Client
	podman     *podman.Client
	containers *podman.ContainerManager
	images     *podman.ImageManager
	proxy      *proxy.Manager
	history    *HistoryStore
	log        *output.Logger
	state      *CanaryState
	stateMu    sync.RWMutex
	statePath  string
}

func NewCanaryDeployer(cfg *config.Config, sshClient *ssh.Client, log *output.Logger, statePath string) *CanaryDeployer {
	if log == nil {
		log = output.DefaultLogger
	}

	podmanClient := podman.NewClient(sshClient)
	proxyManager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())
	proxyManager.SetProxyConfig(newProxyConfigFromCfg(cfg))

	deployer := &CanaryDeployer{
		cfg:        cfg,
		sshClient:  sshClient,
		podman:     podmanClient,
		containers: podman.NewContainerManager(podmanClient),
		images:     podman.NewImageManager(podmanClient),
		proxy:      proxyManager,
		history:    NewDurableHistoryStore(cfg.Deploy.RetainHistory, log),
		log:        log,
		statePath:  statePath,
		state: &CanaryState{
			Status: CanaryStatusNone,
		},
	}

	deployer.loadState()
	return deployer
}

type CanaryDeployOptions struct {
	// Version/tag to deploy as canary
	Version string

	// Initial traffic weight for canary (0-100)
	InitialWeight int

	// Skip image pull
	SkipPull bool

	// Skip health check
	SkipHealthCheck bool

	// Target hosts
	Hosts []string

	// Destination environment
	Destination string
}

func (c *CanaryDeployer) Deploy(opts *CanaryDeployOptions) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if err := c.history.EnsureAvailable(); err != nil {
		return fmt.Errorf("durable deployment history is unavailable: %w", err)
	}
	if !c.cfg.Deploy.Canary.Enabled {
		return fmt.Errorf("canary deployments are disabled")
	}
	if opts == nil {
		return fmt.Errorf("canary deployment options are required")
	}

	if err := c.loadStateLocked(); err != nil {
		return err
	}

	// Check if canary already running
	if c.state.Status == CanaryStatusRunning || c.state.Status == CanaryStatusDeploying {
		return fmt.Errorf("canary deployment already in progress, use 'promote' or 'rollback' first")
	}

	c.state.Status = CanaryStatusDeploying
	timer := c.log.NewTimer("Canary Deployment")
	defer timer.Stop()

	// Determine image to deploy
	image := c.cfg.Image
	if opts.Version != "" {
		image = fmt.Sprintf("%s:%s", stripImageTag(image), opts.Version)
	}

	// Canary traffic is meaningful only for the proxy-serving web role.
	webHosts := c.cfg.GetRoleHosts("web")
	hosts := append([]string(nil), opts.Hosts...)
	if len(hosts) == 0 {
		hosts = webHosts
	} else {
		allowed := make(map[string]struct{}, len(webHosts))
		for _, host := range webHosts {
			allowed[host] = struct{}{}
		}
		for _, host := range hosts {
			if _, ok := allowed[host]; !ok {
				return fmt.Errorf("host %q is not configured for the web role", host)
			}
		}
	}

	if len(hosts) == 0 {
		c.state.Status = CanaryStatusNone
		c.state.LastUpdated = time.Now()
		if err := c.saveStateLocked(); err != nil {
			return err
		}
		return fmt.Errorf("no hosts to deploy to")
	}

	// Ensure required secrets are present on all hosts.
	if err := c.ensureRemoteSecrets(hosts); err != nil {
		return err
	}

	c.log.Header("Starting canary deployment: %s", image)

	// Get initial weight from config if not specified
	initialWeight := opts.InitialWeight
	if initialWeight == 0 {
		initialWeight = c.cfg.Deploy.Canary.InitialWeight
	}
	if initialWeight == 0 {
		initialWeight = 10 // Default to 10%
	}
	if initialWeight < 1 || initialWeight > 99 {
		return fmt.Errorf("initial canary weight must be between 1 and 99")
	}

	// Get current stable version
	stableVersion := ""
	if lastDeploy, err := c.history.GetLastSuccessful(c.cfg.Service); err == nil {
		stableVersion = lastDeploy.Version
	}

	// Initialize state early so it survives across CLI invocations.
	now := time.Now()
	c.state = &CanaryState{
		Service:         c.cfg.Service,
		Status:          CanaryStatusDeploying,
		StableVersion:   stableVersion,
		CanaryVersion:   opts.Version,
		CurrentWeight:   initialWeight,
		TargetWeight:    100,
		StartedAt:       now,
		LastUpdated:     now,
		Hosts:           hosts,
		CanaryContainer: fmt.Sprintf("%s-canary", c.cfg.Service),
		StableContainer: c.cfg.Service,
	}
	if err := c.saveStateLocked(); err != nil {
		return err
	}

	var touchedHosts []string
	weightedHosts := make(map[string]bool)
	routedHosts := make(map[string]bool)
	cleanupTouched := func(deployErr error) error {
		var cleanupErrors []string
		for i := len(touchedHosts) - 1; i >= 0; i-- {
			host := touchedHosts[i]
			proxyHost := c.proxyRouteHost()
			safeToRemove := true
			if routedHosts[host] || weightedHosts[host] {
				canaryUpstream, err := c.upstreamAddr(host, c.state.CanaryContainer)
				if err != nil {
					cleanupErrors = append(cleanupErrors, fmt.Sprintf("%s resolve canary route: %v", host, err))
					safeToRemove = false
				} else {
					stableUpstream, stableErr := c.upstreamAddr(host, c.state.StableContainer)
					switch {
					case stableErr != nil:
						cleanupErrors = append(cleanupErrors, fmt.Sprintf("%s resolve stable route: %v", host, stableErr))
						safeToRemove = false
					case proxyHost == "":
						cleanupErrors = append(cleanupErrors, fmt.Sprintf("%s restore stable traffic: proxy host is not configured", host))
						safeToRemove = false
					default:
						if restoreErr := c.proxy.SetCanaryWeights(host, proxyHost, stableUpstream, 100, canaryUpstream, 0); restoreErr != nil {
							cleanupErrors = append(cleanupErrors, fmt.Sprintf("%s restore stable traffic: %v", host, restoreErr))
							safeToRemove = false
						}
					}
				}
			}
			if safeToRemove {
				if err := c.containers.Remove(host, c.state.CanaryContainer, true); err != nil {
					cleanupErrors = append(cleanupErrors, fmt.Sprintf("%s remove canary container: %v", host, err))
				}
			}
		}
		if len(cleanupErrors) > 0 {
			// Keep an explicitly recoverable state. A later rollback can retry
			// route cleanup without killing a container that may still be live.
			c.state.Status = CanaryStatusRollingBack
		} else {
			c.state.Status = CanaryStatusNone
		}
		c.state.LastUpdated = time.Now()
		if err := c.saveStateLocked(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("persist reset state: %v", err))
		}
		if len(cleanupErrors) > 0 {
			return fmt.Errorf("%w (cleanup incomplete: %s)", deployErr, strings.Join(cleanupErrors, "; "))
		}
		return deployErr
	}

	// Pull image on all hosts
	if !opts.SkipPull {
		c.log.Info("Pulling image on all hosts...")
		errors := c.images.PullAll(hosts, image)
		if len(errors) > 0 {
			c.state.Status = CanaryStatusNone
			c.state.LastUpdated = time.Now()
			var errMsgs []string
			for host, err := range errors {
				errMsgs = append(errMsgs, fmt.Sprintf("%s: %v", host, err))
			}
			pullErr := fmt.Errorf("pull failed: %s", strings.Join(errMsgs, "; "))
			if saveErr := c.saveStateLocked(); saveErr != nil {
				return fmt.Errorf("%v (canary state could not be persisted: %v)", pullErr, saveErr)
			}
			return pullErr
		}
	}

	// Deploy canary container on each host
	canaryContainerName := c.state.CanaryContainer

	for _, host := range hosts {
		phases := []output.Phase{
			{Name: "Pull", Complete: !opts.SkipPull},
			{Name: "Container", Complete: false},
			{Name: "Health", Complete: false},
			{Name: "Proxy", Complete: false},
		}
		c.log.HostPhase(host, phases)

		c.log.Host(host, "Deploying canary container...")
		stableExists, err := c.containers.Exists(host, c.state.StableContainer)
		if err != nil {
			return cleanupTouched(fmt.Errorf("failed to inspect stable container on %s: %w", host, err))
		}
		if !stableExists {
			return cleanupTouched(fmt.Errorf("stable container %s does not exist on %s", c.state.StableContainer, host))
		}

		// Build container config
		containerConfig := c.buildContainerConfig(image, canaryContainerName)

		// Start canary container
		_, err = c.containers.Run(host, containerConfig)
		if err != nil {
			return cleanupTouched(fmt.Errorf("failed to start canary on %s: %w", host, err))
		}
		touchedHosts = append(touchedHosts, host)

		phases[1].Complete = true
		c.log.HostPhase(host, phases)

		// Wait for readiness check
		readinessPath := c.cfg.Proxy.Healthcheck.GetReadinessPath()
		if !opts.SkipHealthCheck && readinessPath != "" {
			c.log.Host(host, "Waiting for canary readiness check...")

			if c.cfg.Deploy.ReadinessDelay > 0 {
				time.Sleep(c.cfg.Deploy.ReadinessDelay)
			}

			if err := c.waitForHealthy(host, canaryContainerName); err != nil {
				return cleanupTouched(fmt.Errorf("canary health check failed on %s: %w", host, err))
			}
		}

		phases[2].Complete = true
		c.log.HostPhase(host, phases)

		// Register canary with proxy at initial weight
		canaryUpstream, err := c.upstreamAddr(host, canaryContainerName)
		if err != nil {
			return cleanupTouched(err)
		}
		stableWeight := 100 - initialWeight

		c.log.Host(host, "Registering canary with proxy (weight=%d%%, stable=%d%%)", initialWeight, stableWeight)

		// Ensure the proxy container is running before admin API calls.
		if err := c.proxy.Boot(host, newProxyConfigFromCfg(c.cfg)); err != nil {
			return cleanupTouched(fmt.Errorf("failed to boot proxy on %s: %w", host, err))
		}
		if err := c.proxy.EnsureConfig(host); err != nil {
			return cleanupTouched(fmt.Errorf("failed to ensure proxy config on %s: %w", host, err))
		}

		// Apply and verify the complete split atomically. Stock Caddy represents
		// the ratio through repeated upstreams under its built-in random policy.
		stableUpstream, err := c.upstreamAddr(host, c.cfg.Service)
		if err != nil {
			return cleanupTouched(fmt.Errorf("failed to resolve stable upstream on %s: %w", host, err))
		}
		proxyHost := c.proxyRouteHost()
		if err := c.proxy.SetCanaryWeights(host, proxyHost, stableUpstream, stableWeight, canaryUpstream, initialWeight); err != nil {
			return cleanupTouched(fmt.Errorf("failed to apply canary traffic split on %s: %w", host, err))
		}
		weightedHosts[host] = true
		routedHosts[host] = true

		phases[3].Complete = true
		c.log.HostPhase(host, phases)

		c.log.HostSuccess(host, "Canary deployed successfully")
	}

	// Update state
	c.state.Status = CanaryStatusRunning
	c.state.LastUpdated = time.Now()
	if err := c.saveStateLocked(); err != nil {
		return cleanupTouched(fmt.Errorf("failed to persist running canary state: %w", err))
	}

	c.log.Success("Canary deployment started: %d%% traffic to canary", initialWeight)
	c.log.TrafficBar(initialWeight,
		fmt.Sprintf("canary (%s)", opts.Version),
		fmt.Sprintf("stable (%s)", stableVersion))

	// Record deployment
	record := NewDeploymentRecord(c.cfg.Service, image, opts.Version, opts.Destination, hosts)
	record.Metadata["type"] = "canary"
	record.Metadata["weight"] = fmt.Sprintf("%d", initialWeight)
	record.Complete()
	if err := c.history.Record(record); err != nil {
		return fmt.Errorf("canary is running but durable history persistence failed: %w", err)
	}

	return nil
}

// Promote shifts all traffic to the canary and removes the old stable container.
func (c *CanaryDeployer) Promote() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if err := c.loadStateLocked(); err != nil {
		return err
	}

	if c.state.Status != CanaryStatusRunning && c.state.Status != CanaryStatusPromoting {
		return fmt.Errorf("no canary deployment in progress")
	}

	c.state.Status = CanaryStatusPromoting
	c.state.LastUpdated = time.Now()
	if err := c.saveStateLocked(); err != nil {
		return err
	}
	c.log.Header("Promoting canary to production")

	for _, host := range c.state.Hosts {
		c.log.Host(host, "Promoting canary...")
		canaryExists, err := c.containers.Exists(host, c.state.CanaryContainer)
		if err != nil {
			return fmt.Errorf("failed to inspect canary on %s: %w", host, err)
		}
		if !canaryExists {
			stableExists, stableErr := c.containers.Exists(host, c.state.StableContainer)
			if stableErr != nil || !stableExists {
				return fmt.Errorf("neither recoverable canary nor stable container exists on %s", host)
			}
			continue
		}
		canaryUpstream, err := c.upstreamAddr(host, c.state.CanaryContainer)
		if err != nil {
			return err
		}
		stableExists, err := c.containers.Exists(host, c.state.StableContainer)
		if err != nil {
			return fmt.Errorf("failed to inspect stable container on %s: %w", host, err)
		}

		proxyHost := c.proxyRouteHost()
		if err := c.proxy.Boot(host, newProxyConfigFromCfg(c.cfg)); err != nil {
			return fmt.Errorf("failed to boot proxy on %s: %w", host, err)
		}
		if err := c.proxy.EnsureConfig(host); err != nil {
			return fmt.Errorf("failed to ensure proxy config on %s: %w", host, err)
		}

		if stableExists {
			stableUpstream, err := c.upstreamAddr(host, c.state.StableContainer)
			if err != nil {
				return err
			}
			// Atomically shift all traffic away from stable before draining it.
			if err := c.proxy.SetCanaryWeights(host, proxyHost, stableUpstream, 0, canaryUpstream, 100); err != nil {
				return fmt.Errorf("failed to route all traffic to canary on %s: %w", host, err)
			}
			if c.cfg.Deploy.DrainTimeout > 0 {
				c.log.Host(host, "Draining connections...")
				if err := c.proxy.DrainUpstream(host, stableUpstream, c.cfg.Deploy.DrainTimeout); err != nil {
					return fmt.Errorf("failed to drain stable upstream on %s: %w", host, err)
				}
			}
			c.log.Host(host, "Removing old stable container...")
			if err := c.containers.Remove(host, c.state.StableContainer, true); err != nil {
				return fmt.Errorf("failed to remove stable container on %s: %w", host, err)
			}
		}

		// In bridge mode, establish the final stable-name route while the
		// canary's stable network alias still resolves, then remove the temp
		// route. A rename can no longer invalidate the only working route.
		if !c.cfg.UseHostPortUpstreams() {
			finalUpstream := fmt.Sprintf("%s:%d", c.state.StableContainer, c.cfg.Proxy.AppPort)
			if err := c.proxy.AddUpstream(host, proxyHost, finalUpstream); err != nil {
				return fmt.Errorf("failed to add final promoted upstream on %s: %w", host, err)
			}
			if err := c.proxy.RemoveUpstream(host, proxyHost, canaryUpstream); err != nil {
				return fmt.Errorf("failed to remove temporary canary upstream on %s: %w", host, err)
			}
		}

		c.log.Host(host, "Finalizing promotion...")
		if err := c.containers.Rename(host, c.state.CanaryContainer, c.state.StableContainer); err != nil {
			return fmt.Errorf("failed to rename promoted canary on %s: %w", host, err)
		}

		c.log.HostSuccess(host, "Canary promoted")
	}

	c.log.TrafficBar(100,
		fmt.Sprintf("promoted (%s)", c.state.CanaryVersion),
		"stable (removed)")

	// Reset state
	c.state = &CanaryState{
		Service:     c.cfg.Service,
		Status:      CanaryStatusNone,
		LastUpdated: time.Now(),
	}
	if err := c.saveStateLocked(); err != nil {
		return fmt.Errorf("promotion completed but state reset failed: %w", err)
	}

	c.log.Success("Canary promoted to production!")
	return nil
}

// Rollback removes the canary and restores full traffic to the stable version.
func (c *CanaryDeployer) Rollback() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if err := c.history.EnsureAvailable(); err != nil {
		return fmt.Errorf("durable deployment history is unavailable: %w", err)
	}

	if err := c.loadStateLocked(); err != nil {
		return err
	}

	if c.state.Status != CanaryStatusRunning && c.state.Status != CanaryStatusRollingBack && c.state.Status != CanaryStatusDeploying {
		return fmt.Errorf("no canary deployment in progress")
	}

	c.state.Status = CanaryStatusRollingBack
	c.state.LastUpdated = time.Now()
	if err := c.saveStateLocked(); err != nil {
		return err
	}
	c.log.Header("Rolling back canary deployment")

	for _, host := range c.state.Hosts {
		c.log.Host(host, "Rolling back canary...")
		canaryExists, err := c.containers.Exists(host, c.state.CanaryContainer)
		if err != nil {
			return fmt.Errorf("failed to inspect canary on %s: %w", host, err)
		}
		if !canaryExists {
			continue
		}
		canaryUpstream, err := c.upstreamAddr(host, c.state.CanaryContainer)
		if err != nil {
			return err
		}
		stableUpstream, err := c.upstreamAddr(host, c.state.StableContainer)
		if err != nil {
			return err
		}

		// Restore stable to 100% and remove canary from selection atomically.
		proxyHost := c.proxyRouteHost()
		if err := c.proxy.SetCanaryWeights(host, proxyHost, stableUpstream, 100, canaryUpstream, 0); err != nil {
			return fmt.Errorf("failed to restore stable traffic on %s: %w", host, err)
		}

		// Drain in-flight requests to the canary before stopping it
		if c.cfg.Deploy.DrainTimeout > 0 {
			c.log.Host(host, "Draining canary connections...")
			if err := c.proxy.DrainUpstream(host, canaryUpstream, c.cfg.Deploy.DrainTimeout); err != nil {
				return fmt.Errorf("failed to drain canary upstream on %s: %w", host, err)
			}
		}

		// Stop and remove canary container
		c.log.Host(host, "Removing canary container...")
		stopTimeout := c.cfg.Deploy.GetStopTimeout()
		if err := c.containers.Stop(host, c.state.CanaryContainer, stopTimeout); err != nil {
			return fmt.Errorf("failed to stop canary container on %s: %w", host, err)
		}
		if err := c.containers.Remove(host, c.state.CanaryContainer, true); err != nil {
			return fmt.Errorf("failed to remove canary container on %s: %w", host, err)
		}

		c.log.HostSuccess(host, "Canary rolled back")
	}

	// Record rollback in history
	record := &DeploymentRecord{
		ID:              GenerateDeploymentID(),
		Service:         c.cfg.Service,
		Version:         c.state.CanaryVersion,
		Hosts:           c.state.Hosts,
		Status:          StatusRolledBack,
		StartedAt:       c.state.StartedAt,
		CompletedAt:     time.Now(),
		RolledBack:      true,
		PreviousVersion: c.state.StableVersion,
		Metadata:        map[string]string{"type": "canary_rollback"},
	}
	record.Duration = record.CompletedAt.Sub(record.StartedAt)
	historyErr := c.history.Record(record)

	c.log.TrafficBar(0,
		"canary (removed)",
		fmt.Sprintf("stable (%s)", c.state.StableVersion))

	// Reset state
	c.state = &CanaryState{
		Service:     c.cfg.Service,
		Status:      CanaryStatusNone,
		LastUpdated: time.Now(),
	}
	if err := c.saveStateLocked(); err != nil {
		if historyErr != nil {
			return fmt.Errorf("rollback completed but history persistence failed (%v) and state reset failed: %w", historyErr, err)
		}
		return fmt.Errorf("rollback completed but state reset failed: %w", err)
	}
	if historyErr != nil {
		return fmt.Errorf("rollback completed but durable history persistence failed: %w", historyErr)
	}

	c.log.Success("Canary rolled back successfully!")
	return nil
}

func (c *CanaryDeployer) Status() (*CanaryState, error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if err := c.loadStateLocked(); err != nil {
		return nil, err
	}

	// Return a copy to prevent external modification
	stateCopy := *c.state
	return &stateCopy, nil
}

func (c *CanaryDeployer) SetWeight(weight int) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if err := c.loadStateLocked(); err != nil {
		return err
	}

	if c.state.Status != CanaryStatusRunning {
		return fmt.Errorf("no canary deployment in progress")
	}

	if weight < 0 || weight > 100 {
		return fmt.Errorf("weight must be between 0 and 100")
	}

	c.log.Info("Adjusting canary weight to %d%%", weight)

	stableWeight := 100 - weight

	for _, host := range c.state.Hosts {
		canaryUpstream, err := c.upstreamAddr(host, c.state.CanaryContainer)
		if err != nil {
			return err
		}
		stableUpstream, err := c.upstreamAddr(host, c.state.StableContainer)
		if err != nil {
			return err
		}
		proxyHost := c.proxyRouteHost()
		if err := c.proxy.SetCanaryWeights(host, proxyHost, stableUpstream, stableWeight, canaryUpstream, weight); err != nil {
			return fmt.Errorf("failed to apply canary traffic split on %s: %w", host, err)
		}
	}

	c.state.CurrentWeight = weight
	c.state.LastUpdated = time.Now()
	if err := c.saveStateLocked(); err != nil {
		return err
	}

	c.log.Success("Canary weight adjusted: %d%% canary, %d%% stable", weight, stableWeight)
	c.log.TrafficBar(weight,
		fmt.Sprintf("canary (%s)", c.state.CanaryVersion),
		fmt.Sprintf("stable (%s)", c.state.StableVersion))
	return nil
}

func (c *CanaryDeployer) upstreamAddr(host, name string) (string, error) {
	if !c.cfg.UseHostPortUpstreams() {
		return fmt.Sprintf("%s:%d", name, c.cfg.Proxy.AppPort), nil
	}

	port, err := c.containers.HostPort(host, name, c.cfg.Proxy.AppPort)
	if err != nil {
		return "", fmt.Errorf("failed to resolve host port for %s on %s: %w", name, host, err)
	}
	return fmt.Sprintf("127.0.0.1:%d", port), nil
}

func (c *CanaryDeployer) proxyRouteHost() string {
	return c.cfg.Proxy.PrimaryHost()
}

func (c *CanaryDeployer) buildContainerConfig(image, name string) *podman.ContainerConfig {
	return newAppContainerConfig(c.cfg, image, name, map[string]string{
		"azud.canary": "true",
	})
}

func (c *CanaryDeployer) waitForHealthy(host, container string) error {
	return waitForContainerHealthy(c.cfg, c.podman, c.sshClient, host, container)
}

func (c *CanaryDeployer) ensureRemoteSecrets(hosts []string) error {
	return ValidateRemoteSecrets(c.sshClient, hosts, config.RemoteSecretsPath(c.cfg), c.cfg.Env.Secret)
}

func (c *CanaryDeployer) loadState() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if err := c.loadStateLocked(); err != nil {
		c.log.Warn("Failed to load canary state: %v", err)
	}
}

func (c *CanaryDeployer) loadStateLocked() error {
	if c.statePath == "" {
		return fmt.Errorf("canary state path is not configured")
	}

	// Acquire file lock to coordinate with other CLI processes
	lockPath := c.statePath + ".lock"
	lock, err := state.AcquireFileLock(lockPath)
	if err != nil {
		return fmt.Errorf("failed to acquire canary state lock: %w", err)
	}
	defer func() { _ = lock.Release() }()

	data, err := os.ReadFile(c.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read canary state: %w", err)
		}
		return nil
	}

	var s CanaryState
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("failed to parse canary state: %w", err)
	}

	if s.Service != "" && s.Service != c.cfg.Service {
		return fmt.Errorf("canary state belongs to service %s, not %s", s.Service, c.cfg.Service)
	}
	if s.Service == "" {
		s.Service = c.cfg.Service
	}

	c.state = &s
	return nil
}

func (c *CanaryDeployer) saveStateLocked() error {
	if c.statePath == "" || c.state == nil {
		return fmt.Errorf("canary state path or state is not configured")
	}

	// Acquire file lock to coordinate with other CLI processes
	lockPath := c.statePath + ".lock"
	lock, err := state.AcquireFileLock(lockPath)
	if err != nil {
		return fmt.Errorf("failed to acquire canary state lock: %w", err)
	}
	defer func() { _ = lock.Release() }()

	s := *c.state
	if s.Service == "" {
		s.Service = c.cfg.Service
	}

	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal canary state: %w", err)
	}

	dir := filepath.Dir(c.statePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create canary state dir: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("failed to secure canary state dir: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, ".canary-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create canary state temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmpFile.Chmod(0600); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to secure canary state: %w", err)
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write canary state: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to sync canary state: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close canary state: %w", err)
	}
	if err := os.Rename(tmpPath, c.statePath); err != nil {
		return fmt.Errorf("failed to persist canary state: %w", err)
	}
	return nil
}
