package deploy

import (
	"context"
	"fmt"
	"os"
	"sort"
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
	cfg         *config.Config
	sshClient   *ssh.Client
	podman      *podman.Client
	containers  *podman.ContainerManager
	images      *podman.ImageManager
	imageDigest func(host, image string) (string, error)
	registry    *podman.RegistryManager
	proxy       *proxy.Manager
	hooks       *HookRunner
	history     *HistoryStore
	log         *output.Logger
}

// newProxyConfigFromCfg builds a proxy.ProxyConfig from the deploy
// configuration. Used by both Deployer and CanaryDeployer to avoid
// duplicating the field mapping.
func newProxyConfigFromCfg(cfg *config.Config) *proxy.ProxyConfig {
	pc := &proxy.ProxyConfig{
		Hosts:                 cfg.Proxy.AllHosts(),
		AutoHTTPS:             cfg.Proxy.SSL,
		Email:                 cfg.Proxy.ACMEEmail,
		Staging:               cfg.Proxy.ACMEStaging,
		SSLRedirect:           cfg.Proxy.SSLRedirect,
		HTTPPort:              cfg.Proxy.HTTPPort,
		HTTPSPort:             cfg.Proxy.HTTPSPort,
		LoggingEnabled:        cfg.Proxy.Logging.Enabled,
		RedactRequestHeaders:  cfg.Proxy.Logging.RedactRequestHeaders,
		RedactResponseHeaders: cfg.Proxy.Logging.RedactResponseHeaders,
	}

	if cfg.Proxy.SSLCertificate != "" && cfg.Proxy.SSLPrivateKey != "" {
		certPEM, certOK := config.GetSecret(cfg.Proxy.SSLCertificate)
		keyPEM, keyOK := config.GetSecret(cfg.Proxy.SSLPrivateKey)
		if certOK && keyOK {
			pc.SSLCertificate = certPEM
			pc.SSLPrivateKey = keyPEM
		}
	}

	return pc
}

func NewDeployer(cfg *config.Config, sshClient *ssh.Client, log *output.Logger) *Deployer {
	if log == nil {
		log = output.DefaultLogger
	}

	podmanClient := podman.NewClient(sshClient)
	proxyManager := proxy.NewManagerWithOptions(sshClient, log, cfg.SSH.User, cfg.Proxy.Rootful, cfg.UseHostPortUpstreams())
	proxyManager.SetProxyConfig(newProxyConfigFromCfg(cfg))

	return &Deployer{
		cfg:        cfg,
		sshClient:  sshClient,
		podman:     podmanClient,
		containers: podman.NewContainerManager(podmanClient),
		images:     podman.NewImageManager(podmanClient),
		registry:   podman.NewRegistryManager(podmanClient),
		proxy:      proxyManager,
		hooks:      NewHookRunner(cfg.HooksPath, cfg.Hooks.Timeout, log),
		history:    NewDurableHistoryStore(cfg.Deploy.RetainHistory, log),
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

// deploymentTarget identifies one role instance on one host. A host may
// legitimately appear more than once when it runs multiple roles.
type deploymentTarget struct {
	Host string
	Role string
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
	switch {
	case version != "":
		// Explicit version: replace any existing tag or digest.
		image = fmt.Sprintf("%s:%s", stripImageTag(image), version)
	case strings.Contains(image, "@"):
		// Digest-pinned image (no explicit version): use the digest as version.
		version = image[strings.Index(image, "@")+1:]
	case hasImageTag(image):
		// Tagged image: extract the tag as the version.
		version = image[strings.LastIndex(image, ":")+1:]
	default:
		// No tag and no version: default to latest.
		version = "latest"
		image = fmt.Sprintf("%s:latest", image)
	}
	d.log.Header("Deploying %s", image)

	// Resolve role/host pairs before doing any remote work. A host can run
	// multiple roles, and each pair needs its own container lifecycle.
	targets, err := d.getTargets(opts)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no deployment targets")
	}
	hosts := targetHosts(targets)
	if err := d.history.EnsureAvailable(); err != nil {
		return fmt.Errorf("durable deployment history is unavailable: %w", err)
	}

	// Create deployment record for history
	record := NewDeploymentRecord(d.cfg.Service, image, version, opts.Destination, hosts)
	record.Start()

	// Ensure required secrets are present on all hosts.
	if err := d.ensureRemoteSecrets(hosts); err != nil {
		return d.failAndRecord(record, err)
	}

	// Try to get previous version for rollback reference
	if lastDeploy, err := d.history.GetLastSuccessful(d.cfg.Service); err == nil {
		record.PreviousVersion = lastDeploy.Version
	}

	// Run pre-deploy hook
	hookCtx := d.hookContext(opts, image, version)
	if err := d.hooks.Run(ctx, "pre-deploy", hookCtx); err != nil {
		return d.failAndRecord(record, fmt.Errorf("pre-deploy hook failed: %w", err))
	}

	d.log.Info("Deploying to %d host(s)", len(hosts))

	// Login to registry if configured
	if !opts.SkipPull && d.cfg.Registry.Server != "" {
		if err := d.loginToRegistry(hosts); err != nil {
			return d.failAndRecord(record, fmt.Errorf("failed to login to registry: %w", err))
		}
	}

	// Pull image on all hosts
	if !opts.SkipPull {
		d.log.Info("Pulling image on all hosts...")
		if err := d.pullImageOnHosts(hosts, image); err != nil {
			return d.failAndRecord(record, fmt.Errorf("failed to pull image: %w", err))
		}

		// Verify image digest is consistent across all hosts to detect
		// supply-chain attacks via mutable tag replacement.
		if digest, err := d.verifyImageDigest(hosts, image); err != nil {
			return d.failAndRecord(record, fmt.Errorf("image digest verification failed: %w", err))
		} else if digest != "" {
			record.Metadata["image_digest"] = digest
			d.log.Info("Image digest: %s", digest)
		} else {
			record.Metadata["image_digest_verification"] = "explicitly_skipped"
		}
	} else {
		d.log.Warn("Image pull and digest verification explicitly skipped")
		record.Metadata["image_digest_verification"] = "skip_pull"
	}

	// Run pre-deploy command from new image (e.g., database migrations)
	if d.cfg.Deploy.PreDeployCommand != "" {
		if err := d.runPreDeployCommand(hosts[0], image); err != nil {
			return d.failAndRecord(record, fmt.Errorf("pre-deploy command failed: %w", err))
		}
	}

	// Deploy to each host, tracking successes for potential fleet rollback.
	_, deployErrors := d.runFleetDeployment(
		targets,
		d.cfg.Deploy.RollbackOnFailure,
		func(target deploymentTarget) error {
			return d.deployToTarget(ctx, target, image, version, opts)
		},
		func(succeeded []deploymentTarget) error {
			return d.rollbackTargets(ctx, succeeded, record.PreviousVersion)
		},
	)

	if len(deployErrors) > 0 {
		err := fmt.Errorf("deployment failed on %d host(s): %s", len(deployErrors), strings.Join(deployErrors, "; "))
		return d.failAndRecord(record, err)
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
		return fmt.Errorf("deployment completed remotely but durable history persistence failed: %w", err)
	}

	d.log.Success("Deployment complete!")
	return nil
}

// runFleetDeployment is the scheduling boundary for a multi-target deploy.
// With rollback enabled, the first failure stops new work and every target
// that already succeeded is handed to the rollback callback exactly once.
func (d *Deployer) runFleetDeployment(
	targets []deploymentTarget,
	rollbackOnFailure bool,
	deployTarget func(deploymentTarget) error,
	rollbackTargets func([]deploymentTarget) error,
) ([]deploymentTarget, []string) {
	var deployErrors []string
	var succeededTargets []deploymentTarget
	for _, target := range targets {
		if err := deployTarget(target); err != nil {
			d.log.HostError(target.Host, "%s role deployment failed: %v", target.Role, err)
			deployErrors = append(deployErrors, fmt.Sprintf("%s/%s: %v", target.Host, target.Role, err))
			if rollbackOnFailure {
				if len(succeededTargets) > 0 {
					d.log.Warn("Rolling back %d already-deployed target(s) due to failure on %s/%s...", len(succeededTargets), target.Host, target.Role)
					if rollbackErr := rollbackTargets(append([]deploymentTarget(nil), succeededTargets...)); rollbackErr != nil {
						deployErrors = append(deployErrors, fmt.Sprintf("automatic rollback: %v", rollbackErr))
					}
				}
				break
			}
			continue
		}
		succeededTargets = append(succeededTargets, target)
		d.log.HostSuccess(target.Host, "%s role deployed successfully", target.Role)
	}
	return succeededTargets, deployErrors
}

func (d *Deployer) failAndRecord(record *DeploymentRecord, cause error) error {
	record.Fail(cause)
	if err := d.history.Record(record); err != nil {
		return fmt.Errorf("%w (failed to persist deployment failure: %v)", cause, err)
	}
	return cause
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
		sort.Strings(errMsgs)
		return fmt.Errorf("registry login failed on hosts: %s", strings.Join(errMsgs, "; "))
	}

	return nil
}

func (d *Deployer) deployToTarget(ctx context.Context, target deploymentTarget, image, version string, opts *DeployOptions) error {
	// Acquire deployment lock to prevent concurrent deployments to the same host/service
	lockFile := state.LockFile(d.cfg.SSH.User, d.cfg.Service+".deploy")
	lockTimeout := d.cfg.Deploy.DeployTimeout * 2
	if lockTimeout < 5*time.Minute {
		lockTimeout = 5 * time.Minute
	}

	var deployErr error
	lockErr := d.sshClient.WithRemoteLock(target.Host, lockFile, lockTimeout, func() error {
		deployErr = d.deployToTargetLocked(ctx, target, image, version, opts)
		return nil
	})
	if lockErr != nil {
		return fmt.Errorf("failed to acquire deployment lock: %w", lockErr)
	}
	return deployErr
}

func (d *Deployer) deployToTargetLocked(ctx context.Context, target deploymentTarget, image, version string, opts *DeployOptions) error {
	host, role := target.Host, target.Role
	d.log.Host(host, "Starting %s role deployment...", role)

	oldContainerName := RoleContainerName(d.cfg, role)
	newContainerName := d.generateContainerName(oldContainerName, "new")
	oldExists, err := d.containers.Exists(host, oldContainerName)
	if err != nil {
		return fmt.Errorf("failed to determine whether current container exists: %w", err)
	}
	containerConfig := d.buildContainerConfig(image, newContainerName, role)

	// Run pre-app-boot hook
	bootCtx := d.hookContext(opts, image, version)
	bootCtx.Hosts = host
	bootCtx.Role = role
	if err := d.hooks.Run(ctx, "pre-app-boot", bootCtx); err != nil {
		return fmt.Errorf("pre-app-boot hook failed: %w", err)
	}

	d.log.Host(host, "Starting new container...")
	_, err = d.containers.Run(host, containerConfig)
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	removeNewContainer := func(cause error) error {
		if removeErr := d.containers.Remove(host, newContainerName, true); removeErr != nil {
			return fmt.Errorf("%w (failed to remove new container %s: %v)", cause, newContainerName, removeErr)
		}
		return cause
	}

	// Wait for container to pass readiness check
	if IsProxyRole(role) && !opts.SkipHealthCheck && HasReadinessProbe(d.cfg) {
		d.log.Host(host, "Waiting for readiness check...")

		// Wait for readiness delay
		if d.cfg.Deploy.ReadinessDelay > 0 {
			time.Sleep(d.cfg.Deploy.ReadinessDelay)
		}

		if err := d.waitForHealthy(host, newContainerName); err != nil {
			return removeNewContainer(fmt.Errorf("readiness check failed: %w", err))
		}
	}
	if !IsProxyRole(role) && !opts.SkipHealthCheck {
		d.log.Host(host, "Waiting for %s role to stabilize...", role)
		if err := d.containers.WaitRunning(host, newContainerName, d.cfg.Deploy.ReadinessDelay); err != nil {
			return removeNewContainer(fmt.Errorf("container startup check failed: %w", err))
		}
	}

	// Run post-app-boot hook
	if err := d.hooks.Run(ctx, "post-app-boot", bootCtx); err != nil {
		d.log.Warn("post-app-boot hook failed: %v", err)
	}

	if !IsProxyRole(role) {
		return d.finalizeStandaloneRole(host, oldContainerName, newContainerName, oldExists)
	}

	// Ensure the proxy container is running before attempting any admin
	// API calls. Boot is idempotent: if the container is already running
	// it applies config and returns quickly; if it was stopped or removed
	// it will (re)start it and wait for the admin API to be ready.
	if err := d.proxy.Boot(host, newProxyConfigFromCfg(d.cfg)); err != nil {
		return removeNewContainer(fmt.Errorf("failed to boot proxy: %w", err))
	}

	// Ensure the proxy has TLS/ACME config applied before registering.
	// This handles the case where the proxy was rebooted or recreated
	// between deploys and lost its config.
	if err := d.proxy.EnsureConfig(host); err != nil {
		return removeNewContainer(fmt.Errorf("failed to ensure proxy config: %w", err))
	}

	// Register new container with proxy
	d.log.Host(host, "Registering with proxy...")
	newUpstream, err := d.upstreamAddr(host, newContainerName)
	if err != nil {
		return removeNewContainer(fmt.Errorf("failed to resolve new upstream: %w", err))
	}
	proxyHost := d.cfg.Proxy.PrimaryHost()
	oldUpstream := ""
	if oldExists {
		oldUpstream, err = d.upstreamAddr(host, oldContainerName)
		if err != nil {
			d.log.Debug("Failed to resolve old upstream: %v", err)
			oldUpstream = fmt.Sprintf("%s:%d", oldContainerName, d.cfg.Proxy.AppPort)
		}
	}

	cleanupNewBeforePreserve := func(cause error, restoreOldRoute, removeNewRoute bool) error {
		var cleanupErrors []string
		if proxyHost != "" && restoreOldRoute && oldUpstream != "" {
			if err := d.proxy.AddUpstream(host, proxyHost, oldUpstream); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("restore old route: %v", err))
			}
		}
		if proxyHost != "" && removeNewRoute {
			if err := d.proxy.RemoveUpstream(host, proxyHost, newUpstream); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove new route: %v", err))
			}
		}
		if err := d.containers.Remove(host, newContainerName, true); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove new container: %v", err))
		}
		if len(cleanupErrors) > 0 {
			return fmt.Errorf("%w (cleanup incomplete: %s)", cause, strings.Join(cleanupErrors, "; "))
		}
		return cause
	}

	// Registering the new container with the proxy MUST succeed before we
	// touch the old container. Otherwise a transient proxy failure could
	// remove the only healthy upstream and leave the service unreachable
	// while the deploy still reports success (masking the outage from
	// rollback_on_failure).
	var regErr error
	if oldExists && proxyHost != "" {
		// Add new upstream alongside the old one so both receive traffic
		// during the transition. Using AddUpstream (not RegisterService)
		// preserves the old upstream in the route, which is required for
		// connection-aware draining to work.
		if err := d.proxy.AddUpstream(host, proxyHost, newUpstream); err != nil {
			// AddUpstream may fail if there's no existing route (e.g., proxy
			// was rebooted). Fall back to a full RegisterService.
			d.log.Debug("Failed to add upstream alongside old: %v", err)
			regErr = d.registerWithProxy(host, newUpstream)
		}
	} else {
		// First deployment, or no primary proxy host — register the service.
		regErr = d.registerWithProxy(host, newUpstream)
	}
	if regErr != nil {
		// Roll back the new container and leave the old one serving so an
		// automatic rollback or the next deploy can recover cleanly.
		return cleanupNewBeforePreserve(
			fmt.Errorf("failed to register new container with proxy: %w", regErr),
			oldExists,
			oldExists,
		)
	}

	var backupName string
	oldPreserved := false
	// If an old container exists, take it out of rotation but preserve it
	// under a backup name until the new name and route are confirmed.
	if oldExists {
		// Remove old container from proxy so no new requests are routed to it.
		// The old upstream is still tracked by Caddy until its in-flight
		// requests complete, allowing the drain step to poll accurately.
		d.log.Host(host, "Removing old upstream from proxy...")
		if proxyHost != "" {
			if err := d.proxy.RemoveUpstream(host, proxyHost, oldUpstream); err != nil {
				return cleanupNewBeforePreserve(
					fmt.Errorf("failed to remove old upstream before drain: %w", err), true, true,
				)
			}
		}

		// Drain: poll Caddy for in-flight requests on the old upstream,
		// falling back to a sleep if the API is unavailable.
		if d.cfg.Deploy.DrainTimeout > 0 {
			if err := d.proxy.DrainUpstream(host, oldUpstream, d.cfg.Deploy.DrainTimeout); err != nil {
				return cleanupNewBeforePreserve(
					fmt.Errorf("failed to drain old upstream: %w", err), true, true,
				)
			}
		}

		backupName = d.generateContainerName(oldContainerName, "old")
		if err := d.containers.Rename(host, oldContainerName, backupName); err != nil {
			return cleanupNewBeforePreserve(
				fmt.Errorf("failed to preserve old container: %w", err), true, true,
			)
		}
		oldPreserved = true
	}

	rollbackSwap := func(cause error, newHasStableName bool) error {
		var rollbackErrors []string
		if newHasStableName {
			if err := d.containers.Rename(host, oldContainerName, newContainerName); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("rename new container back: %v", err))
			}
		}
		if oldPreserved {
			if err := d.containers.Rename(host, backupName, oldContainerName); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("restore old container name: %v", err))
			}
			if err := d.proxy.AddUpstream(host, proxyHost, oldUpstream); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("restore old route: %v", err))
			} else if err := d.proxy.RemoveUpstream(host, proxyHost, newUpstream); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("remove new route: %v", err))
			} else if err := d.containers.Remove(host, newContainerName, true); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("remove new container: %v", err))
			}
		}
		if len(rollbackErrors) > 0 {
			return fmt.Errorf("%w (automatic restore incomplete: %s)", cause, strings.Join(rollbackErrors, "; "))
		}
		return cause
	}

	// Finalize: rename the new container to the service name and swap
	// the proxy upstream using add-then-remove so at least one upstream
	// is always present (no gap = no dropped requests).
	d.log.Host(host, "Finalizing deployment...")
	if err := d.containers.Rename(host, newContainerName, oldContainerName); err != nil {
		return rollbackSwap(fmt.Errorf("failed to assign stable container name: %w", err), false)
	}

	// The current container is now named oldContainerName (the service name),
	// so any remaining "<service>-new-*" container is an orphan from a prior
	// failed rename. Reap them now that we're holding the deploy lock.
	// In mixed rootless/rootful mode, upstreams are host loopback ports.
	// Renaming a container does not change its published host port, so the
	// upstream address stays the same and there is nothing to swap.
	if d.cfg.UseHostPortUpstreams() {
		if oldPreserved {
			if err := d.containers.Remove(host, backupName, true); err != nil {
				return rollbackSwap(fmt.Errorf("failed to remove preserved old container: %w", err), true)
			}
		}
		d.reapStaleTempContainers(host, oldContainerName)
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
				return rollbackSwap(fmt.Errorf("failed to update proxy to final container name: %w", regErr), true)
			}
		} else if err := d.proxy.RemoveUpstream(host, proxyHost, newUpstream); err != nil {
			// The final upstream was added successfully, so traffic remains
			// available. Treat cleanup failure as a deployment failure so it is
			// visible and rollback policy can act on it.
			return rollbackSwap(fmt.Errorf("failed to remove temporary proxy upstream: %w", err), true)
		}
	}
	if oldPreserved {
		if err := d.containers.Remove(host, backupName, true); err != nil {
			return rollbackSwap(fmt.Errorf("failed to remove preserved old container: %w", err), true)
		}
	}
	d.reapStaleTempContainers(host, oldContainerName)

	return nil
}

// finalizeStandaloneRole swaps a non-HTTP role without involving Caddy. The
// old container is renamed out of the way first so a failed second rename can
// restore it. If old-container cleanup fails, the swap is reversed to avoid
// running duplicate workers.
func (d *Deployer) finalizeStandaloneRole(host, stableName, newName string, oldExists bool) error {
	d.log.Host(host, "Finalizing %s container...", stableName)
	removeNew := func(cause error) error {
		if removeErr := d.containers.Remove(host, newName, true); removeErr != nil {
			return fmt.Errorf("%w (failed to remove new container %s: %v)", cause, newName, removeErr)
		}
		return cause
	}
	if !oldExists {
		if err := d.containers.Rename(host, newName, stableName); err != nil {
			return removeNew(fmt.Errorf("failed to assign stable container name: %w", err))
		}
		d.reapStaleTempContainers(host, stableName)
		return nil
	}

	backupName := d.generateContainerName(stableName, "old")
	if err := d.containers.Rename(host, stableName, backupName); err != nil {
		return removeNew(fmt.Errorf("failed to preserve current container: %w", err))
	}
	if err := d.containers.Rename(host, newName, stableName); err != nil {
		restoreErr := d.containers.Rename(host, backupName, stableName)
		cause := fmt.Errorf("failed to activate new container: %w", err)
		if restoreErr != nil {
			cause = fmt.Errorf("failed to activate new container: %v (also failed to restore current container: %v)", err, restoreErr)
		}
		return removeNew(cause)
	}

	if err := d.containers.Remove(host, backupName, true); err != nil {
		rollbackRenameErr := d.containers.Rename(host, stableName, newName)
		restoreErr := d.containers.Rename(host, backupName, stableName)
		cause := fmt.Errorf("failed to remove previous container; restored old container: %w", err)
		if rollbackRenameErr != nil || restoreErr != nil {
			cause = fmt.Errorf("failed to remove previous container: %v (failed to restore cleanly: rename new=%v, restore old=%v)", err, rollbackRenameErr, restoreErr)
		}
		return removeNew(cause)
	}

	d.reapStaleTempContainers(host, stableName)
	return nil
}

func (d *Deployer) buildContainerConfig(image, name, role string) *podman.ContainerConfig {
	return NewAppContainerConfig(d.cfg, image, name, role, nil)
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
	return d.proxy.RegisterService(host, BuildProxyServiceConfig(d.cfg, []string{upstream}, nil))
}

// BuildProxyServiceConfig maps application configuration and discovered
// upstreams to the complete desired proxy route.
func BuildProxyServiceConfig(cfg *config.Config, upstreams []string, weights []proxy.UpstreamWeight) *proxy.ServiceConfig {
	livenessPath := ""
	if !cfg.Proxy.Healthcheck.DisableLiveness && strings.TrimSpace(cfg.Proxy.Healthcheck.LivenessCmd) == "" {
		livenessPath = cfg.Proxy.Healthcheck.GetLivenessPath()
	}
	return &proxy.ServiceConfig{
		Name:                  cfg.Service,
		Host:                  cfg.Proxy.PrimaryHost(),
		Hosts:                 cfg.Proxy.AllHosts(),
		Upstreams:             upstreams,
		UpstreamWeights:       weights,
		UpstreamProtocol:      cfg.Proxy.UpstreamProtocol,
		HealthPath:            livenessPath,
		HealthInterval:        cfg.Proxy.Healthcheck.Interval,
		HealthTimeout:         cfg.Proxy.Healthcheck.Timeout,
		ResponseTimeout:       cfg.Proxy.ResponseTimeout,
		ResponseHeaderTimeout: cfg.Proxy.ResponseHeaderTimeout,
		ForwardHeaders:        cfg.Proxy.ForwardHeaders,
		BufferRequests:        cfg.Proxy.Buffering.Requests,
		BufferResponses:       cfg.Proxy.Buffering.Responses,
		MaxRequestBody:        cfg.Proxy.Buffering.MaxRequestBody,
		BufferMemory:          cfg.Proxy.Buffering.Memory,
		HTTPS:                 cfg.Proxy.SSL,
	}
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
	getDigest := d.imageDigest
	if getDigest == nil {
		getDigest = d.images.GetDigest
	}
	refDigest, err := getDigest(hosts[0], image)
	if err != nil {
		if d.cfg.Deploy.AllowUnverifiedImage {
			d.log.Warn("IMAGE DIGEST VERIFICATION DISABLED: could not verify %s on %s: %v", image, hosts[0], err)
			return "", nil
		}
		return "", fmt.Errorf("failed to get image digest on %s: %w (set deploy.allow_unverified_image only for a deliberate local-image exception)", hosts[0], err)
	}

	// Verify all other hosts have the same digest
	for _, host := range hosts[1:] {
		digest, err := getDigest(host, image)
		if err != nil {
			if d.cfg.Deploy.AllowUnverifiedImage {
				d.log.Warn("IMAGE DIGEST VERIFICATION DISABLED: could not verify %s on %s: %v", image, host, err)
				return "", nil
			}
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

func (d *Deployer) getTargets(opts *DeployOptions) ([]deploymentTarget, error) {
	if opts == nil {
		opts = &DeployOptions{}
	}

	roles := append([]string(nil), opts.Roles...)
	if len(roles) == 0 {
		for role := range d.cfg.Servers {
			roles = append(roles, role)
		}
		sort.Strings(roles)
	}

	requestedHosts := make(map[string]struct{}, len(opts.Hosts))
	for _, host := range opts.Hosts {
		requestedHosts[host] = struct{}{}
	}

	seen := make(map[deploymentTarget]struct{})
	matchedHosts := make(map[string]struct{})
	var targets []deploymentTarget
	for _, role := range roles {
		roleCfg, ok := d.cfg.Servers[role]
		if !ok {
			return nil, fmt.Errorf("unknown role %q", role)
		}
		for _, host := range roleCfg.Hosts {
			if len(requestedHosts) > 0 {
				if _, ok := requestedHosts[host]; !ok {
					continue
				}
				matchedHosts[host] = struct{}{}
			}
			target := deploymentTarget{Host: host, Role: role}
			if _, ok := seen[target]; ok {
				continue
			}
			seen[target] = struct{}{}
			targets = append(targets, target)
		}
	}

	for _, host := range opts.Hosts {
		if _, ok := matchedHosts[host]; !ok {
			return nil, fmt.Errorf("host %q is not configured for the selected role(s)", host)
		}
	}

	return targets, nil
}

func targetHosts(targets []deploymentTarget) []string {
	seen := make(map[string]struct{}, len(targets))
	hosts := make([]string, 0, len(targets))
	for _, target := range targets {
		if _, ok := seen[target.Host]; ok {
			continue
		}
		seen[target.Host] = struct{}{}
		hosts = append(hosts, target.Host)
	}
	return hosts
}

func targetRoles(targets []deploymentTarget) []string {
	seen := make(map[string]struct{}, len(targets))
	roles := make([]string, 0, len(targets))
	for _, target := range targets {
		if _, ok := seen[target.Role]; ok {
			continue
		}
		seen[target.Role] = struct{}{}
		roles = append(roles, target.Role)
	}
	return roles
}

func (d *Deployer) getTargetHosts(opts *DeployOptions) []string {
	targets, err := d.getTargets(opts)
	if err != nil {
		return nil
	}
	return targetHosts(targets)
}

func (d *Deployer) generateContainerName(stableName, suffix string) string {
	return fmt.Sprintf("%s-%s-%d", stableName, suffix, time.Now().UnixNano())
}

// reapStaleTempContainers removes leftover temporary deploy containers named
// "<service>-new-*" that can linger after a failed rename. It is best-effort
// and safe to call under the deploy lock once the current container has been
// renamed to the service name (so the active container is never matched).
func (d *Deployer) reapStaleTempContainers(host, stableName string) {
	prefix := stableName + "-new-"
	// The podman name filter is substring-based; enforce an exact prefix match
	// client-side to avoid touching similarly named services.
	containers, err := d.containers.List(host, true, map[string]string{"name": prefix})
	if err != nil {
		d.log.Debug("Orphan cleanup: failed to list containers on %s: %v", host, err)
		return
	}
	for _, c := range containers {
		if !strings.HasPrefix(c.Name, prefix) {
			continue
		}
		d.log.Debug("Orphan cleanup: removing stale temp container %s", c.Name)
		if err := d.containers.Remove(host, c.Name, true); err != nil {
			d.log.Debug("Orphan cleanup: failed to remove %s: %v", c.Name, err)
		}
	}
}

// stripImageTag removes a trailing :tag or @digest from an image reference,
// returning the bare name (registry/repository). It is digest-aware and does
// not mistake a registry port (e.g., localhost:5000/img) for a tag.
func stripImageTag(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if idx := strings.LastIndex(image, ":"); idx > 0 && !strings.Contains(image[idx:], "/") {
		image = image[:idx]
	}
	return image
}

// hasImageTag reports whether an image reference carries an explicit :tag
// (ignoring a registry port such as localhost:5000/img).
func hasImageTag(image string) bool {
	idx := strings.LastIndex(image, ":")
	return idx > 0 && !strings.Contains(image[idx:], "/")
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
	return d.StopRoles(hosts, nil)
}

func (d *Deployer) StopRoles(hosts, roles []string) error {
	stopTimeout := d.cfg.Deploy.GetStopTimeout()
	return d.runOnTargets("stop", hosts, roles, func(target deploymentTarget) error {
		return d.containers.Stop(target.Host, RoleContainerName(d.cfg, target.Role), stopTimeout)
	})
}

func (d *Deployer) Start(hosts []string) error {
	return d.StartRoles(hosts, nil)
}

func (d *Deployer) StartRoles(hosts, roles []string) error {
	return d.runOnTargets("start", hosts, roles, func(target deploymentTarget) error {
		return d.containers.Start(target.Host, RoleContainerName(d.cfg, target.Role))
	})
}

func (d *Deployer) Restart(hosts []string) error {
	return d.RestartRoles(hosts, nil)
}

func (d *Deployer) RestartRoles(hosts, roles []string) error {
	stopTimeout := d.cfg.Deploy.GetStopTimeout()
	return d.runOnTargets("restart", hosts, roles, func(target deploymentTarget) error {
		return d.containers.Restart(target.Host, RoleContainerName(d.cfg, target.Role), stopTimeout)
	})
}

// rollbackTargets reverts role/host pairs that succeeded, restoring the
// previous version. Every target is attempted and failures are aggregated.
func (d *Deployer) rollbackTargets(ctx context.Context, targets []deploymentTarget, previousVersion string) error {
	if previousVersion == "" {
		return fmt.Errorf("no previous version recorded")
	}

	prevImage := fmt.Sprintf("%s:%s", stripImageTag(d.cfg.Image), previousVersion)
	var rollbackErrors []string

	for _, target := range targets {
		d.log.Host(target.Host, "Rolling back %s role to %s...", target.Role, previousVersion)

		// Re-deploy the previous version. deployToHostLocked performs a full
		// blue-green swap: it starts the previous-version container alongside
		// the current (just-deployed) one, health-checks it, moves the proxy
		// over, and only then drains and removes the current container. This
		// keeps the rollback zero-downtime and — crucially — if the previous
		// image is no longer cached, the new container fails to start *before*
		// the current one is touched, leaving the host on a working version
		// instead of stranded with nothing.
		rollbackOpts := &DeployOptions{
			Version:     previousVersion,
			SkipPull:    true, // Image should still be cached from last deployment
			Hosts:       []string{target.Host},
			Roles:       []string{target.Role},
			Destination: "rollback",
		}

		// Use deployToTarget directly to avoid recursion into the full Deploy
		// method (which would run hooks, record history, etc.)
		if err := d.deployToTarget(ctx, target, prevImage, previousVersion, rollbackOpts); err != nil {
			d.log.HostError(target.Host, "%s role rollback failed: %v", target.Role, err)
			rollbackErrors = append(rollbackErrors, fmt.Sprintf("%s/%s: %v", target.Host, target.Role, err))
			continue
		}
		d.log.HostSuccess(target.Host, "%s role rolled back to %s", target.Role, previousVersion)
	}

	// Run post-rollback hook
	rollbackCtx := &HookContext{
		Service:     d.cfg.Service,
		Image:       prevImage,
		Version:     previousVersion,
		Hosts:       strings.Join(targetHosts(targets), ","),
		Destination: "rollback",
		Performer:   CurrentUser(),
		Role:        strings.Join(targetRoles(targets), ","),
		RecordedAt:  time.Now().Format(time.RFC3339),
	}
	if err := d.hooks.Run(ctx, "post-rollback", rollbackCtx); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("post-rollback hook: %v", err))
	}
	if len(rollbackErrors) > 0 {
		return fmt.Errorf("rollback failed: %s", strings.Join(rollbackErrors, "; "))
	}
	return nil
}

func (d *Deployer) runOnTargets(operation string, hosts, roles []string, fn func(deploymentTarget) error) error {
	targets, err := d.getTargets(&DeployOptions{Hosts: hosts, Roles: roles})
	if err != nil {
		return err
	}
	label := strings.ToUpper(operation[:1]) + operation[1:]
	d.log.Info("%sing %s on %d role target(s)...", label, d.cfg.Service, len(targets))

	var wg sync.WaitGroup
	errors := make(chan error, len(targets))
	for _, target := range targets {
		wg.Add(1)
		go func(target deploymentTarget) {
			defer wg.Done()
			if err := fn(target); err != nil {
				errors <- fmt.Errorf("%s/%s: %w", target.Host, target.Role, err)
				return
			}
			d.log.HostSuccess(target.Host, "%s role %sed", target.Role, operation)
		}(target)
	}
	wg.Wait()
	close(errors)

	var errs []string
	for err := range errors {
		errs = append(errs, err.Error())
	}
	sort.Strings(errs)
	if len(errs) > 0 {
		return fmt.Errorf("%s failed: %s", operation, strings.Join(errs, "; "))
	}
	return nil
}
