package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/shell"
	"github.com/lemonity-org/azud/internal/ssh"
)

// CaddyClient manages the Caddy proxy through its admin API. The API listens
// only on a unix socket inside the proxy container, so every request runs the
// image's curl through `podman exec`.
type CaddyClient struct {
	sshClient *ssh.Client
	podmanCmd string
	container string
	log       *output.Logger
}

// NewCaddyClient creates a Caddy client. podmanCmd is the command prefix used
// to reach the proxy container, for example "sudo -n podman" for a rootful
// proxy managed by a non-root SSH user.
func NewCaddyClient(sshClient *ssh.Client, podmanCmd string, log *output.Logger) *CaddyClient {
	podmanCmd = strings.TrimSpace(podmanCmd)
	if podmanCmd == "" {
		podmanCmd = "podman"
	}
	if log == nil {
		log = output.DefaultLogger
	}
	return &CaddyClient{
		sshClient: sshClient,
		podmanCmd: podmanCmd,
		container: CaddyContainerName,
		log:       log,
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
	Listen           []string         `json:"listen,omitempty"`
	Routes           []*Route         `json:"routes,omitempty"`
	Logs             *ServerLogs      `json:"logs,omitempty"`
	AutoHTTPS        *AutoHTTPSConfig `json:"automatic_https,omitempty"`
	MaxHeaderBytes   int              `json:"max_header_bytes,omitempty"`
	EnableFullDuplex bool             `json:"enable_full_duplex,omitempty"`
}

// Route defines a routing rule
type Route struct {
	ID       string     `json:"@id,omitempty"`
	Match    []*Match   `json:"match,omitempty"`
	Handle   []*Handler `json:"handle,omitempty"`
	Terminal bool       `json:"terminal,omitempty"`
}

// Match defines matching criteria for a route
type Match struct {
	Host []string `json:"host,omitempty"`
	Path []string `json:"path,omitempty"`
}

// Handler defines how to handle matched requests
type Handler struct {
	ID        string      `json:"@id,omitempty"`
	Handler   string      `json:"handler"`
	Upstreams []*Upstream `json:"upstreams,omitempty"`

	// For static_response handler
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`

	// For reverse_proxy handler
	LoadBalancing    *LoadBalancing `json:"load_balancing,omitempty"`
	HealthChecks     *HealthChecks  `json:"health_checks,omitempty"`
	Transport        *Transport     `json:"transport,omitempty"`
	FlushInterval    string         `json:"flush_interval,omitempty"`
	StreamCloseDelay string         `json:"stream_close_delay,omitempty"`
	BufferRequests   bool           `json:"buffer_requests,omitempty"`
	BufferResponses  bool           `json:"buffer_responses,omitempty"`

	// Headers configures reverse_proxy request and response header operations.
	// Static response headers are intentionally not modeled on this handler.
	Headers *HeadersConfig `json:"headers,omitempty"`

	// For request_body handler
	MaxSize int64 `json:"max_size,omitempty"`
}

// Upstream represents a backend server
type Upstream struct {
	Dial string `json:"dial"`
}

// LoadBalancing configures load balancing
type LoadBalancing struct {
	SelectionPolicy *SelectionPolicy `json:"selection_policy,omitempty"`

	// TryDuration bounds how long Caddy keeps retrying a request while no
	// upstream is available, instead of answering 503 immediately.
	TryDuration string `json:"try_duration,omitempty"`

	// TryInterval is the pause between those retries.
	TryInterval string `json:"try_interval,omitempty"`
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
	Path     string              `json:"path,omitempty"`
	Port     int                 `json:"port,omitempty"`
	Interval string              `json:"interval,omitempty"`
	Timeout  string              `json:"timeout,omitempty"`
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
	Protocol              string             `json:"protocol,omitempty"`
	ResponseHeaderTimeout string             `json:"response_header_timeout,omitempty"`
	ReadTimeout           string             `json:"read_timeout,omitempty"`
	Versions              []string           `json:"versions,omitempty"`
	TLS                   *UpstreamTLSConfig `json:"tls,omitempty"`
}

// UpstreamTLSConfig enables TLS with Caddy's secure defaults. Additional
// trust and SNI controls can be added here without changing the route shape.
type UpstreamTLSConfig struct{}

// AutoHTTPSConfig configures automatic HTTPS behavior.
type AutoHTTPSConfig struct {
	Disable             bool `json:"disable,omitempty"`
	DisableRedirects    bool `json:"disable_redirects,omitempty"`
	DisableCertificates bool `json:"disable_certificates,omitempty"`
}

// TLSApp configures TLS/HTTPS
type TLSApp struct {
	Automation   *TLSAutomation      `json:"automation,omitempty"`
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
	Format string             `json:"format,omitempty"` // console, json, filter
	Wrap   *Encoder           `json:"wrap,omitempty"`
	Fields map[string]*Filter `json:"fields,omitempty"`
}

// Filter configures a log field filter.
type Filter struct {
	Filter string `json:"filter,omitempty"`
}

// ServerLogs configures per-server logging
type ServerLogs struct {
	DefaultLoggerName string `json:"default_logger_name,omitempty"`
}

// apiRequest executes an HTTP request against Caddy's admin API through the
// private unix socket inside the proxy container.
func (c *CaddyClient) apiRequest(host, method, path string, body interface{}) ([]byte, error) {
	var bodyJSON []byte
	var err error

	if isCaddyAdminPath(path) {
		return nil, fmt.Errorf("refusing Caddy admin request %s %s: Azud manages the admin endpoint", method, path)
	}

	if body != nil {
		bodyJSON, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
	}

	// A complete configuration carries its own admin object. Replace it at
	// this single choke point so no load, restore or rollback can move the
	// admin endpoint off the private socket.
	if len(bodyJSON) > 0 && replacesWholeCaddyConfig(path) {
		normalized, dropped, err := normalizeAdminConfig(bodyJSON)
		if err != nil {
			return nil, fmt.Errorf("invalid Caddy configuration for %s: %w", path, err)
		}
		if len(dropped) > 0 {
			c.log.Warn("Removed unexpected Caddy admin settings (%s) on %s", strings.Join(dropped, ", "), host)
		}
		bodyJSON = normalized
	}

	cmd, err := c.adminCommand(method, path, len(bodyJSON) > 0)
	if err != nil {
		return nil, err
	}

	// The body is piped via stdin so configuration content, including TLS
	// keys, never appears in a remote command line.
	var result *ssh.Result
	if len(bodyJSON) > 0 {
		result, err = c.sshClient.ExecuteWithStdin(host, cmd, bytes.NewReader(bodyJSON))
	} else {
		result, err = c.sshClient.Execute(host, cmd)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to execute API request: %w", err)
	}

	if result.ExitCode != 0 {
		return nil, adminRequestError(result)
	}

	return []byte(result.Stdout), nil
}

// adminCommand builds the remote command for one admin API request. curl's
// -f (--fail) turns HTTP 4xx/5xx answers into a non-zero exit so a rejected
// configuration change cannot fail silently.
func (c *CaddyClient) adminCommand(method, path string, withBody bool) (string, error) {
	if !validAdminMethod(method) || !validAdminPath(path) {
		return "", fmt.Errorf("invalid Caddy admin request %s %q", method, path)
	}

	args := []string{"exec"}
	if withBody {
		args = append(args, "-i")
	}
	args = append(args, c.container,
		"curl", "-sSf", "--max-time", "30",
		"--unix-socket", CaddyAdminSocket,
		"-X", method,
	)
	if withBody {
		args = append(args, "-H", "Content-Type: application/json", "--data-binary", "@-")
	}
	args = append(args, "http://localhost"+path)

	return c.podmanCmd + " " + shell.Join(args...), nil
}

func validAdminMethod(method string) bool {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

func validAdminPath(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	for _, r := range path {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// isCaddyAdminPath reports whether a request addresses the admin endpoint's
// own configuration. Changing it through the API could move the listener off
// the private socket, so Azud never sends such requests.
func isCaddyAdminPath(path string) bool {
	return path == "/config/admin" || strings.HasPrefix(path, "/config/admin/")
}

// replacesWholeCaddyConfig reports whether a request body is a complete Caddy
// configuration, which carries its own admin object.
func replacesWholeCaddyConfig(path string) bool {
	switch path {
	case "/load", "/config", "/config/":
		return true
	default:
		return false
	}
}

// errProxyNotRunning marks an admin request that failed because the proxy
// container does not exist or is not running.
var errProxyNotRunning = fmt.Errorf("proxy container %s is not running", CaddyContainerName)

// adminRequestError maps the failure modes of `podman exec ... curl` to
// actionable errors. Older Azud releases served the admin API on TCP, so a
// proxy container they created has no socket until it is recreated.
func adminRequestError(result *ssh.Result) error {
	stderr := strings.TrimSpace(result.Stderr)
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "no such container"),
		strings.Contains(lower, "container state improper"),
		strings.Contains(lower, "can only create exec sessions on running containers"):
		return fmt.Errorf("caddy admin API unavailable: %w: %s", errProxyNotRunning, stderr)
	case result.ExitCode == 7,
		strings.Contains(lower, "failed to connect"),
		strings.Contains(lower, "could not connect"):
		return fmt.Errorf("caddy admin socket %s is not accepting connections; if this proxy was created by an older Azud release, run 'azud proxy reboot': %s", CaddyAdminSocket, stderr)
	case result.ExitCode == 28:
		// A listener moved away from the socket leaves it unanswered rather
		// than refused, so curl only gives up at --max-time.
		return fmt.Errorf("caddy admin socket %s did not answer; if its listener was moved or this proxy was created by an older Azud release, run 'azud proxy reboot': %s", CaddyAdminSocket, stderr)
	default:
		return fmt.Errorf("API request failed: %s", result.Stderr)
	}
}

// normalizeAdminConfig replaces the admin object of a complete Caddy
// configuration with Azud's private socket listener and keeps every other key
// unchanged, including large integers. It returns the admin keys other than
// listen that were removed. Keys such as remote, identity or config.load would
// otherwise let a previously modified configuration reopen or replace the
// control plane on the next load or restart.
func normalizeAdminConfig(data []byte) ([]byte, []string, error) {
	config, err := decodeJSONObject(data)
	if err != nil {
		return nil, nil, err
	}
	var dropped []string
	if admin, ok := config["admin"].(map[string]interface{}); ok {
		for key := range admin {
			if key != "listen" {
				dropped = append(dropped, key)
			}
		}
	}
	sort.Strings(dropped)
	config["admin"] = map[string]interface{}{"listen": CaddyAdminListen}
	normalized, err := json.Marshal(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode normalized Caddy configuration: %w", err)
	}
	return normalized, dropped, nil
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

// LoadConfig loads a typed Caddy configuration.
func (c *CaddyClient) LoadConfig(host string, config *CaddyConfig) error {
	_, err := c.apiRequest(host, "POST", "/load", config)
	return err
}

// LoadRawConfig loads already validated JSON without discarding fields Azud
// does not model. This is required for safe rollback and persisted restores.
// apiRequest still replaces the admin object with the private socket listener.
func (c *CaddyClient) LoadRawConfig(host string, data []byte) error {
	if !json.Valid(data) {
		return fmt.Errorf("invalid raw Caddy configuration")
	}
	_, err := c.apiRequest(host, "POST", "/load", json.RawMessage(data))
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
