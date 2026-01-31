package deploy

import (
	"fmt"
	"os"
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
	cfg        *config.Config
	sshClient  *ssh.Client
	podman     *podman.Client
	containers *podman.ContainerManager
	images     *podman.ImageManager
	registry   *podman.RegistryManager
	proxy      *proxy.Manager
	hooks      *HookRunner
	history    *HistoryStore
	log        *output.Logger
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

	// Ensure required secrets are present on all hosts.
	if err := d.ensureRemoteSecrets(hosts); err != nil {
		record.Fail(err)
		_ = d.history.Record(record)
		return err
	}

	// Try to get previous version for rollback reference
	if lastDeploy, err := d.history.GetLastSuccessful(d.cfg.Service); err == nil {
		record.PreviousVersion = lastDeploy.Version
	}

	// Run pre-deploy hook
	if err := d.hooks.Run("pre-deploy"); err != nil {
		record.Fail(err)
		_ = d.history.Record(record)
		return fmt.Errorf("pre-deploy hook failed: %w", err)
	}

	d.log.Info("Deploying to %d host(s)", len(hosts))

	// Login to registry if configured
	if !opts.SkipPull && d.cfg.Registry.Server != "" {
		if err := d.loginToRegistry(hosts); err != nil {
			record.Fail(err)
			_ = d.history.Record(record)
			return fmt.Errorf("failed to login to registry: %w", err)
		}
	}

	// Pull image on all hosts
	if !opts.SkipPull {
		d.log.Info("Pulling image on all hosts...")
		if err := d.pullImageOnHosts(hosts, image); err != nil {
			record.Fail(err)
			_ = d.history.Record(record)
			return fmt.Errorf("failed to pull image: %w", err)
		}
	}

	// Deploy to each host, tracking successes for potential rollback
	var deployErrors []string
	var succeededHosts []string
	for _, host := range hosts {
		if err := d.deployToHost(host, image, opts); err != nil {
			d.log.HostError(host, "deployment failed: %v", err)
			deployErrors = append(deployErrors, fmt.Sprintf("%s: %v", host, err))

			// If rollback_on_failure is enabled, roll back all successful
			// hosts immediately to keep the fleet on a single version.
			if d.cfg.Deploy.RollbackOnFailure && len(succeededHosts) > 0 {
				d.log.Warn("Rolling back %d already-deployed host(s) due to failure on %s...", len(succeededHosts), host)
				d.rollbackHosts(succeededHosts, record.PreviousVersion)
				succeededHosts = nil
			}
			continue
		}
		succeededHosts = append(succeededHosts, host)
		d.log.HostSuccess(host, "deployed successfully")
	}

	if len(deployErrors) > 0 {
		err := fmt.Errorf("deployment failed on %d host(s): %s", len(deployErrors), strings.Join(deployErrors, "; "))
		record.Fail(err)
		_ = d.history.Record(record)
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

func (d *Deployer) ensureRemoteSecrets(hosts []string) error {
	return ValidateRemoteSecrets(d.sshClient, hosts, config.RemoteSecretsPath(d.cfg), d.cfg.Env.Secret)
}

func (d *Deployer) loginToRegistry(hosts []string) error {
	username := d.cfg.Registry.Username
	if username == "" {
		return nil
	}

	password := ""
	if len(d.cfg.Registry.Password) > 0 {
		secretKey := d.cfg.Registry.Password[0]
		password = os.Getenv(secretKey)
		if password == "" {
			if p, ok := config.GetSecret(secretKey); ok {
				password = p
			}
		}
	}

	if password == "" {
		return fmt.Errorf("registry password not found (secret: %v)", d.cfg.Registry.Password)
	}

	d.log.Info("Logging into registry %s...", d.cfg.Registry.Server)

	regConfig := &podman.RegistryConfig{
		Server:   d.cfg.Registry.Server,
		Username: username,
		Password: password,
	}

	errors := d.registry.LoginAll(hosts, regConfig)
	if len(errors) > 0 {
		var errMsgs []string
		for host, err := range errors {
			errMsgs = append(errMsgs, fmt.Sprintf("%s: %v", host, err))
		}
		return fmt.Errorf("registry login failed on hosts: %s", strings.Join(errMsgs, "; "))
	}

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

	// Wait for container to pass readiness check
	readinessPath := d.cfg.Proxy.Healthcheck.GetReadinessPath()
	if !opts.SkipHealthCheck && readinessPath != "" {
		d.log.Host(host, "Waiting for readiness check...")

		// Wait for readiness delay
		if d.cfg.Deploy.ReadinessDelay > 0 {
			time.Sleep(d.cfg.Deploy.ReadinessDelay)
		}

		if err := d.waitForHealthy(host, newContainerName); err != nil {
			// Cleanup failed container
			_ = d.containers.Remove(host, newContainerName, true)
			return fmt.Errorf("readiness check failed: %w", err)
		}
	}

	// Register new container with proxy
	d.log.Host(host, "Registering with proxy...")
	newUpstream := fmt.Sprintf("%s:%d", newContainerName, d.cfg.Proxy.AppPort)
	proxyHost := d.cfg.Proxy.PrimaryHost()

	if oldExists {
		// Add new upstream alongside the old one so both receive traffic
		// during the transition. Using AddUpstream (not RegisterService)
		// preserves the old upstream in the route, which is required for
		// connection-aware draining to work.
		if proxyHost != "" {
			if err := d.proxy.AddUpstream(host, proxyHost, newUpstream); err != nil {
				// AddUpstream may fail if there's no existing route (e.g., proxy
				// was rebooted). Fall back to a full RegisterService.
				d.log.Debug("Failed to add upstream alongside old: %v", err)
				if regErr := d.registerWithProxy(host, newUpstream); regErr != nil {
					d.log.Warn("Failed to register with proxy: %v", regErr)
				}
			}
		} else if regErr := d.registerWithProxy(host, newUpstream); regErr != nil {
			d.log.Warn("Failed to register with proxy: %v", regErr)
		}
	} else {
		// First deployment — no existing route to add to.
		if err := d.registerWithProxy(host, newUpstream); err != nil {
			d.log.Warn("Failed to register with proxy: %v", err)
		}
	}

	// If old container exists, drain and remove it
	if oldExists {
		oldUpstream := fmt.Sprintf("%s:%d", oldContainerName, d.cfg.Proxy.AppPort)

		// Remove old container from proxy so no new requests are routed to it.
		// The old upstream is still tracked by Caddy until its in-flight
		// requests complete, allowing the drain step to poll accurately.
		d.log.Host(host, "Removing old upstream from proxy...")
		if proxyHost != "" {
			if err := d.proxy.RemoveUpstream(host, proxyHost, oldUpstream); err != nil {
				d.log.Debug("Failed to remove old upstream: %v", err)
			}
		}

		// Drain: poll Caddy for in-flight requests on the old upstream,
		// falling back to a sleep if the API is unavailable.
		if d.cfg.Deploy.DrainTimeout > 0 {
			_ = d.proxy.DrainUpstream(host, oldUpstream, d.cfg.Deploy.DrainTimeout)
		}

		// Stop and remove old container
		d.log.Host(host, "Removing old container...")
		stopTimeout := d.cfg.Deploy.GetStopTimeout()
		if err := d.containers.Stop(host, oldContainerName, stopTimeout); err != nil {
			d.log.Debug("Failed to stop old container: %v", err)
		}
		if err := d.containers.Remove(host, oldContainerName, true); err != nil {
			d.log.Debug("Failed to remove old container: %v", err)
		}
	}

	// Finalize: rename the new container to the service name and swap
	// the proxy upstream using add-then-remove so at least one upstream
	// is always present (no gap = no dropped requests).
	d.log.Host(host, "Finalizing deployment...")
	if err := d.containers.Rename(host, newContainerName, oldContainerName); err != nil {
		// Rename failed — the container is still running under newContainerName
		// and the proxy is already pointing to it. This is an acceptable state;
		// the next deployment will need to handle this container name.
		d.log.Warn("Failed to rename container (proxy still points to %s): %v", newContainerName, err)
		return nil
	}

	// Swap the upstream from the temporary name to the final service name.
	// Add the final upstream first, then remove the temporary one. This
	// ensures there is always at least one healthy upstream in the route,
	// eliminating the brief gap that a full route replacement would cause.
	finalUpstream := fmt.Sprintf("%s:%d", oldContainerName, d.cfg.Proxy.AppPort)
	if proxyHost != "" {
		if err := d.proxy.AddUpstream(host, proxyHost, finalUpstream); err != nil {
			// Fallback: if add fails, do a full route replacement
			d.log.Debug("Failed to add final upstream, falling back to full replace: %v", err)
			if regErr := d.registerWithProxy(host, finalUpstream); regErr != nil {
				d.log.Warn("Failed to update proxy to final name: %v", regErr)
			}
			return nil
		}
		if err := d.proxy.RemoveUpstream(host, proxyHost, newUpstream); err != nil {
			d.log.Debug("Failed to remove temp upstream: %v", err)
		}
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
	// Use liveness path for Caddy's active health checks (continuous monitoring).
	// The readiness path is only used during deployment to gate proxy registration.
	livenessPath := ""
	if !d.cfg.Proxy.Healthcheck.DisableLiveness {
		livenessPath = d.cfg.Proxy.Healthcheck.GetLivenessPath()
	}
	hosts := d.cfg.Proxy.AllHosts()
	proxyHost := d.cfg.Proxy.PrimaryHost()

	serviceConfig := &proxy.ServiceConfig{
		Name:            d.cfg.Service,
		Host:            proxyHost,
		Hosts:           hosts,
		Upstreams:       []string{upstream},
		HealthPath:      livenessPath,
		HealthInterval:  d.cfg.Proxy.Healthcheck.Interval,
		HealthTimeout:   d.cfg.Proxy.Healthcheck.Timeout,
		ResponseTimeout:       d.cfg.Proxy.ResponseTimeout,
		ResponseHeaderTimeout: d.cfg.Proxy.ResponseHeaderTimeout,
		ForwardHeaders:  d.cfg.Proxy.ForwardHeaders,
		BufferRequests:  d.cfg.Proxy.Buffering.Requests,
		BufferResponses: d.cfg.Proxy.Buffering.Responses,
		MaxRequestBody:  d.cfg.Proxy.Buffering.MaxRequestBody,
		BufferMemory:    d.cfg.Proxy.Buffering.Memory,
		HTTPS:           d.cfg.Proxy.SSL,
	}

	return d.proxy.RegisterService(host, serviceConfig)
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
	stopTimeout := d.cfg.Deploy.GetStopTimeout()
	return d.runOnHosts("stop", hosts, func(host string) error {
		return d.containers.Stop(host, d.cfg.Service, stopTimeout)
	})
}

func (d *Deployer) Start(hosts []string) error {
	return d.runOnHosts("start", hosts, func(host string) error {
		return d.containers.Start(host, d.cfg.Service)
	})
}

func (d *Deployer) Restart(hosts []string) error {
	stopTimeout := d.cfg.Deploy.GetStopTimeout()
	return d.runOnHosts("restart", hosts, func(host string) error {
		return d.containers.Restart(host, d.cfg.Service, stopTimeout)
	})
}

// rollbackHosts reverts a deployment on hosts that succeeded, restoring the
// previous version. This is a best-effort operation: errors are logged but
// do not prevent rollback attempts on other hosts.
func (d *Deployer) rollbackHosts(hosts []string, previousVersion string) {
	if previousVersion == "" {
		d.log.Warn("No previous version recorded, cannot auto-rollback")
		return
	}

	for _, host := range hosts {
		d.log.Host(host, "Rolling back to %s...", previousVersion)
		proxyHost := d.cfg.Proxy.PrimaryHost()

		// The current container (just deployed) is named d.cfg.Service.
		// Stop and remove it, then re-deploy the previous version.
		serviceName := d.cfg.Service
		stopTimeout := d.cfg.Deploy.GetStopTimeout()

		// Remove the newly deployed container from the proxy
		upstream := fmt.Sprintf("%s:%d", serviceName, d.cfg.Proxy.AppPort)
		if proxyHost != "" {
			if err := d.proxy.RemoveUpstream(host, proxyHost, upstream); err != nil {
				d.log.Debug("Rollback: failed to remove upstream on %s: %v", host, err)
			}
		}

		// Stop and remove the new container
		if err := d.containers.Stop(host, serviceName, stopTimeout); err != nil {
			d.log.Debug("Rollback: failed to stop container on %s: %v", host, err)
		}
		if err := d.containers.Remove(host, serviceName, true); err != nil {
			d.log.Debug("Rollback: failed to remove container on %s: %v", host, err)
		}

		// Re-deploy the previous version
		rollbackOpts := &DeployOptions{
			Version:     previousVersion,
			SkipPull:    true, // Image should still be cached from last deployment
			Hosts:       []string{host},
			Destination: "rollback",
		}

		// Use deployToHost directly to avoid recursion into the full Deploy
		// method (which would run hooks, record history, etc.)
		prevImage := d.cfg.Image
		if idx := strings.LastIndex(prevImage, ":"); idx > 0 && !strings.Contains(prevImage[idx:], "/") {
			prevImage = prevImage[:idx]
		}
		prevImage = fmt.Sprintf("%s:%s", prevImage, previousVersion)

		if err := d.deployToHost(host, prevImage, rollbackOpts); err != nil {
			d.log.HostError(host, "rollback failed: %v", err)
			continue
		}
		d.log.HostSuccess(host, "rolled back to %s", previousVersion)
	}
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
