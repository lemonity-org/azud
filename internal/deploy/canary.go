package deploy

import (
	"fmt"
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
	CanaryStatusNone      CanaryStatus = "none"
	CanaryStatusDeploying CanaryStatus = "deploying"
	CanaryStatusRunning   CanaryStatus = "running"
	CanaryStatusPromoting CanaryStatus = "promoting"
	CanaryStatusRollingBack CanaryStatus = "rolling_back"
)

// CanaryState holds the current state of a canary deployment
type CanaryState struct {
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
}

func NewCanaryDeployer(cfg *config.Config, sshClient *ssh.Client, log *output.Logger) *CanaryDeployer {
	if log == nil {
		log = output.DefaultLogger
	}

	podmanClient := podman.NewClient(sshClient)

	return &CanaryDeployer{
		cfg:        cfg,
		sshClient:  sshClient,
		podman:     podmanClient,
		containers: podman.NewContainerManager(podmanClient),
		images:     podman.NewImageManager(podmanClient),
		proxy:      proxy.NewManager(sshClient, log),
		history:    NewHistoryStore(".", cfg.Deploy.RetainHistory, log),
		log:        log,
		state: &CanaryState{
			Status: CanaryStatusNone,
		},
	}
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
		return fmt.Errorf("no hosts to deploy to")
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

	// Pull image on all hosts
	if !opts.SkipPull {
		c.log.Info("Pulling image on all hosts...")
		errors := c.images.PullAll(hosts, image)
		if len(errors) > 0 {
			c.state.Status = CanaryStatusNone
			var errMsgs []string
			for host, err := range errors {
				errMsgs = append(errMsgs, fmt.Sprintf("%s: %v", host, err))
			}
			return fmt.Errorf("pull failed: %s", strings.Join(errMsgs, "; "))
		}
	}

	// Get current stable version
	stableVersion := ""
	if lastDeploy, err := c.history.GetLastSuccessful(c.cfg.Service); err == nil {
		stableVersion = lastDeploy.Version
	}

	// Deploy canary container on each host
	canaryContainerName := fmt.Sprintf("%s-canary", c.cfg.Service)

	for _, host := range hosts {
		c.log.Host(host, "Deploying canary container...")

		// Build container config
		containerConfig := c.buildContainerConfig(image, canaryContainerName)

		// Start canary container
		_, err := c.containers.Run(host, containerConfig)
		if err != nil {
			c.state.Status = CanaryStatusNone
			return fmt.Errorf("failed to start canary on %s: %w", host, err)
		}

		// Wait for health check
		if !opts.SkipHealthCheck && c.cfg.Proxy.Healthcheck.Path != "" {
			c.log.Host(host, "Waiting for canary health check...")

			if c.cfg.Deploy.ReadinessDelay > 0 {
				time.Sleep(c.cfg.Deploy.ReadinessDelay)
			}

			if err := c.waitForHealthy(host, canaryContainerName); err != nil {
				// Cleanup failed canary
				c.containers.Remove(host, canaryContainerName, true)
				c.state.Status = CanaryStatusNone
				return fmt.Errorf("canary health check failed on %s: %w", host, err)
			}
		}

		// Register canary with proxy at initial weight
		canaryUpstream := fmt.Sprintf("%s:%d", canaryContainerName, c.cfg.Proxy.AppPort)
		stableWeight := 100 - initialWeight

		c.log.Host(host, "Registering canary with proxy (weight=%d%%, stable=%d%%)", initialWeight, stableWeight)

		// Update stable container weight first
		stableUpstream := fmt.Sprintf("%s:%d", c.cfg.Service, c.cfg.Proxy.AppPort)
		if err := c.proxy.SetUpstreamWeight(host, c.cfg.Proxy.Host, stableUpstream, stableWeight); err != nil {
			c.log.Debug("Failed to set stable weight (may not exist yet): %v", err)
		}

		// Add canary upstream with initial weight
		if err := c.proxy.AddWeightedUpstream(host, c.cfg.Proxy.Host, canaryUpstream, initialWeight); err != nil {
			return fmt.Errorf("failed to register canary with proxy on %s: %w", host, err)
		}

		c.log.HostSuccess(host, "Canary deployed successfully")
	}

	// Update state
	c.state = &CanaryState{
		Status:          CanaryStatusRunning,
		StableVersion:   stableVersion,
		CanaryVersion:   opts.Version,
		CurrentWeight:   initialWeight,
		TargetWeight:    100,
		StartedAt:       time.Now(),
		LastUpdated:     time.Now(),
		Hosts:           hosts,
		CanaryContainer: canaryContainerName,
		StableContainer: c.cfg.Service,
	}

	c.log.Success("Canary deployment started: %d%% traffic to canary", initialWeight)

	// Record deployment
	record := NewDeploymentRecord(c.cfg.Service, image, opts.Version, opts.Destination, hosts)
	record.Metadata["type"] = "canary"
	record.Metadata["weight"] = fmt.Sprintf("%d", initialWeight)
	record.Complete()
	c.history.Record(record)

	return nil
}

// Promote shifts all traffic to the canary and removes the old stable container.
func (c *CanaryDeployer) Promote() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if c.state.Status != CanaryStatusRunning {
		return fmt.Errorf("no canary deployment in progress")
	}

	c.state.Status = CanaryStatusPromoting
	c.log.Header("Promoting canary to production")

	canaryUpstream := fmt.Sprintf("%s:%d", c.state.CanaryContainer, c.cfg.Proxy.AppPort)
	stableUpstream := fmt.Sprintf("%s:%d", c.state.StableContainer, c.cfg.Proxy.AppPort)

	for _, host := range c.state.Hosts {
		c.log.Host(host, "Promoting canary...")

		// Set canary to 100%
		if err := c.proxy.SetUpstreamWeight(host, c.cfg.Proxy.Host, canaryUpstream, 100); err != nil {
			return fmt.Errorf("failed to update canary weight on %s: %w", host, err)
		}

		// Remove stable upstream
		if err := c.proxy.RemoveUpstream(host, c.cfg.Proxy.Host, stableUpstream); err != nil {
			c.log.Debug("Failed to remove stable upstream: %v", err)
		}

		// Wait for drain
		if c.cfg.Deploy.DrainTimeout > 0 {
			c.log.Host(host, "Draining connections...")
			time.Sleep(c.cfg.Deploy.DrainTimeout)
		}

		// Stop and remove old stable container
		c.log.Host(host, "Removing old stable container...")
		if err := c.containers.Stop(host, c.state.StableContainer, 30); err != nil {
			c.log.Debug("Failed to stop stable container: %v", err)
		}
		if err := c.containers.Remove(host, c.state.StableContainer, true); err != nil {
			c.log.Debug("Failed to remove stable container: %v", err)
		}

		// Rename canary to stable
		c.log.Host(host, "Finalizing promotion...")
		if err := c.containers.Rename(host, c.state.CanaryContainer, c.state.StableContainer); err != nil {
			c.log.Warn("Failed to rename container: %v", err)
		}

		// Update proxy with new container name
		newUpstream := fmt.Sprintf("%s:%d", c.state.StableContainer, c.cfg.Proxy.AppPort)
		if err := c.proxy.AddUpstream(host, c.cfg.Proxy.Host, newUpstream); err != nil {
			c.log.Debug("Failed to update proxy upstream: %v", err)
		}
		if err := c.proxy.RemoveUpstream(host, c.cfg.Proxy.Host, canaryUpstream); err != nil {
			c.log.Debug("Failed to remove canary upstream: %v", err)
		}

		c.log.HostSuccess(host, "Canary promoted")
	}

	// Reset state
	c.state = &CanaryState{
		Status: CanaryStatusNone,
	}

	c.log.Success("Canary promoted to production!")
	return nil
}

// Rollback removes the canary and restores full traffic to the stable version.
func (c *CanaryDeployer) Rollback() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if c.state.Status != CanaryStatusRunning {
		return fmt.Errorf("no canary deployment in progress")
	}

	c.state.Status = CanaryStatusRollingBack
	c.log.Header("Rolling back canary deployment")

	canaryUpstream := fmt.Sprintf("%s:%d", c.state.CanaryContainer, c.cfg.Proxy.AppPort)
	stableUpstream := fmt.Sprintf("%s:%d", c.state.StableContainer, c.cfg.Proxy.AppPort)

	for _, host := range c.state.Hosts {
		c.log.Host(host, "Rolling back canary...")

		// Remove canary from proxy
		if err := c.proxy.RemoveUpstream(host, c.cfg.Proxy.Host, canaryUpstream); err != nil {
			c.log.Debug("Failed to remove canary upstream: %v", err)
		}

		// Restore stable to 100%
		if err := c.proxy.SetUpstreamWeight(host, c.cfg.Proxy.Host, stableUpstream, 100); err != nil {
			c.log.Debug("Failed to restore stable weight: %v", err)
		}

		// Stop and remove canary container
		c.log.Host(host, "Removing canary container...")
		if err := c.containers.Stop(host, c.state.CanaryContainer, 30); err != nil {
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
	c.history.Record(record)

	// Reset state
	c.state = &CanaryState{
		Status: CanaryStatusNone,
	}

	c.log.Success("Canary rolled back successfully!")
	return nil
}

func (c *CanaryDeployer) Status() *CanaryState {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()

	// Return a copy to prevent external modification
	stateCopy := *c.state
	return &stateCopy
}

func (c *CanaryDeployer) SetWeight(weight int) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if c.state.Status != CanaryStatusRunning {
		return fmt.Errorf("no canary deployment in progress")
	}

	if weight < 0 || weight > 100 {
		return fmt.Errorf("weight must be between 0 and 100")
	}

	c.log.Info("Adjusting canary weight to %d%%", weight)

	canaryUpstream := fmt.Sprintf("%s:%d", c.state.CanaryContainer, c.cfg.Proxy.AppPort)
	stableUpstream := fmt.Sprintf("%s:%d", c.state.StableContainer, c.cfg.Proxy.AppPort)
	stableWeight := 100 - weight

	for _, host := range c.state.Hosts {
		if err := c.proxy.SetUpstreamWeight(host, c.cfg.Proxy.Host, canaryUpstream, weight); err != nil {
			return fmt.Errorf("failed to set canary weight on %s: %w", host, err)
		}
		if err := c.proxy.SetUpstreamWeight(host, c.cfg.Proxy.Host, stableUpstream, stableWeight); err != nil {
			return fmt.Errorf("failed to set stable weight on %s: %w", host, err)
		}
	}

	c.state.CurrentWeight = weight
	c.state.LastUpdated = time.Now()

	c.log.Success("Canary weight adjusted: %d%% canary, %d%% stable", weight, stableWeight)
	return nil
}

func (c *CanaryDeployer) buildContainerConfig(image, name string) *podman.ContainerConfig {
	return newAppContainerConfig(c.cfg, image, name, map[string]string{
		"azud.canary": "true",
	})
}

func (c *CanaryDeployer) waitForHealthy(host, container string) error {
	return waitForContainerHealthy(c.cfg, c.podman, c.sshClient, host, container)
}
