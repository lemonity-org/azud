package config

import (
	"fmt"
	"net"
	"strings"
)

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationErrors holds multiple validation errors
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return "no validation errors"
	}

	var msgs []string
	for _, err := range e {
		msgs = append(msgs, err.Error())
	}
	return strings.Join(msgs, "; ")
}

// Validate checks the configuration for errors
func Validate(cfg *Config) error {
	var errs ValidationErrors

	// Required fields
	if cfg.Service == "" {
		errs = append(errs, ValidationError{
			Field:   "service",
			Message: "service name is required",
		})
	}

	if cfg.Image == "" {
		errs = append(errs, ValidationError{
			Field:   "image",
			Message: "image name is required",
		})
	}

	// Validate servers
	if len(cfg.Servers) == 0 {
		errs = append(errs, ValidationError{
			Field:   "servers",
			Message: "at least one server role must be defined",
		})
	} else {
		for role, rc := range cfg.Servers {
			if len(rc.Hosts) == 0 {
				errs = append(errs, ValidationError{
					Field:   fmt.Sprintf("servers.%s.hosts", role),
					Message: "at least one host is required",
				})
			}

			// Validate host addresses
			for i, host := range rc.Hosts {
				if !isValidHost(host) {
					errs = append(errs, ValidationError{
						Field:   fmt.Sprintf("servers.%s.hosts[%d]", role, i),
						Message: fmt.Sprintf("invalid host address: %s", host),
					})
				}
			}
		}
	}

	// Validate proxy configuration
	if cfg.Proxy.Host == "" && len(cfg.Proxy.Hosts) == 0 {
		// Proxy host is optional, but if SSL is enabled, we need a host
		if cfg.Proxy.SSL {
			errs = append(errs, ValidationError{
				Field:   "proxy.host",
				Message: "proxy host is required when SSL is enabled",
			})
		}
	}

	// Validate ACME email when SSL is enabled (skip if custom certificates are provided)
	if cfg.Proxy.SSL && cfg.Proxy.ACMEEmail == "" {
		// Only require ACME email if custom certificates are NOT provided
		if cfg.Proxy.SSLCertificate == "" || cfg.Proxy.SSLPrivateKey == "" {
			errs = append(errs, ValidationError{
				Field:   "proxy.acme_email",
				Message: "acme_email is required when SSL is enabled (unless custom ssl_certificate and ssl_private_key are provided)",
			})
		}
	}

	// Validate canary configuration
	if cfg.Deploy.Canary.Enabled {
		if cfg.Deploy.Canary.InitialWeight < 0 || cfg.Deploy.Canary.InitialWeight > 100 {
			errs = append(errs, ValidationError{
				Field:   "deploy.canary.initial_weight",
				Message: "initial_weight must be between 0 and 100",
			})
		}
		if cfg.Deploy.Canary.StepWeight < 0 || cfg.Deploy.Canary.StepWeight > 100 {
			errs = append(errs, ValidationError{
				Field:   "deploy.canary.step_weight",
				Message: "step_weight must be between 0 and 100",
			})
		}
	}

	if cfg.Proxy.AppPort < 0 || cfg.Proxy.AppPort > 65535 {
		errs = append(errs, ValidationError{
			Field:   "proxy.app_port",
			Message: "app_port must be between 0 and 65535",
		})
	}

	// Validate accessories
	for name, acc := range cfg.Accessories {
		if acc.Image == "" {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("accessories.%s.image", name),
				Message: "image is required for accessory",
			})
		}

		if acc.Host == "" && len(acc.Hosts) == 0 {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("accessories.%s.host", name),
				Message: "at least one host is required for accessory",
			})
		}
	}

	// Validate SSH configuration
	if cfg.SSH.Port < 1 || cfg.SSH.Port > 65535 {
		errs = append(errs, ValidationError{
			Field:   "ssh.port",
			Message: "SSH port must be between 1 and 65535",
		})
	}

	// Validate builder configuration
	if cfg.Builder.Remote.Host != "" {
		if !isValidHost(cfg.Builder.Remote.Host) {
			errs = append(errs, ValidationError{
				Field:   "builder.remote.host",
				Message: fmt.Sprintf("invalid remote builder host: %s", cfg.Builder.Remote.Host),
			})
		}
	}

	// Validate Podman configuration
	validBackends := map[string]bool{"netavark": true, "cni": true}
	if cfg.Podman.NetworkBackend != "" && !validBackends[cfg.Podman.NetworkBackend] {
		errs = append(errs, ValidationError{
			Field:   "podman.network_backend",
			Message: "network_backend must be 'netavark' or 'cni'",
		})
	}

	// Validate deploy configuration
	if cfg.Deploy.RetainContainers < 0 {
		errs = append(errs, ValidationError{
			Field:   "deploy.retain_containers",
			Message: "retain_containers must be non-negative",
		})
	}

	if len(errs) > 0 {
		return errs
	}

	return nil
}

// isValidHost checks if a string is a valid hostname or IP address
func isValidHost(host string) bool {
	// Check if it's an IP address
	if ip := net.ParseIP(host); ip != nil {
		return true
	}

	// Check if it's a valid hostname
	// Hostnames can contain letters, digits, and hyphens
	// They must start with a letter or digit
	if len(host) == 0 || len(host) > 253 {
		return false
	}

	// Split by dots and validate each label
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}

		// First character must be alphanumeric
		if !isAlphanumeric(label[0]) {
			return false
		}

		// Last character must be alphanumeric
		if !isAlphanumeric(label[len(label)-1]) {
			return false
		}

		// Middle characters can be alphanumeric or hyphen
		for i := 1; i < len(label)-1; i++ {
			if !isAlphanumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}

	return true
}

// isAlphanumeric checks if a byte is a letter or digit
func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// ValidateMinimumVersion checks if the current version meets the minimum requirement
func ValidateMinimumVersion(cfg *Config, currentVersion string) error {
	if cfg.MinimumVersion == "" {
		return nil
	}

	// Simple version comparison (semantic versioning)
	// In a real implementation, use a proper semver library
	if currentVersion == "dev" {
		return nil // Dev versions bypass the check
	}

	// For now, just do a simple string comparison
	// This should be replaced with proper semver comparison
	if cfg.MinimumVersion > currentVersion {
		return fmt.Errorf("minimum version %s required, but running %s", cfg.MinimumVersion, currentVersion)
	}

	return nil
}
