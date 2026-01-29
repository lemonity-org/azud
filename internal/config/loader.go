package config

import (
	"bufio"
	"fmt"
	"os"
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
			destCfg, err := l.loadFile(destPath)
			if err != nil {
				return nil, fmt.Errorf("failed to load destination config %s: %w", destPath, err)
			}
			cfg = mergeConfigs(cfg, destCfg)
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

// loadFile reads and parses a single YAML file
func (l *Loader) loadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Expand environment variables
	data = []byte(os.ExpandEnv(string(data)))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	return &cfg, nil
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
	secretsPath := cfg.SecretsPath
	if secretsPath == "" {
		secretsPath = ".azud/secrets"
	}

	// Check if secrets file exists
	if _, err := os.Stat(secretsPath); os.IsNotExist(err) {
		return nil // No secrets file, that's okay
	}

	file, err := os.Open(secretsPath)
	if err != nil {
		return err
	}
	defer file.Close()

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

// mergeConfigs merges destination config into base config
func mergeConfigs(base, dest *Config) *Config {
	// Create a copy of base
	merged := *base

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
	if len(dest.Env.Clear) > 0 {
		if merged.Env.Clear == nil {
			merged.Env.Clear = make(map[string]string)
		}
		for k, v := range dest.Env.Clear {
			merged.Env.Clear[k] = v
		}
	}
	if len(dest.Env.Secret) > 0 {
		merged.Env.Secret = append(merged.Env.Secret, dest.Env.Secret...)
	}

	// Merge proxy
	if dest.Proxy.Host != "" {
		merged.Proxy.Host = dest.Proxy.Host
	}
	if len(dest.Proxy.Hosts) > 0 {
		merged.Proxy.Hosts = dest.Proxy.Hosts
	}
	if dest.Proxy.AppPort != 0 {
		merged.Proxy.AppPort = dest.Proxy.AppPort
	}
	if dest.Proxy.SSL {
		merged.Proxy.SSL = dest.Proxy.SSL
	}
	if dest.Proxy.SSLRedirect {
		merged.Proxy.SSLRedirect = dest.Proxy.SSLRedirect
	}
	if dest.Proxy.ACMEEmail != "" {
		merged.Proxy.ACMEEmail = dest.Proxy.ACMEEmail
	}
	if dest.Proxy.ACMEStaging {
		merged.Proxy.ACMEStaging = dest.Proxy.ACMEStaging
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

	return &merged
}

// applyDefaults sets default values for unset configuration options
func applyDefaults(cfg *Config) {
	// SSH defaults
	if cfg.SSH.User == "" {
		cfg.SSH.User = "root"
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

	// Paths defaults
	if cfg.SecretsPath == "" {
		cfg.SecretsPath = ".azud/secrets"
	}
	if cfg.HooksPath == "" {
		cfg.HooksPath = ".azud/hooks"
	}
}

// Add loadedSecrets field to Config (internal use)
// This is added via a separate file to avoid circular imports
