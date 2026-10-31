package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/ssh"
	"github.com/lemonity-org/azud/internal/state"
	"github.com/lemonity-org/azud/pkg/version"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Deploy from the server by following a registry tag",
	Long: `Run Azud on the server in pull mode.

The agent follows the registry tag in agent.track. When the tag moves to a new
image, the agent runs agent.verify_command against the image pinned by digest
and, if it passes, deploys it on this server with the same steps as
'azud deploy': pre-deploy command, health-checked container swap, and
rollback on failure. CI only builds, signs, and tags images; it needs no SSH
access or deploy secrets.

The agent runs as ssh.user on the single server the service is configured
for. Install it as a systemd user service with 'azud agent unit'.`,
}

var agentRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Follow agent.track and deploy each new image",
	Long: `Follow agent.track and deploy each new image on this server.

Without --once the agent checks the tag every agent.interval until it is
stopped. A deploy that has started runs to completion before the agent
exits. A release that fails verification or deployment is not tried again
until the tag moves; use --once --retry-failed to try it again.

Example:
  azud agent run                        # Run until stopped (systemd)
  azud agent run --once                 # Check the tag once
  azud agent run --once --retry-failed  # Retry a release that failed`,
	RunE: runAgentRun,
}

var agentStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show what the agent last saw and deployed",
	Annotations: map[string]string{
		skipSecretsAnnotation: "true",
	},
	RunE: runAgentStatus,
}

var agentUnitCmd = &cobra.Command{
	Use:   "unit",
	Short: "Print a systemd user unit that runs the agent",
	Long: `Print a systemd user unit that runs 'azud agent run' with this
configuration, destination, and working directory.

Example:
  mkdir -p ~/.config/systemd/user
  azud agent unit > ~/.config/systemd/user/azud-agent-myapp.service
  systemctl --user daemon-reload
  systemctl --user enable --now azud-agent-myapp.service`,
	Annotations: map[string]string{
		skipSecretsAnnotation: "true",
	},
	RunE: runAgentUnit,
}

var agentInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create this server's key for release bundles",
	Long: `Create the age identity that decrypts release bundles' secrets on this
server, at agent.identity (default ~/.config/azud/agent/<service>.age, mode
0600), and print its public recipient. Add the recipient to
release.recipients in the repository's config/deploy.yml, publish a release,
and set agent.source: bundle on this server.

The private key never leaves this server. An existing identity is never
replaced.`,
	Annotations: map[string]string{
		skipSecretsAnnotation: "true",
	},
	RunE: runAgentInit,
}

var (
	agentOnce           bool
	agentRetryFailed    bool
	agentAllowDowngrade bool
)

func init() {
	agentRunCmd.Flags().BoolVar(&agentOnce, "once", false, "Check the tag once and exit")
	agentRunCmd.Flags().BoolVar(&agentRetryFailed, "retry-failed", false, "With --once, retry a release that failed before")
	agentRunCmd.Flags().BoolVar(&agentAllowDowngrade, "allow-downgrade", false, "With --once, deploy a release bundle older than the deployed one (a rollback)")

	agentCmd.AddCommand(agentInitCmd)
	agentCmd.AddCommand(agentRunCmd)
	agentCmd.AddCommand(agentStatusCmd)
	agentCmd.AddCommand(agentUnitCmd)
	rootCmd.AddCommand(agentCmd)
}

func runAgentRun(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if agentRetryFailed && !agentOnce {
		return fmt.Errorf("--retry-failed requires --once")
	}
	if agentAllowDowngrade && !agentOnce {
		return fmt.Errorf("--allow-downgrade requires --once")
	}
	if agentAllowDowngrade && !cfg.Agent.Bundles() {
		return fmt.Errorf("--allow-downgrade applies to agent.source: bundle")
	}
	if cfg.Agent.Track == "" {
		return fmt.Errorf("agent.track is not configured")
	}
	if err := checkAgentUser(cfg); err != nil {
		return err
	}

	log.Header("Agent / %s", cfg.Service)

	host, err := agentHost(cfg)
	if err != nil {
		return err
	}
	repository := imageRepository(cfg.Image)
	ref, err := registry.ParseReference(repository + ":" + cfg.Agent.Track)
	if err != nil {
		return fmt.Errorf("invalid image to follow: %w", err)
	}
	stateDir, err := state.LocalDir()
	if err != nil {
		return err
	}

	// Deploy commands outlive a stop request: the agent finishes the deploy
	// it started instead of leaving the server between releases.
	sshClient := ssh.NewClient(&ssh.Config{
		Context:        context.WithoutCancel(cmd.Context()),
		User:           cfg.SSH.User,
		CommandTimeout: cfg.SSH.CommandTimeout,
		LocalHost:      host,
	})
	defer func() { _ = sshClient.Close() }()

	resolver := newRegistryResolver()
	creds, err := registryCredentials(cfg, ref)
	if err != nil {
		return err
	}

	store := agent.NewStore(stateDir, cfg.Service)
	a := &agent.Agent{
		Ref:        repository + ":" + cfg.Agent.Track,
		Repository: repository,
		Resolve: func(ctx context.Context) (string, error) {
			return resolver.Digest(ctx, ref, creds)
		},
		Running: runningDigest(podman.NewClient(sshClient), cfg, host, repository),
		Deploy: func(ctx context.Context, image string) error {
			pinned := *cfg
			pinned.Image = image
			deployer := deploy.NewDeployer(&pinned, sshClient, log)
			return deployer.Deploy(ctx, &deploy.DeployOptions{
				Hosts:       []string{host},
				Destination: GetDestination(),
			})
		},
		State: store,
		Log:   log,
	}
	if cfg.Agent.Bundles() {
		identity, err := agentIdentityPath(cfg)
		if err != nil {
			return err
		}
		applier := &bundleApplier{
			server:         cfg,
			repo:           resolver.Repository(ref.Registry, ref.Repository, creds),
			dir:            agentReleasesDir(stateDir, cfg.Service),
			identity:       identity,
			allowDowngrade: cfg.Agent.AllowDowngrade || agentAllowDowngrade,
			deployed: func() (string, error) {
				st, err := store.Load()
				return st.DeployedDigest, err
			},
			withLock: func(fn func() error) error {
				lockFile, timeout := deploy.DeployLock(cfg)
				var fnErr error
				if err := sshClient.WithRemoteLock(host, lockFile, timeout, func() error {
					fnErr = fn()
					return nil
				}); err != nil {
					return fmt.Errorf("failed to acquire deployment lock: %w", err)
				}
				return fnErr
			},
			deploy: func(ctx context.Context, release *config.Config, image string) error {
				pinned := *release
				pinned.Image = image
				// Hooks never run for a bundle: they are host commands outside
				// what its signature covers.
				deployer := deploy.NewDeployer(&pinned, sshClient, log).WithoutHooks()
				return deployer.Deploy(ctx, &deploy.DeployOptions{
					Hosts:       []string{host},
					Destination: GetDestination(),
					LockHeld:    true,
				})
			},
			stopRoles: func(release *config.Config, roles []string) error {
				return deploy.NewDeployer(release, sshClient, log).StopRoles([]string{host}, roles)
			},
			accessoryEnv: accessoryEnvironment(podman.NewClient(sshClient), cfg.Service, host),
			version:      version.Version,
			log:          log,
		}
		// A bundle carries configuration and secrets as well as the image,
		// so a matching running image proves nothing: never adopt.
		a.Running = nil
		a.Deploy = applier.Deploy
		a.Interrupted = applier.Interrupted
	}
	if cfg.Agent.DeployCommand != "" {
		a.Deploy = commandDeploy(cfg, sshClient, host, a.Ref, os.Stdout)
		log.Info("Releases deploy through agent.deploy_command")
	}
	if cfg.Agent.InsecureSkipVerify {
		log.Warn("agent.insecure_skip_verify is set: images are deployed without verification")
	} else {
		verifier := &agent.CommandVerifier{
			Command: cfg.Agent.VerifyCommand,
			Timeout: cfg.Agent.VerifyTimeout,
			Env: []string{
				"AZUD_SERVICE=" + cfg.Service,
				"AZUD_AGENT_REF=" + a.Ref,
				"AZUD_DESTINATION=" + GetDestination(),
			},
		}
		a.Verify = verifier.Verify
	}

	if !agentOnce {
		return a.Run(cmd.Context(), cfg.Agent.Interval)
	}

	outcome, digest, err := a.Reconcile(cmd.Context(), agentRetryFailed || agentAllowDowngrade)
	switch outcome {
	case agent.Unchanged:
		log.Success("%s is deployed (%s)", a.Ref, digest)
	case agent.Adopted:
		log.Success("%s is already running; recorded it as deployed", a.Image(digest))
	case agent.Deployed:
		log.Success("Deployed %s", a.Image(digest))
	case agent.SkippedFailed:
		return fmt.Errorf("%s failed before; run with --retry-failed to try it again", a.Image(digest))
	}
	return err
}

func runAgentStatus(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	stateDir, err := state.LocalDir()
	if err != nil {
		return err
	}
	store := agent.NewStore(stateDir, cfg.Service)
	st, err := store.Load()
	if err != nil {
		return err
	}

	log.Header("Agent / %s", cfg.Service)
	if st.LastCheckAt.IsZero() {
		log.Info("The agent has not run for %s on this machine (%s)", cfg.Service, store.Path())
		return nil
	}

	formatTime := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format(time.RFC3339)
	}
	orDash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	rows := [][]string{
		{"Following", st.Ref},
		{"Last check", formatTime(st.LastCheckAt)},
		{"Tag points to", orDash(st.LastSeenDigest)},
		{"Deployed", orDash(st.DeployedDigest)},
		{"Deployed at", formatTime(st.DeployedAt)},
		{"Failed", orDash(st.FailedDigest)},
		{"Failed at", formatTime(st.FailedAt)},
		{"Last error", orDash(firstLine(st.LastError))},
	}
	if cfg.Agent.Bundles() {
		rows = append(rows, bundleStatusRows(stateDir, cfg, st.DeployedDigest)...)
	}
	if cfg.Agent.DeployCommand != "" {
		rows = append(rows, []string{"Deploys with", "agent.deploy_command"})
	}
	log.Table([]string{"Field", "Value"}, rows)
	return nil
}

func runAgentUnit(cmd *cobra.Command, args []string) error {
	path := configPath
	if path == "" {
		path = findConfigFile()
	}
	configFile, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}

	_, err = fmt.Fprint(cmd.OutOrStdout(), agentUnit(cfg.Service, executable, configFile, GetDestination(), workDir))
	return err
}

// agentUnit renders the systemd user unit for the agent.
func agentUnit(service, executable, configFile, destination, workDir string) string {
	execStart := []string{systemdQuote(executable), "agent", "run", "--config", systemdQuote(configFile)}
	if destination != "" {
		execStart = append(execStart, "--destination", systemdQuote(destination))
	}

	return fmt.Sprintf(`# Azud pull agent for %[1]s.
# Install: save as ~/.config/systemd/user/azud-agent-%[1]s.service, then
#   systemctl --user daemon-reload
#   systemctl --user enable --now azud-agent-%[1]s.service
# The user needs lingering (loginctl enable-linger) to run it without a login.
[Unit]
Description=Azud pull agent for %[1]s
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
WorkingDirectory=%[2]s
ExecStart=%[3]s
Restart=always
RestartSec=30
# On stop, only the agent gets SIGTERM. It finishes a deploy in progress and
# exits; podman commands it started are left to complete.
KillMode=process
TimeoutStopSec=30min

[Install]
WantedBy=default.target
`, service, systemdPath(workDir), strings.Join(execStart, " "))
}

// systemdPath escapes a path for a unit setting that takes it verbatim, such
// as WorkingDirectory=: systemd does not unquote those values but still
// expands % specifiers.
func systemdPath(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

// systemdQuote quotes an ExecStart argument when it contains characters that
// systemd would otherwise split or expand.
func systemdQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\"'\\$%;") {
		return value
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + replacer.Replace(value) + `"`
}

// checkAgentUser requires the agent to run as ssh.user, so state paths,
// rootless Podman, and the deploy lock match what pushed deploys use.
// commandDeploy runs agent.deploy_command under the host's deploy lock, the
// lock a pushed `azud deploy` takes, so the two never overlap. The command
// learns the lock's path from AZUD_DEPLOY_LOCK and must not start an
// `azud deploy` or `azud rollback` of its own, which would wait for it.
func commandDeploy(cfg *config.Config, locker remoteLocker, host, ref string, out io.Writer) func(context.Context, string) error {
	lockFile, lockTimeout := deploy.DeployLock(cfg)
	deployer := &agent.CommandDeployer{
		Command:   cfg.Agent.DeployCommand,
		Timeout:   cfg.Agent.DeployTimeout,
		StopGrace: cfg.Agent.DeployStopGrace,
		Env: []string{
			"AZUD_SERVICE=" + cfg.Service,
			"AZUD_AGENT_REF=" + ref,
			"AZUD_DESTINATION=" + GetDestination(),
			"AZUD_DEPLOY_LOCK=" + lockFile,
		},
		Output: out,
	}
	return func(ctx context.Context, image string) error {
		var deployErr error
		if err := locker.WithRemoteLock(host, lockFile, lockTimeout, func() error {
			deployErr = deployer.Deploy(ctx, image)
			return nil
		}); err != nil {
			return fmt.Errorf("failed to acquire deployment lock: %w", err)
		}
		return deployErr
	}
}

// remoteLocker is the part of the SSH client commandDeploy needs.
type remoteLocker interface {
	WithRemoteLock(host, lockFile string, timeout time.Duration, fn func() error) error
}

func checkAgentUser(cfg *config.Config) error {
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("failed to determine the current user: %w", err)
	}
	if current.Username != cfg.SSH.User {
		return fmt.Errorf("run the agent as %q (ssh.user, the user pushed deploys log in as); it is running as %q", cfg.SSH.User, current.Username)
	}
	return nil
}

// agentHost returns the one server the agent deploys. Validation limits pull
// mode to services that run on a single server.
func agentHost(cfg *config.Config) (string, error) {
	hosts := cfg.GetAllHosts()
	if len(hosts) != 1 {
		return "", fmt.Errorf("pull mode deploys services that run on one server; servers lists %d hosts", len(hosts))
	}
	return hosts[0], nil
}

// imageRepository strips the tag or digest from an image reference.
func imageRepository(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if idx := strings.LastIndex(image, ":"); idx > strings.LastIndex(image, "/") {
		image = image[:idx]
	}
	return image
}

// registryCredentials returns the configured registry credentials when
// they belong to the registry the followed image lives in, and nil otherwise,
// so credentials are never sent to a different registry. A configured password
// that cannot be found is an error, as it is for a pushed deploy: resolving
// anonymously instead would only fail later with a less useful message.
func registryCredentials(cfg *config.Config, ref registry.Reference) (*registry.Credentials, error) {
	if cfg.Registry.Username == "" || len(cfg.Registry.Password) == 0 {
		return nil, nil
	}
	server := cfg.Registry.Server
	if server == "" || server == "index.docker.io" || server == "registry-1.docker.io" {
		server = "docker.io"
	}
	if server != ref.Registry {
		return nil, nil
	}
	key := cfg.Registry.Password[0]
	password, ok := getSecret(key)
	if !ok {
		where := "the agent's environment"
		if strings.EqualFold(strings.TrimSpace(cfg.SecretsProvider), "file") || cfg.SecretsProvider == "" {
			path := cfg.SecretsPath
			if path == "" {
				path = ".azud/secrets"
			}
			where = fmt.Sprintf("%s (KEY=value, mode 0600) or the agent's environment", path)
		}
		return nil, fmt.Errorf("registry password %s for %s was not found: put it in %s", key, ref.Registry, where)
	}
	return &registry.Credentials{Username: cfg.Registry.Username, Password: password}, nil
}

// runningDigest reports the digest every role container runs, or "" when a
// role is missing, runs a different image, or was deployed by tag.
func runningDigest(podmanClient *podman.Client, cfg *config.Config, host, repository string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		digest := ""
		for _, role := range cfg.GetRoles() {
			result, err := podmanClient.Execute(host, "container", "inspect", "--format", "{{.ImageName}}", deploy.RoleContainerName(cfg, role))
			if err != nil {
				return "", err
			}
			if result.ExitCode != 0 {
				return "", nil
			}
			name, roleDigest, pinned := strings.Cut(strings.TrimSpace(result.Stdout), "@")
			if !pinned || name != repository || (digest != "" && roleDigest != digest) {
				return "", nil
			}
			digest = roleDigest
		}
		return digest, nil
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
