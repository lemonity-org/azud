package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/ssh"
	"github.com/lemonity-org/azud/internal/state"
)

const (
	CaddyImage         = "docker.io/library/caddy:2-alpine"
	CaddyContainerName = "azud-proxy"
	CaddyAdminPort     = 2019
	CaddyHTTPPort      = 80
	CaddyHTTPSPort     = 443

	// CaddyConfigFileName is the name of the Caddy config file.
	CaddyConfigFileName = "caddy-config.json"

	// CaddyLockFileName is the name of the Caddy lock file.
	CaddyLockFileName = "caddy.lock"

	CaddyLockTimeout = 120 * time.Second
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
}

// NewManager creates a new proxy manager. Defaults to root user for state paths.
func NewManager(sshClient *ssh.Client, log *output.Logger) *Manager {
	return NewManagerWithUser(sshClient, log, "root")
}

// NewManagerWithUser creates a new proxy manager with a specific SSH user.
func NewManagerWithUser(sshClient *ssh.Client, log *output.Logger, user string) *Manager {
	if log == nil {
		log = output.DefaultLogger
	}
	if user == "" {
		user = "root"
	}

	podmanClient := podman.NewClient(sshClient)

	return &Manager{
		sshClient:   sshClient,
		caddyClient: NewCaddyClient(sshClient),
		podman:      podman.NewContainerManager(podmanClient),
		log:         log,
		user:        user,
	}
}

// withCaddyLock acquires the remote Caddy lock on the given host, runs fn,
// then releases. This serializes mutating Caddy admin API operations.
func (m *Manager) withCaddyLock(host string, fn func() error) error {
	return m.sshClient.WithRemoteLock(host, CaddyLockFile(m.user), CaddyLockTimeout, fn)
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

	configDir := state.DirQuoted(m.user)
	configFile := state.ConfigFileQuoted(m.user, CaddyConfigFileName)
	tmpFile := state.ConfigFileQuoted(m.user, CaddyConfigFileName+".tmp")

	// Write atomically via a temp file to avoid partial writes on failure.
	// Note: paths are pre-quoted with ${HOME} expansion support for non-root users.
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && mv %s %s",
		configDir, tmpFile, tmpFile, configFile)
	result, err := m.sshClient.ExecuteWithStdin(host, cmd, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write command failed: %s", result.Stderr)
	}
	m.log.Debug("persistConfig: saved config to %s", configFile)
	return nil
}

// persistConfigWithWarning calls persistConfig and logs a warning on failure.
// Use this in places where persistence failure should not block operations.
func (m *Manager) persistConfigWithWarning(host string) {
	if err := m.persistConfig(host); err != nil {
		m.log.Warn("Failed to persist proxy config on %s: %v (config may be lost on reboot)", host, err)
	}
}

// restoreConfig reads a previously persisted Caddy config from CaddyConfigFile
// on the remote host and loads it into Caddy via the admin API. Returns an
// error so callers can decide whether to warn or fail.
func (m *Manager) restoreConfig(host string) error {
	configFile := state.ConfigFileQuoted(m.user, CaddyConfigFileName)
	cmd := fmt.Sprintf("test -f %s && cat %s", configFile, configFile)
	result, err := m.sshClient.Execute(host, cmd)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("config file does not exist")
	}

	var config CaddyConfig
	if err := json.Unmarshal([]byte(result.Stdout), &config); err != nil {
		return fmt.Errorf("persisted config is invalid JSON: %w", err)
	}

	if err := m.caddyClient.LoadConfig(host, &config); err != nil {
		return fmt.Errorf("failed to load persisted config: %w", err)
	}

	m.log.Debug("restoreConfig: restored config from %s", configFile)
	return nil
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

	// Enable access logging (even without header redaction)
	LoggingEnabled bool

	// Request headers to redact from access logs
	RedactRequestHeaders []string

	// Response headers to redact from access logs
	RedactResponseHeaders []string
}

// Boot starts the Caddy proxy on a host
func (m *Manager) Boot(host string, config *ProxyConfig) error {
	m.log.Host(host, "Starting proxy...")

	running, err := m.podman.IsRunning(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to check proxy status: %w", err)
	}
	if running {
		m.log.Host(host, "Proxy already running")
		return nil
	}

	exists, err := m.podman.Exists(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to check proxy container: %w", err)
	}

	if exists {
		if err := m.podman.Start(host, CaddyContainerName); err != nil {
			return fmt.Errorf("failed to start proxy: %w", err)
		}
		// Wait for Caddy to be ready, then restore persisted config
		time.Sleep(2 * time.Second)
		if err := m.withCaddyLock(host, func() error {
			return m.restoreConfig(host)
		}); err != nil {
			m.log.Warn("Failed to restore proxy config: %v", err)
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

	containerConfig := &podman.ContainerConfig{
		Name:    CaddyContainerName,
		Image:   CaddyImage,
		Detach:  true,
		Restart: "unless-stopped",
		Ports: []string{
			fmt.Sprintf("%d:%d", httpPort, 80),
			fmt.Sprintf("%d:%d", httpsPort, 443),
			fmt.Sprintf("127.0.0.1:%d:%d", CaddyAdminPort, CaddyAdminPort),
		},
		Volumes: []string{
			"caddy_data:/data",
			"caddy_config:/config",
		},
		Network: "azud",
		Labels: map[string]string{
			"azud.managed": "true",
			"azud.type":    "proxy",
		},
		Command: []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile", "--watch"},
		Env: map[string]string{
			"CADDY_ADMIN": "127.0.0.1:2019",
		},
	}

	_, err = m.podman.Run(host, containerConfig)
	if err != nil {
		return fmt.Errorf("failed to start proxy container: %w", err)
	}

	// Wait for Caddy to be ready
	time.Sleep(2 * time.Second)

	// Load initial configuration under the Caddy lock
	if err := m.withCaddyLock(host, func() error {
		if err := m.loadInitialConfig(host, config); err != nil {
			return err
		}
		m.persistConfigWithWarning(host)
		return nil
	}); err != nil {
		m.log.Warn("Failed to load initial config: %v", err)
	}

	m.log.HostSuccess(host, "Proxy started")
	return nil
}

func (m *Manager) loadInitialConfig(host string, config *ProxyConfig) error {
	caddyConfig := &CaddyConfig{
		Admin: &AdminConfig{
			Listen: "127.0.0.1:2019",
		},
		Apps: &AppsConfig{
			HTTP: &HTTPApp{
				Servers: map[string]*HTTPServer{
					"srv0": {
						Listen: []string{":80", ":443"},
						Routes: []*Route{},
					},
				},
			},
		},
	}
	server := caddyConfig.Apps.HTTP.Servers["srv0"]

	if config != nil && config.AutoHTTPS && !config.SSLRedirect {
		server.AutoHTTPS = &AutoHTTPSConfig{
			DisableRedirects: true,
		}
	}

	if config != nil && (config.LoggingEnabled || len(config.RedactRequestHeaders) > 0 || len(config.RedactResponseHeaders) > 0) {
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
	}

	if config != nil && config.SSLCertificate != "" && config.SSLPrivateKey != "" {
		m.log.Host(host, "Configuring custom SSL certificates...")

		caddyConfig.Apps.TLS = &TLSApp{
			Certificates: &CertificatesConfig{
				LoadPEM: []LoadedCertificate{
					{
						Certificate: config.SSLCertificate,
						Key:         config.SSLPrivateKey,
					},
				},
			},
			Automation: &TLSAutomation{
				Policies: []*TLSPolicy{
					{
						Subjects: config.Hosts,
						Issuers:  []*Issuer{}, // empty disables ACME
					},
				},
			},
		}
	} else if config != nil && config.AutoHTTPS && config.Email != "" {
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

	return m.caddyClient.LoadConfig(host, caddyConfig)
}

// Stop stops the Caddy proxy on a host
func (m *Manager) Stop(host string) error {
	m.log.Host(host, "Stopping proxy...")

	if err := m.podman.Stop(host, CaddyContainerName, 30); err != nil {
		return fmt.Errorf("failed to stop proxy: %w", err)
	}

	m.log.HostSuccess(host, "Proxy stopped")
	return nil
}

// Reboot restarts the Caddy proxy
func (m *Manager) Reboot(host string) error {
	m.log.Host(host, "Rebooting proxy...")

	if err := m.podman.Restart(host, CaddyContainerName, 30); err != nil {
		return fmt.Errorf("failed to restart proxy: %w", err)
	}

	// Wait for Caddy to be ready, then restore persisted config
	time.Sleep(2 * time.Second)
	if err := m.withCaddyLock(host, func() error {
		return m.restoreConfig(host)
	}); err != nil {
		m.log.Warn("Failed to restore proxy config after reboot: %v", err)
	}

	m.log.HostSuccess(host, "Proxy rebooted")
	return nil
}

// Remove removes the Caddy proxy container
func (m *Manager) Remove(host string) error {
	m.log.Host(host, "Removing proxy...")

	if err := m.podman.Remove(host, CaddyContainerName, true); err != nil {
		return fmt.Errorf("failed to remove proxy: %w", err)
	}

	// Best-effort removal of persisted config file
	rmCmd := fmt.Sprintf("rm -f %s", state.ConfigFileQuoted(m.user, CaddyConfigFileName))
	if _, err := m.sshClient.Execute(host, rmCmd); err != nil {
		m.log.Debug("Failed to remove persisted config file: %v", err)
	}

	m.log.HostSuccess(host, "Proxy removed")
	return nil
}

// Status returns the proxy status on a host
func (m *Manager) Status(host string) (*ProxyStatus, error) {
	status := &ProxyStatus{Host: host}

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
	logsConfig := &podman.LogsConfig{
		Container: CaddyContainerName,
		Follow:    follow,
		Tail:      tail,
	}
	return m.podman.Logs(host, logsConfig)
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

	if err := m.withCaddyLock(host, func() error {
		// Try route-specific update first
		if err := m.upsertRoute(host, service.Host, route); err != nil {
			// Fall back to full config replacement if route-specific update fails
			// (e.g., the server doesn't exist yet and needs bootstrapping)
			m.log.Debug("Route-specific update failed, falling back to full config: %v", err)
			if fallbackErr := m.registerServiceFull(host, service, route); fallbackErr != nil {
				return fallbackErr
			}
		}

		m.persistConfigWithWarning(host)
		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Service %s registered", service.Name)
	return nil
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

	var routes []*Route
	if err := json.Unmarshal(data, &routes); err != nil {
		return fmt.Errorf("failed to parse routes: %w", err)
	}

	for i, r := range routes {
		if routeMatchesHost(r, serviceHost) {
			routePath := fmt.Sprintf("%s/%d", routesPath, i)
			_, err := m.caddyClient.apiRequest(host, "PATCH", routePath, route)
			if err != nil {
				return fmt.Errorf("failed to patch route at index %d: %w", i, err)
			}
			return nil
		}
	}

	// Route doesn't exist — append it by PATCHing the full routes array.
	// Caddy's POST to routes/... fails with RouteList unmarshal errors,
	// so we rebuild the array and replace it atomically.
	routes = append(routes, route)
	_, err = m.caddyClient.apiRequest(host, "PATCH", routesPath, routes)
	if err != nil {
		return fmt.Errorf("failed to append route: %w", err)
	}

	return nil
}

// registerServiceFull is the fallback path that replaces the full Caddy
// configuration. Used when the server or routes path doesn't exist yet
// (first deployment).
func (m *Manager) registerServiceFull(host string, service *ServiceConfig, route *Route) error {
	config, err := m.caddyClient.GetConfig(host)
	if err != nil {
		config = &CaddyConfig{
			Apps: &AppsConfig{
				HTTP: &HTTPApp{
					Servers: map[string]*HTTPServer{
						"srv0": {
							Listen: []string{":80", ":443"},
							Routes: []*Route{},
						},
					},
				},
			},
		}
	}

	if config.Apps == nil {
		config.Apps = &AppsConfig{}
	}
	if config.Apps.HTTP == nil {
		config.Apps.HTTP = &HTTPApp{Servers: make(map[string]*HTTPServer)}
	}
	if config.Apps.HTTP.Servers["srv0"] == nil {
		config.Apps.HTTP.Servers["srv0"] = &HTTPServer{
			Listen: []string{":80", ":443"},
			Routes: []*Route{},
		}
	}

	server := config.Apps.HTTP.Servers["srv0"]
	found := false
	for i, r := range server.Routes {
		if routeMatchesHost(r, service.Host) {
			server.Routes[i] = route
			found = true
			break
		}
	}

	if !found {
		server.Routes = append(server.Routes, route)
	}

	if err := m.caddyClient.LoadConfig(host, config); err != nil {
		return fmt.Errorf("failed to apply full config: %w", err)
	}

	return nil
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

	handler := &Handler{
		Handler:   "reverse_proxy",
		Upstreams: upstreams,
		LoadBalancing: &LoadBalancing{
			SelectionPolicy: &SelectionPolicy{
				Policy: "round_robin",
			},
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
		handler.HealthChecks = &HealthChecks{
			Active: &ActiveHealthCheck{
				Path:     service.HealthPath,
				Interval: interval,
				Timeout:  timeout,
			},
			Passive: &PassiveHealthCheck{
				FailDuration: "30s",
				MaxFails:     3,
			},
		}
	}

	if service.ResponseTimeout != "" || service.ResponseHeaderTimeout != "" {
		handler.Transport = &Transport{
			Protocol:              "http",
			ReadTimeout:           service.ResponseTimeout,
			ResponseHeaderTimeout: service.ResponseHeaderTimeout,
		}
	}

	if service.ForwardHeaders {
		handler.ProxyHeaders = &HeadersConfig{
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
		Match: []*Match{
			{Host: hostMatches},
		},
		Handle:   handlers,
		Terminal: true,
	}

	return route
}

// DeregisterService removes a service from the proxy using route-specific
// API operations. Falls back to full config replacement on error.
func (m *Manager) DeregisterService(host, serviceHost string) error {
	m.log.Host(host, "Deregistering service for %s...", serviceHost)

	if err := m.withCaddyLock(host, func() error {
		// Try route-specific deletion first
		routesPath := "/config/apps/http/servers/srv0/routes"
		data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
		if err == nil {
			var routes []*Route
			if jsonErr := json.Unmarshal(data, &routes); jsonErr == nil {
				for i, r := range routes {
					if routeMatchesHost(r, serviceHost) {
						routePath := fmt.Sprintf("%s/%d", routesPath, i)
						if _, delErr := m.caddyClient.apiRequest(host, "DELETE", routePath, nil); delErr == nil {
							m.persistConfigWithWarning(host)
							return nil
						}
						break
					}
				}
			}
		}

		// Fall back to full config replacement
		m.log.Debug("Route-specific deregister failed, falling back to full config")
		config, err := m.caddyClient.GetConfig(host)
		if err != nil {
			return fmt.Errorf("failed to get config: %w", err)
		}

		if config.Apps == nil || config.Apps.HTTP == nil {
			return nil
		}

		server := config.Apps.HTTP.Servers["srv0"]
		if server == nil {
			return nil
		}

		var filtered []*Route
		for _, r := range server.Routes {
			if !routeMatchesHost(r, serviceHost) {
				filtered = append(filtered, r)
			}
		}
		server.Routes = filtered

		if err := m.caddyClient.LoadConfig(host, config); err != nil {
			return fmt.Errorf("failed to apply config: %w", err)
		}

		m.persistConfigWithWarning(host)
		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Service deregistered")
	return nil
}

// AddUpstream adds an upstream to an existing service using route-specific
// API operations. Falls back to full config replacement on error.
func (m *Manager) AddUpstream(host, serviceHost, upstream string) error {
	m.log.Host(host, "Adding upstream %s to %s...", upstream, serviceHost)

	if err := m.withCaddyLock(host, func() error {
		if err := m.modifyUpstreams(host, serviceHost, func(upstreams []*Upstream) []*Upstream {
			return append(upstreams, &Upstream{Dial: upstream})
		}); err != nil {
			return err
		}

		m.persistConfigWithWarning(host)
		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Upstream added")
	return nil
}

// RemoveUpstream removes an upstream from a service using route-specific
// API operations. Falls back to full config replacement on error.
func (m *Manager) RemoveUpstream(host, serviceHost, upstream string) error {
	m.log.Host(host, "Removing upstream %s from %s...", upstream, serviceHost)

	if err := m.withCaddyLock(host, func() error {
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

		m.persistConfigWithWarning(host)
		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Upstream removed")
	return nil
}

// modifyUpstreams finds the route for serviceHost and applies a transformation
// function to its upstreams list. Uses route-specific API with fallback to
// full config replacement.
func (m *Manager) modifyUpstreams(host, serviceHost string, transform func([]*Upstream) []*Upstream) error {
	routesPath := "/config/apps/http/servers/srv0/routes"

	data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
	if err != nil {
		return m.modifyUpstreamsFull(host, serviceHost, transform)
	}

	var routes []*Route
	if err := json.Unmarshal(data, &routes); err != nil {
		return m.modifyUpstreamsFull(host, serviceHost, transform)
	}

	for i, route := range routes {
		if routeMatchesHost(route, serviceHost) && len(route.Handle) > 0 {
			route.Handle[0].Upstreams = transform(route.Handle[0].Upstreams)

			upstreamsPath := fmt.Sprintf("%s/%d/handle/0/upstreams", routesPath, i)
			_, err := m.caddyClient.apiRequest(host, "PATCH", upstreamsPath, route.Handle[0].Upstreams)
			if err != nil {
				routePath := fmt.Sprintf("%s/%d", routesPath, i)
				if _, routeErr := m.caddyClient.apiRequest(host, "PATCH", routePath, route); routeErr != nil {
					return m.modifyUpstreamsFull(host, serviceHost, transform)
				}
			}
			return nil
		}
	}

	return fmt.Errorf("no route found for host %s", serviceHost)
}

// modifyUpstreamsFull is the fallback that reads and replaces the full config.
func (m *Manager) modifyUpstreamsFull(host, serviceHost string, transform func([]*Upstream) []*Upstream) error {
	config, err := m.caddyClient.GetConfig(host)
	if err != nil {
		return err
	}

	if config.Apps == nil || config.Apps.HTTP == nil {
		return fmt.Errorf("no HTTP config found")
	}

	server := config.Apps.HTTP.Servers["srv0"]
	if server == nil {
		return fmt.Errorf("server not found")
	}

	found := false
	for _, route := range server.Routes {
		if routeMatchesHost(route, serviceHost) && len(route.Handle) > 0 {
			route.Handle[0].Upstreams = transform(route.Handle[0].Upstreams)
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("no route found for host %s", serviceHost)
	}

	if err := m.caddyClient.LoadConfig(host, config); err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	return nil
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

func routeMatchesHost(route *Route, host string) bool {
	for _, match := range route.Match {
		for _, h := range match.Host {
			if h == host {
				return true
			}
		}
	}
	return false
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

// AddWeightedUpstream adds an upstream with a specific weight (for canary deployments)
// using route-specific API operations.
func (m *Manager) AddWeightedUpstream(host, serviceHost, upstream string, weight int) error {
	m.log.Host(host, "Adding weighted upstream %s (weight=%d) to %s...", upstream, weight, serviceHost)

	if err := m.withCaddyLock(host, func() error {
		if err := m.modifyRoute(host, serviceHost, func(handler *Handler) {
			handler.Upstreams = append(handler.Upstreams, &Upstream{
				Dial:   upstream,
				Weight: weight,
			})
			if handler.LoadBalancing == nil {
				handler.LoadBalancing = &LoadBalancing{}
			}
			if handler.LoadBalancing.SelectionPolicy == nil {
				handler.LoadBalancing.SelectionPolicy = &SelectionPolicy{}
			}
			handler.LoadBalancing.SelectionPolicy.Policy = "weighted_round_robin"
		}); err != nil {
			return err
		}

		m.persistConfigWithWarning(host)
		return nil
	}); err != nil {
		return err
	}

	m.log.HostSuccess(host, "Weighted upstream added")
	return nil
}

// SetUpstreamWeight modifies the weight of an existing upstream using
// route-specific API operations.
func (m *Manager) SetUpstreamWeight(host, serviceHost, upstream string, weight int) error {
	m.log.Host(host, "Setting weight=%d for upstream %s on %s...", weight, upstream, serviceHost)

	found := false
	if err := m.withCaddyLock(host, func() error {
		if err := m.modifyRoute(host, serviceHost, func(handler *Handler) {
			for _, u := range handler.Upstreams {
				if u.Dial == upstream {
					u.Weight = weight
					found = true
					break
				}
			}
		}); err != nil {
			return err
		}

		m.persistConfigWithWarning(host)
		return nil
	}); err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("upstream %s not found for service %s", upstream, serviceHost)
	}

	m.log.HostSuccess(host, "Upstream weight updated")
	return nil
}

// modifyRoute finds the route for serviceHost and applies a transformation
// function to its first handler. Uses route-specific API with fallback.
func (m *Manager) modifyRoute(host, serviceHost string, transform func(*Handler)) error {
	routesPath := "/config/apps/http/servers/srv0/routes"

	data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
	if err != nil {
		return m.modifyRouteFull(host, serviceHost, transform)
	}

	var routes []*Route
	if err := json.Unmarshal(data, &routes); err != nil {
		return m.modifyRouteFull(host, serviceHost, transform)
	}

	for i, route := range routes {
		if routeMatchesHost(route, serviceHost) && len(route.Handle) > 0 {
			transform(route.Handle[0])

			routePath := fmt.Sprintf("%s/%d", routesPath, i)
			if _, err := m.caddyClient.apiRequest(host, "PATCH", routePath, route); err != nil {
				return m.modifyRouteFull(host, serviceHost, transform)
			}
			return nil
		}
	}

	return fmt.Errorf("no route found for host %s", serviceHost)
}

// modifyRouteFull is the fallback that reads and replaces the full config.
func (m *Manager) modifyRouteFull(host, serviceHost string, transform func(*Handler)) error {
	config, err := m.caddyClient.GetConfig(host)
	if err != nil {
		return err
	}

	if config.Apps == nil || config.Apps.HTTP == nil {
		return fmt.Errorf("no HTTP config found")
	}

	server := config.Apps.HTTP.Servers["srv0"]
	if server == nil {
		return fmt.Errorf("server not found")
	}

	found := false
	for _, route := range server.Routes {
		if routeMatchesHost(route, serviceHost) && len(route.Handle) > 0 {
			transform(route.Handle[0])
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no route found for host %s", serviceHost)
	}

	if err := m.caddyClient.LoadConfig(host, config); err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	return nil
}

type UpstreamWeight struct {
	Dial   string
	Weight int
}

// GetUpstreamWeights returns all upstreams with their weights for a service
// using route-specific API operations.
func (m *Manager) GetUpstreamWeights(host, serviceHost string) ([]UpstreamWeight, error) {
	// Try route-specific query first
	routesPath := "/config/apps/http/servers/srv0/routes"
	data, err := m.caddyClient.apiRequest(host, "GET", routesPath, nil)
	if err == nil {
		var routes []*Route
		if jsonErr := json.Unmarshal(data, &routes); jsonErr == nil {
			for _, route := range routes {
				if routeMatchesHost(route, serviceHost) && len(route.Handle) > 0 && route.Handle[0].Upstreams != nil {
					return extractWeights(route.Handle[0].Upstreams), nil
				}
			}
		}
	}

	// Fall back to full config
	config, err := m.caddyClient.GetConfig(host)
	if err != nil {
		return nil, err
	}

	if config.Apps == nil || config.Apps.HTTP == nil {
		return nil, fmt.Errorf("no HTTP config found")
	}

	server := config.Apps.HTTP.Servers["srv0"]
	if server == nil {
		return nil, fmt.Errorf("server not found")
	}

	for _, route := range server.Routes {
		if routeMatchesHost(route, serviceHost) && len(route.Handle) > 0 && route.Handle[0].Upstreams != nil {
			return extractWeights(route.Handle[0].Upstreams), nil
		}
	}

	return nil, fmt.Errorf("service %s not found", serviceHost)
}

func extractWeights(upstreams []*Upstream) []UpstreamWeight {
	weights := make([]UpstreamWeight, len(upstreams))
	for i, u := range upstreams {
		weight := u.Weight
		if weight == 0 {
			weight = 1
		}
		weights[i] = UpstreamWeight{
			Dial:   u.Dial,
			Weight: weight,
		}
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
