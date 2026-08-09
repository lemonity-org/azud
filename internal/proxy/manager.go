package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/ssh"
	"github.com/lemonity-org/azud/internal/state"
)

const (
	// Pinned by version and multi-platform OCI digest. Update deliberately after
	// reviewing Caddy release notes and the official-image manifest.
	CaddyImage         = "docker.io/library/caddy:2.11.4-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648"
	CaddyContainerName = "azud-proxy"
	CaddyAdminPort     = 2019
	CaddyHTTPPort      = 80
	CaddyHTTPSPort     = 443

	// CaddyConfigFileName is the name of the Caddy config file.
	CaddyConfigFileName = "caddy-config.json"

	// CaddyLockFileName is the name of the Caddy lock file.
	CaddyLockFileName = "caddy.lock"

	CaddyLockTimeout = 120 * time.Second

	azudRouteIDPrefix   = "azud-route-"
	azudHandlerIDPrefix = "azud-proxy-"

	// upstreamTryDuration and upstreamTryInterval keep a request alive while a
	// route momentarily has no available upstream, which is what a container
	// swap looks like from Caddy's side. Without them Caddy answers 503 on the
	// first such request. The window is deliberately much larger than a swap
	// gap and much smaller than the passive-health fail_duration: during a real
	// outage requests should fail rather than pile up.
	upstreamTryDuration = "5s"
	upstreamTryInterval = "250ms"

	// Bridged containers must listen on the container interface so Podman's
	// loopback-only host port can reach the API. Host-networked containers share
	// the host namespace and therefore stay bound to host loopback.
	caddyAdminBridgeListen = "0.0.0.0:2019" // safe: container-only; Podman publishes this port to host loopback
	caddyAdminHostListen   = "127.0.0.1:2019"
)

// CaddyConfigDir returns the Caddy config directory for the given user.
func CaddyConfigDir(user string) string {
	return state.Dir(user)
}

// CaddyConfigFile returns the path to the Caddy config file for the given user.
func CaddyConfigFile(user string) string {
	return state.ConfigFile(user, CaddyConfigFileName)
}

// CaddyLockFile returns the path to the Caddy lock file for the given user.
func CaddyLockFile(user string) string {
	return state.LockFile(user, "caddy")
}

// Manager provisions and configures Caddy reverse proxies on remote hosts.
type Manager struct {
	sshClient   *ssh.Client
	caddyClient *CaddyClient
	podman      *podman.ContainerManager
	log         *output.Logger
	user        string // SSH user for state directory paths
	rootful     bool
	hostPorts   bool
	proxyConfig *ProxyConfig // cached proxy config for fallback rebuilds
}

// NewManager creates a new proxy manager. Defaults to root user for state paths.
func NewManager(sshClient *ssh.Client, log *output.Logger) *Manager {
	return NewManagerWithOptions(sshClient, log, "root", false, false)
}

// NewManagerWithUser creates a new proxy manager with a specific SSH user.
func NewManagerWithUser(sshClient *ssh.Client, log *output.Logger, user string) *Manager {
	return NewManagerWithOptions(sshClient, log, user, false, false)
}

// NewManagerWithOptions creates a proxy manager with explicit runtime options.
func NewManagerWithOptions(sshClient *ssh.Client, log *output.Logger, user string, rootful bool, hostPortUpstreams bool) *Manager {
	if log == nil {
		log = output.DefaultLogger
	}
	if user == "" {
		user = "root"
	}

	podmanCmd := "podman"
	if rootful && user != "root" {
		podmanCmd = "sudo -n podman"
	}
	podmanClient := podman.NewClientWithCommand(sshClient, podmanCmd)

	return &Manager{
		sshClient:   sshClient,
		caddyClient: NewCaddyClient(sshClient),
		podman:      podman.NewContainerManager(podmanClient),
		log:         log,
		user:        user,
		rootful:     rootful,
		hostPorts:   hostPortUpstreams,
	}
}

// SetProxyConfig stores the proxy configuration for use when rebuilding
// the full Caddy config during service registration fallback.
func (m *Manager) SetProxyConfig(config *ProxyConfig) {
	m.proxyConfig = config
}

func (m *Manager) adminListen() string {
	if m.hostPorts {
		return caddyAdminHostListen
	}
	return caddyAdminBridgeListen
}

// EnsureConfig ensures the proxy has TLS/ACME and logging settings applied.
// Safe to call on every deploy — it reads the running config and updates
// only the settings layer (TLS, AutoHTTPS, logging) without touching routes.
func (m *Manager) EnsureConfig(host string) error {
	if m.proxyConfig == nil {
		return nil
	}
	if err := validateProxyConfig(m.proxyConfig); err != nil {
		return err
	}
	return m.withPersistedMutation(host, func() error {
		return m.applyConfigPreservingRoutes(host, m.proxyConfig)
	})
}

// waitForAdminAPI polls the Caddy admin API until it responds or the
// timeout is reached. This replaces fixed sleeps after container starts.
func (m *Manager) waitForAdminAPI(host string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := 500 * time.Millisecond
	for time.Now().Before(deadline) {
		if _, err := m.caddyClient.apiRequest(host, "GET", "/config/", nil); err == nil {
			return nil
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("caddy admin API not ready after %s", timeout)
}

// withCaddyLock acquires the remote Caddy lock on the given host, runs fn,
// then releases. This serializes mutating Caddy admin API operations.
func (m *Manager) withCaddyLock(host string, fn func() error) error {
	return m.sshClient.WithRemoteLock(host, CaddyLockFile(m.user), CaddyLockTimeout, fn)
}

// withPersistedMutation makes a Caddy change transactional across the live
// admin API and Azud's reboot state. If the protected on-disk copy cannot be
// written, the prior live configuration is restored before the error returns.
func (m *Manager) withPersistedMutation(host string, mutate func() error) error {
	return m.withCaddyLock(host, func() error {
		return m.withPersistedMutationLocked(host, mutate)
	})
}

// withPersistedMutationLocked runs a transactional mutation while the caller
// already holds the remote Caddy lock.
func (m *Manager) withPersistedMutationLocked(host string, mutate func() error) error {
	before, err := m.caddyClient.apiRequest(host, "GET", "/config/", nil)
	if err != nil {
		return fmt.Errorf("failed to snapshot Caddy config before mutation: %w", err)
	}
	if !json.Valid(before) {
		return fmt.Errorf("failed to snapshot Caddy config before mutation: invalid JSON")
	}
	if err := mutate(); err != nil {
		if restoreErr := m.caddyClient.LoadRawConfig(host, before); restoreErr != nil {
			return fmt.Errorf("caddy mutation failed: %v (live rollback also failed: %v)", err, restoreErr)
		}
		return fmt.Errorf("caddy mutation failed; restored previous live config: %w", err)
	}
	if err := m.persistConfig(host); err != nil {
		if restoreErr := m.caddyClient.LoadRawConfig(host, before); restoreErr != nil {
			return fmt.Errorf("failed to persist Caddy mutation: %v (live rollback also failed: %v)", err, restoreErr)
		}
		return fmt.Errorf("failed to persist Caddy mutation; restored previous live config: %w", err)
	}
	return nil
}

func (m *Manager) ensureRootfulAccess(host string) error {
	if !m.rootful || m.user == "root" {
		return nil
	}

	// Verify the exact command family we need for rootful proxy operations.
	// This avoids requiring broad sudo privileges such as `sudo -n true`.
	result, err := m.sshClient.Execute(host, "sudo -n podman version --format '{{.Client.Version}}'")
	if err != nil {
		return fmt.Errorf("proxy.rootful requires passwordless sudo for podman: %w", err)
	}
	if result.ExitCode != 0 {
		msg := strings.TrimSpace(result.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(result.Stdout)
		}
		if msg == "" {
			msg = "sudo -n podman version failed"
		}
		return fmt.Errorf("proxy.rootful requires passwordless sudo for podman on %s: %s", host, msg)
	}
	return nil
}

func (m *Manager) containerUsesResume(host, container string) (bool, error) {
	raw, err := m.podman.Inspect(host, container)
	if err != nil {
		return false, err
	}
	var payload []struct {
		Config struct {
			Cmd []string `json:"Cmd"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return false, err
	}
	if len(payload) == 0 {
		return false, fmt.Errorf("empty inspect result")
	}
	return commandUsesResume(payload[0].Config.Cmd), nil
}

func commandUsesResume(arguments []string) bool {
	for _, argument := range arguments {
		for _, field := range strings.Fields(argument) {
			if strings.Trim(field, "'\";") == "--resume" {
				return true
			}
		}
	}
	return false
}

func (m *Manager) normalizedCaddyAutosave(host string) ([]byte, bool, error) {
	result, err := m.podman.Run(host, &podman.ContainerConfig{
		Image:           CaddyImage,
		Entrypoint:      "/bin/sh",
		Command:         []string{"-c", "if [ -s /config/caddy/autosave.json ]; then cat /config/caddy/autosave.json; fi"},
		Volumes:         []string{"caddy_config:/config:ro"},
		ReadOnly:        true,
		CapDrop:         []string{"all"},
		NoNewPrivileges: true,
		Remove:          true,
	})
	if err != nil {
		return nil, false, fmt.Errorf("failed to inspect Caddy autosave volume: %w", err)
	}
	if strings.TrimSpace(result) == "" {
		return nil, false, nil
	}
	normalized, err := normalizeCaddyAutosave([]byte(result), m.adminListen())
	if err != nil {
		return nil, false, err
	}
	return normalized, true, nil
}

func normalizeCaddyAutosave(data []byte, adminListen string) ([]byte, error) {
	autosave, err := decodeJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("invalid Caddy autosave JSON: %w", err)
	}
	admin, ok := autosave["admin"].(map[string]interface{})
	if !ok {
		admin = make(map[string]interface{})
		autosave["admin"] = admin
	}
	admin["listen"] = adminListen
	normalized, err := json.Marshal(autosave)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize Caddy autosave: %w", err)
	}
	return normalized, nil
}

func (m *Manager) installCaddyAutosave(host string, autosave []byte) error {
	if !json.Valid(autosave) {
		return fmt.Errorf("refusing to install invalid Caddy autosave JSON")
	}
	tempName := fmt.Sprintf("caddy-autosave-%d.json", time.Now().UnixNano())
	tempFile := state.ConfigFile(m.user, tempName)
	tempFileQuoted := state.ConfigFileQuoted(m.user, tempName)
	defer func() {
		_, _ = m.sshClient.Execute(host, fmt.Sprintf("rm -f %s", tempFileQuoted)) // safe: tempFileQuoted comes from state.ConfigFileQuoted
	}()
	writeCommand := fmt.Sprintf("umask 077 && mkdir -p %s && chmod 700 %s && cat > %s && chmod 600 %s",
		state.DirQuoted(m.user), state.DirQuoted(m.user), tempFileQuoted, tempFileQuoted)
	result, err := m.sshClient.ExecuteWithStdin(host, writeCommand, bytes.NewReader(autosave))
	if err != nil {
		return fmt.Errorf("failed to stage normalized Caddy autosave: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to stage normalized Caddy autosave: %s", strings.TrimSpace(result.Stderr))
	}

	_, err = m.podman.Run(host, &podman.ContainerConfig{
		Image:      CaddyImage,
		Entrypoint: "/bin/sh",
		Command:    []string{"-c", caddyAutosaveInstallCommand()},
		Volumes: []string{
			"caddy_config:/config",
			tempFile + ":/azud-autosave.json:ro",
		},
		ReadOnly:        true,
		CapDrop:         []string{"all"},
		NoNewPrivileges: true,
		Remove:          true,
	})
	if err != nil {
		return fmt.Errorf("failed to install normalized Caddy autosave: %w", err)
	}
	return nil
}

func caddyAutosaveInstallCommand() string {
	return "set -eu; mkdir -p /config/caddy; " +
		"tmp=/config/caddy/autosave.json.azud-tmp; " +
		"trap 'rm -f \"$tmp\"' EXIT HUP INT TERM; " +
		"cp /azud-autosave.json \"$tmp\"; chmod 600 \"$tmp\"; " +
		"mv -f \"$tmp\" /config/caddy/autosave.json; trap - EXIT"
}

func (m *Manager) containerUsesHostNetwork(host, container string) (bool, error) {
	raw, err := m.podman.Inspect(host, container)
	if err != nil {
		return false, err
	}

	var payload []struct {
		HostConfig struct {
			NetworkMode string `json:"NetworkMode"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return false, err
	}
	if len(payload) == 0 {
		return false, fmt.Errorf("empty inspect result")
	}

	if payload[0].HostConfig.NetworkMode == "host" {
		return true, nil
	}
	if _, ok := payload[0].NetworkSettings.Networks["host"]; ok {
		return true, nil
	}
	return false, nil
}

// persistConfig fetches the current Caddy config from the admin API and
// writes it to CaddyConfigFile on the remote host. Returns an error if
// persistence fails so callers can surface warnings to users.
func (m *Manager) persistConfig(host string) error {
	data, err := m.caddyClient.apiRequest(host, "GET", "/config/", nil)
	if err != nil {
		return fmt.Errorf("failed to GET config from Caddy API: %w", err)
	}

	if !json.Valid(data) {
		return fmt.Errorf("invalid JSON from Caddy admin API")
	}

	// Write atomically via a temp file to avoid partial writes on failure.
	// Note: paths are pre-quoted with ${HOME} expansion support for non-root users.
	cmd := persistConfigCommand(m.user)
	result, err := m.sshClient.ExecuteWithStdin(host, cmd, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write command failed: %s", result.Stderr)
	}
	m.log.Debug("persistConfig: saved protected Caddy state")
	return nil
}

func persistConfigCommand(user string) string {
	configDir := state.DirQuoted(user)
	configFile := state.ConfigFileQuoted(user, CaddyConfigFileName)
	tmpFile := state.ConfigFileQuoted(user, CaddyConfigFileName+".tmp")
	return fmt.Sprintf("umask 077 && mkdir -p %s && chmod 700 %s && cat > %s && chmod 600 %s && mv %s %s && chmod 600 %s", // safe: all values come from state.*Quoted
		configDir, configDir, tmpFile, tmpFile, tmpFile, configFile, configFile)
}

// restoreConfig reads a previously persisted Caddy config from CaddyConfigFile
// on the remote host and loads it into Caddy via the admin API. Returns an
// error so callers can decide whether to warn or fail.
func (m *Manager) restoreConfig(host string) error {
	cmd := restoreConfigCommand(m.user)
	result, err := m.sshClient.Execute(host, cmd)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("config file does not exist")
	}

	data := []byte(result.Stdout)
	if !json.Valid(data) {
		return fmt.Errorf("persisted config is invalid JSON")
	}

	if err := m.caddyClient.LoadRawConfig(host, data); err != nil {
		return fmt.Errorf("failed to load persisted config: %w", err)
	}

	m.log.Debug("restoreConfig: restored protected Caddy state")
	return nil
}

func restoreConfigCommand(user string) string {
	configDir := state.DirQuoted(user)
	configFile := state.ConfigFileQuoted(user, CaddyConfigFileName)
	return fmt.Sprintf("test -f %s && chmod 700 %s && chmod 600 %s && cat %s", configFile, configDir, configFile, configFile) // safe: all values come from state.*Quoted
}

type ProxyConfig struct {
	// Hosts to route to (e.g., myapp.example.com)
	Hosts []string

	// Enable automatic HTTPS via Let's Encrypt
	AutoHTTPS bool

	// Email for Let's Encrypt notifications
	Email string

	// Use Let's Encrypt staging CA (for testing, avoids rate limits)
	Staging bool

	// Redirect HTTP to HTTPS
	SSLRedirect bool

	// Admin API listen address (default: localhost:2019)
	AdminListen string

	// HTTP port (default 80)
	HTTPPort int

	// HTTPS port (default 443)
	HTTPSPort int

	// Custom SSL certificate PEM content (if provided, disables ACME)
	SSLCertificate string

	// Custom SSL private key PEM content
	SSLPrivateKey string

	// Require valid custom certificate material instead of falling back to ACME.
	CustomCertificate bool

	// Maximum client request header size in bytes.
	MaxHeaderBytes int

	// Enable concurrent HTTP/1 request reads and response writes.
	EnableFullDuplex bool

	// Enable access logging (even without header redaction)
	LoggingEnabled bool

	// Request headers to redact from access logs
	RedactRequestHeaders []string

	// Response headers to redact from access logs
	RedactResponseHeaders []string
}

// Boot starts the Caddy proxy on a host
func (m *Manager) Boot(host string, config *ProxyConfig) error {
	if err := validateProxyConfig(config); err != nil {
		return err
	}
	m.proxyConfig = config
	m.log.Host(host, "Starting proxy...")
	if err := m.ensureRootfulAccess(host); err != nil {
		return err
	}

	running, err := m.podman.IsRunning(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to check proxy status: %w", err)
	}
	exists, err := m.podman.Exists(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to check proxy container: %w", err)
	}

	// Mixed mode requires host networking so Caddy can reach app host ports
	// on 127.0.0.1. Recreate existing proxy containers that use bridge mode.
	if m.hostPorts && exists {
		hostNet, inspectErr := m.containerUsesHostNetwork(host, CaddyContainerName)
		if inspectErr != nil {
			return fmt.Errorf("failed to inspect proxy network mode on %s: %w", host, inspectErr)
		}
		if !hostNet {
			m.log.Host(host, "Recreating proxy container for mixed rootful/rootless mode...")
			if running {
				_ = m.podman.Stop(host, CaddyContainerName, 30)
			}
			if err := m.podman.Remove(host, CaddyContainerName, true); err != nil {
				return fmt.Errorf("failed to recreate proxy container: %w", err)
			}
			running = false
			exists = false
		}
	}

	// Older proxy containers restart from the stock Caddyfile and serve its
	// catch-all file route until Azud reconnects. Recreate them once so Caddy
	// resumes its autosaved live JSON before accepting traffic after a restart.
	if exists {
		usesResume, inspectErr := m.containerUsesResume(host, CaddyContainerName)
		if inspectErr != nil {
			return fmt.Errorf("failed to inspect proxy restart command on %s: %w", host, inspectErr)
		}
		if !usesResume {
			m.log.Host(host, "Recreating proxy container with Caddy config resume enabled...")
			if running {
				_ = m.podman.Stop(host, CaddyContainerName, 30)
			}
			if err := m.podman.Remove(host, CaddyContainerName, true); err != nil {
				return fmt.Errorf("failed to recreate proxy container: %w", err)
			}
			running = false
			exists = false
		}
	}

	if running {
		if _, err := m.caddyClient.apiRequest(host, "GET", "/config/", nil); err != nil {
			// Caddy may be healthy on the data plane but unreachable at Azud's
			// configured admin endpoint after an out-of-band listener change.
			// Stop it before reading and normalizing the authoritative autosave.
			m.log.Host(host, "Recovering proxy admin access from autosaved state...")
			if stopErr := m.podman.Stop(host, CaddyContainerName, 30); stopErr != nil {
				return fmt.Errorf("failed to stop proxy for admin recovery: %w", stopErr)
			}
		} else {
			m.log.Host(host, "Proxy already running")
			// Apply TLS/ACME and logging settings from deploy.yml while
			// preserving existing routes so that registered services are
			// not wiped out.
			if config != nil {
				if err := m.withPersistedMutation(host, func() error {
					return m.applyConfigPreservingRoutes(host, config)
				}); err != nil {
					return fmt.Errorf("failed to apply proxy config: %w", err)
				}
			}
			return nil
		}
	}

	if exists {
		autosave, hasAutosave, err := m.normalizedCaddyAutosave(host)
		if err != nil {
			return err
		}
		if hasAutosave {
			if err := m.installCaddyAutosave(host, autosave); err != nil {
				return err
			}
		}
		if err := m.podman.Start(host, CaddyContainerName); err != nil {
			return fmt.Errorf("failed to start proxy: %w", err)
		}
		if err := m.waitForAdminAPI(host, 10*time.Second); err != nil {
			return err
		}
		if err := m.finalizeProxyStartup(host, config, hasAutosave); err != nil {
			return err
		}
		m.log.HostSuccess(host, "Proxy started")
		return nil
	}

	httpPort := CaddyHTTPPort
	httpsPort := CaddyHTTPSPort
	if config != nil && config.HTTPPort > 0 {
		httpPort = config.HTTPPort
	}
	if config != nil && config.HTTPSPort > 0 {
		httpsPort = config.HTTPSPort
	}

	autosave, hasAutosave, err := m.normalizedCaddyAutosave(host)
	if err != nil {
		return err
	}
	if hasAutosave {
		if err := m.installCaddyAutosave(host, autosave); err != nil {
			return err
		}
	}

	containerConfig := &podman.ContainerConfig{
		Name:    CaddyContainerName,
		Image:   CaddyImage,
		Detach:  true,
		Restart: "unless-stopped",
		Volumes: []string{
			"caddy_data:/data",
			"caddy_config:/config",
		},
		Labels: map[string]string{
			"azud.managed": "true",
			"azud.type":    "proxy",
		},
		Command: []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile", "--resume"},
		Env: map[string]string{
			"CADDY_ADMIN": m.adminListen(),
		},
	}
	if m.hostPorts {
		containerConfig.Network = "host"
	} else {
		containerConfig.Network = "azud"
		containerConfig.Ports = []string{
			fmt.Sprintf("%d:%d", httpPort, 80),
			fmt.Sprintf("%d:%d", httpsPort, 443),
			fmt.Sprintf("127.0.0.1:%d:%d", CaddyAdminPort, CaddyAdminPort),
		}
	}

	_, err = m.podman.Run(host, containerConfig)
	if err != nil {
		return fmt.Errorf("failed to start proxy container: %w", err)
	}

	// Wait for Caddy admin API to be ready
	if err := m.waitForAdminAPI(host, 10*time.Second); err != nil {
		return err
	}

	if err := m.finalizeProxyStartup(host, config, hasAutosave); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Proxy started")
	return nil
}

func (m *Manager) finalizeProxyStartup(host string, config *ProxyConfig, hasAutosave bool) error {
	if hasAutosave {
		// --resume selected the authoritative autosave. Never overwrite it with
		// Azud's potentially older protected bootstrap copy.
		if config != nil {
			if err := m.withPersistedMutation(host, func() error {
				return m.applyConfigPreservingRoutes(host, config)
			}); err != nil {
				return fmt.Errorf("failed to apply proxy config after resume: %w", err)
			}
			return nil
		}
		if err := m.withCaddyLock(host, func() error { return m.persistConfig(host) }); err != nil {
			return fmt.Errorf("failed to persist resumed proxy config: %w", err)
		}
		return nil
	}

	// No autosave existed before startup, so Caddy used the stock fallback.
	// Restore Azud's protected bootstrap copy or initialize a minimal config.
	if err := m.withPersistedMutation(host, func() error {
		if err := m.restoreConfig(host); err != nil {
			return m.loadInitialConfig(host, config)
		}
		if config != nil {
			return m.applyConfigPreservingRoutes(host, config)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to load initial proxy config: %w", err)
	}
	return nil
}

func (m *Manager) loadInitialConfig(host string, config *ProxyConfig) error {
	if err := validateProxyConfig(config); err != nil {
		return err
	}
	caddyConfig := m.buildBaseConfig()
	m.applyProxySettingsFrom(caddyConfig, config)

	if config != nil && config.SSLCertificate != "" {
		m.log.Host(host, "Configuring custom SSL certificates...")
	}

	return m.caddyClient.LoadConfig(host, caddyConfig)
}

// ensureHTTPServer ensures the Caddy config has a valid Apps.HTTP.Servers["srv0"]
// structure, creating it if missing.
func ensureHTTPServer(caddyConfig *CaddyConfig) {
	if caddyConfig.Apps == nil {
		caddyConfig.Apps = &AppsConfig{}
	}
	if caddyConfig.Apps.HTTP == nil {
		caddyConfig.Apps.HTTP = &HTTPApp{Servers: make(map[string]*HTTPServer)}
	}
	if caddyConfig.Apps.HTTP.Servers["srv0"] == nil {
		caddyConfig.Apps.HTTP.Servers["srv0"] = &HTTPServer{
			// Default to plaintext only until the desired TLS mode is applied.
			// Listening with plaintext on :443 produces invalid TLS responses.
			Listen: []string{":80"},
			Routes: []*Route{},
		}
	}
}

// applyConfigPreservingRoutes fetches the current Caddy config, applies
// TLS/ACME and logging settings from the given ProxyConfig, and reloads
// it. Existing routes (registered services) are preserved.
func (m *Manager) applyConfigPreservingRoutes(host string, config *ProxyConfig) error {
	if err := validateProxyConfig(config); err != nil {
		return err
	}
	data, err := m.caddyClient.apiRequest(host, "GET", "/config/", nil)
	if err != nil {
		return fmt.Errorf("failed to read running Caddy config: %w", err)
	}

	currentRaw, err := decodeJSONObject(data)
	if err != nil {
		return fmt.Errorf("failed to parse running Caddy config: %w", err)
	}
	desired := m.buildBaseConfig()
	m.applyProxySettingsFrom(desired, config)

	desiredData, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("failed to encode desired Caddy settings: %w", err)
	}
	var desiredRaw map[string]interface{}
	if err := json.Unmarshal(desiredData, &desiredRaw); err != nil {
		return fmt.Errorf("failed to parse desired Caddy settings: %w", err)
	}
	if err := copyManagedProxySettings(currentRaw, desiredRaw); err != nil {
		return err
	}

	if config.SSLCertificate != "" {
		m.log.Host(host, "Configuring custom SSL certificates...")
	}

	updated, err := json.Marshal(currentRaw)
	if err != nil {
		return fmt.Errorf("failed to encode updated Caddy config: %w", err)
	}
	return m.caddyClient.LoadRawConfig(host, updated)
}

func decodeJSONObject(data []byte) (map[string]interface{}, error) {
	value, err := decodeJSONValue(data)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]interface{})
	if !ok || object == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	return object, nil
}

func decodeJSONValue(data []byte) (interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func copyManagedProxySettings(current, desired map[string]interface{}) error {
	currentApps, err := nestedJSONObject(current, "apps")
	if err != nil {
		return err
	}
	desiredApps, err := nestedJSONObject(desired, "apps")
	if err != nil {
		return err
	}
	currentServer, err := nestedJSONObject(currentApps, "http", "servers", "srv0")
	if err != nil {
		return err
	}
	desiredServer, err := nestedJSONObject(desiredApps, "http", "servers", "srv0")
	if err != nil {
		return err
	}

	for _, key := range []string{"listen", "automatic_https", "logs", "max_header_bytes", "enable_full_duplex"} {
		copyOptionalJSONField(currentServer, desiredServer, key)
	}
	copyOptionalJSONField(current, desired, "logging")
	copyOptionalJSONField(currentApps, desiredApps, "tls")
	return nil
}

func nestedJSONObject(root map[string]interface{}, path ...string) (map[string]interface{}, error) {
	current := root
	for _, key := range path {
		value, ok := current[key]
		if !ok {
			return nil, fmt.Errorf("running Caddy config is missing %s", strings.Join(path, "."))
		}
		next, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("running Caddy config field %s is not an object", strings.Join(path, "."))
		}
		current = next
	}
	return current, nil
}

func copyOptionalJSONField(destination, source map[string]interface{}, key string) {
	if value, ok := source[key]; ok {
		destination[key] = value
	} else {
		delete(destination, key)
	}
}

// buildBaseConfig creates a minimal Caddy config with admin and HTTP server.
func (m *Manager) buildBaseConfig() *CaddyConfig {
	return &CaddyConfig{
		Admin: &AdminConfig{
			Listen: m.adminListen(),
		},
		Apps: &AppsConfig{
			HTTP: &HTTPApp{
				Servers: map[string]*HTTPServer{
					"srv0": {
						// applyProxySettings replaces this safe plaintext
						// default with the listener set for the desired mode.
						Listen: []string{":80"},
						Routes: []*Route{},
					},
				},
			},
		},
	}
}

// applyProxySettingsFrom applies TLS, AutoHTTPS, and logging settings from the
// given ProxyConfig onto a Caddy config. Accepts an explicit config to avoid
// temporary field swaps on the Manager.
func (m *Manager) applyProxySettingsFrom(caddyConfig *CaddyConfig, config *ProxyConfig) {
	if config == nil {
		return
	}

	ensureHTTPServer(caddyConfig)
	server := caddyConfig.Apps.HTTP.Servers["srv0"]
	server.Listen = proxyListenAddresses(config)
	server.MaxHeaderBytes = config.MaxHeaderBytes
	server.EnableFullDuplex = config.EnableFullDuplex

	switch {
	case !config.AutoHTTPS:
		server.AutoHTTPS = &AutoHTTPSConfig{Disable: true}
	case !config.SSLRedirect:
		server.AutoHTTPS = &AutoHTTPSConfig{
			DisableRedirects: true,
		}
	default:
		server.AutoHTTPS = nil
	}

	if config.LoggingEnabled || len(config.RedactRequestHeaders) > 0 || len(config.RedactResponseHeaders) > 0 {
		loggerName := "access"
		encoder := &Encoder{Format: "json"}

		// Wrap the encoder with a filter if any headers need redacting.
		if len(config.RedactRequestHeaders) > 0 || len(config.RedactResponseHeaders) > 0 {
			fields := make(map[string]*Filter)
			for _, header := range config.RedactRequestHeaders {
				header = strings.TrimSpace(header)
				if header != "" && isValidHeaderName(header) {
					fields[fmt.Sprintf("request>headers>%s", header)] = &Filter{Filter: "delete"}
				}
			}
			for _, header := range config.RedactResponseHeaders {
				header = strings.TrimSpace(header)
				if header != "" && isValidHeaderName(header) {
					fields[fmt.Sprintf("resp_headers>%s", header)] = &Filter{Filter: "delete"}
				}
			}
			encoder = &Encoder{
				Format: "filter",
				Wrap:   &Encoder{Format: "json"},
				Fields: fields,
			}
		}

		caddyConfig.Logging = &LoggingConfig{
			Logs: map[string]*Log{
				loggerName: {
					Level:   "INFO",
					Writer:  &Writer{Output: "stdout"},
					Encoder: encoder,
				},
			},
		}
		server.Logs = &ServerLogs{DefaultLoggerName: loggerName}
	} else {
		caddyConfig.Logging = nil
		server.Logs = nil
	}

	// Clear previously managed TLS material before applying the desired mode.
	caddyConfig.Apps.TLS = nil
	if config.SSLCertificate != "" && config.SSLPrivateKey != "" {
		// Loaded PEM certificates are the only permitted certificate source in
		// custom TLS mode. Disable automatic issuance explicitly; an empty issuer
		// list is omitted from JSON and would otherwise fall back to Caddy defaults.
		if server.AutoHTTPS == nil {
			server.AutoHTTPS = &AutoHTTPSConfig{}
		}
		server.AutoHTTPS.DisableCertificates = true
		caddyConfig.Apps.TLS = &TLSApp{
			Certificates: &CertificatesConfig{
				LoadPEM: []LoadedCertificate{
					{
						Certificate: config.SSLCertificate,
						Key:         config.SSLPrivateKey,
					},
				},
			},
		}
	} else if config.AutoHTTPS && config.Email != "" {
		issuer := &Issuer{
			Module: "acme",
			Email:  config.Email,
		}
		if config.Staging {
			issuer.CA = "https://acme-staging-v02.api.letsencrypt.org/directory"
		}

		caddyConfig.Apps.TLS = &TLSApp{
			Automation: &TLSAutomation{
				Policies: []*TLSPolicy{
					{
						Issuers: []*Issuer{issuer},
					},
				},
			},
		}
	}
}

// proxyListenAddresses keeps plaintext and TLS traffic on distinct listeners
// whenever Caddy owns the HTTP-to-HTTPS redirect. A TLS-only application server
// lets Caddy synthesize its dedicated port-80 redirect server. When redirects
// are explicitly disabled, both listeners remain attached to the application
// server so the service is intentionally reachable over HTTP and HTTPS.
func proxyListenAddresses(config *ProxyConfig) []string {
	switch {
	case config == nil || !config.AutoHTTPS:
		return []string{":80"}
	case config.SSLRedirect:
		return []string{":443"}
	default:
		return []string{":80", ":443"}
	}
}

func validateProxyConfig(config *ProxyConfig) error {
	if config == nil || !config.CustomCertificate {
		return nil
	}
	if strings.TrimSpace(config.SSLCertificate) == "" || strings.TrimSpace(config.SSLPrivateKey) == "" {
		return fmt.Errorf("custom SSL certificate secrets are missing or empty")
	}
	if _, err := tls.X509KeyPair([]byte(config.SSLCertificate), []byte(config.SSLPrivateKey)); err != nil {
		return fmt.Errorf("invalid custom SSL certificate or private key: %w", err)
	}
	return nil
}

// Stop stops the Caddy proxy on a host
func (m *Manager) Stop(host string) error {
	m.log.Host(host, "Stopping proxy...")
	if err := m.ensureRootfulAccess(host); err != nil {
		return err
	}

	if err := m.podman.Stop(host, CaddyContainerName, 30); err != nil {
		return fmt.Errorf("failed to stop proxy: %w", err)
	}

	m.log.HostSuccess(host, "Proxy stopped")
	return nil
}

// Reboot restarts the Caddy proxy. If config is provided, it is applied
// after restart so that TLS/ACME changes take effect immediately.
func (m *Manager) Reboot(host string, config *ProxyConfig) error {
	if err := validateProxyConfig(config); err != nil {
		return err
	}
	m.log.Host(host, "Rebooting proxy...")
	if err := m.ensureRootfulAccess(host); err != nil {
		return err
	}

	exists, err := m.podman.Exists(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to check proxy container: %w", err)
	}
	if !exists {
		return fmt.Errorf("proxy container does not exist")
	}
	usesResume, err := m.containerUsesResume(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to inspect proxy restart command on %s: %w", host, err)
	}
	if !usesResume {
		m.log.Host(host, "Migrating proxy container to Caddy config resume before reboot...")
		_ = m.podman.Stop(host, CaddyContainerName, 30)
		if err := m.podman.Remove(host, CaddyContainerName, true); err != nil {
			return fmt.Errorf("failed to migrate proxy container: %w", err)
		}
		return m.Boot(host, config)
	}

	running, err := m.podman.IsRunning(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to check proxy status: %w", err)
	}
	if running {
		if err := m.podman.Stop(host, CaddyContainerName, 30); err != nil {
			return fmt.Errorf("failed to stop proxy for reboot: %w", err)
		}
	}
	// Snapshot only after Caddy has stopped and completed its final autosave,
	// so no concurrent shutdown write can be lost when admin.listen is repaired.
	autosave, hasAutosave, err := m.normalizedCaddyAutosave(host)
	if err != nil {
		return err
	}
	if hasAutosave {
		if err := m.installCaddyAutosave(host, autosave); err != nil {
			return err
		}
	}
	if err := m.podman.Start(host, CaddyContainerName); err != nil {
		return fmt.Errorf("failed to start proxy after reboot: %w", err)
	}
	if err := m.waitForAdminAPI(host, 10*time.Second); err != nil {
		return err
	}
	if err := m.finalizeProxyStartup(host, config, hasAutosave); err != nil {
		return fmt.Errorf("failed to finalize proxy reboot: %w", err)
	}

	m.log.HostSuccess(host, "Proxy rebooted")
	return nil
}

// Remove removes the Caddy proxy container
func (m *Manager) Remove(host string) error {
	m.log.Host(host, "Removing proxy...")
	if err := m.ensureRootfulAccess(host); err != nil {
		return err
	}

	if err := m.podman.Remove(host, CaddyContainerName, true); err != nil {
		return fmt.Errorf("failed to remove proxy: %w", err)
	}

	// The persisted JSON may contain private key material. Treat failure to
	// remove it as a command failure instead of claiming the proxy was fully
	// removed while sensitive state remains behind.
	rmCmd := fmt.Sprintf("rm -f %s", state.ConfigFileQuoted(m.user, CaddyConfigFileName))
	result, err := m.sshClient.Execute(host, rmCmd)
	if err != nil {
		return fmt.Errorf("proxy container removed but persisted config cleanup failed: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("proxy container removed but persisted config cleanup failed: %s", strings.TrimSpace(result.Stderr))
	}

	m.log.HostSuccess(host, "Proxy removed")
	return nil
}

// Status returns the proxy status on a host
func (m *Manager) Status(host string) (*ProxyStatus, error) {
	status := &ProxyStatus{Host: host}
	if err := m.ensureRootfulAccess(host); err != nil {
		return nil, err
	}

	running, err := m.podman.IsRunning(host, CaddyContainerName)
	if err != nil {
		return nil, err
	}
	status.Running = running

	if running {
		// Get container stats
		stats, err := m.podman.Stats(host, CaddyContainerName)
		if err == nil {
			status.Stats = stats
		}

		// Try to get config info
		config, err := m.caddyClient.GetConfig(host)
		if err == nil && config.Apps != nil && config.Apps.HTTP != nil {
			for _, server := range config.Apps.HTTP.Servers {
				status.RouteCount += len(server.Routes)
			}
		}
	}

	return status, nil
}

type ProxyStatus struct {
	Host       string
	Running    bool
	Stats      string
	RouteCount int
}

// Logs retrieves proxy logs
func (m *Manager) Logs(host string, follow bool, tail string) (*ssh.Result, error) {
	if err := m.ensureRootfulAccess(host); err != nil {
		return nil, err
	}
	logsConfig := &podman.LogsConfig{
		Container: CaddyContainerName,
		Follow:    follow,
		Tail:      tail,
	}
	return m.podman.Logs(host, logsConfig)
}

// LogsStream follows proxy logs without buffering an unbounded stream in memory.
func (m *Manager) LogsStream(host string, follow bool, tail string, stdout, stderr io.Writer) error {
	if err := m.ensureRootfulAccess(host); err != nil {
		return err
	}
	logsConfig := &podman.LogsConfig{
		Container: CaddyContainerName,
		Follow:    follow,
		Tail:      tail,
	}
	return m.podman.LogsStream(host, logsConfig, stdout, stderr)
}

// RegisterService registers a service with the proxy using route-specific
// API operations. If a route for the service host already exists, only that
// route is updated via a PATCH. Otherwise a new route is appended. This
// avoids replacing the entire Caddy configuration and prevents race
// conditions when multiple services are deployed concurrently.
func (m *Manager) RegisterService(host string, service *ServiceConfig) error {
	m.log.Host(host, "Registering service %s...", service.Name)
	if service.Host == "" && len(service.Hosts) > 0 {
		service.Host = service.Hosts[0]
	}

	// Build the route for this service
	route := m.buildServiceRoute(service)

	if err := m.withPersistedMutation(host, func() error {
		if err := m.upsertRoute(host, service.Host, route); err != nil {
			return fmt.Errorf("failed to update owned service route without replacing Caddy config: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Service %s registered", service.Name)
	return nil
}

// EnsureServiceHosts synchronizes an existing Azud route's host matchers and
// streaming options without replacing its upstreams or sibling handlers.
func (m *Manager) EnsureServiceHosts(host string, service *ServiceConfig) error {
	if service.Host == "" && len(service.Hosts) > 0 {
		service.Host = service.Hosts[0]
	}
	desired := m.buildServiceRoute(service)

	return m.withPersistedMutation(host, func() error {
		routesPath := "/config/apps/http/servers/srv0/routes"
		data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
		if err != nil {
			return fmt.Errorf("failed to read Caddy routes: %w", err)
		}
		rawRoutes, routes, err := parseRouteIdentities(data)
		if err != nil {
			return err
		}
		if err := ensureNoForeignHostOwner(routes, desired); err != nil {
			return err
		}

		desiredHandler, _, ok := reverseProxyHandler(desired)
		if !ok {
			return fmt.Errorf("desired service route has no reverse_proxy handler")
		}
		for i, route := range routes {
			if !routesHaveSameOwner(route, desired, service.Host) {
				continue
			}
			if !reflect.DeepEqual(route.Match, desired.Match) {
				if _, err := m.caddyClient.apiRequest(host, "PATCH", routeAPIPath(routesPath, i, route)+"/match", desired.Match); err != nil {
					return fmt.Errorf("failed to patch service hosts: %w", err)
				}
			}
			handler, handlerIndex, err := parseReverseProxyIdentity(rawRoutes[i])
			if err != nil {
				return err
			}
			rawRoute, err := decodeJSONObject(rawRoutes[i])
			if err != nil {
				return err
			}
			rawHandlers, err := jsonObjectSlice(rawRoute["handle"])
			if err != nil || handlerIndex >= len(rawHandlers) {
				return fmt.Errorf("failed to locate owned reverse_proxy handler")
			}
			rawHandler := rawHandlers[handlerIndex]
			handlerPath := handlerAPIPath(routesPath, i, handlerIndex, handler)
			_, flushPresent := rawHandler["flush_interval"]
			if err := m.reconcileStringField(host, handlerPath+"/flush_interval", handler.FlushInterval, desiredHandler.FlushInterval, flushPresent); err != nil {
				return fmt.Errorf("failed to reconcile flush_interval: %w", err)
			}
			_, closeDelayPresent := rawHandler["stream_close_delay"]
			if err := m.reconcileStringField(host, handlerPath+"/stream_close_delay", handler.StreamCloseDelay, desiredHandler.StreamCloseDelay, closeDelayPresent); err != nil {
				return fmt.Errorf("failed to reconcile stream_close_delay: %w", err)
			}
			return nil
		}

		return nil
	})
}

func (m *Manager) reconcileStringField(host, path, actual, desired string, present bool) error {
	method, body, mutate := planStringFieldMutation(actual, desired, present)
	if !mutate {
		return nil
	}
	_, err := m.caddyClient.apiRequest(host, method, path, body)
	return err
}

func planStringFieldMutation(actual, desired string, present bool) (string, interface{}, bool) {
	if desired == "" {
		if !present {
			return "", nil, false
		}
		return "DELETE", nil, true
	}
	if present && actual == desired {
		return "", nil, false
	}
	if present {
		return "PATCH", desired, true
	}
	return "PUT", desired, true
}

// planServiceHostPatch returns a targeted matcher patch for an owned route.
// An empty path means the route is missing or already current.
func planServiceHostPatch(routes []*Route, desired *Route, fallbackHost string) (string, []*Match, error) {
	if err := ensureNoForeignHostOwner(routes, desired); err != nil {
		return "", nil, err
	}

	for i, route := range routes {
		if !routesHaveSameOwner(route, desired, fallbackHost) {
			continue
		}
		if reflect.DeepEqual(route.Match, desired.Match) {
			return "", nil, nil
		}
		return routeAPIPath("/config/apps/http/servers/srv0/routes", i, route) + "/match", desired.Match, nil
	}

	return "", nil, nil
}

// upsertRoute updates an existing route for serviceHost or appends a new one
// using Caddy's path-specific admin API.
func (m *Manager) upsertRoute(host, serviceHost string, route *Route) error {
	// Get only the routes array, not the full config
	routesPath := "/config/apps/http/servers/srv0/routes"
	data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
	if err != nil {
		return fmt.Errorf("failed to get routes: %w", err)
	}

	rawRoutes, routes, err := parseRouteIdentities(data)
	if err != nil {
		return err
	}
	if err := ensureNoForeignHostOwner(routes, route); err != nil {
		return err
	}

	for i, existing := range routes {
		if existing == nil || !routesHaveSameOwner(existing, route, serviceHost) {
			continue
		}
		merged, err := mergeOwnedRoute(rawRoutes[i], route)
		if err != nil {
			return err
		}
		if _, err := m.caddyClient.apiRequest(host, "PATCH", routeAPIPath(routesPath, i, existing), merged); err != nil {
			return fmt.Errorf("failed to update owned route: %w", err)
		}
		return nil
	}

	// Route doesn't exist — POST appends one Azud-owned route without replacing
	// routes or handlers owned by other services and manual operators.
	_, err = m.caddyClient.apiRequest(host, "POST", routesPath, routeAppendPayload(routes, route))
	if err != nil {
		return fmt.Errorf("failed to append route: %w", err)
	}

	return nil
}

func parseRouteIdentities(data []byte) ([]json.RawMessage, []*Route, error) {
	var rawRoutes []json.RawMessage
	if err := json.Unmarshal(data, &rawRoutes); err != nil {
		return nil, nil, fmt.Errorf("failed to parse routes: %w", err)
	}
	var routes []*Route
	if rawRoutes != nil {
		routes = make([]*Route, 0, len(rawRoutes))
	}
	for _, raw := range rawRoutes {
		var identity struct {
			ID    string   `json:"@id,omitempty"`
			Match []*Match `json:"match,omitempty"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil {
			return nil, nil, fmt.Errorf("failed to parse route identity: %w", err)
		}
		routes = append(routes, &Route{ID: identity.ID, Match: identity.Match})
	}
	return rawRoutes, routes, nil
}

func parseReverseProxyIdentity(rawRoute json.RawMessage) (*Handler, int, error) {
	var route struct {
		Handle []json.RawMessage `json:"handle,omitempty"`
	}
	if err := json.Unmarshal(rawRoute, &route); err != nil {
		return nil, -1, fmt.Errorf("failed to parse owned route handlers: %w", err)
	}
	for index, rawHandler := range route.Handle {
		var identity struct {
			Handler string `json:"handler"`
		}
		if err := json.Unmarshal(rawHandler, &identity); err != nil {
			return nil, -1, fmt.Errorf("failed to parse handler identity: %w", err)
		}
		if identity.Handler != "reverse_proxy" {
			continue
		}
		var handler Handler
		if err := json.Unmarshal(rawHandler, &handler); err != nil {
			return nil, -1, fmt.Errorf("failed to parse owned reverse_proxy handler: %w", err)
		}
		return &handler, index, nil
	}
	return nil, -1, fmt.Errorf("owned service route has no reverse_proxy handler")
}

var reverseProxyManagedSchema = map[string]interface{}{
	"@id": nil, "handler": nil, "flush_interval": nil, "stream_close_delay": nil,
	"buffer_requests": nil, "buffer_responses": nil,
	"load_balancing": map[string]interface{}{
		"selection_policy": map[string]interface{}{"policy": nil},
		"try_duration":     nil, "try_interval": nil,
	},
	"health_checks": map[string]interface{}{
		"active": map[string]interface{}{
			"path": nil, "port": nil, "interval": nil, "timeout": nil, "headers": nil,
		},
		"passive": map[string]interface{}{
			"fail_duration": nil, "max_fails": nil, "unhealthy_latency": nil,
		},
	},
	"transport": map[string]interface{}{
		"protocol": nil, "response_header_timeout": nil, "read_timeout": nil,
		"versions": nil, "tls": map[string]interface{}{},
	},
	"headers": map[string]interface{}{
		"request":  map[string]interface{}{"set": nil, "add": nil, "delete": nil},
		"response": map[string]interface{}{"set": nil, "add": nil, "delete": nil},
	},
}

var requestBodyManagedSchema = map[string]interface{}{
	"@id": nil, "handler": nil, "max_size": nil,
}

// mergeOwnedRoute overlays every field Azud owns while preserving arbitrary
// route fields, sibling handlers, nested module settings, and upstream fields
// that Azud does not model.
func mergeOwnedRoute(rawRoute json.RawMessage, desired *Route) (map[string]interface{}, error) {
	existing, err := decodeJSONObject(rawRoute)
	if err != nil {
		return nil, fmt.Errorf("failed to decode owned route: %w", err)
	}
	desiredData, err := json.Marshal(desired)
	if err != nil {
		return nil, fmt.Errorf("failed to encode desired route: %w", err)
	}
	desiredObject, err := decodeJSONObject(desiredData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode desired route: %w", err)
	}
	for _, key := range []string{"@id", "match", "terminal"} {
		copyOptionalJSONField(existing, desiredObject, key)
	}

	existingHandlers, err := jsonObjectSlice(existing["handle"])
	if err != nil {
		return nil, fmt.Errorf("failed to decode owned route handlers: %w", err)
	}
	desiredHandlers, err := jsonObjectSlice(desiredObject["handle"])
	if err != nil {
		return nil, fmt.Errorf("failed to decode desired route handlers: %w", err)
	}
	desiredReverse := findJSONHandler(desiredHandlers, "reverse_proxy")
	if desiredReverse == nil {
		return nil, fmt.Errorf("desired service route has no reverse_proxy handler")
	}
	desiredBody := findJSONHandler(desiredHandlers, "request_body")

	bodyIndex := findJSONHandlerIndex(existingHandlers, "request_body")
	switch {
	case desiredBody != nil && bodyIndex >= 0:
		mergeManagedJSONObject(existingHandlers[bodyIndex], desiredBody, requestBodyManagedSchema)
	case desiredBody != nil:
		reverseIndex := findJSONHandlerIndex(existingHandlers, "reverse_proxy")
		if reverseIndex < 0 {
			reverseIndex = len(existingHandlers)
		}
		existingHandlers = append(existingHandlers, nil)
		copy(existingHandlers[reverseIndex+1:], existingHandlers[reverseIndex:])
		existingHandlers[reverseIndex] = desiredBody
	case desiredBody == nil && bodyIndex >= 0:
		existingHandlers = append(existingHandlers[:bodyIndex], existingHandlers[bodyIndex+1:]...)
	}

	reverseIndex := findJSONHandlerIndex(existingHandlers, "reverse_proxy")
	if reverseIndex < 0 {
		existingHandlers = append(existingHandlers, desiredReverse)
	} else {
		mergeReverseProxyJSON(existingHandlers[reverseIndex], desiredReverse)
	}
	existing["handle"] = existingHandlers
	return existing, nil
}

func mergeReverseProxyJSON(existing, desired map[string]interface{}) {
	mergedUpstreams := mergeRawUpstreamValues(existing["upstreams"], desired["upstreams"])
	mergeManagedJSONObject(existing, desired, reverseProxyManagedSchema)
	if _, ok := desired["upstreams"]; ok {
		existing["upstreams"] = mergedUpstreams
	} else {
		delete(existing, "upstreams")
	}
}

func mergeManagedJSONObject(existing, desired map[string]interface{}, schema map[string]interface{}) {
	for key, child := range schema {
		desiredValue, ok := desired[key]
		if !ok {
			delete(existing, key)
			continue
		}
		childSchema, nested := child.(map[string]interface{})
		if !nested {
			existing[key] = desiredValue
			continue
		}
		desiredChild, desiredIsObject := desiredValue.(map[string]interface{})
		if !desiredIsObject {
			existing[key] = desiredValue
			continue
		}
		existingChild, existingIsObject := existing[key].(map[string]interface{})
		if !existingIsObject {
			existingChild = make(map[string]interface{})
		}
		mergeManagedJSONObject(existingChild, desiredChild, childSchema)
		existing[key] = existingChild
	}
}

func mergeRawUpstreamValues(existingValue, desiredValue interface{}) []interface{} {
	existing, _ := jsonObjectSlice(existingValue)
	desired, _ := jsonObjectSlice(desiredValue)
	byDial := make(map[string][]map[string]interface{})
	for _, upstream := range existing {
		if dial, ok := upstream["dial"].(string); ok {
			byDial[dial] = append(byDial[dial], upstream)
		}
	}
	used := make(map[string]int)
	merged := make([]interface{}, 0, len(desired))
	for _, wanted := range desired {
		dial, _ := wanted["dial"].(string)
		base := make(map[string]interface{})
		candidates := byDial[dial]
		if len(candidates) > 0 {
			index := used[dial]
			if index >= len(candidates) {
				index = len(candidates) - 1
			}
			for key, value := range candidates[index] {
				base[key] = value
			}
			used[dial]++
		}
		base["dial"] = dial
		merged = append(merged, base)
	}
	return merged
}

func jsonObjectSlice(value interface{}) ([]map[string]interface{}, error) {
	if value == nil {
		return nil, nil
	}
	if objects, ok := value.([]map[string]interface{}); ok {
		return objects, nil
	}
	values, ok := value.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected JSON array, got %T", value)
	}
	objects := make([]map[string]interface{}, 0, len(values))
	for _, value := range values {
		object, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("expected JSON object in array, got %T", value)
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func findJSONHandler(handlers []map[string]interface{}, name string) map[string]interface{} {
	index := findJSONHandlerIndex(handlers, name)
	if index < 0 {
		return nil
	}
	return handlers[index]
}

func findJSONHandlerIndex(handlers []map[string]interface{}, name string) int {
	for index, handler := range handlers {
		if handler["handler"] == name {
			return index
		}
	}
	return -1
}

type ServiceConfig struct {
	// Service name
	Name string

	// Hostname for routing
	Host string

	// Additional hostnames for routing
	Hosts []string

	// Upstream addresses (host:port)
	Upstreams []string

	// Optional weighted upstreams. When set, these replace Upstreams and use
	// Caddy's stock random policy with reduced repeated entries.
	UpstreamWeights []UpstreamWeight

	// Protocol used to communicate with upstreams: http, h2c, or https.
	UpstreamProtocol string

	// Health check path for liveness (used by Caddy active health checks)
	HealthPath string

	// Health check interval
	HealthInterval string

	// Health check timeout
	HealthTimeout string

	// Full response timeout (maps to Caddy read_timeout)
	ResponseTimeout string

	// Response header timeout (maps to Caddy response_header_timeout)
	ResponseHeaderTimeout string

	// Response flush interval. Negative values enable low-latency mode.
	FlushInterval string

	// Delay closing streaming connections when Caddy reloads.
	StreamCloseDelay string

	// Forward proxy headers to upstream
	ForwardHeaders bool

	// Buffer request bodies
	BufferRequests bool

	// Buffer responses
	BufferResponses bool

	// Maximum request body size (bytes)
	MaxRequestBody int64

	// Request body buffer size (bytes) when max size is not provided
	BufferMemory int64

	// Enable HTTPS
	HTTPS bool
}

func (m *Manager) buildServiceRoute(service *ServiceConfig) *Route {
	upstreams := make([]*Upstream, len(service.Upstreams))
	for i, addr := range service.Upstreams {
		upstreams[i] = &Upstream{Dial: addr}
	}
	policy := "round_robin"
	if len(service.UpstreamWeights) > 0 {
		upstreams = weightedUpstreams(service.UpstreamWeights...)
		policy = "random"
	}

	handler := &Handler{
		ID:        serviceHandlerID(service.Name),
		Handler:   "reverse_proxy",
		Upstreams: upstreams,
		LoadBalancing: &LoadBalancing{
			SelectionPolicy: &SelectionPolicy{
				Policy: policy,
			},
			// A deploy can leave a sub-second window in which the route has no
			// available upstream, and passive health checking can extend that
			// to a full fail_duration. Retrying briefly turns what would be a
			// hard 503 into a slightly slower response.
			TryDuration: upstreamTryDuration,
			TryInterval: upstreamTryInterval,
		},
	}

	if service.HealthPath != "" {
		interval := service.HealthInterval
		if interval == "" {
			interval = "10s"
		}
		timeout := service.HealthTimeout
		if timeout == "" {
			timeout = "5s"
		}
		activeCheck := &ActiveHealthCheck{
			Path:     service.HealthPath,
			Interval: interval,
			Timeout:  timeout,
		}
		// When HTTPS is enabled, send X-Forwarded-Proto so apps with
		// force_ssl / HSTS don't redirect the health check to HTTPS.
		if service.HTTPS {
			activeCheck.Headers = map[string][]string{
				"X-Forwarded-Proto": {"https"},
			}
		}
		handler.HealthChecks = &HealthChecks{
			Active: activeCheck,
			Passive: &PassiveHealthCheck{
				FailDuration: "30s",
				MaxFails:     3,
			},
		}
	}

	if service.ResponseTimeout != "" || service.ResponseHeaderTimeout != "" || service.UpstreamProtocol != "" {
		transport := &Transport{
			Protocol:              "http",
			ReadTimeout:           service.ResponseTimeout,
			ResponseHeaderTimeout: service.ResponseHeaderTimeout,
		}
		switch service.UpstreamProtocol {
		case "h2c":
			transport.Versions = []string{"h2c", "2"}
		case "https":
			transport.TLS = &UpstreamTLSConfig{}
		}
		handler.Transport = transport
	}

	if service.ForwardHeaders || service.HTTPS {
		handler.Headers = &HeadersConfig{
			Request: &HeaderOps{
				Set: map[string][]string{
					"X-Forwarded-For":   {"{http.request.remote.host}"},
					"X-Forwarded-Proto": {"{http.request.scheme}"},
					"X-Forwarded-Host":  {"{http.request.host}"},
					"X-Forwarded-Port":  {"{http.request.port}"},
					"X-Real-IP":         {"{http.request.remote.host}"},
				},
			},
		}
	}

	handler.FlushInterval = service.FlushInterval
	handler.StreamCloseDelay = service.StreamCloseDelay
	handler.BufferRequests = service.BufferRequests
	handler.BufferResponses = service.BufferResponses

	var handlers []*Handler
	if service.MaxRequestBody > 0 || service.BufferMemory > 0 {
		maxSize := service.MaxRequestBody
		if maxSize == 0 {
			maxSize = service.BufferMemory
		}
		handlers = append(handlers, &Handler{
			Handler: "request_body",
			MaxSize: maxSize,
		})
	}
	handlers = append(handlers, handler)

	hostSet := make(map[string]bool)
	var hostMatches []string
	if service.Host != "" {
		hostSet[service.Host] = true
		hostMatches = append(hostMatches, service.Host)
	}
	for _, host := range service.Hosts {
		if host == "" || hostSet[host] {
			continue
		}
		hostSet[host] = true
		hostMatches = append(hostMatches, host)
	}
	if len(hostMatches) == 0 {
		hostMatches = []string{service.Host}
	}

	route := &Route{
		ID: serviceRouteID(service.Name),
		Match: []*Match{
			{Host: hostMatches},
		},
		Handle:   handlers,
		Terminal: true,
	}

	return route
}

// ReconcileStatus describes the relationship between desired and live route state.
type ReconcileStatus string

const (
	ReconcileMissing ReconcileStatus = "missing"
	ReconcileStale   ReconcileStatus = "stale"
	ReconcileLegacy  ReconcileStatus = "legacy"
	ReconcileInSync  ReconcileStatus = "in-sync"
)

// ReconcileService checks or repairs the single route owned by service.Name.
// A host-only route is considered only as an adoption candidate when upstreams exist.
func (m *Manager) ReconcileService(host string, service *ServiceConfig, repair bool) (ReconcileStatus, error) {
	desired := m.buildServiceRoute(service)
	hasDesiredUpstreams := len(service.Upstreams)+len(service.UpstreamWeights) > 0
	routesPath := "/config/apps/http/servers/srv0/routes"
	data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
	if err != nil {
		return "", err
	}
	rawRoutes, routes, err := parseRouteIdentities(data)
	if err != nil {
		return "", err
	}
	if hasDesiredUpstreams {
		if ownerErr := ensureNoForeignHostOwner(routes, desired); ownerErr != nil {
			return "", ownerErr
		}
	}
	status, _, _ := reconcileRawRouteStatus(rawRoutes, routes, desired, service.Host, hasDesiredUpstreams)
	if !repair || status == ReconcileInSync {
		return status, nil
	}

	err = m.withPersistedMutation(host, func() error {
		data, getErr := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
		if getErr != nil {
			return getErr
		}
		liveRawRoutes, liveRoutes, parseErr := parseRouteIdentities(data)
		if parseErr != nil {
			return parseErr
		}
		if hasDesiredUpstreams {
			if ownerErr := ensureNoForeignHostOwner(liveRoutes, desired); ownerErr != nil {
				return ownerErr
			}
		}
		if !hasDesiredUpstreams {
			_, liveOwned, _ := reconcileRawRouteStatus(liveRawRoutes, liveRoutes, desired, service.Host, false)
			if liveOwned >= 0 {
				_, deleteErr := m.caddyClient.apiRequest(host, "DELETE", caddyIDPath(desired.ID), nil)
				return deleteErr
			}
			return nil
		}
		return m.upsertRoute(host, service.Host, desired)
	})
	return status, err
}

func reconcileRawRouteStatus(rawRoutes []json.RawMessage, routes []*Route, desired *Route, serviceHost string, hasDesiredUpstreams bool) (ReconcileStatus, int, int) {
	status, owned, legacy := reconcileRouteStatus(routes, desired, serviceHost, hasDesiredUpstreams)
	// An owned route with no desired upstreams is intentionally stale so repair
	// deletes it; raw equality must never reclassify that orphan as in-sync.
	if !hasDesiredUpstreams {
		return status, owned, legacy
	}
	if status != ReconcileStale || owned < 0 || owned >= len(rawRoutes) {
		return status, owned, legacy
	}
	actual, err := decodeJSONObject(rawRoutes[owned])
	if err != nil {
		return ReconcileStale, owned, legacy
	}
	merged, err := mergeOwnedRoute(rawRoutes[owned], desired)
	if err == nil {
		actualJSON, actualErr := json.Marshal(actual)
		mergedJSON, mergedErr := json.Marshal(merged)
		if actualErr == nil && mergedErr == nil && bytes.Equal(actualJSON, mergedJSON) {
			return ReconcileInSync, owned, legacy
		}
	}
	return ReconcileStale, owned, legacy
}

// routeAppendPayload preserves Caddy's routes array shape when the routes key
// is absent and GET returns JSON null. When the array exists, POSTing one route
// appends it without replacing unrelated routes.
func routeAppendPayload(routes []*Route, route *Route) interface{} {
	if routes == nil {
		return []*Route{route}
	}
	return route
}

func reconcileRouteStatus(routes []*Route, desired *Route, serviceHost string, hasDesiredUpstreams bool) (ReconcileStatus, int, int) {
	ownedIndex, legacyIndex := -1, -1
	for i, route := range routes {
		if route == nil {
			continue
		}
		if desired != nil && route.ID == desired.ID {
			ownedIndex = i
			break
		}
		if hasDesiredUpstreams && route.ID == "" && legacyIndex < 0 && routeMatchesHost(route, serviceHost) {
			legacyIndex = i
		}
	}
	if ownedIndex >= 0 {
		if hasDesiredUpstreams && routesEquivalent(routes[ownedIndex], desired) {
			return ReconcileInSync, ownedIndex, legacyIndex
		}
		return ReconcileStale, ownedIndex, legacyIndex
	}
	if legacyIndex >= 0 {
		return ReconcileLegacy, ownedIndex, legacyIndex
	}
	if !hasDesiredUpstreams {
		return ReconcileInSync, ownedIndex, legacyIndex
	}
	return ReconcileMissing, ownedIndex, legacyIndex
}

func routesEquivalent(actual, desired *Route) bool {
	actualCopy := cloneRoute(actual)
	desiredCopy := cloneRoute(desired)
	if actualCopy == nil || desiredCopy == nil {
		return actualCopy == desiredCopy
	}
	actualHandler, _, actualOK := reverseProxyHandler(actualCopy)
	desiredHandler, _, desiredOK := reverseProxyHandler(desiredCopy)
	if actualOK != desiredOK {
		return false
	}
	if actualOK {
		if !validUpstreams(actualHandler.Upstreams) || !validUpstreams(desiredHandler.Upstreams) {
			return false
		}
		sort.Slice(actualHandler.Upstreams, func(i, j int) bool { return actualHandler.Upstreams[i].Dial < actualHandler.Upstreams[j].Dial })
		sort.Slice(desiredHandler.Upstreams, func(i, j int) bool { return desiredHandler.Upstreams[i].Dial < desiredHandler.Upstreams[j].Dial })
		if selectionPolicy(actualHandler) == "random" && selectionPolicy(desiredHandler) == "round_robin" && uniformUpstreamMultiplicity(actualHandler.Upstreams) {
			actualHandler.LoadBalancing.SelectionPolicy.Policy = "round_robin"
		}
	}
	return reflect.DeepEqual(actualCopy, desiredCopy)
}

func validUpstreams(upstreams []*Upstream) bool {
	for _, upstream := range upstreams {
		if upstream == nil {
			return false
		}
	}
	return true
}

func cloneRoute(route *Route) *Route {
	if route == nil {
		return nil
	}
	data, err := json.Marshal(route)
	if err != nil {
		return nil
	}
	var cloned Route
	if err := json.Unmarshal(data, &cloned); err != nil {
		return nil
	}
	return &cloned
}

func selectionPolicy(handler *Handler) string {
	if handler == nil || handler.LoadBalancing == nil || handler.LoadBalancing.SelectionPolicy == nil {
		return ""
	}
	return handler.LoadBalancing.SelectionPolicy.Policy
}

func uniformUpstreamMultiplicity(upstreams []*Upstream) bool {
	counts := make(map[string]int)
	for _, upstream := range upstreams {
		if upstream == nil {
			return false
		}
		counts[upstream.Dial]++
	}
	want := 0
	for _, count := range counts {
		if want == 0 {
			want = count
		} else if count != want {
			return false
		}
	}
	return len(counts) > 0
}

// DeregisterService removes a service through a route-specific API operation
// so unrelated Caddy JSON is never decoded and replaced.
func (m *Manager) DeregisterService(host, serviceHost string) error {
	m.log.Host(host, "Deregistering service for %s...", serviceHost)

	if err := m.withPersistedMutation(host, func() error {
		routesPath := "/config/apps/http/servers/srv0/routes"
		data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
		if err != nil {
			return fmt.Errorf("failed to read routes for deregistration: %w", err)
		}
		_, routes, err := parseRouteIdentities(data)
		if err != nil {
			return err
		}
		for i, route := range routes {
			if !routeMatchesHost(route, serviceHost) {
				continue
			}
			if !isAzudOwnedRoute(route) {
				return fmt.Errorf("refusing to deregister foreign route for host %s", serviceHost)
			}
			if _, err := m.caddyClient.apiRequest(host, "DELETE", routeAPIPath(routesPath, i, route), nil); err != nil {
				return fmt.Errorf("failed to delete owned route: %w", err)
			}
			return nil
		}
		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Service deregistered")
	return nil
}

// AddUpstream adds an upstream using only the owned handler's API path.
func (m *Manager) AddUpstream(host, serviceHost, upstream string) error {
	m.log.Host(host, "Adding upstream %s to %s...", upstream, serviceHost)

	if err := m.withPersistedMutation(host, func() error {
		if err := m.modifyUpstreams(host, serviceHost, func(upstreams []*Upstream) []*Upstream {
			return addUpstreamIfMissing(upstreams, upstream)
		}); err != nil {
			return err
		}

		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Upstream added")
	return nil
}

func addUpstreamIfMissing(upstreams []*Upstream, dial string) []*Upstream {
	for _, existing := range upstreams {
		if existing.Dial == dial {
			return upstreams
		}
	}
	return append(upstreams, &Upstream{Dial: dial})
}

func reverseProxyHandler(route *Route) (*Handler, int, bool) {
	if route == nil {
		return nil, -1, false
	}
	for index, handler := range route.Handle {
		if handler != nil && handler.Handler == "reverse_proxy" {
			return handler, index, true
		}
	}
	return nil, -1, false
}

// RemoveUpstream removes an upstream using only the owned handler's API path.
func (m *Manager) RemoveUpstream(host, serviceHost, upstream string) error {
	m.log.Host(host, "Removing upstream %s from %s...", upstream, serviceHost)

	if err := m.withPersistedMutation(host, func() error {
		if err := m.modifyUpstreams(host, serviceHost, func(upstreams []*Upstream) []*Upstream {
			var filtered []*Upstream
			for _, u := range upstreams {
				if u.Dial != upstream {
					filtered = append(filtered, u)
				}
			}
			return filtered
		}); err != nil {
			return err
		}

		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Upstream removed")
	return nil
}

// modifyUpstreams finds the route for serviceHost and applies a transformation
// to only the owned reverse proxy's upstream list.
func (m *Manager) modifyUpstreams(host, serviceHost string, transform func([]*Upstream) []*Upstream) error {
	routesPath := "/config/apps/http/servers/srv0/routes"

	data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
	if err != nil {
		return fmt.Errorf("failed to read routes for upstream update: %w", err)
	}
	rawRoutes, routes, err := parseRouteIdentities(data)
	if err != nil {
		return err
	}

	for i, route := range routes {
		if !routeMatchesHost(route, serviceHost) {
			continue
		}
		if !isAzudOwnedRoute(route) {
			return fmt.Errorf("refusing to update upstreams on foreign route for host %s", serviceHost)
		}
		handler, handlerIndex, err := parseReverseProxyIdentity(rawRoutes[i])
		if err != nil {
			return err
		}
		upstreams := transform(handler.Upstreams)
		rawRoute, err := decodeJSONObject(rawRoutes[i])
		if err != nil {
			return err
		}
		rawHandlers, err := jsonObjectSlice(rawRoute["handle"])
		if err != nil || handlerIndex >= len(rawHandlers) {
			return fmt.Errorf("failed to locate owned reverse_proxy handler")
		}
		desiredData, err := json.Marshal(upstreams)
		if err != nil {
			return err
		}
		desiredValue, err := decodeJSONValue(desiredData)
		if err != nil {
			return err
		}
		merged := mergeRawUpstreamValues(rawHandlers[handlerIndex]["upstreams"], desiredValue)
		method := "PATCH"
		if _, exists := rawHandlers[handlerIndex]["upstreams"]; !exists {
			method = "PUT"
		}
		upstreamsPath := handlerAPIPath(routesPath, i, handlerIndex, handler) + "/upstreams"
		if _, err := m.caddyClient.apiRequest(host, method, upstreamsPath, merged); err != nil {
			return fmt.Errorf("failed to update upstreams without replacing the owned handler: %w", err)
		}
		return nil
	}

	return fmt.Errorf("no route found for host %s", serviceHost)
}

// DrainUpstream waits for all in-flight requests to the given upstream to
// complete by polling Caddy's /reverse_proxy/upstreams admin endpoint. The
// upstream should already have been removed from the route so no new requests
// are sent to it. The method returns when the active request count reaches
// zero or the timeout expires.
//
// A minimum grace period of 5 seconds is always applied, even if the
// upstream is no longer tracked by Caddy, to allow in-transit TCP
// connections to complete naturally.
func (m *Manager) DrainUpstream(host, upstream string, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}

	m.log.Host(host, "Draining connections from %s (timeout=%s)...", upstream, timeout)

	deadline := time.Now().Add(timeout)
	pollInterval := 2 * time.Second

	// Minimum grace period: even if Caddy reports 0 active requests
	// (upstream already removed from its tracking), in-transit TCP
	// data may still be flowing through kernel buffers.
	minGrace := 5 * time.Second
	if timeout < minGrace {
		minGrace = timeout
	}
	graceDeadline := time.Now().Add(minGrace)
	everFoundActive := false

	for time.Now().Before(deadline) {
		count, err := m.caddyClient.GetUpstreamRequestCount(host, upstream)
		if err != nil {
			// If we can't query Caddy (e.g., endpoint not available), fall
			// back to waiting out the remaining timeout.
			m.log.Debug("Cannot query upstream request count, waiting remaining timeout: %v", err)
			remaining := time.Until(deadline)
			if remaining > 0 {
				time.Sleep(remaining)
			}
			return nil
		}

		if count > 0 {
			everFoundActive = true
			m.log.Debug("Upstream %s still has %d active request(s), waiting...", upstream, count)
			time.Sleep(pollInterval)
			continue
		}

		// count == 0: upstream reports no active requests.
		if everFoundActive {
			// We previously saw active requests and now they've drained.
			m.log.Host(host, "Upstream %s fully drained", upstream)
			return nil
		}

		// Never saw active requests — Caddy may have already removed the
		// upstream from its tracking. Apply the minimum grace period.
		if time.Now().Before(graceDeadline) {
			time.Sleep(pollInterval)
			continue
		}

		m.log.Host(host, "Upstream %s drain grace period complete", upstream)
		return nil
	}

	m.log.Warn("Drain timeout reached for upstream %s, proceeding", upstream)
	return nil
}

// GetUpstreamRequestCount returns the active request count for an upstream.
func (m *Manager) GetUpstreamRequestCount(host, upstream string) (int, error) {
	return m.caddyClient.GetUpstreamRequestCount(host, upstream)
}

func isAzudOwnedRoute(route *Route) bool {
	return route != nil && strings.HasPrefix(route.ID, azudRouteIDPrefix)
}

func routeMatchesHost(route *Route, host string) bool {
	if route == nil {
		return false
	}
	for _, match := range route.Match {
		if match == nil {
			continue
		}
		for _, h := range match.Host {
			if strings.EqualFold(h, host) {
				return true
			}
		}
	}
	return false
}

func proxyHostsOverlap(first, second string) bool {
	first = strings.ToLower(first)
	second = strings.ToLower(second)
	if first == second {
		return true
	}

	firstSuffix, firstWildcard := strings.CutPrefix(first, "*.")
	secondSuffix, secondWildcard := strings.CutPrefix(second, "*.")
	switch {
	case firstWildcard && secondWildcard:
		return firstSuffix == secondSuffix
	case firstWildcard:
		return wildcardMatchesExactHost(firstSuffix, second)
	case secondWildcard:
		return wildcardMatchesExactHost(secondSuffix, first)
	default:
		return false
	}
}

func wildcardMatchesExactHost(suffix, exact string) bool {
	prefix, found := strings.CutSuffix(exact, "."+suffix)
	return found && prefix != "" && !strings.Contains(prefix, ".")
}

func routeHosts(route *Route) []string {
	if route == nil {
		return nil
	}
	var hosts []string
	for _, match := range route.Match {
		if match != nil {
			hosts = append(hosts, match.Host...)
		}
	}
	return hosts
}

func ensureNoForeignHostOwner(routes []*Route, desired *Route) error {
	if desired == nil {
		return nil
	}
	desiredHosts := routeHosts(desired)
	primaryHost := ""
	if len(desiredHosts) > 0 {
		primaryHost = desiredHosts[0]
	}

	for _, route := range routes {
		if route == nil || route.ID == desired.ID {
			continue
		}
		// An ID-less route with the exact primary host is an Azud legacy route
		// that the caller can safely adopt through a preserving managed overlay.
		if route.ID == "" && routeMatchesHost(route, primaryHost) {
			continue
		}
		for _, desiredHost := range desiredHosts {
			for _, existingHost := range routeHosts(route) {
				if !proxyHostsOverlap(desiredHost, existingHost) {
					continue
				}
				owner := route.ID
				if owner == "" {
					owner = "an ID-less route"
				}
				return fmt.Errorf("proxy host %s overlaps %s already owned by Caddy route %s", desiredHost, existingHost, owner)
			}
		}
	}
	return nil
}

func routesHaveSameOwner(existing, desired *Route, fallbackHost string) bool {
	if existing != nil && desired != nil && existing.ID != "" && existing.ID == desired.ID {
		return true
	}
	return existing != nil && existing.ID == "" && routeMatchesHost(existing, fallbackHost)
}

func serviceRouteID(service string) string {
	if service == "" {
		return ""
	}
	return azudRouteIDPrefix + service
}

func serviceHandlerID(service string) string {
	if service == "" {
		return ""
	}
	return azudHandlerIDPrefix + service
}

func caddyIDPath(id string) string {
	return "/id/" + url.PathEscape(id)
}

func routeAPIPath(routesPath string, routeIndex int, route *Route) string {
	if route != nil && route.ID != "" {
		return caddyIDPath(route.ID)
	}
	return fmt.Sprintf("%s/%d", routesPath, routeIndex)
}

func handlerAPIPath(routesPath string, routeIndex, handlerIndex int, handler *Handler) string {
	if handler != nil && handler.ID != "" {
		return caddyIDPath(handler.ID)
	}
	return fmt.Sprintf("%s/%d/handle/%d", routesPath, routeIndex, handlerIndex)
}

// BootAll starts the proxy on multiple hosts in parallel.
func (m *Manager) BootAll(hosts []string, config *ProxyConfig) error {
	m.log.Header("Starting proxy on %d host(s)", len(hosts))

	type bootErr struct {
		host string
		err  error
	}
	errCh := make(chan bootErr, len(hosts))
	var wg sync.WaitGroup

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if err := m.Boot(h, config); err != nil {
				errCh <- bootErr{host: h, err: err}
			}
		}(host)
	}

	wg.Wait()
	close(errCh)

	var errors []error
	for e := range errCh {
		errors = append(errors, fmt.Errorf("%s: %w", e.host, e.err))
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed on %d host(s)", len(errors))
	}

	return nil
}

// SetCanaryWeights atomically replaces a two-upstream route with the requested
// stable/canary percentages and reads it back before success. It rejects extra
// upstreams so scaling and canary cannot silently produce misleading weights.
func (m *Manager) SetCanaryWeights(host, serviceHost, stable string, stableWeight int, canary string, canaryWeight int) error {
	if stable == "" || canary == "" || stable == canary {
		return fmt.Errorf("stable and canary upstreams must be distinct and non-empty")
	}
	if stableWeight < 0 || canaryWeight < 0 || stableWeight+canaryWeight != 100 {
		return fmt.Errorf("stable and canary weights must be non-negative and total 100")
	}

	return m.withPersistedMutation(host, func() error {
		var transformErr error
		if err := m.modifyRoute(host, serviceHost, func(handler *Handler) {
			stableFound := false
			for _, upstream := range handler.Upstreams {
				switch upstream.Dial {
				case stable:
					stableFound = true
				case canary:
				default:
					transformErr = fmt.Errorf("route contains unexpected upstream %s; scale the web role to one instance before canary deployment", upstream.Dial)
					return
				}
			}
			if !stableFound {
				transformErr = fmt.Errorf("stable upstream %s not found for service %s", stable, serviceHost)
				return
			}
			handler.Upstreams = weightedUpstreams(
				UpstreamWeight{Dial: stable, Weight: stableWeight},
				UpstreamWeight{Dial: canary, Weight: canaryWeight},
			)
			setStockWeightedPolicy(handler)
		}); err != nil {
			return err
		}
		if transformErr != nil {
			return transformErr
		}
		return m.verifyUpstreamWeights(host, serviceHost, map[string]int{
			stable: stableWeight,
			canary: canaryWeight,
		})
	})
}

func setStockWeightedPolicy(handler *Handler) {
	if handler.LoadBalancing == nil {
		handler.LoadBalancing = &LoadBalancing{}
	}
	handler.LoadBalancing.SelectionPolicy = &SelectionPolicy{Policy: "random"}
}

func weightedUpstreams(weights ...UpstreamWeight) []*Upstream {
	divisor := 0
	for _, weighted := range weights {
		if weighted.Weight > 0 {
			if divisor == 0 {
				divisor = weighted.Weight
			} else {
				divisor = greatestCommonDivisor(divisor, weighted.Weight)
			}
		}
	}
	if divisor == 0 {
		return nil
	}
	var upstreams []*Upstream
	for _, weighted := range weights {
		for i := 0; i < weighted.Weight/divisor; i++ {
			upstreams = append(upstreams, &Upstream{Dial: weighted.Dial})
		}
	}
	return upstreams
}

func greatestCommonDivisor(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// modifyRoute finds the route for serviceHost and applies a transformation to
// only its Azud-owned reverse_proxy handler.
func (m *Manager) modifyRoute(host, serviceHost string, transform func(*Handler)) error {
	routesPath := "/config/apps/http/servers/srv0/routes"
	data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
	if err != nil {
		return fmt.Errorf("failed to read routes: %w", err)
	}
	rawRoutes, routes, err := parseRouteIdentities(data)
	if err != nil {
		return err
	}
	for i, route := range routes {
		if !routeMatchesHost(route, serviceHost) {
			continue
		}
		if !isAzudOwnedRoute(route) {
			return fmt.Errorf("refusing to update canary weights on foreign route for host %s", serviceHost)
		}
		handler, handlerIndex, err := parseReverseProxyIdentity(rawRoutes[i])
		if err != nil {
			return err
		}
		rawRoute, err := decodeJSONObject(rawRoutes[i])
		if err != nil {
			return err
		}
		rawHandlers, err := jsonObjectSlice(rawRoute["handle"])
		if err != nil || handlerIndex >= len(rawHandlers) {
			return fmt.Errorf("failed to locate owned reverse_proxy handler")
		}
		rawHandler := rawHandlers[handlerIndex]
		transform(handler)
		if handler.LoadBalancing == nil || handler.LoadBalancing.SelectionPolicy == nil {
			return fmt.Errorf("canary route has no selection policy")
		}
		desiredUpstreamsData, err := json.Marshal(handler.Upstreams)
		if err != nil {
			return err
		}
		desiredUpstreams, err := decodeJSONValue(desiredUpstreamsData)
		if err != nil {
			return err
		}
		mergedUpstreams := mergeRawUpstreamValues(rawHandler["upstreams"], desiredUpstreams)
		handlerPath := handlerAPIPath(routesPath, i, handlerIndex, handler)
		upstreamMethod := "PATCH"
		if _, exists := rawHandler["upstreams"]; !exists {
			upstreamMethod = "PUT"
		}
		if _, err := m.caddyClient.apiRequest(host, upstreamMethod, handlerPath+"/upstreams", mergedUpstreams); err != nil {
			return fmt.Errorf("failed to update canary upstreams: %w", err)
		}
		rawLoadBalancingValue, loadBalancingExists := rawHandler["load_balancing"]
		if !loadBalancingExists {
			body := map[string]interface{}{"selection_policy": handler.LoadBalancing.SelectionPolicy}
			if _, err := m.caddyClient.apiRequest(host, "PUT", handlerPath+"/load_balancing", body); err != nil {
				return fmt.Errorf("failed to create canary load balancing policy: %w", err)
			}
			return nil
		}
		rawLoadBalancing, ok := rawLoadBalancingValue.(map[string]interface{})
		if !ok {
			return fmt.Errorf("owned route has invalid load_balancing configuration")
		}
		selectionMethod := "PATCH"
		if _, exists := rawLoadBalancing["selection_policy"]; !exists {
			selectionMethod = "PUT"
		}
		if _, err := m.caddyClient.apiRequest(host, selectionMethod, handlerPath+"/load_balancing/selection_policy", handler.LoadBalancing.SelectionPolicy); err != nil {
			return fmt.Errorf("failed to update canary selection policy: %w", err)
		}
		return nil
	}
	return fmt.Errorf("no route found for host %s", serviceHost)
}

type UpstreamWeight struct {
	Dial   string
	Weight int
}

// GetUpstreamWeights returns all upstreams with their weights for a service
// using route-specific API operations.
func (m *Manager) GetUpstreamWeights(host, serviceHost string) ([]UpstreamWeight, error) {
	routesPath := "/config/apps/http/servers/srv0/routes"
	data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
	if err != nil {
		return nil, err
	}
	rawRoutes, routes, err := parseRouteIdentities(data)
	if err != nil {
		return nil, err
	}
	for i, route := range routes {
		if !routeMatchesHost(route, serviceHost) {
			continue
		}
		if !isAzudOwnedRoute(route) {
			return nil, fmt.Errorf("refusing to read canary weights from foreign route for host %s", serviceHost)
		}
		handler, _, err := parseReverseProxyIdentity(rawRoutes[i])
		if err != nil {
			return nil, err
		}
		return extractWeights(handler.Upstreams), nil
	}
	return nil, fmt.Errorf("service %s not found", serviceHost)
}

func (m *Manager) verifyUpstreamWeights(host, serviceHost string, expected map[string]int) error {
	weights, err := m.GetUpstreamWeights(host, serviceHost)
	if err != nil {
		return fmt.Errorf("failed to read back upstream weights: %w", err)
	}
	actual := make(map[string]int, len(weights))
	for _, weighted := range weights {
		actual[weighted.Dial] = weighted.Weight
	}
	for dial, want := range expected {
		if actual[dial] != want {
			return fmt.Errorf("upstream %s weight readback is %d, want %d", dial, actual[dial], want)
		}
	}
	return nil
}

func extractWeights(upstreams []*Upstream) []UpstreamWeight {
	if len(upstreams) == 0 {
		return nil
	}
	counts := make(map[string]int)
	order := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		if counts[upstream.Dial] == 0 {
			order = append(order, upstream.Dial)
		}
		counts[upstream.Dial]++
	}
	weights := make([]UpstreamWeight, 0, len(order))
	for _, dial := range order {
		weights = append(weights, UpstreamWeight{
			Dial:   dial,
			Weight: counts[dial] * 100 / len(upstreams),
		})
	}
	return weights
}

// isValidHeaderName checks if s is a valid HTTP header field name per RFC 7230.
// This prevents injection of Caddy filter path separators (>) or other
// control characters into log field paths.
func isValidHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		// RFC 7230 token characters: !#$%&'*+-.^_`|~ plus DIGIT and ALPHA
		if c <= ' ' || c >= 0x7F || c == '>' || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}
