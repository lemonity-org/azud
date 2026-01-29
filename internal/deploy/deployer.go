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

// Deployer orchestrates zero-downtime application deployments across hosts.
type Deployer struct {
	cfg          *config.Config
	sshClient    *ssh.Client
	podman       *podman.Client
	containers   *podman.ContainerManager
	images       *podman.ImageManager
	registry     *podman.RegistryManager
	proxy        *proxy.Manager
	hooks        *HookRunner
	history      *HistoryStore
	log          *output.Logger
}

func NewDeployer(cfg *config.Config, sshClient *ssh.Client, log *output.Logger) *Deployer {
	if log == nil {
		log = output.DefaultLogger
	}

	podmanClient := podman.NewClient(sshClient)

	return &Deployer{
		cfg:        cfg,
		sshClient:  sshClient,
		podman:     podmanClient,
		containers: podman.NewContainerManager(podmanClient),
		images:     podman.NewImageManager(podmanClient),
		registry:   podman.NewRegistryManager(podmanClient),
		proxy:      proxy.NewManager(sshClient, log),
		hooks:      NewHookRunner(cfg.HooksPath, log),
		history:    NewHistoryStore(".", cfg.Deploy.RetainHistory, log),
		log:        log,
	}
}

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

// Deploy pulls the image, starts new containers, health-checks them,
// registers them with the proxy, and drains old containers.
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

func (d *Deployer) deployToHost(host, image string, opts *DeployOptions) error {
	d.log.Host(host, "Starting deployment...")

	newContainerName := d.generateContainerName("new")
	oldContainerName := d.cfg.Service
	oldExists, _ := d.containers.Exists(host, oldContainerName)
	containerConfig := d.buildContainerConfig(image, newContainerName)

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

func (d *Deployer) buildContainerConfig(image, name string) *podman.ContainerConfig {
	return newAppContainerConfig(d.cfg, image, name, nil)
}

func (d *Deployer) waitForHealthy(host, container string) error {
	return waitForContainerHealthy(d.cfg, d.podman, d.sshClient, host, container)
}

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

func (d *Deployer) updateProxyUpstream(host, oldUpstream, newUpstream string) error {
	if err := d.proxy.AddUpstream(host, d.cfg.Proxy.Host, newUpstream); err != nil {
		return err
	}
	return d.proxy.RemoveUpstream(host, d.cfg.Proxy.Host, oldUpstream)
}

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

func (d *Deployer) generateContainerName(suffix string) string {
	return fmt.Sprintf("%s-%s-%d", d.cfg.Service, suffix, time.Now().Unix())
}

// Rollback re-deploys a previous version.
func (d *Deployer) Rollback(version string) error {
	d.log.Header("Rolling back to %s", version)

	opts := &DeployOptions{
		Version: version,
	}

	return d.Deploy(opts)
}

func (d *Deployer) Redeploy(opts *DeployOptions) error {
	d.log.Header("Redeploying %s", d.cfg.Service)
	return d.Deploy(opts)
}

func (d *Deployer) Stop(hosts []string) error {
	return d.runOnHosts("stop", hosts, func(host string) error {
		return d.containers.Stop(host, d.cfg.Service, 30)
	})
}

func (d *Deployer) Start(hosts []string) error {
	return d.runOnHosts("start", hosts, func(host string) error {
		return d.containers.Start(host, d.cfg.Service)
	})
}

func (d *Deployer) Restart(hosts []string) error {
	return d.runOnHosts("restart", hosts, func(host string) error {
		return d.containers.Restart(host, d.cfg.Service, 30)
	})
}

// runOnHosts executes an operation on all hosts in parallel, collecting errors.
func (d *Deployer) runOnHosts(operation string, hosts []string, fn func(host string) error) error {
	if len(hosts) == 0 {
		hosts = d.cfg.GetAllHosts()
	}

	label := strings.ToUpper(operation[:1]) + operation[1:]
	d.log.Info("%sing %s on %d host(s)...", label, d.cfg.Service, len(hosts))

	var wg sync.WaitGroup
	errors := make(chan error, len(hosts))

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if err := fn(h); err != nil {
				errors <- fmt.Errorf("%s: %w", h, err)
				return
			}
			d.log.HostSuccess(h, "%sed", operation)
		}(host)
	}

	wg.Wait()
	close(errors)

	var errs []string
	for err := range errors {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s failed: %s", operation, strings.Join(errs, "; "))
	}

	return nil
}
