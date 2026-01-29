package deploy

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/adriancarayol/azud/internal/config"
	"github.com/adriancarayol/azud/internal/docker"
	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/proxy"
	"github.com/adriancarayol/azud/internal/ssh"
)

// Deployer handles application deployments
type Deployer struct {
	cfg          *config.Config
	sshClient    *ssh.Client
	docker       *docker.Client
	containers   *docker.ContainerManager
	images       *docker.ImageManager
	registry     *docker.RegistryManager
	proxy        *proxy.Manager
	hooks        *HookRunner
	history      *HistoryStore
	log          *output.Logger
}

// NewDeployer creates a new deployer
func NewDeployer(cfg *config.Config, sshClient *ssh.Client, log *output.Logger) *Deployer {
	if log == nil {
		log = output.DefaultLogger
	}

	dockerClient := docker.NewClient(sshClient)

	return &Deployer{
		cfg:        cfg,
		sshClient:  sshClient,
		docker:     dockerClient,
		containers: docker.NewContainerManager(dockerClient),
		images:     docker.NewImageManager(dockerClient),
		registry:   docker.NewRegistryManager(dockerClient),
		proxy:      proxy.NewManager(sshClient, log),
		hooks:      NewHookRunner(cfg.HooksPath, log),
		history:    NewHistoryStore(".", cfg.Deploy.RetainHistory, log),
		log:        log,
	}
}

// DeployOptions holds deployment options
type DeployOptions struct {
	// Image tag to deploy (default: latest)
	Version string

	// Skip image pull (assume already present)
	SkipPull bool

	// Skip health check wait
	SkipHealthCheck bool

	// Specific hosts to deploy to
	Hosts []string

	// Specific roles to deploy
	Roles []string

	// Destination environment (for history tracking)
	Destination string
}

// Deploy performs a full deployment
func (d *Deployer) Deploy(opts *DeployOptions) error {
	timer := d.log.NewTimer("Deployment")
	defer timer.Stop()

	// Determine the image to deploy
	image := d.cfg.Image
	version := opts.Version
	if version != "" {
		// If explicit version provided, use it
		// Strip existing tag if present
		if idx := strings.LastIndex(image, ":"); idx > 0 && !strings.Contains(image[idx:], "/") {
			image = image[:idx]
		}
		image = fmt.Sprintf("%s:%s", image, version)
	} else if !strings.Contains(image, ":") {
		// No tag in config and no version specified, use latest
		version = "latest"
		image = fmt.Sprintf("%s:latest", image)
	} else {
		// Extract version from image tag
		if idx := strings.LastIndex(image, ":"); idx > 0 {
			version = image[idx+1:]
		}
	}
	// Otherwise use the image as-is from config (with its tag)
	d.log.Header("Deploying %s", image)

	// Get target hosts
	hosts := d.getTargetHosts(opts)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts to deploy to")
	}

	// Create deployment record for history
	record := NewDeploymentRecord(d.cfg.Service, image, version, opts.Destination, hosts)
	record.Start()

	// Try to get previous version for rollback reference
	if lastDeploy, err := d.history.GetLastSuccessful(d.cfg.Service); err == nil {
		record.PreviousVersion = lastDeploy.Version
	}

	// Run pre-deploy hook
	if err := d.hooks.Run("pre-deploy"); err != nil {
		record.Fail(err)
		d.history.Record(record)
		return fmt.Errorf("pre-deploy hook failed: %w", err)
	}

	d.log.Info("Deploying to %d host(s)", len(hosts))

	// Pull image on all hosts
	if !opts.SkipPull {
		d.log.Info("Pulling image on all hosts...")
		if err := d.pullImageOnHosts(hosts, image); err != nil {
			record.Fail(err)
			d.history.Record(record)
			return fmt.Errorf("failed to pull image: %w", err)
		}
	}

	// Deploy to each host
	var deployErrors []string
	for _, host := range hosts {
		if err := d.deployToHost(host, image, opts); err != nil {
			d.log.HostError(host, "deployment failed: %v", err)
			deployErrors = append(deployErrors, fmt.Sprintf("%s: %v", host, err))
			continue
		}
		d.log.HostSuccess(host, "deployed successfully")
	}

	if len(deployErrors) > 0 {
		err := fmt.Errorf("deployment failed on %d host(s): %s", len(deployErrors), strings.Join(deployErrors, "; "))
		record.Fail(err)
		d.history.Record(record)
		return err
	}

	// Run post-deploy hook
	if err := d.hooks.Run("post-deploy"); err != nil {
		d.log.Warn("post-deploy hook failed: %v", err)
	}

	// Record successful deployment
	record.Complete()
	if err := d.history.Record(record); err != nil {
		d.log.Warn("Failed to record deployment history: %v", err)
	}

	d.log.Success("Deployment complete!")
	return nil
}

// deployToHost deploys the application to a single host
func (d *Deployer) deployToHost(host, image string, opts *DeployOptions) error {
	d.log.Host(host, "Starting deployment...")

	// Generate container names
	newContainerName := d.generateContainerName("new")
	oldContainerName := d.cfg.Service

	// Check if old container exists
	oldExists, _ := d.containers.Exists(host, oldContainerName)

	// Build container configuration
	containerConfig := d.buildContainerConfig(image, newContainerName)

	// Start new container
	d.log.Host(host, "Starting new container...")
	_, err := d.containers.Run(host, containerConfig)
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Wait for container to be healthy
	if !opts.SkipHealthCheck && d.cfg.Proxy.Healthcheck.Path != "" {
		d.log.Host(host, "Waiting for health check...")

		// Wait for readiness delay
		if d.cfg.Deploy.ReadinessDelay > 0 {
			time.Sleep(d.cfg.Deploy.ReadinessDelay)
		}

		if err := d.waitForHealthy(host, newContainerName); err != nil {
			// Cleanup failed container
			d.containers.Remove(host, newContainerName, true)
			return fmt.Errorf("health check failed: %w", err)
		}
	}

	// Register new container with proxy
	d.log.Host(host, "Registering with proxy...")
	newUpstream := fmt.Sprintf("%s:%d", newContainerName, d.cfg.Proxy.AppPort)

	if err := d.registerWithProxy(host, newUpstream); err != nil {
		d.log.Warn("Failed to register with proxy: %v", err)
	}

	// If old container exists, drain and remove it
	if oldExists {
		d.log.Host(host, "Draining old container...")

		// Remove old container from proxy
		oldUpstream := fmt.Sprintf("%s:%d", oldContainerName, d.cfg.Proxy.AppPort)
		if err := d.proxy.RemoveUpstream(host, d.cfg.Proxy.Host, oldUpstream); err != nil {
			d.log.Debug("Failed to remove old upstream: %v", err)
		}

		// Wait for drain
		if d.cfg.Deploy.DrainTimeout > 0 {
			time.Sleep(d.cfg.Deploy.DrainTimeout)
		}

		// Stop and remove old container
		d.log.Host(host, "Removing old container...")
		if err := d.containers.Stop(host, oldContainerName, 30); err != nil {
			d.log.Debug("Failed to stop old container: %v", err)
		}
		if err := d.containers.Remove(host, oldContainerName, true); err != nil {
			d.log.Debug("Failed to remove old container: %v", err)
		}
	}

	// Rename new container to service name
	d.log.Host(host, "Finalizing deployment...")
	if err := d.containers.Rename(host, newContainerName, oldContainerName); err != nil {
		d.log.Warn("Failed to rename container: %v", err)
	}

	// Update proxy with final container name
	if err := d.updateProxyUpstream(host, newUpstream, fmt.Sprintf("%s:%d", oldContainerName, d.cfg.Proxy.AppPort)); err != nil {
		d.log.Debug("Failed to update proxy upstream: %v", err)
	}

	return nil
}

// buildContainerConfig creates a container configuration from the app config
func (d *Deployer) buildContainerConfig(image, name string) *docker.ContainerConfig {
	config := &docker.ContainerConfig{
		Name:    name,
		Image:   image,
		Detach:  true,
		Restart: "unless-stopped",
		Network: "azud",
		Labels: map[string]string{
			"azud.managed": "true",
			"azud.service": d.cfg.Service,
		},
		Env: make(map[string]string),
	}

	// Add environment variables
	for key, value := range d.cfg.Env.Clear {
		config.Env[key] = value
	}

	// Add secret environment variable names
	config.SecretEnv = d.cfg.Env.Secret

	// Add volumes
	config.Volumes = d.cfg.Volumes

	// Add resource limits from server config
	// These would come from role-specific options

	// Add health check if configured
	if d.cfg.Proxy.Healthcheck.Path != "" {
		config.HealthCmd = fmt.Sprintf("curl -f http://localhost:%d%s || exit 1",
			d.cfg.Proxy.AppPort, d.cfg.Proxy.Healthcheck.Path)
		config.HealthInterval = d.cfg.Proxy.Healthcheck.Interval
		config.HealthTimeout = d.cfg.Proxy.Healthcheck.Timeout
		config.HealthRetries = 3
	}

	return config
}

// waitForHealthy waits for a container to pass health checks
func (d *Deployer) waitForHealthy(host, container string) error {
	timeout := d.cfg.Deploy.DeployTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	deadline := time.Now().Add(timeout)
	checkInterval := 2 * time.Second

	for time.Now().Before(deadline) {
		// Check container health status
		result, err := d.docker.Execute(host, "inspect", container, "--format", "'{{.State.Health.Status}}'")
		if err == nil && result.ExitCode == 0 {
			status := strings.Trim(result.Stdout, "'\n ")
			switch status {
			case "healthy":
				return nil
			case "unhealthy":
				return fmt.Errorf("container is unhealthy")
			}
		}

		// Also try direct health check
		healthPath := d.cfg.Proxy.Healthcheck.Path
		if healthPath != "" {
			checkCmd := fmt.Sprintf("docker exec %s curl -sf http://localhost:%d%s",
				container, d.cfg.Proxy.AppPort, healthPath)
			result, err := d.sshClient.Execute(host, checkCmd)
			if err == nil && result.ExitCode == 0 {
				return nil
			}
		}

		time.Sleep(checkInterval)
	}

	return fmt.Errorf("timeout waiting for container to become healthy")
}

// registerWithProxy registers the service with Caddy
func (d *Deployer) registerWithProxy(host, upstream string) error {
	serviceConfig := &proxy.ServiceConfig{
		Name:           d.cfg.Service,
		Host:           d.cfg.Proxy.Host,
		Upstreams:      []string{upstream},
		HealthPath:     d.cfg.Proxy.Healthcheck.Path,
		HealthInterval: d.cfg.Proxy.Healthcheck.Interval,
		HTTPS:          d.cfg.Proxy.SSL,
	}

	return d.proxy.RegisterService(host, serviceConfig)
}

// updateProxyUpstream updates the upstream address in the proxy
func (d *Deployer) updateProxyUpstream(host, oldUpstream, newUpstream string) error {
	// Add new upstream first
	if err := d.proxy.AddUpstream(host, d.cfg.Proxy.Host, newUpstream); err != nil {
		return err
	}

	// Remove old upstream
	return d.proxy.RemoveUpstream(host, d.cfg.Proxy.Host, oldUpstream)
}

// pullImageOnHosts pulls the image on multiple hosts in parallel
func (d *Deployer) pullImageOnHosts(hosts []string, image string) error {
	errors := d.images.PullAll(hosts, image)
	if len(errors) > 0 {
		var errMsgs []string
		for host, err := range errors {
			errMsgs = append(errMsgs, fmt.Sprintf("%s: %v", host, err))
		}
		return fmt.Errorf("pull failed on hosts: %s", strings.Join(errMsgs, "; "))
	}
	return nil
}

// getTargetHosts returns the hosts to deploy to
func (d *Deployer) getTargetHosts(opts *DeployOptions) []string {
	if len(opts.Hosts) > 0 {
		return opts.Hosts
	}

	if len(opts.Roles) > 0 {
		var hosts []string
		for _, role := range opts.Roles {
			hosts = append(hosts, d.cfg.GetRoleHosts(role)...)
		}
		return hosts
	}

	return d.cfg.GetAllHosts()
}

// generateContainerName generates a unique container name
func (d *Deployer) generateContainerName(suffix string) string {
	return fmt.Sprintf("%s-%s-%d", d.cfg.Service, suffix, time.Now().Unix())
}

// Rollback rolls back to a previous version
func (d *Deployer) Rollback(version string) error {
	d.log.Header("Rolling back to %s", version)

	opts := &DeployOptions{
		Version: version,
	}

	return d.Deploy(opts)
}

// Redeploy performs a quick redeployment without building
func (d *Deployer) Redeploy(opts *DeployOptions) error {
	d.log.Header("Redeploying %s", d.cfg.Service)
	return d.Deploy(opts)
}

// Stop stops the application on all hosts
func (d *Deployer) Stop(hosts []string) error {
	if len(hosts) == 0 {
		hosts = d.cfg.GetAllHosts()
	}

	d.log.Info("Stopping %s on %d host(s)...", d.cfg.Service, len(hosts))

	var wg sync.WaitGroup
	errors := make(chan error, len(hosts))

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if err := d.containers.Stop(h, d.cfg.Service, 30); err != nil {
				errors <- fmt.Errorf("%s: %w", h, err)
				return
			}
			d.log.HostSuccess(h, "stopped")
		}(host)
	}

	wg.Wait()
	close(errors)

	var errs []string
	for err := range errors {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("stop failed: %s", strings.Join(errs, "; "))
	}

	return nil
}

// Start starts the application on all hosts
func (d *Deployer) Start(hosts []string) error {
	if len(hosts) == 0 {
		hosts = d.cfg.GetAllHosts()
	}

	d.log.Info("Starting %s on %d host(s)...", d.cfg.Service, len(hosts))

	var wg sync.WaitGroup
	errors := make(chan error, len(hosts))

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if err := d.containers.Start(h, d.cfg.Service); err != nil {
				errors <- fmt.Errorf("%s: %w", h, err)
				return
			}
			d.log.HostSuccess(h, "started")
		}(host)
	}

	wg.Wait()
	close(errors)

	var errs []string
	for err := range errors {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("start failed: %s", strings.Join(errs, "; "))
	}

	return nil
}

// Restart restarts the application on all hosts
func (d *Deployer) Restart(hosts []string) error {
	if len(hosts) == 0 {
		hosts = d.cfg.GetAllHosts()
	}

	d.log.Info("Restarting %s on %d host(s)...", d.cfg.Service, len(hosts))

	var wg sync.WaitGroup
	errors := make(chan error, len(hosts))

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if err := d.containers.Restart(h, d.cfg.Service, 30); err != nil {
				errors <- fmt.Errorf("%s: %w", h, err)
				return
			}
			d.log.HostSuccess(h, "restarted")
		}(host)
	}

	wg.Wait()
	close(errors)

	var errs []string
	for err := range errors {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("restart failed: %s", strings.Join(errs, "; "))
	}

	return nil
}
