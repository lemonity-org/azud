package config

import "time"

// Config represents the main deployment configuration
type Config struct {
	// Service name (used as container name prefix)
	Service string `yaml:"service"`

	// loadedSecrets holds secrets loaded from the secrets file (internal)
	loadedSecrets map[string]string `yaml:"-"`

	// Docker image name
	Image string `yaml:"image"`

	// Docker registry configuration
	Registry RegistryConfig `yaml:"registry"`

	// Target servers configuration by role
	Servers map[string]RoleConfig `yaml:"servers"`

	// Builder configuration
	Builder BuilderConfig `yaml:"builder"`

	// Environment variables
	Env EnvConfig `yaml:"env"`

	// Proxy configuration
	Proxy ProxyConfig `yaml:"proxy"`

	// Accessories (databases, caches, etc.)
	Accessories map[string]AccessoryConfig `yaml:"accessories"`

	// Deployment settings
	Deploy DeployConfig `yaml:"deploy"`

	// SSH configuration
	SSH SSHConfig `yaml:"ssh"`

	// Hooks configuration
	Hooks HooksConfig `yaml:"hooks"`

	// Cron jobs configuration
	Cron map[string]CronConfig `yaml:"cron"`

	// Volumes to mount
	Volumes []string `yaml:"volumes"`

	// Asset path for bridging between versions
	AssetPath string `yaml:"asset_path"`

	// Path to secrets file
	SecretsPath string `yaml:"secrets_path"`

	// Path to hooks directory
	HooksPath string `yaml:"hooks_path"`

	// Minimum Azud version required
	MinimumVersion string `yaml:"minimum_version"`

	// Command aliases
	Aliases map[string]string `yaml:"aliases"`
}

// RegistryConfig holds Docker registry settings
type RegistryConfig struct {
	// Registry server (e.g., ghcr.io, docker.io)
	Server string `yaml:"server"`

	// Registry username
	Username string `yaml:"username"`

	// Password or reference to secret
	Password []string `yaml:"password"`
}

// RoleConfig defines servers for a specific role
type RoleConfig struct {
	// List of host addresses
	Hosts []string `yaml:"hosts"`

	// Command to run (overrides default)
	Cmd string `yaml:"cmd"`

	// Container labels
	Labels map[string]string `yaml:"labels"`

	// Container options (memory, cpus, etc.)
	Options map[string]string `yaml:"options"`

	// Tags for filtering
	Tags []string `yaml:"tags"`

	// Environment variables specific to this role
	Env map[string]string `yaml:"env"`
}

// BuilderConfig holds build settings
type BuilderConfig struct {
	// Build for multiple architectures
	Multiarch bool `yaml:"multiarch"`

	// Target architecture
	Arch string `yaml:"arch"`

	// Cache configuration
	Cache CacheConfig `yaml:"cache"`

	// Build arguments
	Args map[string]string `yaml:"args"`

	// Dockerfile path
	Dockerfile string `yaml:"dockerfile"`

	// Build context
	Context string `yaml:"context"`

	// Remote builder configuration
	Remote RemoteBuilderConfig `yaml:"remote"`

	// Secrets for build
	Secrets []string `yaml:"secrets"`

	// Tag template with placeholders: {destination}, {version}, {timestamp}
	// Default: "{version}" for backward compatibility
	// Recommended for multi-env: "{destination}-{version}"
	TagTemplate string `yaml:"tag_template"`
}

// CacheConfig holds build cache settings
type CacheConfig struct {
	// Cache type: registry, local, gha
	Type string `yaml:"type"`

	// Cache options
	Options map[string]string `yaml:"options"`
}

// RemoteBuilderConfig holds remote builder settings
type RemoteBuilderConfig struct {
	// Remote host
	Host string `yaml:"host"`

	// Target architecture
	Arch string `yaml:"arch"`
}

// EnvConfig holds environment variable configuration
type EnvConfig struct {
	// Clear (non-secret) environment variables
	Clear map[string]string `yaml:"clear"`

	// Secret environment variable names
	Secret []string `yaml:"secret"`

	// Tag-specific environment variables
	Tags map[string]map[string]string `yaml:"tags"`
}

// ProxyConfig holds reverse proxy settings
type ProxyConfig struct {
	// Primary hostname for routing
	Host string `yaml:"host"`

	// Additional hostnames
	Hosts []string `yaml:"hosts"`

	// Enable SSL/TLS
	SSL bool `yaml:"ssl"`

	// Redirect HTTP to HTTPS
	SSLRedirect bool `yaml:"ssl_redirect"`

	// Email for Let's Encrypt ACME notifications
	ACMEEmail string `yaml:"acme_email"`

	// Use Let's Encrypt staging CA (for testing, avoids rate limits)
	ACMEStaging bool `yaml:"acme_staging"`

	// Custom SSL certificate (secret name containing PEM content)
	SSLCertificate string `yaml:"ssl_certificate"`

	// Custom SSL private key (secret name containing PEM content)
	SSLPrivateKey string `yaml:"ssl_private_key"`

	// Application port inside container
	AppPort int `yaml:"app_port"`

	// HTTP port for proxy (default 80)
	HTTPPort int `yaml:"http_port"`

	// HTTPS port for proxy (default 443)
	HTTPSPort int `yaml:"https_port"`

	// Health check configuration
	Healthcheck HealthcheckConfig `yaml:"healthcheck"`

	// Request/response buffering
	Buffering BufferingConfig `yaml:"buffering"`

	// Response timeout
	ResponseTimeout string `yaml:"response_timeout"`

	// Forward headers to backend
	ForwardHeaders bool `yaml:"forward_headers"`

	// Logging configuration
	Logging LoggingConfig `yaml:"logging"`
}

// HealthcheckConfig holds health check settings
type HealthcheckConfig struct {
	// Health check path
	Path string `yaml:"path"`

	// Check interval
	Interval string `yaml:"interval"`

	// Check timeout
	Timeout string `yaml:"timeout"`
}

// BufferingConfig holds buffering settings
type BufferingConfig struct {
	// Buffer requests
	Requests bool `yaml:"requests"`

	// Buffer responses
	Responses bool `yaml:"responses"`

	// Maximum request body size in bytes
	MaxRequestBody int64 `yaml:"max_request_body"`

	// Memory buffer size in bytes
	Memory int64 `yaml:"memory"`
}

// LoggingConfig holds logging settings
type LoggingConfig struct {
	// Request headers to log
	RequestHeaders []string `yaml:"request_headers"`

	// Response headers to log
	ResponseHeaders []string `yaml:"response_headers"`
}

// AccessoryConfig holds accessory (database, cache, etc.) settings
type AccessoryConfig struct {
	// Docker image
	Image string `yaml:"image"`

	// Single host
	Host string `yaml:"host"`

	// Multiple hosts
	Hosts []string `yaml:"hosts"`

	// Port mapping (host:container or ip:host:container)
	Port string `yaml:"port"`

	// Environment variables
	Env EnvConfig `yaml:"env"`

	// Volume mappings
	Volumes []string `yaml:"volumes"`

	// Files to upload and mount
	Files []FileMapping `yaml:"files"`

	// Directories to create and mount
	Directories []string `yaml:"directories"`

	// Command to run
	Cmd string `yaml:"cmd"`

	// Container options
	Options map[string]string `yaml:"options"`

	// Roles that need access to this accessory
	Roles []string `yaml:"roles"`
}

// FileMapping represents a file to upload and mount
type FileMapping struct {
	// Local file path
	Local string `yaml:"local"`

	// Remote file path
	Remote string `yaml:"remote"`

	// File mode (e.g., "0600")
	Mode string `yaml:"mode"`

	// File owner (e.g., "mysql:mysql")
	Owner string `yaml:"owner"`
}

// DeployConfig holds deployment settings
type DeployConfig struct {
	// Delay before starting health checks
	ReadinessDelay time.Duration `yaml:"readiness_delay"`

	// Maximum deployment time
	DeployTimeout time.Duration `yaml:"deploy_timeout"`

	// Time to drain connections
	DrainTimeout time.Duration `yaml:"drain_timeout"`

	// Number of old containers to retain
	RetainContainers int `yaml:"retain_containers"`

	// Number of deployment history records to retain
	RetainHistory int `yaml:"retain_history"`

	// Canary deployment configuration
	Canary CanaryConfig `yaml:"canary"`
}

// CanaryConfig holds canary deployment settings
type CanaryConfig struct {
	// Enable canary deployment mode
	Enabled bool `yaml:"enabled"`

	// Initial percentage of traffic to canary (0-100)
	InitialWeight int `yaml:"initial_weight"`

	// Percentage increase per step during auto-promote
	StepWeight int `yaml:"step_weight"`

	// Time between weight increases during auto-promote
	StepInterval time.Duration `yaml:"step_interval"`

	// Automatically promote canary if healthy
	AutoPromote bool `yaml:"auto_promote"`
}

// SSHConfig holds SSH connection settings
type SSHConfig struct {
	// SSH username
	User string `yaml:"user"`

	// SSH port
	Port int `yaml:"port"`

	// SSH key paths
	Keys []string `yaml:"keys"`

	// Proxy/jump host configuration
	Proxy SSHProxyConfig `yaml:"proxy"`

	// Connection timeout
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
}

// SSHProxyConfig holds SSH proxy/bastion settings
type SSHProxyConfig struct {
	// Proxy host
	Host string `yaml:"host"`

	// Proxy username
	User string `yaml:"user"`
}

// HooksConfig holds hook script paths
type HooksConfig struct {
	// Run before connecting to servers
	PreConnect string `yaml:"pre_connect"`

	// Run before building image
	PreBuild string `yaml:"pre_build"`

	// Run before deploying
	PreDeploy string `yaml:"pre_deploy"`

	// Run after deploying
	PostDeploy string `yaml:"post_deploy"`
}

// CronConfig holds cron job settings
type CronConfig struct {
	// Cron schedule expression (e.g., "0 0 * * *" for daily at midnight)
	Schedule string `yaml:"schedule"`

	// Command to run
	Command string `yaml:"command"`

	// Single host to run on (optional, defaults to first web host)
	Host string `yaml:"host"`

	// Multiple hosts to run on
	Hosts []string `yaml:"hosts"`

	// Log output to file
	LogPath string `yaml:"log_path"`

	// Timeout for the job (e.g., "1h", "30m")
	Timeout string `yaml:"timeout"`

	// Lock to prevent overlapping runs
	Lock bool `yaml:"lock"`

	// Environment variables specific to this job
	Env map[string]string `yaml:"env"`
}

// GetAllHosts returns all unique hosts from all roles
func (c *Config) GetAllHosts() []string {
	hostSet := make(map[string]bool)
	var hosts []string

	for _, role := range c.Servers {
		for _, host := range role.Hosts {
			if !hostSet[host] {
				hostSet[host] = true
				hosts = append(hosts, host)
			}
		}
	}

	return hosts
}

// GetRoleHosts returns hosts for a specific role
func (c *Config) GetRoleHosts(role string) []string {
	if r, ok := c.Servers[role]; ok {
		return r.Hosts
	}
	return nil
}

// GetAccessoryHosts returns all unique hosts used by accessories
func (c *Config) GetAccessoryHosts() []string {
	hostSet := make(map[string]bool)
	var hosts []string

	for _, acc := range c.Accessories {
		if acc.Host != "" && !hostSet[acc.Host] {
			hostSet[acc.Host] = true
			hosts = append(hosts, acc.Host)
		}
		for _, host := range acc.Hosts {
			if !hostSet[host] {
				hostSet[host] = true
				hosts = append(hosts, host)
			}
		}
	}

	return hosts
}

// HasRole checks if a role is defined
func (c *Config) HasRole(role string) bool {
	_, ok := c.Servers[role]
	return ok
}

// HasAccessory checks if an accessory is defined
func (c *Config) HasAccessory(name string) bool {
	_, ok := c.Accessories[name]
	return ok
}

// GetRoles returns all defined role names
func (c *Config) GetRoles() []string {
	roles := make([]string, 0, len(c.Servers))
	for role := range c.Servers {
		roles = append(roles, role)
	}
	return roles
}

// GetAccessoryNames returns all defined accessory names
func (c *Config) GetAccessoryNames() []string {
	names := make([]string, 0, len(c.Accessories))
	for name := range c.Accessories {
		names = append(names, name)
	}
	return names
}

// GetCronNames returns all defined cron job names
func (c *Config) GetCronNames() []string {
	names := make([]string, 0, len(c.Cron))
	for name := range c.Cron {
		names = append(names, name)
	}
	return names
}

// HasCron checks if a cron job is defined
func (c *Config) HasCron(name string) bool {
	_, ok := c.Cron[name]
	return ok
}

// GetCronHosts returns hosts for a specific cron job
func (c *Config) GetCronHosts(name string) []string {
	cron, ok := c.Cron[name]
	if !ok {
		return nil
	}

	if cron.Host != "" {
		return []string{cron.Host}
	}

	if len(cron.Hosts) > 0 {
		return cron.Hosts
	}

	// Default to first host from web role, or first host overall
	if hosts := c.GetRoleHosts("web"); len(hosts) > 0 {
		return []string{hosts[0]}
	}

	if hosts := c.GetAllHosts(); len(hosts) > 0 {
		return []string{hosts[0]}
	}

	return nil
}
