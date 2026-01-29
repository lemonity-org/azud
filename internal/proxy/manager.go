package proxy

import (
	"fmt"
	"time"

	"github.com/adriancarayol/azud/internal/output"
	"github.com/adriancarayol/azud/internal/podman"
	"github.com/adriancarayol/azud/internal/ssh"
)

const (
	// CaddyImage is the official Caddy container image
	CaddyImage = "caddy:2-alpine"

	// CaddyContainerName is the name of the Caddy container
	CaddyContainerName = "azud-proxy"

	// CaddyAdminPort is the admin API port
	CaddyAdminPort = 2019

	// CaddyHTTPPort is the HTTP port
	CaddyHTTPPort = 80

	// CaddyHTTPSPort is the HTTPS port
	CaddyHTTPSPort = 443
)

// Manager provisions and configures Caddy reverse proxies on remote hosts.
type Manager struct {
	sshClient   *ssh.Client
	caddyClient *CaddyClient
	podman      *podman.ContainerManager
	log         *output.Logger
}

func NewManager(sshClient *ssh.Client, log *output.Logger) *Manager {
	if log == nil {
		log = output.DefaultLogger
	}

	podmanClient := podman.NewClient(sshClient)

	return &Manager{
		sshClient:   sshClient,
		caddyClient: NewCaddyClient(sshClient),
		podman:      podman.NewContainerManager(podmanClient),
		log:         log,
	}
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
}

// Boot starts the Caddy proxy on a host
func (m *Manager) Boot(host string, config *ProxyConfig) error {
	m.log.Host(host, "Starting proxy...")

	// Check if already running
	running, err := m.podman.IsRunning(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to check proxy status: %w", err)
	}

	if running {
		m.log.Host(host, "Proxy already running")
		return nil
	}

	// Check if container exists but is stopped
	exists, err := m.podman.Exists(host, CaddyContainerName)
	if err != nil {
		return fmt.Errorf("failed to check proxy container: %w", err)
	}

	if exists {
		// Start existing container
		if err := m.podman.Start(host, CaddyContainerName); err != nil {
			return fmt.Errorf("failed to start proxy: %w", err)
		}
		m.log.HostSuccess(host, "Proxy started")
		return nil
	}

	// Determine ports to use (use config if provided, otherwise defaults)
	httpPort := CaddyHTTPPort
	httpsPort := CaddyHTTPSPort
	if config != nil {
		if config.HTTPPort > 0 {
			httpPort = config.HTTPPort
		}
		if config.HTTPSPort > 0 {
			httpsPort = config.HTTPSPort
		}
	}

	// Create and start new container with custom admin API config
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
		// Start Caddy with admin API listening on 0.0.0.0 so it's accessible via container port forwarding
		Command: []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile", "--watch"},
		Env: map[string]string{
			"CADDY_ADMIN": "0.0.0.0:2019",
		},
	}

	_, err = m.podman.Run(host, containerConfig)
	if err != nil {
		return fmt.Errorf("failed to start proxy container: %w", err)
	}

	// Wait for Caddy to be ready
	time.Sleep(2 * time.Second)

	// Load initial configuration
	if err := m.loadInitialConfig(host, config); err != nil {
		m.log.Warn("Failed to load initial config: %v", err)
	}

	m.log.HostSuccess(host, "Proxy started")
	return nil
}

func (m *Manager) loadInitialConfig(host string, config *ProxyConfig) error {
	caddyConfig := &CaddyConfig{
		Admin: &AdminConfig{
			Listen: "0.0.0.0:2019",
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

	// Check if custom SSL certificates are provided
	if config != nil && config.SSLCertificate != "" && config.SSLPrivateKey != "" {
		m.log.Host(host, "Configuring custom SSL certificates...")

		// Load custom certificates via load_pem
		caddyConfig.Apps.TLS = &TLSApp{
			Certificates: &CertificatesConfig{
				LoadPEM: []LoadedCertificate{
					{
						Certificate: config.SSLCertificate,
						Key:         config.SSLPrivateKey,
					},
				},
			},
			// Disable automatic certificate management for these hosts
			Automation: &TLSAutomation{
				Policies: []*TLSPolicy{
					{
						Subjects: config.Hosts,
						// Empty issuers array disables automatic certificate management
						Issuers: []*Issuer{},
					},
				},
			},
		}
	} else if config != nil && config.AutoHTTPS && config.Email != "" {
		// Add TLS automation if HTTPS is enabled (Let's Encrypt)
		issuer := &Issuer{
			Module: "acme",
			Email:  config.Email,
		}

		// Use staging CA if configured (avoids rate limits during testing)
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

	m.log.HostSuccess(host, "Proxy rebooted")
	return nil
}

// Remove removes the Caddy proxy container
func (m *Manager) Remove(host string) error {
	m.log.Host(host, "Removing proxy...")

	if err := m.podman.Remove(host, CaddyContainerName, true); err != nil {
		return fmt.Errorf("failed to remove proxy: %w", err)
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

// RegisterService registers a service with the proxy
func (m *Manager) RegisterService(host string, service *ServiceConfig) error {
	m.log.Host(host, "Registering service %s...", service.Name)

	// Build the route for this service
	route := m.buildServiceRoute(service)

	// Get current config
	config, err := m.caddyClient.GetConfig(host)
	if err != nil {
		// If we can't get config, create a new one
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

	// Ensure server exists
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

	// Check if route for this host already exists
	server := config.Apps.HTTP.Servers["srv0"]
	found := false
	for i, r := range server.Routes {
		if routeMatchesHost(r, service.Host) {
			// Update existing route
			server.Routes[i] = route
			found = true
			break
		}
	}

	if !found {
		// Add new route
		server.Routes = append(server.Routes, route)
	}

	// Apply configuration
	if err := m.caddyClient.LoadConfig(host, config); err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	m.log.HostSuccess(host, "Service %s registered", service.Name)
	return nil
}

type ServiceConfig struct {
	// Service name
	Name string

	// Hostname for routing
	Host string

	// Upstream addresses (host:port)
	Upstreams []string

	// Health check path
	HealthPath string

	// Health check interval
	HealthInterval string

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

	// Add health checks if configured
	if service.HealthPath != "" {
		interval := service.HealthInterval
		if interval == "" {
			interval = "10s"
		}
		handler.HealthChecks = &HealthChecks{
			Active: &ActiveHealthCheck{
				Path:     service.HealthPath,
				Interval: interval,
				Timeout:  "5s",
			},
			Passive: &PassiveHealthCheck{
				FailDuration: "30s",
				MaxFails:     3,
			},
		}
	}

	route := &Route{
		Match: []*Match{
			{Host: []string{service.Host}},
		},
		Handle:   []*Handler{handler},
		Terminal: true,
	}

	return route
}

// DeregisterService removes a service from the proxy
func (m *Manager) DeregisterService(host, serviceHost string) error {
	m.log.Host(host, "Deregistering service for %s...", serviceHost)

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

	// Filter out the route for this host
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

	m.log.HostSuccess(host, "Service deregistered")
	return nil
}

// AddUpstream adds an upstream to an existing service
func (m *Manager) AddUpstream(host, serviceHost, upstream string) error {
	m.log.Host(host, "Adding upstream %s to %s...", upstream, serviceHost)

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

	// Find the route and add upstream
	for _, route := range server.Routes {
		if routeMatchesHost(route, serviceHost) {
			if len(route.Handle) > 0 {
				route.Handle[0].Upstreams = append(route.Handle[0].Upstreams, &Upstream{Dial: upstream})
			}
			break
		}
	}

	if err := m.caddyClient.LoadConfig(host, config); err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	m.log.HostSuccess(host, "Upstream added")
	return nil
}

// RemoveUpstream removes an upstream from a service
func (m *Manager) RemoveUpstream(host, serviceHost, upstream string) error {
	m.log.Host(host, "Removing upstream %s from %s...", upstream, serviceHost)

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

	// Find the route and remove upstream
	for _, route := range server.Routes {
		if routeMatchesHost(route, serviceHost) {
			if len(route.Handle) > 0 {
				var filtered []*Upstream
				for _, u := range route.Handle[0].Upstreams {
					if u.Dial != upstream {
						filtered = append(filtered, u)
					}
				}
				route.Handle[0].Upstreams = filtered
			}
			break
		}
	}

	if err := m.caddyClient.LoadConfig(host, config); err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	m.log.HostSuccess(host, "Upstream removed")
	return nil
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

// BootAll starts the proxy on multiple hosts
func (m *Manager) BootAll(hosts []string, config *ProxyConfig) error {
	m.log.Header("Starting proxy on %d host(s)", len(hosts))

	var errors []error
	for _, host := range hosts {
		if err := m.Boot(host, config); err != nil {
			errors = append(errors, fmt.Errorf("%s: %w", host, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed on %d host(s)", len(errors))
	}

	return nil
}

// AddWeightedUpstream adds an upstream with a specific weight (for canary deployments)
func (m *Manager) AddWeightedUpstream(host, serviceHost, upstream string, weight int) error {
	m.log.Host(host, "Adding weighted upstream %s (weight=%d) to %s...", upstream, weight, serviceHost)

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

	// Find the route and add weighted upstream
	for _, route := range server.Routes {
		if routeMatchesHost(route, serviceHost) {
			if len(route.Handle) > 0 {
				// Add upstream with weight
				route.Handle[0].Upstreams = append(route.Handle[0].Upstreams, &Upstream{
					Dial:   upstream,
					Weight: weight,
				})

				// Ensure we're using weighted round robin for load balancing
				if route.Handle[0].LoadBalancing == nil {
					route.Handle[0].LoadBalancing = &LoadBalancing{}
				}
				if route.Handle[0].LoadBalancing.SelectionPolicy == nil {
					route.Handle[0].LoadBalancing.SelectionPolicy = &SelectionPolicy{}
				}
				route.Handle[0].LoadBalancing.SelectionPolicy.Policy = "weighted_round_robin"
			}
			break
		}
	}

	if err := m.caddyClient.LoadConfig(host, config); err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	m.log.HostSuccess(host, "Weighted upstream added")
	return nil
}

// SetUpstreamWeight modifies the weight of an existing upstream
func (m *Manager) SetUpstreamWeight(host, serviceHost, upstream string, weight int) error {
	m.log.Host(host, "Setting weight=%d for upstream %s on %s...", weight, upstream, serviceHost)

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

	// Find the route and update upstream weight
	found := false
	for _, route := range server.Routes {
		if routeMatchesHost(route, serviceHost) {
			if len(route.Handle) > 0 {
				for _, u := range route.Handle[0].Upstreams {
					if u.Dial == upstream {
						u.Weight = weight
						found = true
						break
					}
				}
			}
			break
		}
	}

	if !found {
		return fmt.Errorf("upstream %s not found for service %s", upstream, serviceHost)
	}

	if err := m.caddyClient.LoadConfig(host, config); err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	m.log.HostSuccess(host, "Upstream weight updated")
	return nil
}

type UpstreamWeight struct {
	Dial   string
	Weight int
}

// GetUpstreamWeights returns all upstreams with their weights for a service
func (m *Manager) GetUpstreamWeights(host, serviceHost string) ([]UpstreamWeight, error) {
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

	// Find the route and get upstream weights
	for _, route := range server.Routes {
		if routeMatchesHost(route, serviceHost) {
			if len(route.Handle) > 0 && route.Handle[0].Upstreams != nil {
				weights := make([]UpstreamWeight, len(route.Handle[0].Upstreams))
				for i, u := range route.Handle[0].Upstreams {
					weight := u.Weight
					if weight == 0 {
						weight = 1 // Default weight
					}
					weights[i] = UpstreamWeight{
						Dial:   u.Dial,
						Weight: weight,
					}
				}
				return weights, nil
			}
		}
	}

	return nil, fmt.Errorf("service %s not found", serviceHost)
}
