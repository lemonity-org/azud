package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	} else if !isValidImageRef(cfg.Image) {
		errs = append(errs, ValidationError{
			Field:   "image",
			Message: fmt.Sprintf("invalid image reference: %s", cfg.Image),
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

	// Validate trusted host fingerprints if required
	if cfg.Security.RequireTrustedFingerprints && len(cfg.Servers) > 0 {
		for _, host := range cfg.GetAllSSHHosts() {
			if !hasTrustedFingerprint(cfg, host) {
				errs = append(errs, ValidationError{
					Field:   "ssh.trusted_host_fingerprints",
					Message: fmt.Sprintf("missing fingerprint for host %s", host),
				})
			}
		}
	}

	// Validate proxy configuration
	if cfg.Proxy.Host == "" && len(cfg.Proxy.Hosts) == 0 {
		errs = append(errs, ValidationError{
			Field:   "proxy.host",
			Message: "proxy.host or proxy.hosts is required",
		})
	}
	if cfg.Proxy.Host != "" && !isValidHost(cfg.Proxy.Host) {
		errs = append(errs, ValidationError{
			Field:   "proxy.host",
			Message: fmt.Sprintf("invalid host address: %s", cfg.Proxy.Host),
		})
	}
	for i, host := range cfg.Proxy.Hosts {
		if !isValidHost(host) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("proxy.hosts[%d]", i),
				Message: fmt.Sprintf("invalid host address: %s", host),
			})
		}
	}
	if cfg.Proxy.HTTPPort < 0 || cfg.Proxy.HTTPPort > 65535 {
		errs = append(errs, ValidationError{
			Field:   "proxy.http_port",
			Message: "http_port must be between 0 and 65535",
		})
	}
	if cfg.Proxy.HTTPSPort < 0 || cfg.Proxy.HTTPSPort > 65535 {
		errs = append(errs, ValidationError{
			Field:   "proxy.https_port",
			Message: "https_port must be between 0 and 65535",
		})
	}
	if cfg.Proxy.ResponseTimeout != "" {
		if _, err := time.ParseDuration(cfg.Proxy.ResponseTimeout); err != nil {
			errs = append(errs, ValidationError{
				Field:   "proxy.response_timeout",
				Message: "response_timeout must be a valid duration (e.g., 30s, 1m)",
			})
		}
	}
	if cfg.Proxy.ResponseHeaderTimeout != "" {
		if _, err := time.ParseDuration(cfg.Proxy.ResponseHeaderTimeout); err != nil {
			errs = append(errs, ValidationError{
				Field:   "proxy.response_header_timeout",
				Message: "response_header_timeout must be a valid duration (e.g., 30s, 1m)",
			})
		}
	}
	if cfg.Proxy.Healthcheck.Interval != "" {
		if _, err := time.ParseDuration(cfg.Proxy.Healthcheck.Interval); err != nil {
			errs = append(errs, ValidationError{
				Field:   "proxy.healthcheck.interval",
				Message: "healthcheck.interval must be a valid duration (e.g., 5s, 1m)",
			})
		}
	}
	if cfg.Proxy.Healthcheck.Timeout != "" {
		if _, err := time.ParseDuration(cfg.Proxy.Healthcheck.Timeout); err != nil {
			errs = append(errs, ValidationError{
				Field:   "proxy.healthcheck.timeout",
				Message: "healthcheck.timeout must be a valid duration (e.g., 5s, 1m)",
			})
		}
	}
	if cfg.Proxy.Buffering.MaxRequestBody < 0 {
		errs = append(errs, ValidationError{
			Field:   "proxy.buffering.max_request_body",
			Message: "max_request_body must be non-negative",
		})
	}
	if cfg.Proxy.Buffering.Memory < 0 {
		errs = append(errs, ValidationError{
			Field:   "proxy.buffering.memory",
			Message: "buffering.memory must be non-negative",
		})
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

	// Validate logging header names
	for i, header := range cfg.Proxy.Logging.RedactRequestHeaders {
		if !isValidHeaderName(header) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("proxy.logging.redact_request_headers[%d]", i),
				Message: fmt.Sprintf("invalid HTTP header name: %q", header),
			})
		}
	}
	for i, header := range cfg.Proxy.Logging.RedactResponseHeaders {
		if !isValidHeaderName(header) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("proxy.logging.redact_response_headers[%d]", i),
				Message: fmt.Sprintf("invalid HTTP header name: %q", header),
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
		} else if !isValidImageRef(acc.Image) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("accessories.%s.image", name),
				Message: fmt.Sprintf("invalid image reference: %s", acc.Image),
			})
		}

		if acc.Host == "" && len(acc.Hosts) == 0 {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("accessories.%s.host", name),
				Message: "at least one host is required for accessory",
			})
		}

		if acc.Host != "" && !isValidHost(acc.Host) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("accessories.%s.host", name),
				Message: fmt.Sprintf("invalid host address: %s", acc.Host),
			})
		}
		for i, host := range acc.Hosts {
			if !isValidHost(host) {
				errs = append(errs, ValidationError{
					Field:   fmt.Sprintf("accessories.%s.hosts[%d]", name, i),
					Message: fmt.Sprintf("invalid host address: %s", host),
				})
			}
		}
	}

	// Validate cron jobs
	for name, cron := range cfg.Cron {
		if strings.TrimSpace(cron.Schedule) == "" {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("cron.%s.schedule", name),
				Message: "schedule is required for cron job",
			})
		} else if !isValidCronSchedule(cron.Schedule) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("cron.%s.schedule", name),
				Message: fmt.Sprintf("invalid cron schedule: %s", cron.Schedule),
			})
		}
		if strings.TrimSpace(cron.Command) == "" {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("cron.%s.command", name),
				Message: "command is required for cron job",
			})
		}
		if cron.Timeout != "" {
			if _, err := time.ParseDuration(cron.Timeout); err != nil {
				errs = append(errs, ValidationError{
					Field:   fmt.Sprintf("cron.%s.timeout", name),
					Message: "timeout must be a valid duration (e.g., 30s, 1m)",
				})
			}
		}
	}

	// Validate cron host resolution (when cron jobs exist but no explicit hosts)
	if len(cfg.Cron) > 0 {
		for name, cron := range cfg.Cron {
			if cron.Host == "" && len(cron.Hosts) == 0 {
				// Cron defaults to first web host or first any host
				resolved := cfg.GetCronHosts(name)
				if len(resolved) == 0 {
					errs = append(errs, ValidationError{
						Field:   fmt.Sprintf("cron.%s.host", name),
						Message: "no host can be resolved for cron job (no servers defined)",
					})
				}
			}
		}
	}

	// Validate SSH configuration
	if cfg.SSH.Port < 1 || cfg.SSH.Port > 65535 {
		errs = append(errs, ValidationError{
			Field:   "ssh.port",
			Message: "SSH port must be between 1 and 65535",
		})
	}
	if cfg.Security.RequireNonRootSSH && cfg.SSH.User == "root" {
		errs = append(errs, ValidationError{
			Field:   "security.require_non_root_ssh",
			Message: "non-root SSH user required (set ssh.user to a non-root account)",
		})
	}
	if cfg.Security.RequireKnownHosts && cfg.SSH.InsecureIgnoreHostKey {
		errs = append(errs, ValidationError{
			Field:   "security.require_known_hosts",
			Message: "host key verification required (disable ssh.insecure_ignore_host_key)",
		})
	}
	if cfg.SSH.Proxy.Host != "" && !isValidHost(cfg.SSH.Proxy.Host) {
		errs = append(errs, ValidationError{
			Field:   "ssh.proxy.host",
			Message: fmt.Sprintf("invalid host address: %s", cfg.SSH.Proxy.Host),
		})
	}
	if cfg.Security.RequireTrustedFingerprints && len(cfg.SSH.TrustedHostFingerprints) == 0 {
		errs = append(errs, ValidationError{
			Field:   "security.require_trusted_fingerprints",
			Message: "trusted_host_fingerprints must be configured",
		})
	}

	// Validate cron hosts if explicitly configured
	for name, cron := range cfg.Cron {
		if cron.Host != "" && !isValidHost(cron.Host) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("cron.%s.host", name),
				Message: fmt.Sprintf("invalid host address: %s", cron.Host),
			})
		}
		for i, host := range cron.Hosts {
			if !isValidHost(host) {
				errs = append(errs, ValidationError{
					Field:   fmt.Sprintf("cron.%s.hosts[%d]", name, i),
					Message: fmt.Sprintf("invalid host address: %s", host),
				})
			}
		}
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

	// Validate builder remote arch
	if cfg.Builder.Remote.Host != "" && cfg.Builder.Remote.Arch != "" {
		if !isValidArch(cfg.Builder.Remote.Arch) {
			errs = append(errs, ValidationError{
				Field:   "builder.remote.arch",
				Message: fmt.Sprintf("invalid architecture: %s (expected one of: amd64, arm64, arm, 386, ppc64le, s390x, riscv64)", cfg.Builder.Remote.Arch),
			})
		}
	}

	cacheType := strings.TrimSpace(cfg.Builder.Cache.Type)
	if cacheType == "" && len(cfg.Builder.Cache.Options) > 0 {
		errs = append(errs, ValidationError{
			Field:   "builder.cache.type",
			Message: "cache.type is required when cache.options are set",
		})
	}
	if cacheType != "" {
		validCache := map[string]bool{"registry": true, "local": true, "gha": true}
		if !validCache[cacheType] {
			errs = append(errs, ValidationError{
				Field:   "builder.cache.type",
				Message: "cache.type must be registry, local, or gha",
			})
		}
		if len(cfg.Builder.Cache.Options) == 0 {
			errs = append(errs, ValidationError{
				Field:   "builder.cache.options",
				Message: "cache.options must be set when cache.type is provided",
			})
		}

		// Validate type-specific cache options
		if validCache[cacheType] && len(cfg.Builder.Cache.Options) > 0 {
			switch cacheType {
			case "registry":
				if _, ok := cfg.Builder.Cache.Options["ref"]; !ok {
					errs = append(errs, ValidationError{
						Field:   "builder.cache.options",
						Message: "cache type 'registry' requires 'ref' option",
					})
				}
			case "local":
				if _, ok := cfg.Builder.Cache.Options["src"]; !ok {
					errs = append(errs, ValidationError{
						Field:   "builder.cache.options",
						Message: "cache type 'local' requires 'src' option",
					})
				}
				if _, ok := cfg.Builder.Cache.Options["dest"]; !ok {
					errs = append(errs, ValidationError{
						Field:   "builder.cache.options",
						Message: "cache type 'local' requires 'dest' option",
					})
				}
			case "gha":
				if _, ok := cfg.Builder.Cache.Options["scope"]; !ok {
					errs = append(errs, ValidationError{
						Field:   "builder.cache.options",
						Message: "cache type 'gha' requires 'scope' option",
					})
				}
			}
		}
	}

	// Validate builder platforms
	for i, platform := range cfg.Builder.Platforms {
		if !isValidPlatform(platform) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("builder.platforms[%d]", i),
				Message: fmt.Sprintf("invalid platform: %s (expected OS/ARCH or OS/ARCH/VARIANT)", platform),
			})
		}
	}

	// Validate builder secrets format
	for i, secret := range cfg.Builder.Secrets {
		if !isValidBuilderSecret(secret) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("builder.secrets[%d]", i),
				Message: fmt.Sprintf("invalid builder secret spec: %s", secret),
			})
		}
	}

	// Validate builder SSH format
	for i, sshSpec := range cfg.Builder.SSH {
		if !isValidBuilderSSH(sshSpec) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("builder.ssh[%d]", i),
				Message: fmt.Sprintf("invalid builder SSH spec: %s", sshSpec),
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
	if cfg.Security.RequireRootlessPodman && !cfg.Podman.Rootless {
		errs = append(errs, ValidationError{
			Field:   "security.require_rootless_podman",
			Message: "rootless Podman required (set podman.rootless: true)",
		})
	}

	// Validate healthcheck helper settings
	if cfg.Proxy.Healthcheck.HelperPull != "" {
		validPull := map[string]bool{"missing": true, "always": true, "never": true}
		if !validPull[strings.ToLower(strings.TrimSpace(cfg.Proxy.Healthcheck.HelperPull))] {
			errs = append(errs, ValidationError{
				Field:   "proxy.healthcheck.helper_pull",
				Message: "helper_pull must be missing, always, or never",
			})
		}
	}

	// Validate secrets provider configuration
	switch strings.ToLower(strings.TrimSpace(cfg.SecretsProvider)) {
	case "", "file":
		// ok
	case "env":
		if cfg.SecretsEnvPrefix == "" {
			errs = append(errs, ValidationError{
				Field:   "secrets_env_prefix",
				Message: "secrets_env_prefix is required when secrets_provider=env",
			})
		}
	case "command":
		if cfg.SecretsCommand == "" {
			errs = append(errs, ValidationError{
				Field:   "secrets_command",
				Message: "secrets_command is required when secrets_provider=command",
			})
		}
	default:
		errs = append(errs, ValidationError{
			Field:   "secrets_provider",
			Message: "secrets_provider must be file, env, or command",
		})
	}

	// Validate deploy configuration
	if cfg.Deploy.RetainContainers < 0 {
		errs = append(errs, ValidationError{
			Field:   "deploy.retain_containers",
			Message: "retain_containers must be non-negative",
		})
	}

	// Validate minimum_version format
	if cfg.MinimumVersion != "" && !isValidSemver(cfg.MinimumVersion) {
		errs = append(errs, ValidationError{
			Field:   "minimum_version",
			Message: fmt.Sprintf("invalid semver format: %s (expected MAJOR.MINOR.PATCH)", cfg.MinimumVersion),
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

// isValidCronSchedule validates a cron schedule expression.
// Accepts 5-field expressions (minute hour day-of-month month day-of-week)
// and shortcut names (@yearly, @monthly, @weekly, @daily, @hourly).
func isValidCronSchedule(schedule string) bool {
	schedule = strings.TrimSpace(schedule)
	if schedule == "" {
		return false
	}

	// Check shortcuts
	shortcuts := map[string]bool{
		"@yearly": true, "@annually": true, "@monthly": true,
		"@weekly": true, "@daily": true, "@midnight": true,
		"@hourly": true,
	}
	if shortcuts[strings.ToLower(schedule)] {
		return true
	}

	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return false
	}

	type fieldRange struct {
		min, max int
	}
	ranges := []fieldRange{
		{0, 59},  // minute
		{0, 23},  // hour
		{1, 31},  // day of month
		{1, 12},  // month
		{0, 7},   // day of week (0 and 7 are both Sunday)
	}

	for i, field := range fields {
		if !isValidCronField(field, ranges[i].min, ranges[i].max) {
			return false
		}
	}

	return true
}

// isValidCronField validates a single cron field.
func isValidCronField(field string, min, max int) bool {
	// Handle lists (comma-separated)
	parts := strings.Split(field, ",")
	for _, part := range parts {
		if !isValidCronPart(part, min, max) {
			return false
		}
	}
	return true
}

// isValidCronPart validates a single part of a cron field (handles *, ranges, steps).
func isValidCronPart(part string, min, max int) bool {
	if part == "*" {
		return true
	}

	// Handle step values (*/5, 1-10/2)
	if idx := strings.Index(part, "/"); idx >= 0 {
		base := part[:idx]
		step := part[idx+1:]
		stepVal, err := strconv.Atoi(step)
		if err != nil || stepVal < 1 {
			return false
		}
		if base == "*" {
			return true
		}
		return isValidCronRange(base, min, max)
	}

	// Handle ranges (1-5)
	if strings.Contains(part, "-") {
		return isValidCronRange(part, min, max)
	}

	// Single value
	val, err := strconv.Atoi(part)
	return err == nil && val >= min && val <= max
}

// isValidCronRange validates a range expression (e.g., "1-5").
func isValidCronRange(rangeStr string, min, max int) bool {
	parts := strings.SplitN(rangeStr, "-", 2)
	if len(parts) != 2 {
		return false
	}
	low, err := strconv.Atoi(parts[0])
	if err != nil || low < min || low > max {
		return false
	}
	high, err := strconv.Atoi(parts[1])
	if err != nil || high < min || high > max {
		return false
	}
	return low <= high
}

// isValidHeaderName checks if a string is a valid HTTP header name per RFC 7230.
func isValidHeaderName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, c := range name {
		// RFC 7230 token characters: printable ASCII excluding delimiters
		if c <= 0x20 || c >= 0x7f {
			return false
		}
		// Delimiters per RFC 7230
		switch c {
		case '(', ')', '<', '>', '@', ',', ';', '\\', '"', '/', '[', ']', '?', '=', '{', '}':
			return false
		}
	}
	return true
}

// isValidSemver validates a semantic version string (MAJOR.MINOR.PATCH with optional v prefix).
func isValidSemver(version string) bool {
	version = strings.TrimPrefix(version, "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

// parseSemver extracts major, minor, patch from a semver string.
func parseSemver(version string) (int, int, int, bool) {
	version = strings.TrimPrefix(version, "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

// ValidateMinimumVersion checks if the current version meets the minimum requirement
func ValidateMinimumVersion(cfg *Config, currentVersion string) error {
	if cfg.MinimumVersion == "" {
		return nil
	}

	if currentVersion == "dev" {
		return nil // Dev versions bypass the check
	}

	reqMajor, reqMinor, reqPatch, ok := parseSemver(cfg.MinimumVersion)
	if !ok {
		return fmt.Errorf("invalid minimum_version format: %s", cfg.MinimumVersion)
	}
	curMajor, curMinor, curPatch, ok := parseSemver(currentVersion)
	if !ok {
		return fmt.Errorf("invalid current version format: %s", currentVersion)
	}

	if curMajor < reqMajor ||
		(curMajor == reqMajor && curMinor < reqMinor) ||
		(curMajor == reqMajor && curMinor == reqMinor && curPatch < reqPatch) {
		return fmt.Errorf("minimum version %s required, but running %s", cfg.MinimumVersion, currentVersion)
	}

	return nil
}

var knownArchitectures = map[string]bool{
	"amd64": true, "arm64": true, "arm": true, "386": true,
	"ppc64le": true, "s390x": true, "riscv64": true, "mips64le": true,
}

// isValidArch checks if an architecture string is a known value.
func isValidArch(arch string) bool {
	return knownArchitectures[strings.ToLower(arch)]
}

var platformRegex = regexp.MustCompile(`^[a-z]+/[a-z0-9]+(/[a-z0-9]+)?$`)

var knownPlatformOS = map[string]bool{
	"linux": true, "darwin": true, "windows": true,
	"freebsd": true, "netbsd": true, "openbsd": true,
}

// isValidPlatform validates a platform string like "linux/amd64" or "linux/arm/v7".
func isValidPlatform(platform string) bool {
	if !platformRegex.MatchString(platform) {
		return false
	}
	parts := strings.Split(platform, "/")
	if !knownPlatformOS[parts[0]] {
		return false
	}
	if !knownArchitectures[parts[1]] {
		return false
	}
	return true
}

// isValidBuilderSecret validates a builder secret spec.
// Accepts simple key names or key-value specs like "id=name,src=path".
func isValidBuilderSecret(spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return false
	}

	// Simple key name (alphanumeric + underscore + hyphen)
	if !strings.Contains(spec, "=") {
		return regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(spec)
	}

	// Key-value spec: must have "id=" at minimum
	parts := strings.Split(spec, ",")
	fields := make(map[string]string)
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			return false
		}
		fields[kv[0]] = kv[1]
	}

	if _, ok := fields["id"]; !ok {
		return false
	}
	// Must have at least one source: src or env
	_, hasSrc := fields["src"]
	_, hasEnv := fields["env"]
	return hasSrc || hasEnv
}

// isValidBuilderSSH validates a builder SSH forwarding spec.
// Accepts "default" or "id=name,src=path" format.
func isValidBuilderSSH(spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return false
	}

	if spec == "default" {
		return true
	}

	if !strings.Contains(spec, "=") {
		return false
	}

	parts := strings.Split(spec, ",")
	fields := make(map[string]string)
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			return false
		}
		fields[kv[0]] = kv[1]
	}

	_, hasID := fields["id"]
	return hasID
}

// isValidImageRef validates a container image reference.
func isValidImageRef(image string) bool {
	image = strings.TrimSpace(image)
	if image == "" {
		return false
	}

	// No spaces or control characters
	for _, c := range image {
		if c <= 0x20 || c == 0x7f {
			return false
		}
	}

	// Split off digest (@sha256:...)
	ref := image
	if idx := strings.Index(ref, "@"); idx >= 0 {
		digest := ref[idx+1:]
		ref = ref[:idx]
		// Digest must be algorithm:hex
		digestParts := strings.SplitN(digest, ":", 2)
		if len(digestParts) != 2 || digestParts[0] == "" || digestParts[1] == "" {
			return false
		}
		if !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(digestParts[0]) {
			return false
		}
		if !regexp.MustCompile(`^[a-f0-9]+$`).MatchString(digestParts[1]) {
			return false
		}
	}

	// Split off tag (:tag)
	if idx := strings.LastIndex(ref, ":"); idx >= 0 {
		// Make sure this isn't a port in the registry (e.g., localhost:5000/image)
		afterColon := ref[idx+1:]
		beforeColon := ref[:idx]
		// If there's a slash after the colon, it's a port not a tag
		if !strings.Contains(afterColon, "/") {
			// Validate tag: alphanumeric + . - _ (1-128 chars)
			if len(afterColon) == 0 || len(afterColon) > 128 {
				return false
			}
			if !regexp.MustCompile(`^[a-zA-Z0-9._-]+$`).MatchString(afterColon) {
				return false
			}
			ref = beforeColon
		}
	}

	// Remaining ref must be a valid name
	if ref == "" {
		return false
	}

	return true
}

func hasTrustedFingerprint(cfg *Config, host string) bool {
	if cfg == nil || len(cfg.SSH.TrustedHostFingerprints) == 0 {
		return false
	}

	if fps := cfg.SSH.TrustedHostFingerprints[host]; len(fps) > 0 {
		return true
	}

	port := cfg.SSH.Port
	if port == 0 {
		port = 22
	}
	if port != 22 {
		key := fmt.Sprintf("[%s]:%d", host, port)
		if fps := cfg.SSH.TrustedHostFingerprints[key]; len(fps) > 0 {
			return true
		}
	}

	return false
}
