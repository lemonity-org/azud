package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/adriancarayol/azud/internal/ssh"
)

// CaddyClient manages Caddy proxy via its admin API
type CaddyClient struct {
	sshClient  *ssh.Client
	adminPort  int
	httpClient *http.Client
}

// NewCaddyClient creates a new Caddy client
func NewCaddyClient(sshClient *ssh.Client) *CaddyClient {
	return &CaddyClient{
		sshClient: sshClient,
		adminPort: 2019, // Caddy's default admin API port
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// CaddyConfig represents the full Caddy configuration
type CaddyConfig struct {
	Admin   *AdminConfig   `json:"admin,omitempty"`
	Apps    *AppsConfig    `json:"apps,omitempty"`
	Logging *LoggingConfig `json:"logging,omitempty"`
}

// AdminConfig configures the Caddy admin API
type AdminConfig struct {
	Listen string `json:"listen,omitempty"`
}

// AppsConfig holds Caddy application configurations
type AppsConfig struct {
	HTTP *HTTPApp `json:"http,omitempty"`
	TLS  *TLSApp  `json:"tls,omitempty"`
}

// HTTPApp configures the HTTP server
type HTTPApp struct {
	Servers map[string]*HTTPServer `json:"servers,omitempty"`
}

// HTTPServer represents an HTTP server configuration
type HTTPServer struct {
	Listen []string      `json:"listen,omitempty"`
	Routes []*Route      `json:"routes,omitempty"`
	Logs   *ServerLogs   `json:"logs,omitempty"`
}

// Route defines a routing rule
type Route struct {
	Match   []*Match   `json:"match,omitempty"`
	Handle  []*Handler `json:"handle,omitempty"`
	Terminal bool      `json:"terminal,omitempty"`
}

// Match defines matching criteria for a route
type Match struct {
	Host []string `json:"host,omitempty"`
	Path []string `json:"path,omitempty"`
}

// Handler defines how to handle matched requests
type Handler struct {
	Handler   string      `json:"handler"`
	Upstreams []*Upstream `json:"upstreams,omitempty"`

	// For static_response handler
	StatusCode      int                 `json:"status_code,omitempty"`
	StaticHeaders   map[string][]string `json:"headers,omitempty"`
	Body            string              `json:"body,omitempty"`

	// For reverse_proxy handler
	LoadBalancing   *LoadBalancing   `json:"load_balancing,omitempty"`
	HealthChecks    *HealthChecks    `json:"health_checks,omitempty"`
	ProxyHeaders    *HeadersConfig   `json:"header_up,omitempty"`
	Transport       *Transport       `json:"transport,omitempty"`
	FlushInterval   string           `json:"flush_interval,omitempty"`
}

// Upstream represents a backend server
type Upstream struct {
	Dial string `json:"dial"`
	// Weight for weighted round-robin load balancing (used for canary deployments)
	// Higher weight = more traffic. Default is 1 if omitted.
	Weight int `json:"weight,omitempty"`
}

// LoadBalancing configures load balancing
type LoadBalancing struct {
	SelectionPolicy *SelectionPolicy `json:"selection_policy,omitempty"`
}

// SelectionPolicy defines how to select upstreams
type SelectionPolicy struct {
	Policy string `json:"policy,omitempty"` // round_robin, least_conn, random, first, ip_hash, uri_hash, header
}

// HealthChecks configures health checking
type HealthChecks struct {
	Active  *ActiveHealthCheck  `json:"active,omitempty"`
	Passive *PassiveHealthCheck `json:"passive,omitempty"`
}

// ActiveHealthCheck configures active health checking
type ActiveHealthCheck struct {
	Path     string `json:"path,omitempty"`
	Port     int    `json:"port,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Headers  map[string][]string `json:"headers,omitempty"`
}

// PassiveHealthCheck configures passive health checking
type PassiveHealthCheck struct {
	FailDuration     string `json:"fail_duration,omitempty"`
	MaxFails         int    `json:"max_fails,omitempty"`
	UnhealthyLatency string `json:"unhealthy_latency,omitempty"`
}

// HeadersConfig configures header manipulation
type HeadersConfig struct {
	Request  *HeaderOps `json:"request,omitempty"`
	Response *HeaderOps `json:"response,omitempty"`
}

// HeaderOps defines header operations
type HeaderOps struct {
	Set    map[string][]string `json:"set,omitempty"`
	Add    map[string][]string `json:"add,omitempty"`
	Delete []string            `json:"delete,omitempty"`
}

// Transport configures the HTTP transport
type Transport struct {
	Protocol string `json:"protocol,omitempty"`
}

// TLSApp configures TLS/HTTPS
type TLSApp struct {
	Automation   *TLSAutomation     `json:"automation,omitempty"`
	Certificates *CertificatesConfig `json:"certificates,omitempty"`
}

// CertificatesConfig holds certificate loading configuration
type CertificatesConfig struct {
	LoadPEM []LoadedCertificate `json:"load_pem,omitempty"`
}

// LoadedCertificate represents a certificate loaded from PEM content
type LoadedCertificate struct {
	Certificate string   `json:"certificate"` // PEM content of the certificate
	Key         string   `json:"key"`         // PEM content of the private key
	Tags        []string `json:"tags,omitempty"`
}

// TLSAutomation configures automatic certificate management
type TLSAutomation struct {
	Policies []*TLSPolicy `json:"policies,omitempty"`
}

// TLSPolicy defines a TLS automation policy
type TLSPolicy struct {
	Subjects         []string  `json:"subjects,omitempty"`
	Issuers          []*Issuer `json:"issuers,omitempty"`
	OnDemand         bool      `json:"on_demand,omitempty"`
	DisableAutomatic bool      `json:"disable_automatic,omitempty"` // Disable automatic cert management for these subjects
}

// Issuer configures a certificate issuer
type Issuer struct {
	Module string `json:"module,omitempty"` // acme, zerossl, internal
	CA     string `json:"ca,omitempty"`     // For ACME: https://acme-v02.api.letsencrypt.org/directory
	Email  string `json:"email,omitempty"`
}

// LoggingConfig configures logging
type LoggingConfig struct {
	Logs map[string]*Log `json:"logs,omitempty"`
}

// Log configures a logger
type Log struct {
	Level   string   `json:"level,omitempty"`
	Writer  *Writer  `json:"writer,omitempty"`
	Encoder *Encoder `json:"encoder,omitempty"`
}

// Writer configures log output
type Writer struct {
	Output string `json:"output,omitempty"` // stdout, stderr, file
}

// Encoder configures log format
type Encoder struct {
	Format string `json:"format,omitempty"` // console, json
}

// ServerLogs configures per-server logging
type ServerLogs struct {
	DefaultLoggerName string `json:"default_logger_name,omitempty"`
}

// apiRequest executes an HTTP request against Caddy's admin API via SSH tunnel
func (c *CaddyClient) apiRequest(host, method, path string, body interface{}) ([]byte, error) {
	var bodyJSON []byte
	var err error

	if body != nil {
		bodyJSON, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
	}

	// Execute curl command via SSH to reach Caddy's admin API
	curlCmd := fmt.Sprintf("curl -s -X %s", method)
	if len(bodyJSON) > 0 {
		curlCmd += fmt.Sprintf(" -H 'Content-Type: application/json' -d '%s'", string(bodyJSON))
	}
	curlCmd += fmt.Sprintf(" http://localhost:%d%s", c.adminPort, path)

	result, err := c.sshClient.Execute(host, curlCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to execute API request: %w", err)
	}

	if result.ExitCode != 0 {
		return nil, fmt.Errorf("API request failed: %s", result.Stderr)
	}

	return []byte(result.Stdout), nil
}

// GetConfig retrieves the current Caddy configuration
func (c *CaddyClient) GetConfig(host string) (*CaddyConfig, error) {
	data, err := c.apiRequest(host, "GET", "/config/", nil)
	if err != nil {
		return nil, err
	}

	var config CaddyConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &config, nil
}

// SetConfig sets the full Caddy configuration
func (c *CaddyClient) SetConfig(host string, config *CaddyConfig) error {
	_, err := c.apiRequest(host, "POST", "/config/", config)
	return err
}

// LoadConfig loads a configuration from a path
func (c *CaddyClient) LoadConfig(host string, config *CaddyConfig) error {
	_, err := c.apiRequest(host, "POST", "/load", config)
	return err
}

// AddUpstream adds an upstream to a route
func (c *CaddyClient) AddUpstream(host, serverName string, routeIndex int, upstream *Upstream) error {
	path := fmt.Sprintf("/config/apps/http/servers/%s/routes/%d/handle/0/upstreams", serverName, routeIndex)

	// First get existing upstreams
	data, err := c.apiRequest(host, "GET", path, nil)
	if err != nil {
		// Path might not exist, try to create it
		_, err = c.apiRequest(host, "POST", path, []*Upstream{upstream})
		return err
	}

	var upstreams []*Upstream
	if err := json.Unmarshal(data, &upstreams); err != nil {
		return fmt.Errorf("failed to parse upstreams: %w", err)
	}

	upstreams = append(upstreams, upstream)
	_, err = c.apiRequest(host, "PATCH", path, upstreams)
	return err
}

// RemoveUpstream removes an upstream from a route
func (c *CaddyClient) RemoveUpstream(host, serverName string, routeIndex int, dial string) error {
	path := fmt.Sprintf("/config/apps/http/servers/%s/routes/%d/handle/0/upstreams", serverName, routeIndex)

	data, err := c.apiRequest(host, "GET", path, nil)
	if err != nil {
		return err
	}

	var upstreams []*Upstream
	if err := json.Unmarshal(data, &upstreams); err != nil {
		return fmt.Errorf("failed to parse upstreams: %w", err)
	}

	// Filter out the upstream to remove
	var filtered []*Upstream
	for _, u := range upstreams {
		if u.Dial != dial {
			filtered = append(filtered, u)
		}
	}

	_, err = c.apiRequest(host, "PATCH", path, filtered)
	return err
}

// UpstreamStatus represents the status of a reverse proxy upstream as
// reported by Caddy's /reverse_proxy/upstreams admin endpoint.
type UpstreamStatus struct {
	Address     string `json:"address"`
	Healthy     bool   `json:"healthy"`
	NumRequests int    `json:"num_requests"`
}

// GetUpstreamStatuses queries Caddy's admin API for all upstream statuses.
// Returns the active request counts and health status per upstream.
func (c *CaddyClient) GetUpstreamStatuses(host string) ([]UpstreamStatus, error) {
	data, err := c.apiRequest(host, "GET", "/reverse_proxy/upstreams", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to query upstream statuses: %w", err)
	}

	var statuses []UpstreamStatus
	if err := json.Unmarshal(data, &statuses); err != nil {
		return nil, fmt.Errorf("failed to parse upstream statuses: %w", err)
	}

	return statuses, nil
}

// GetUpstreamRequestCount returns the number of active requests for a
// specific upstream address (e.g., "my-app:3000"). Returns 0 if the
// upstream is not found (already removed).
func (c *CaddyClient) GetUpstreamRequestCount(host, upstreamAddr string) (int, error) {
	statuses, err := c.GetUpstreamStatuses(host)
	if err != nil {
		return 0, err
	}

	for _, s := range statuses {
		if s.Address == upstreamAddr {
			return s.NumRequests, nil
		}
	}

	// Upstream not found means it's already been removed — 0 active requests.
	return 0, nil
}

// Stop stops the Caddy server
func (c *CaddyClient) Stop(host string) error {
	_, err := c.apiRequest(host, "POST", "/stop", nil)
	return err
}

