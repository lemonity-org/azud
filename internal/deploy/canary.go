package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/podman"
	"github.com/adriancarayol/azud/internal/proxy"
	"github.com/adriancarayol/azud/internal/ssh"
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

	deployer := &CanaryDeployer{
		cfg:        cfg,
		sshClient:  sshClient,
		podman:     podmanClient,
		containers: podman.NewContainerManager(podmanClient),
		images:     podman.NewImageManager(podmanClient),
		proxy:      proxy.NewManager(sshClient, log),
		history:    NewHistoryStore(".", cfg.Deploy.RetainHistory, log),
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

	c.loadStateLocked()

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
		if idx := strings.LastIndex(image, ":"); idx > 0 && !strings.Contains(image[idx:], "/") {
			image = image[:idx]
		}
		image = fmt.Sprintf("%s:%s", image, opts.Version)
	}

	// Get target hosts
	hosts := opts.Hosts
	if len(hosts) == 0 {
		hosts = c.cfg.GetAllHosts()
	}

	if len(hosts) == 0 {
		c.state.Status = CanaryStatusNone
		c.state.LastUpdated = time.Now()
		c.saveStateLocked()
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
	c.saveStateLocked()

	// Pull image on all hosts
	if !opts.SkipPull {
		c.log.Info("Pulling image on all hosts...")
		errors := c.images.PullAll(hosts, image)
		if len(errors) > 0 {
			c.state.Status = CanaryStatusNone
			c.state.LastUpdated = time.Now()
			c.saveStateLocked()
			var errMsgs []string
			for host, err := range errors {
				errMsgs = append(errMsgs, fmt.Sprintf("%s: %v", host, err))
			}
			return fmt.Errorf("pull failed: %s", strings.Join(errMsgs, "; "))
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

		// Build container config
		containerConfig := c.buildContainerConfig(image, canaryContainerName)

		// Start canary container
		_, err := c.containers.Run(host, containerConfig)
		if err != nil {
			c.state.Status = CanaryStatusNone
			c.state.LastUpdated = time.Now()
			c.saveStateLocked()
			return fmt.Errorf("failed to start canary on %s: %w", host, err)
		}

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
				// Cleanup failed canary
				_ = c.containers.Remove(host, canaryContainerName, true)
				c.state.Status = CanaryStatusNone
				c.state.LastUpdated = time.Now()
				c.saveStateLocked()
				return fmt.Errorf("canary health check failed on %s: %w", host, err)
			}
		}

		phases[2].Complete = true
		c.log.HostPhase(host, phases)

		// Register canary with proxy at initial weight
		canaryUpstream := c.upstreamAddr(canaryContainerName)
		stableWeight := 100 - initialWeight

		c.log.Host(host, "Registering canary with proxy (weight=%d%%, stable=%d%%)", initialWeight, stableWeight)

		// Update stable container weight first
		stableUpstream := c.upstreamAddr(c.cfg.Service)
		proxyHost := c.proxyRouteHost()
		if err := c.proxy.SetUpstreamWeight(host, proxyHost, stableUpstream, stableWeight); err != nil {
			c.log.Debug("Failed to set stable weight (may not exist yet): %v", err)
		}

		// Add canary upstream with initial weight
		if err := c.proxy.AddWeightedUpstream(host, proxyHost, canaryUpstream, initialWeight); err != nil {
			c.state.Status = CanaryStatusNone
			c.state.LastUpdated = time.Now()
			c.saveStateLocked()
			return fmt.Errorf("failed to register canary with proxy on %s: %w", host, err)
		}

		phases[3].Complete = true
		c.log.HostPhase(host, phases)

		c.log.HostSuccess(host, "Canary deployed successfully")
	}

	// Update state
	c.state.Status = CanaryStatusRunning
	c.state.LastUpdated = time.Now()
	c.saveStateLocked()

	c.log.Success("Canary deployment started: %d%% traffic to canary", initialWeight)
	c.log.TrafficBar(initialWeight,
		fmt.Sprintf("canary (%s)", opts.Version),
		fmt.Sprintf("stable (%s)", stableVersion))

	// Record deployment
	record := NewDeploymentRecord(c.cfg.Service, image, opts.Version, opts.Destination, hosts)
	record.Metadata["type"] = "canary"
	record.Metadata["weight"] = fmt.Sprintf("%d", initialWeight)
	record.Complete()
	_ = c.history.Record(record)

	return nil
}

// Promote shifts all traffic to the canary and removes the old stable container.
func (c *CanaryDeployer) Promote() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	c.loadStateLocked()

	if c.state.Status != CanaryStatusRunning {
		return fmt.Errorf("no canary deployment in progress")
	}

	c.state.Status = CanaryStatusPromoting
	c.state.LastUpdated = time.Now()
	c.saveStateLocked()
	c.log.Header("Promoting canary to production")

	canaryUpstream := c.upstreamAddr(c.state.CanaryContainer)
	stableUpstream := c.upstreamAddr(c.state.StableContainer)

	for _, host := range c.state.Hosts {
		c.log.Host(host, "Promoting canary...")

		// Set canary to 100%
		proxyHost := c.proxyRouteHost()
		if err := c.proxy.SetUpstreamWeight(host, proxyHost, canaryUpstream, 100); err != nil {
			return fmt.Errorf("failed to update canary weight on %s: %w", host, err)
		}

		// Remove stable upstream from proxy so no new requests are sent to it
		if err := c.proxy.RemoveUpstream(host, proxyHost, stableUpstream); err != nil {
			c.log.Debug("Failed to remove stable upstream: %v", err)
		}

		// Drain: poll Caddy for in-flight requests on the stable upstream
		if c.cfg.Deploy.DrainTimeout > 0 {
			c.log.Host(host, "Draining connections...")
			_ = c.proxy.DrainUpstream(host, stableUpstream, c.cfg.Deploy.DrainTimeout)
		}

		// Stop and remove old stable container
		c.log.Host(host, "Removing old stable container...")
		stopTimeout := c.cfg.Deploy.GetStopTimeout()
		if err := c.containers.Stop(host, c.state.StableContainer, stopTimeout); err != nil {
			c.log.Debug("Failed to stop stable container: %v", err)
		}
		if err := c.containers.Remove(host, c.state.StableContainer, true); err != nil {
			c.log.Debug("Failed to remove stable container: %v", err)
		}

		// Rename canary to stable name
		c.log.Host(host, "Finalizing promotion...")
		if err := c.containers.Rename(host, c.state.CanaryContainer, c.state.StableContainer); err != nil {
			c.log.Warn("Failed to rename container (proxy still points to canary name): %v", err)
			c.log.HostSuccess(host, "Canary promoted (kept canary container name)")
			continue
		}

		// Atomically update proxy to point to the renamed container using
		// a single RegisterService call instead of separate add/remove.
		livenessPath := ""
		if !c.cfg.Proxy.Healthcheck.DisableLiveness {
			livenessPath = c.cfg.Proxy.Healthcheck.GetLivenessPath()
		}
		hosts := c.cfg.Proxy.AllHosts()
		finalUpstream := c.upstreamAddr(c.state.StableContainer)
		serviceConfig := &proxy.ServiceConfig{
			Name:            c.cfg.Service,
			Host:            proxyHost,
			Hosts:           hosts,
			Upstreams:       []string{finalUpstream},
			HealthPath:      livenessPath,
			HealthInterval:  c.cfg.Proxy.Healthcheck.Interval,
			HealthTimeout:   c.cfg.Proxy.Healthcheck.Timeout,
			ResponseTimeout:       c.cfg.Proxy.ResponseTimeout,
			ResponseHeaderTimeout: c.cfg.Proxy.ResponseHeaderTimeout,
			ForwardHeaders:  c.cfg.Proxy.ForwardHeaders,
			BufferRequests:  c.cfg.Proxy.Buffering.Requests,
			BufferResponses: c.cfg.Proxy.Buffering.Responses,
			MaxRequestBody:  c.cfg.Proxy.Buffering.MaxRequestBody,
			BufferMemory:    c.cfg.Proxy.Buffering.Memory,
			HTTPS:           c.cfg.Proxy.SSL,
		}
		if err := c.proxy.RegisterService(host, serviceConfig); err != nil {
			c.log.Warn("Failed to update proxy to final name: %v", err)
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
	c.saveStateLocked()

	c.log.Success("Canary promoted to production!")
	return nil
}

// Rollback removes the canary and restores full traffic to the stable version.
func (c *CanaryDeployer) Rollback() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	c.loadStateLocked()

	if c.state.Status != CanaryStatusRunning {
		return fmt.Errorf("no canary deployment in progress")
	}

	c.state.Status = CanaryStatusRollingBack
	c.state.LastUpdated = time.Now()
	c.saveStateLocked()
	c.log.Header("Rolling back canary deployment")

	canaryUpstream := c.upstreamAddr(c.state.CanaryContainer)
	stableUpstream := c.upstreamAddr(c.state.StableContainer)

	for _, host := range c.state.Hosts {
		c.log.Host(host, "Rolling back canary...")

		// Restore stable to 100% first so it handles all new traffic
		proxyHost := c.proxyRouteHost()
		if err := c.proxy.SetUpstreamWeight(host, proxyHost, stableUpstream, 100); err != nil {
			c.log.Debug("Failed to restore stable weight: %v", err)
		}

		// Remove canary from proxy (stops new traffic to canary)
		if err := c.proxy.RemoveUpstream(host, proxyHost, canaryUpstream); err != nil {
			c.log.Debug("Failed to remove canary upstream: %v", err)
		}

		// Drain in-flight requests to the canary before stopping it
		if c.cfg.Deploy.DrainTimeout > 0 {
			c.log.Host(host, "Draining canary connections...")
			_ = c.proxy.DrainUpstream(host, canaryUpstream, c.cfg.Deploy.DrainTimeout)
		}

		// Stop and remove canary container
		c.log.Host(host, "Removing canary container...")
		stopTimeout := c.cfg.Deploy.GetStopTimeout()
		if err := c.containers.Stop(host, c.state.CanaryContainer, stopTimeout); err != nil {
			c.log.Debug("Failed to stop canary container: %v", err)
		}
		if err := c.containers.Remove(host, c.state.CanaryContainer, true); err != nil {
			c.log.Debug("Failed to remove canary container: %v", err)
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
	_ = c.history.Record(record)

	c.log.TrafficBar(0,
		"canary (removed)",
		fmt.Sprintf("stable (%s)", c.state.StableVersion))

	// Reset state
	c.state = &CanaryState{
		Service:     c.cfg.Service,
		Status:      CanaryStatusNone,
		LastUpdated: time.Now(),
	}
	c.saveStateLocked()

	c.log.Success("Canary rolled back successfully!")
	return nil
}

func (c *CanaryDeployer) Status() *CanaryState {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	c.loadStateLocked()

	// Return a copy to prevent external modification
	stateCopy := *c.state
	return &stateCopy
}

func (c *CanaryDeployer) SetWeight(weight int) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	c.loadStateLocked()

	if c.state.Status != CanaryStatusRunning {
		return fmt.Errorf("no canary deployment in progress")
	}

	if weight < 0 || weight > 100 {
		return fmt.Errorf("weight must be between 0 and 100")
	}

	c.log.Info("Adjusting canary weight to %d%%", weight)

	canaryUpstream := c.upstreamAddr(c.state.CanaryContainer)
	stableUpstream := c.upstreamAddr(c.state.StableContainer)
	stableWeight := 100 - weight

	for _, host := range c.state.Hosts {
		proxyHost := c.proxyRouteHost()
		if err := c.proxy.SetUpstreamWeight(host, proxyHost, canaryUpstream, weight); err != nil {
			return fmt.Errorf("failed to set canary weight on %s: %w", host, err)
		}
		if err := c.proxy.SetUpstreamWeight(host, proxyHost, stableUpstream, stableWeight); err != nil {
			return fmt.Errorf("failed to set stable weight on %s: %w", host, err)
		}
	}

	c.state.CurrentWeight = weight
	c.state.LastUpdated = time.Now()
	c.saveStateLocked()

	c.log.Success("Canary weight adjusted: %d%% canary, %d%% stable", weight, stableWeight)
	c.log.TrafficBar(weight,
		fmt.Sprintf("canary (%s)", c.state.CanaryVersion),
		fmt.Sprintf("stable (%s)", c.state.StableVersion))
	return nil
}

// upstreamAddr returns "name:port" for the configured application port.
func (c *CanaryDeployer) upstreamAddr(name string) string {
	return fmt.Sprintf("%s:%d", name, c.cfg.Proxy.AppPort)
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
	c.loadStateLocked()
}

func (c *CanaryDeployer) loadStateLocked() {
	if c.statePath == "" {
		return
	}

	data, err := os.ReadFile(c.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			c.log.Debug("Failed to read canary state: %v", err)
		}
		return
	}

	var state CanaryState
	if err := json.Unmarshal(data, &state); err != nil {
		c.log.Debug("Failed to parse canary state: %v", err)
		return
	}

	if state.Service != "" && state.Service != c.cfg.Service {
		c.log.Debug("Ignoring canary state for service %s", state.Service)
		return
	}
	if state.Service == "" {
		state.Service = c.cfg.Service
	}

	c.state = &state
}

func (c *CanaryDeployer) saveStateLocked() {
	if c.statePath == "" || c.state == nil {
		return
	}

	state := *c.state
	if state.Service == "" {
		state.Service = c.cfg.Service
	}

	data, err := json.MarshalIndent(&state, "", "  ")
	if err != nil {
		c.log.Debug("Failed to marshal canary state: %v", err)
		return
	}

	dir := filepath.Dir(c.statePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		c.log.Debug("Failed to create canary state dir: %v", err)
		return
	}

	tmpPath := c.statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		c.log.Debug("Failed to write canary state: %v", err)
		return
	}
	if err := os.Rename(tmpPath, c.statePath); err != nil {
		c.log.Debug("Failed to persist canary state: %v", err)
		return
	}
}
