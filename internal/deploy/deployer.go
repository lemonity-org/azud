package deploy

import (
	"context"
	"fmt"
	"os"
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
		registry:   podman.NewRegistryManagerWithUser(podmanClient, cfg.SSH.User),
		proxy:      proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams()),
		hooks:      NewHookRunner(cfg.HooksPath, cfg.Hooks.Timeout, log),
		history:    NewHistoryStore(".", cfg.Deploy.RetainHistory, log),
		log:        log,
	}
}

func (d *Deployer) hookContext(opts *DeployOptions, image, version string) *HookContext {
	return &HookContext{
		Service:     d.cfg.Service,
		Image:       image,
		Version:     version,
		Hosts:       strings.Join(d.getTargetHosts(opts), ","),
		Destination: opts.Destination,
		Performer:   CurrentUser(),
		Role:        strings.Join(opts.Roles, ","),
		RecordedAt:  time.Now().Format(time.RFC3339),
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
func (d *Deployer) Deploy(ctx context.Context, opts *DeployOptions) error {
	deployStart := time.Now()
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
	hookCtx := d.hookContext(opts, image, version)
	if err := d.hooks.Run(ctx, "pre-deploy", hookCtx); err != nil {
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

		// Verify image digest is consistent across all hosts to detect
		// supply-chain attacks via mutable tag replacement.
		if digest, err := d.verifyImageDigest(hosts, image); err != nil {
			record.Fail(err)
			_ = d.history.Record(record)
			return fmt.Errorf("image digest verification failed: %w", err)
		} else if digest != "" {
			record.Metadata["image_digest"] = digest
			d.log.Info("Image digest: %s", digest)
		}
	}

	// Run pre-deploy command from new image (e.g., database migrations)
	if d.cfg.Deploy.PreDeployCommand != "" {
		if err := d.runPreDeployCommand(hosts[0], image); err != nil {
			record.Fail(err)
			_ = d.history.Record(record)
			return fmt.Errorf("pre-deploy command failed: %w", err)
		}
	}

	// Deploy to each host, tracking successes for potential rollback
	var deployErrors []string
	var succeededHosts []string
	for _, host := range hosts {
		if err := d.deployToHost(ctx, host, image, version, opts); err != nil {
			d.log.HostError(host, "deployment failed: %v", err)
			deployErrors = append(deployErrors, fmt.Sprintf("%s: %v", host, err))

			// If rollback_on_failure is enabled, roll back all successful
			// hosts immediately to keep the fleet on a single version.
			if d.cfg.Deploy.RollbackOnFailure && len(succeededHosts) > 0 {
				d.log.Warn("Rolling back %d already-deployed host(s) due to failure on %s...", len(succeededHosts), host)
				d.rollbackHosts(ctx, succeededHosts, record.PreviousVersion, opts.Roles)
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
	hookCtx.Runtime = fmt.Sprintf("%.0f", time.Since(deployStart).Seconds())
	hookCtx.RecordedAt = time.Now().Format(time.RFC3339)
	if err := d.hooks.Run(ctx, "post-deploy", hookCtx); err != nil {
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

func (d *Deployer) deployToHost(ctx context.Context, host, image, version string, opts *DeployOptions) error {
	// Acquire deployment lock to prevent concurrent deployments to the same host/service
	lockFile := state.LockFile(d.cfg.SSH.User, d.cfg.Service+".deploy")
	lockTimeout := d.cfg.Deploy.DeployTimeout * 2
	if lockTimeout < 5*time.Minute {
		lockTimeout = 5 * time.Minute
	}

	var deployErr error
	lockErr := d.sshClient.WithRemoteLock(host, lockFile, lockTimeout, func() error {
		deployErr = d.deployToHostLocked(ctx, host, image, version, opts)
		return nil
	})
	if lockErr != nil {
		return fmt.Errorf("failed to acquire deployment lock: %w", lockErr)
	}
	return deployErr
}

func (d *Deployer) deployToHostLocked(ctx context.Context, host, image, version string, opts *DeployOptions) error {
	d.log.Host(host, "Starting deployment...")

	newContainerName := d.generateContainerName("new")
	oldContainerName := d.cfg.Service
	oldExists, _ := d.containers.Exists(host, oldContainerName)
	containerConfig := d.buildContainerConfig(image, newContainerName)

	// Run pre-app-boot hook
	bootCtx := d.hookContext(opts, image, version)
	bootCtx.Hosts = host
	if err := d.hooks.Run(ctx, "pre-app-boot", bootCtx); err != nil {
		return fmt.Errorf("pre-app-boot hook failed: %w", err)
	}

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

	// Run post-app-boot hook
	if err := d.hooks.Run(ctx, "post-app-boot", bootCtx); err != nil {
		d.log.Warn("post-app-boot hook failed: %v", err)
	}

	// Register new container with proxy
	d.log.Host(host, "Registering with proxy...")
	newUpstream, err := d.upstreamAddr(host, newContainerName)
	if err != nil {
		return err
	}
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
		oldUpstream, oldUpstreamErr := d.upstreamAddr(host, oldContainerName)
		if oldUpstreamErr != nil {
			d.log.Debug("Failed to resolve old upstream: %v", oldUpstreamErr)
			oldUpstream = fmt.Sprintf("%s:%d", oldContainerName, d.cfg.Proxy.AppPort)
		}

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
			if err := d.proxy.DrainUpstream(host, oldUpstream, d.cfg.Deploy.DrainTimeout); err != nil {
				d.log.Warn("Drain did not complete cleanly (continuing anyway): %v", err)
			}
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

	// In mixed rootless/rootful mode, upstreams are host loopback ports.
	// Renaming a container does not change its published host port, so the
	// upstream address stays the same and there is nothing to swap.
	if d.cfg.UseHostPortUpstreams() {
		return nil
	}

	// Swap the upstream from the temporary name to the final service name.
	// Add the final upstream first, then remove the temporary one. This
	// ensures there is always at least one healthy upstream in the route,
	// eliminating the brief gap that a full route replacement would cause.
	finalUpstream, finalUpstreamErr := d.upstreamAddr(host, oldContainerName)
	if finalUpstreamErr != nil {
		d.log.Debug("Failed to resolve final upstream after rename: %v", finalUpstreamErr)
		finalUpstream = fmt.Sprintf("%s:%d", oldContainerName, d.cfg.Proxy.AppPort)
	}
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

// runPreDeployCommand runs the configured pre_deploy_command in a one-off
// container from the new image. The container shares the same network and
// environment as the app, runs with --rm, and blocks until complete.
func (d *Deployer) runPreDeployCommand(host, image string) error {
	cmd := d.cfg.Deploy.PreDeployCommand
	d.log.Info("Running pre-deploy command: %s", cmd)

	name := fmt.Sprintf("%s-pre-deploy-%d", d.cfg.Service, time.Now().Unix())
	containerCfg := newPreDeployContainerConfig(d.cfg, image, name)
	containerCfg.Command = parseCommandArgs(cmd)

	_, err := d.containers.Run(host, containerCfg)
	if err != nil {
		return fmt.Errorf("command exited with error: %w", err)
	}

	d.log.Success("Pre-deploy command completed successfully")
	return nil
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
		Name:                  d.cfg.Service,
		Host:                  proxyHost,
		Hosts:                 hosts,
		Upstreams:             []string{upstream},
		HealthPath:            livenessPath,
		HealthInterval:        d.cfg.Proxy.Healthcheck.Interval,
		HealthTimeout:         d.cfg.Proxy.Healthcheck.Timeout,
		ResponseTimeout:       d.cfg.Proxy.ResponseTimeout,
		ResponseHeaderTimeout: d.cfg.Proxy.ResponseHeaderTimeout,
		ForwardHeaders:        d.cfg.Proxy.ForwardHeaders,
		BufferRequests:        d.cfg.Proxy.Buffering.Requests,
		BufferResponses:       d.cfg.Proxy.Buffering.Responses,
		MaxRequestBody:        d.cfg.Proxy.Buffering.MaxRequestBody,
		BufferMemory:          d.cfg.Proxy.Buffering.Memory,
		HTTPS:                 d.cfg.Proxy.SSL,
	}

	return d.proxy.RegisterService(host, serviceConfig)
}

func (d *Deployer) upstreamAddr(host, container string) (string, error) {
	if !d.cfg.UseHostPortUpstreams() {
		return fmt.Sprintf("%s:%d", container, d.cfg.Proxy.AppPort), nil
	}

	port, err := d.containers.HostPort(host, container, d.cfg.Proxy.AppPort)
	if err != nil {
		return "", fmt.Errorf("failed to resolve host port for %s on %s: %w", container, host, err)
	}
	return fmt.Sprintf("127.0.0.1:%d", port), nil
}

// verifyImageDigest checks that the pulled image has the same digest on all
// hosts. A mismatch indicates a possible supply-chain attack (e.g., tag was
// replaced between pulls). Returns the verified digest or an error.
func (d *Deployer) verifyImageDigest(hosts []string, image string) (string, error) {
	if len(hosts) == 0 {
		return "", nil
	}

	// Get digest from the first host as reference
	refDigest, err := d.images.GetDigest(hosts[0], image)
	if err != nil {
		// Digest retrieval may fail for local images without repo digests.
		d.log.Debug("Could not retrieve image digest (non-registry image?): %v", err)
		return "", nil
	}

	// Verify all other hosts have the same digest
	for _, host := range hosts[1:] {
		digest, err := d.images.GetDigest(host, image)
		if err != nil {
			return "", fmt.Errorf("failed to get image digest on %s: %w", host, err)
		}
		if digest != refDigest {
			return "", fmt.Errorf("image digest mismatch: %s has %s, %s has %s", hosts[0], refDigest, host, digest)
		}
	}

	return refDigest, nil
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
func (d *Deployer) Rollback(ctx context.Context, version, destination string, hosts []string) error {
	d.log.Header("Rolling back to %s", version)

	opts := &DeployOptions{
		Version:     version,
		Destination: destination,
		Hosts:       hosts,
	}

	return d.Deploy(ctx, opts)
}

func (d *Deployer) Redeploy(ctx context.Context, opts *DeployOptions) error {
	d.log.Header("Redeploying %s", d.cfg.Service)
	return d.Deploy(ctx, opts)
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
func (d *Deployer) rollbackHosts(ctx context.Context, hosts []string, previousVersion string, roles []string) {
	if previousVersion == "" {
		d.log.Warn("No previous version recorded, cannot auto-rollback")
		return
	}

	prevImage := d.cfg.Image
	if idx := strings.LastIndex(prevImage, ":"); idx > 0 && !strings.Contains(prevImage[idx:], "/") {
		prevImage = prevImage[:idx]
	}
	prevImage = fmt.Sprintf("%s:%s", prevImage, previousVersion)

	for _, host := range hosts {
		d.log.Host(host, "Rolling back to %s...", previousVersion)
		proxyHost := d.cfg.Proxy.PrimaryHost()

		// The current container (just deployed) is named d.cfg.Service.
		// Stop and remove it, then re-deploy the previous version.
		serviceName := d.cfg.Service
		stopTimeout := d.cfg.Deploy.GetStopTimeout()

		// Remove the newly deployed container from the proxy
		upstream, upstreamErr := d.upstreamAddr(host, serviceName)
		if upstreamErr != nil {
			d.log.Debug("Rollback: failed to resolve upstream for %s on %s: %v", serviceName, host, upstreamErr)
			upstream = fmt.Sprintf("%s:%d", serviceName, d.cfg.Proxy.AppPort)
		}
		if proxyHost != "" {
			if err := d.proxy.RemoveUpstream(host, proxyHost, upstream); err != nil {
				d.log.Warn("Rollback: failed to remove upstream on %s: %v", host, err)
			}
		}

		// Stop and remove the new container
		if err := d.containers.Stop(host, serviceName, stopTimeout); err != nil {
			d.log.Warn("Rollback: failed to stop container on %s: %v", host, err)
		}
		if err := d.containers.Remove(host, serviceName, true); err != nil {
			d.log.Warn("Rollback: failed to remove container on %s: %v", host, err)
		}

		// Re-deploy the previous version
		rollbackOpts := &DeployOptions{
			Version:     previousVersion,
			SkipPull:    true, // Image should still be cached from last deployment
			Hosts:       []string{host},
			Roles:       roles,
			Destination: "rollback",
		}

		// Use deployToHost directly to avoid recursion into the full Deploy
		// method (which would run hooks, record history, etc.)
		if err := d.deployToHost(ctx, host, prevImage, previousVersion, rollbackOpts); err != nil {
			d.log.HostError(host, "rollback failed: %v", err)
			continue
		}
		d.log.HostSuccess(host, "rolled back to %s", previousVersion)
	}

	// Run post-rollback hook
	rollbackCtx := &HookContext{
		Service:     d.cfg.Service,
		Image:       prevImage,
		Version:     previousVersion,
		Hosts:       strings.Join(hosts, ","),
		Destination: "rollback",
		Performer:   CurrentUser(),
		Role:        strings.Join(roles, ","),
		RecordedAt:  time.Now().Format(time.RFC3339),
	}
	if err := d.hooks.Run(ctx, "post-rollback", rollbackCtx); err != nil {
		d.log.Warn("post-rollback hook failed: %v", err)
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
