package config

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Loader handles configuration file loading and merging
type Loader struct {
	basePath    string
	destination string
}

// NewLoader creates a new configuration loader
func NewLoader(basePath, destination string) *Loader {
	return &Loader{
		basePath:    basePath,
		destination: destination,
	}
}

// Load reads and parses the configuration file(s)
func (l *Loader) Load() (*Config, error) {
	// Load base configuration
	cfg, err := l.loadFile(l.basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config %s: %w", l.basePath, err)
	}

	// Load destination-specific configuration if specified
	if l.destination != "" {
		destPath := l.getDestinationPath()
		if _, err := os.Stat(destPath); err == nil {
			destCfg, destNode, err := l.loadFileWithNode(destPath)
			if err != nil {
				return nil, fmt.Errorf("failed to load destination config %s: %w", destPath, err)
			}
			cfg = mergeConfigs(cfg, destCfg, destNode)
		}
	}

	// Apply defaults
	applyDefaults(cfg)

	// Load secrets
	if err := l.loadSecrets(cfg); err != nil {
		return nil, fmt.Errorf("failed to load secrets: %w", err)
	}

	// Validate configuration
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return cfg, nil
}

// maxConfigFileSize is the maximum allowed size for a configuration file.
// This prevents memory exhaustion from extremely large or malicious YAML input.
const maxConfigFileSize = 1 << 20 // 1 MiB

// safeExpandEnv performs environment variable expansion only for variables
// whose names match safe patterns (uppercase letters, digits, underscores).
// This prevents accidental leakage of sensitive variables like AWS_SECRET_KEY
// that might match patterns in the YAML file.
func safeExpandEnv(s string) string {
	return os.Expand(s, func(key string) string {
		// Only expand variables that look like intentional config references:
		// uppercase letters, digits, and underscores (e.g., APP_NAME, PORT).
		for _, c := range key {
			if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
				return fmt.Sprintf("${%s}", key) // leave unexpanded
			}
		}
		return os.Getenv(key)
	})
}

// loadFile reads and parses a single YAML file
func (l *Loader) loadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("config file exceeds maximum size (%d bytes)", maxConfigFileSize)
	}

	// Expand only safe environment variables
	data = []byte(safeExpandEnv(string(data)))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	return &cfg, nil
}

func (l *Loader) loadFileWithNode(path string) (*Config, *yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	if len(data) > maxConfigFileSize {
		return nil, nil, fmt.Errorf("config file exceeds maximum size (%d bytes)", maxConfigFileSize)
	}

	data = []byte(safeExpandEnv(string(data)))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, nil, fmt.Errorf("failed to parse YAML nodes: %w", err)
	}

	return &cfg, &node, nil
}

// getDestinationPath returns the path for destination-specific config
func (l *Loader) getDestinationPath() string {
	dir := filepath.Dir(l.basePath)
	ext := filepath.Ext(l.basePath)
	base := strings.TrimSuffix(filepath.Base(l.basePath), ext)

	return filepath.Join(dir, fmt.Sprintf("%s.%s%s", base, l.destination, ext))
}

// loadSecrets loads secrets from the secrets file
func (l *Loader) loadSecrets(cfg *Config) error {
	provider := strings.ToLower(strings.TrimSpace(cfg.SecretsProvider))
	if provider == "" {
		provider = "file"
	}

	switch provider {
	case "file":
		return l.loadSecretsFromFile(cfg)
	case "env":
		return l.loadSecretsFromEnv(cfg)
	case "command":
		return l.loadSecretsFromCommand(cfg)
	default:
		return fmt.Errorf("unknown secrets_provider: %s", provider)
	}
}

func (l *Loader) loadSecretsFromFile(cfg *Config) error {
	secretsPath := cfg.SecretsPath
	if secretsPath == "" {
		secretsPath = ".azud/secrets"
	}

	// Prevent path traversal: reject paths with ".." components.
	cleaned := filepath.Clean(secretsPath)
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("secrets_path must not contain path traversal (..): %s", secretsPath)
	}

	// Check if secrets file exists
	info, err := os.Stat(secretsPath)
	if os.IsNotExist(err) {
		return nil // No secrets file, that's okay
	}
	if err != nil {
		return err
	}

	// Warn if the secrets file is readable by group or others
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		fmt.Fprintf(os.Stderr, "WARNING: secrets file %s has insecure permissions %04o (recommended: 0600)\n", secretsPath, perm)
	}

	file, err := os.Open(secretsPath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	secrets := make(map[string]string)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse KEY=VALUE
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Remove surrounding quotes if present
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		secrets[key] = value
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// Store secrets for later use
	cfg.loadedSecrets = secrets

	// Also set global store for GetSecret function
	SetLoadedSecrets(secrets)

	return nil
}

func (l *Loader) loadSecretsFromEnv(cfg *Config) error {
	prefix := cfg.SecretsEnvPrefix
	if prefix == "" {
		return fmt.Errorf("secrets_env_prefix is required when secrets_provider=env")
	}

	secrets := make(map[string]string)
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		name := strings.TrimPrefix(key, prefix)
		if name == "" {
			continue
		}
		secrets[name] = parts[1]
	}

	cfg.loadedSecrets = secrets
	SetLoadedSecrets(secrets)
	return nil
}

func (l *Loader) loadSecretsFromCommand(cfg *Config) error {
	if strings.TrimSpace(cfg.SecretsCommand) == "" {
		return fmt.Errorf("secrets_command is required when secrets_provider=command")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cfg.SecretsCommand)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("secrets_command timed out after 30s")
		}
		return fmt.Errorf("secrets_command failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	secrets := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		secrets[key] = value
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	cfg.loadedSecrets = secrets
	SetLoadedSecrets(secrets)
	return nil
}

// mergeConfigs merges destination config into base config
func mergeConfigs(base, dest *Config, destNode *yaml.Node) *Config {
	// Create a copy of base
	merged := *base
	has := func(path ...string) bool {
		return nodeHasPath(destNode, path...)
	}

	// Override with destination values if set
	if dest.Service != "" {
		merged.Service = dest.Service
	}
	if dest.Image != "" {
		merged.Image = dest.Image
	}

	// Merge servers
	if len(dest.Servers) > 0 {
		if merged.Servers == nil {
			merged.Servers = make(map[string]RoleConfig)
		}
		for role, config := range dest.Servers {
			merged.Servers[role] = config
		}
	}

	// Merge registry
	if dest.Registry.Server != "" {
		merged.Registry.Server = dest.Registry.Server
	}
	if dest.Registry.Username != "" {
		merged.Registry.Username = dest.Registry.Username
	}
	if len(dest.Registry.Password) > 0 {
		merged.Registry.Password = dest.Registry.Password
	}

	// Merge env
	if has("env", "clear") {
		merged.Env.Clear = dest.Env.Clear // replace (empty map clears)
	} else if len(dest.Env.Clear) > 0 {
		if merged.Env.Clear == nil {
			merged.Env.Clear = make(map[string]string)
		}
		for k, v := range dest.Env.Clear {
			merged.Env.Clear[k] = v
		}
	}
	if has("env", "secret") {
		merged.Env.Secret = dest.Env.Secret // replace (empty list clears)
	} else if len(dest.Env.Secret) > 0 {
		merged.Env.Secret = append(merged.Env.Secret, dest.Env.Secret...)
	}
	if len(dest.Env.Tags) > 0 {
		if merged.Env.Tags == nil {
			merged.Env.Tags = make(map[string]map[string]string)
		}
		for tag, values := range dest.Env.Tags {
			if merged.Env.Tags[tag] == nil {
				merged.Env.Tags[tag] = make(map[string]string)
			}
			for k, v := range values {
				merged.Env.Tags[tag][k] = v
			}
		}
	}

	// Merge proxy
	if dest.Proxy.Host != "" {
		merged.Proxy.Host = dest.Proxy.Host
	}
	if len(dest.Proxy.Hosts) > 0 {
		merged.Proxy.Hosts = dest.Proxy.Hosts
	}
	if has("proxy", "app_port") || destNode == nil && dest.Proxy.AppPort != 0 {
		merged.Proxy.AppPort = dest.Proxy.AppPort
	}
	if has("proxy", "http_port") || destNode == nil && dest.Proxy.HTTPPort != 0 {
		merged.Proxy.HTTPPort = dest.Proxy.HTTPPort
	}
	if has("proxy", "https_port") || destNode == nil && dest.Proxy.HTTPSPort != 0 {
		merged.Proxy.HTTPSPort = dest.Proxy.HTTPSPort
	}
	if has("proxy", "ssl") || destNode == nil && dest.Proxy.SSL {
		merged.Proxy.SSL = dest.Proxy.SSL
	}
	if has("proxy", "ssl_redirect") || destNode == nil && dest.Proxy.SSLRedirect {
		merged.Proxy.SSLRedirect = dest.Proxy.SSLRedirect
	}
	if dest.Proxy.ACMEEmail != "" {
		merged.Proxy.ACMEEmail = dest.Proxy.ACMEEmail
	}
	if has("proxy", "acme_staging") || destNode == nil && dest.Proxy.ACMEStaging {
		merged.Proxy.ACMEStaging = dest.Proxy.ACMEStaging
	}
	if dest.Proxy.SSLCertificate != "" {
		merged.Proxy.SSLCertificate = dest.Proxy.SSLCertificate
	}
	if dest.Proxy.SSLPrivateKey != "" {
		merged.Proxy.SSLPrivateKey = dest.Proxy.SSLPrivateKey
	}
	if dest.Proxy.Healthcheck.Path != "" {
		merged.Proxy.Healthcheck.Path = dest.Proxy.Healthcheck.Path
	}
	if dest.Proxy.Healthcheck.ReadinessPath != "" {
		merged.Proxy.Healthcheck.ReadinessPath = dest.Proxy.Healthcheck.ReadinessPath
	}
	if dest.Proxy.Healthcheck.LivenessPath != "" {
		merged.Proxy.Healthcheck.LivenessPath = dest.Proxy.Healthcheck.LivenessPath
	}
	if has("proxy", "healthcheck", "disable_liveness") || destNode == nil && dest.Proxy.Healthcheck.DisableLiveness {
		merged.Proxy.Healthcheck.DisableLiveness = dest.Proxy.Healthcheck.DisableLiveness
	}
	if dest.Proxy.Healthcheck.LivenessCmd != "" {
		merged.Proxy.Healthcheck.LivenessCmd = dest.Proxy.Healthcheck.LivenessCmd
	}
	if dest.Proxy.Healthcheck.Interval != "" {
		merged.Proxy.Healthcheck.Interval = dest.Proxy.Healthcheck.Interval
	}
	if dest.Proxy.Healthcheck.Timeout != "" {
		merged.Proxy.Healthcheck.Timeout = dest.Proxy.Healthcheck.Timeout
	}
	if dest.Proxy.Healthcheck.HelperImage != "" {
		merged.Proxy.Healthcheck.HelperImage = dest.Proxy.Healthcheck.HelperImage
	}
	if dest.Proxy.Healthcheck.HelperPull != "" {
		merged.Proxy.Healthcheck.HelperPull = dest.Proxy.Healthcheck.HelperPull
	}
	if has("proxy", "buffering", "requests") || destNode == nil && dest.Proxy.Buffering.Requests {
		merged.Proxy.Buffering.Requests = dest.Proxy.Buffering.Requests
	}
	if has("proxy", "buffering", "responses") || destNode == nil && dest.Proxy.Buffering.Responses {
		merged.Proxy.Buffering.Responses = dest.Proxy.Buffering.Responses
	}
	if has("proxy", "buffering", "max_request_body") || destNode == nil && dest.Proxy.Buffering.MaxRequestBody != 0 {
		merged.Proxy.Buffering.MaxRequestBody = dest.Proxy.Buffering.MaxRequestBody
	}
	if has("proxy", "buffering", "memory") || destNode == nil && dest.Proxy.Buffering.Memory != 0 {
		merged.Proxy.Buffering.Memory = dest.Proxy.Buffering.Memory
	}
	if dest.Proxy.ResponseTimeout != "" {
		merged.Proxy.ResponseTimeout = dest.Proxy.ResponseTimeout
	}
	if dest.Proxy.ResponseHeaderTimeout != "" {
		merged.Proxy.ResponseHeaderTimeout = dest.Proxy.ResponseHeaderTimeout
	}
	if has("proxy", "forward_headers") || destNode == nil && dest.Proxy.ForwardHeaders {
		merged.Proxy.ForwardHeaders = dest.Proxy.ForwardHeaders
	}
	if has("proxy", "logging", "enabled") || destNode == nil && dest.Proxy.Logging.Enabled {
		merged.Proxy.Logging.Enabled = dest.Proxy.Logging.Enabled
	}
	if len(dest.Proxy.Logging.RedactRequestHeaders) > 0 {
		merged.Proxy.Logging.RedactRequestHeaders = dest.Proxy.Logging.RedactRequestHeaders
	}
	if len(dest.Proxy.Logging.RedactResponseHeaders) > 0 {
		merged.Proxy.Logging.RedactResponseHeaders = dest.Proxy.Logging.RedactResponseHeaders
	}

	// Merge builder
	if has("builder", "multiarch") || destNode == nil && dest.Builder.Multiarch {
		merged.Builder.Multiarch = dest.Builder.Multiarch
	}
	if dest.Builder.Arch != "" {
		merged.Builder.Arch = dest.Builder.Arch
	}
	if len(dest.Builder.Platforms) > 0 {
		merged.Builder.Platforms = dest.Builder.Platforms
	}
	if dest.Builder.Cache.Type != "" {
		merged.Builder.Cache.Type = dest.Builder.Cache.Type
	}
	if has("builder", "cache", "options") {
		merged.Builder.Cache.Options = dest.Builder.Cache.Options // replace (empty map clears)
	} else if len(dest.Builder.Cache.Options) > 0 {
		if merged.Builder.Cache.Options == nil {
			merged.Builder.Cache.Options = make(map[string]string)
		}
		for k, v := range dest.Builder.Cache.Options {
			merged.Builder.Cache.Options[k] = v
		}
	}
	if has("builder", "args") {
		merged.Builder.Args = dest.Builder.Args // replace (empty map clears)
	} else if len(dest.Builder.Args) > 0 {
		if merged.Builder.Args == nil {
			merged.Builder.Args = make(map[string]string)
		}
		for k, v := range dest.Builder.Args {
			merged.Builder.Args[k] = v
		}
	}
	if dest.Builder.Target != "" {
		merged.Builder.Target = dest.Builder.Target
	}
	if len(dest.Builder.SSH) > 0 {
		merged.Builder.SSH = dest.Builder.SSH
	}
	if dest.Builder.Dockerfile != "" {
		merged.Builder.Dockerfile = dest.Builder.Dockerfile
	}
	if dest.Builder.Context != "" {
		merged.Builder.Context = dest.Builder.Context
	}
	if dest.Builder.Remote.Host != "" {
		merged.Builder.Remote.Host = dest.Builder.Remote.Host
	}
	if dest.Builder.Remote.Arch != "" {
		merged.Builder.Remote.Arch = dest.Builder.Remote.Arch
	}
	if has("builder", "secrets") {
		merged.Builder.Secrets = dest.Builder.Secrets // replace (empty list clears)
	} else if len(dest.Builder.Secrets) > 0 {
		merged.Builder.Secrets = append(merged.Builder.Secrets, dest.Builder.Secrets...)
	}
	if dest.Builder.TagTemplate != "" {
		merged.Builder.TagTemplate = dest.Builder.TagTemplate
	}

	// Merge deploy
	if has("deploy", "readiness_delay") || destNode == nil && dest.Deploy.ReadinessDelay != 0 {
		merged.Deploy.ReadinessDelay = dest.Deploy.ReadinessDelay
	}
	if has("deploy", "deploy_timeout") || destNode == nil && dest.Deploy.DeployTimeout != 0 {
		merged.Deploy.DeployTimeout = dest.Deploy.DeployTimeout
	}
	if has("deploy", "drain_timeout") || destNode == nil && dest.Deploy.DrainTimeout != 0 {
		merged.Deploy.DrainTimeout = dest.Deploy.DrainTimeout
	}
	if has("deploy", "stop_timeout") || destNode == nil && dest.Deploy.StopTimeout != 0 {
		merged.Deploy.StopTimeout = dest.Deploy.StopTimeout
	}
	if has("deploy", "retain_containers") || destNode == nil && dest.Deploy.RetainContainers != 0 {
		merged.Deploy.RetainContainers = dest.Deploy.RetainContainers
	}
	if has("deploy", "retain_history") || destNode == nil && dest.Deploy.RetainHistory != 0 {
		merged.Deploy.RetainHistory = dest.Deploy.RetainHistory
	}
	if has("deploy", "rollback_on_failure") || destNode == nil && dest.Deploy.RollbackOnFailure {
		merged.Deploy.RollbackOnFailure = dest.Deploy.RollbackOnFailure
	}
	if has("deploy", "canary", "enabled") || destNode == nil && dest.Deploy.Canary.Enabled {
		merged.Deploy.Canary.Enabled = dest.Deploy.Canary.Enabled
	}
	if has("deploy", "canary", "initial_weight") || destNode == nil && dest.Deploy.Canary.InitialWeight != 0 {
		merged.Deploy.Canary.InitialWeight = dest.Deploy.Canary.InitialWeight
	}
	if has("deploy", "canary", "step_weight") || destNode == nil && dest.Deploy.Canary.StepWeight != 0 {
		merged.Deploy.Canary.StepWeight = dest.Deploy.Canary.StepWeight
	}
	if has("deploy", "canary", "step_interval") || destNode == nil && dest.Deploy.Canary.StepInterval != 0 {
		merged.Deploy.Canary.StepInterval = dest.Deploy.Canary.StepInterval
	}
	if has("deploy", "canary", "auto_promote") || destNode == nil && dest.Deploy.Canary.AutoPromote {
		merged.Deploy.Canary.AutoPromote = dest.Deploy.Canary.AutoPromote
	}

	// Merge podman
	if has("podman", "rootless") || destNode == nil && dest.Podman.Rootless {
		merged.Podman.Rootless = dest.Podman.Rootless
	}
	if dest.Podman.QuadletPath != "" {
		merged.Podman.QuadletPath = dest.Podman.QuadletPath
	}
	if dest.Podman.NetworkBackend != "" {
		merged.Podman.NetworkBackend = dest.Podman.NetworkBackend
	}

	// Merge security
	if has("security", "require_non_root_ssh") || destNode == nil && dest.Security.RequireNonRootSSH {
		merged.Security.RequireNonRootSSH = dest.Security.RequireNonRootSSH
	}
	if has("security", "require_rootless_podman") || destNode == nil && dest.Security.RequireRootlessPodman {
		merged.Security.RequireRootlessPodman = dest.Security.RequireRootlessPodman
	}
	if has("security", "require_known_hosts") || destNode == nil && dest.Security.RequireKnownHosts {
		merged.Security.RequireKnownHosts = dest.Security.RequireKnownHosts
	}
	if has("security", "require_trusted_fingerprints") || destNode == nil && dest.Security.RequireTrustedFingerprints {
		merged.Security.RequireTrustedFingerprints = dest.Security.RequireTrustedFingerprints
	}

	// Merge hooks
	if dest.Hooks.Timeout != 0 {
		merged.Hooks.Timeout = dest.Hooks.Timeout
	}

	// Merge cron
	if len(dest.Cron) > 0 {
		if merged.Cron == nil {
			merged.Cron = make(map[string]CronConfig)
		}
		for name, cron := range dest.Cron {
			merged.Cron[name] = cron
		}
	}

	// Merge volumes
	if has("volumes") {
		merged.Volumes = dest.Volumes // replace (empty list clears)
	} else if len(dest.Volumes) > 0 {
		merged.Volumes = append(merged.Volumes, dest.Volumes...)
	}

	if dest.AssetPath != "" {
		merged.AssetPath = dest.AssetPath
	}
	if dest.SecretsPath != "" {
		merged.SecretsPath = dest.SecretsPath
	}
	if dest.SecretsProvider != "" {
		merged.SecretsProvider = dest.SecretsProvider
	}
	if dest.SecretsCommand != "" {
		merged.SecretsCommand = dest.SecretsCommand
	}
	if dest.SecretsEnvPrefix != "" {
		merged.SecretsEnvPrefix = dest.SecretsEnvPrefix
	}
	if dest.SecretsRemotePath != "" {
		merged.SecretsRemotePath = dest.SecretsRemotePath
	}
	if dest.HooksPath != "" {
		merged.HooksPath = dest.HooksPath
	}
	if dest.MinimumVersion != "" {
		merged.MinimumVersion = dest.MinimumVersion
	}
	if has("aliases") {
		merged.Aliases = dest.Aliases // replace (empty map clears)
	} else if len(dest.Aliases) > 0 {
		if merged.Aliases == nil {
			merged.Aliases = make(map[string]string)
		}
		for k, v := range dest.Aliases {
			merged.Aliases[k] = v
		}
	}

	// Merge accessories
	if len(dest.Accessories) > 0 {
		if merged.Accessories == nil {
			merged.Accessories = make(map[string]AccessoryConfig)
		}
		for name, config := range dest.Accessories {
			merged.Accessories[name] = config
		}
	}

	// Merge SSH
	if dest.SSH.User != "" {
		merged.SSH.User = dest.SSH.User
	}
	if dest.SSH.Port != 0 {
		merged.SSH.Port = dest.SSH.Port
	}
	if len(dest.SSH.Keys) > 0 {
		merged.SSH.Keys = dest.SSH.Keys
	}
	if dest.SSH.Proxy.Host != "" {
		merged.SSH.Proxy.Host = dest.SSH.Proxy.Host
	}
	if dest.SSH.Proxy.User != "" {
		merged.SSH.Proxy.User = dest.SSH.Proxy.User
	}
	if dest.SSH.KnownHostsFile != "" {
		merged.SSH.KnownHostsFile = dest.SSH.KnownHostsFile
	}
	if has("ssh", "trusted_host_fingerprints") {
		merged.SSH.TrustedHostFingerprints = dest.SSH.TrustedHostFingerprints // replace (empty map clears)
	} else if len(dest.SSH.TrustedHostFingerprints) > 0 {
		if merged.SSH.TrustedHostFingerprints == nil {
			merged.SSH.TrustedHostFingerprints = make(map[string][]string)
		}
		for host, fps := range dest.SSH.TrustedHostFingerprints {
			merged.SSH.TrustedHostFingerprints[host] = fps
		}
	}
	if has("ssh", "connect_timeout") || destNode == nil && dest.SSH.ConnectTimeout != 0 {
		merged.SSH.ConnectTimeout = dest.SSH.ConnectTimeout
	}
	if has("ssh", "insecure_ignore_host_key") || destNode == nil && dest.SSH.InsecureIgnoreHostKey {
		merged.SSH.InsecureIgnoreHostKey = dest.SSH.InsecureIgnoreHostKey
	}

	return &merged
}

func nodeHasPath(node *yaml.Node, path ...string) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	for _, segment := range path {
		if node == nil || node.Kind != yaml.MappingNode {
			return false
		}
		found := false
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == segment {
				node = node.Content[i+1]
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// applyDefaults sets default values for unset configuration options
func applyDefaults(cfg *Config) {
	// SSH defaults - use current user instead of root for security
	if cfg.SSH.User == "" {
		cfg.SSH.User = currentUsername()
	}
	if cfg.SSH.Port == 0 {
		cfg.SSH.Port = 22
	}
	if cfg.SSH.ConnectTimeout == 0 {
		cfg.SSH.ConnectTimeout = 30 * time.Second
	}

	// Proxy defaults
	if cfg.Proxy.AppPort == 0 {
		cfg.Proxy.AppPort = 3000
	}
	if cfg.Proxy.Host == "" && len(cfg.Proxy.Hosts) > 0 {
		cfg.Proxy.Host = cfg.Proxy.Hosts[0]
	}
	if cfg.Proxy.Healthcheck.Path == "" {
		cfg.Proxy.Healthcheck.Path = "/up"
	}
	if cfg.Proxy.Healthcheck.Interval == "" {
		cfg.Proxy.Healthcheck.Interval = "1s"
	}
	if cfg.Proxy.Healthcheck.Timeout == "" {
		cfg.Proxy.Healthcheck.Timeout = "5s"
	}
	if cfg.Proxy.ResponseTimeout == "" {
		cfg.Proxy.ResponseTimeout = "30s"
	}

	// Deploy defaults
	if cfg.Deploy.ReadinessDelay == 0 {
		cfg.Deploy.ReadinessDelay = 7 * time.Second
	}
	if cfg.Deploy.DeployTimeout == 0 {
		cfg.Deploy.DeployTimeout = 30 * time.Second
	}
	if cfg.Deploy.DrainTimeout == 0 {
		cfg.Deploy.DrainTimeout = 30 * time.Second
	}
	if cfg.Deploy.RetainContainers == 0 {
		cfg.Deploy.RetainContainers = 5
	}
	if cfg.Deploy.RetainHistory == 0 {
		cfg.Deploy.RetainHistory = 100
	}

	// Canary defaults (only apply if enabled)
	if cfg.Deploy.Canary.Enabled {
		if cfg.Deploy.Canary.InitialWeight == 0 {
			cfg.Deploy.Canary.InitialWeight = 10
		}
		if cfg.Deploy.Canary.StepWeight == 0 {
			cfg.Deploy.Canary.StepWeight = 10
		}
		if cfg.Deploy.Canary.StepInterval == 0 {
			cfg.Deploy.Canary.StepInterval = 5 * time.Minute
		}
	}

	// Builder defaults
	if cfg.Builder.Dockerfile == "" {
		cfg.Builder.Dockerfile = "Dockerfile"
	}
	if cfg.Builder.Context == "" {
		cfg.Builder.Context = "."
	}

	// Podman defaults
	if cfg.Podman.NetworkBackend == "" {
		cfg.Podman.NetworkBackend = "netavark"
	}
	if cfg.Podman.QuadletPath == "" {
		if cfg.Podman.Rootless {
			cfg.Podman.QuadletPath = "~/.config/containers/systemd/"
		} else {
			cfg.Podman.QuadletPath = "/etc/containers/systemd/"
		}
	}

	// Paths defaults
	if cfg.SecretsPath == "" {
		cfg.SecretsPath = ".azud/secrets"
	}
	if cfg.HooksPath == "" {
		cfg.HooksPath = ".azud/hooks"
	}
	if cfg.Hooks.Timeout == 0 {
		cfg.Hooks.Timeout = 5 * time.Minute
	}
}

// currentUsername returns the current OS user's username.
// Falls back to "root" if the user cannot be determined.
func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	// Fall back to checking USER environment variable
	if username := os.Getenv("USER"); username != "" {
		return username
	}
	// Last resort fallback
	return "root"
}
