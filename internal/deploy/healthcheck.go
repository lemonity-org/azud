package deploy

import (
	"fmt"
	"strings"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/shell"
)

const (
	defaultHealthcheckPull = "missing"
)

// BuildHTTPCheckCommand builds a shell command that performs an HTTP GET against
// localhost using the first available HTTP client (curl, wget, or busybox wget).
// Note: this requires a shell inside the container and is best for images that
// include /bin/sh. Readiness checks use exec candidates instead. It returns ""
// when the path is not a valid health path.
func BuildHTTPCheckCommand(port int, path string) string {
	quotedURL, ok := quotedHealthCheckURL(port, path)
	if !ok {
		return ""
	}

	return fmt.Sprintf(
		"if command -v curl >/dev/null 2>&1; then curl -fsS %s >/dev/null; "+
			"elif command -v wget >/dev/null 2>&1; then wget -qO- %s >/dev/null; "+
			"elif command -v busybox >/dev/null 2>&1; then busybox wget -qO- %s >/dev/null; "+
			"else echo \"no http client (curl/wget/busybox) available\" >&2; exit 1; fi",
		quotedURL, quotedURL, quotedURL,
	)
}

// LivenessCommand returns the configured healthcheck command or empty if disabled.
func LivenessCommand(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}

	hc := cfg.Proxy.Healthcheck
	if hc.DisableLiveness {
		return ""
	}

	if cmd := strings.TrimSpace(hc.LivenessCmd); cmd != "" {
		return cmd
	}

	path := hc.GetLivenessPath()
	if path == "" {
		return ""
	}

	return BuildHTTPCheckCommand(cfg.Proxy.AppPort, path)
}

// ReadinessCommand returns the command that should gate traffic admission.
// A custom command takes precedence over the HTTP readiness path.
func ReadinessCommand(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Proxy.Healthcheck.ReadinessCmd)
}

// HasReadinessProbe reports whether deployment should wait for an explicit
// readiness signal before registering the container with the proxy.
func HasReadinessProbe(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return ReadinessCommand(cfg) != "" || cfg.Proxy.Healthcheck.GetReadinessPath() != ""
}

// BuildHTTPCheckExecCandidates builds podman exec commands that do not require a
// shell inside the target container. The host's shell interprets them, so every
// value is shell-quoted. It returns nil when the path is not a valid health
// path.
func BuildHTTPCheckExecCandidates(container string, port int, path string) []string {
	if container == "" {
		return nil
	}
	quotedURL, ok := quotedHealthCheckURL(port, path)
	if !ok {
		return nil
	}

	qc := shell.Quote(container)
	return []string{
		fmt.Sprintf("podman exec %s curl -fsS %s", qc, quotedURL),
		fmt.Sprintf("podman exec %s wget -qO- %s", qc, quotedURL),
		fmt.Sprintf("podman exec %s busybox wget -qO- %s", qc, quotedURL),
	}
}

// BuildHTTPCheckHelperCommand builds a helper container command to check readiness
// without requiring tools inside the target container. It shares the target
// container network namespace to avoid DNS and IP lookups. It returns "" when
// the path is not a valid health path.
func BuildHTTPCheckHelperCommand(container string, port int, path, image, pullPolicy string) string {
	if strings.TrimSpace(container) == "" {
		return ""
	}
	quotedURL, ok := quotedHealthCheckURL(port, path)
	if !ok {
		return ""
	}

	image = strings.TrimSpace(image)
	if image == "" {
		image = config.DefaultHealthcheckHelperImage
	}

	pullPolicy = strings.TrimSpace(pullPolicy)
	if pullPolicy == "" {
		pullPolicy = defaultHealthcheckPull
	}

	name := fmt.Sprintf("azud-hc-%s", container)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, ".", "-")

	return fmt.Sprintf(
		"podman run --rm --pull=%s --network container:%s --name %s %s -fsS -o /dev/null %s",
		shell.Quote(pullPolicy),
		shell.Quote(container),
		shell.Quote(name),
		shell.Quote(image),
		quotedURL,
	)
}

// quotedHealthCheckURL returns the shell-quoted loopback URL for an HTTP
// probe. It validates the path itself rather than trusting the caller, and
// single-quotes the URL so a path can never expand shell syntax on the host or
// inside the container.
func quotedHealthCheckURL(port int, path string) (string, bool) {
	if port <= 0 || !config.IsValidHealthPath(path) {
		return "", false
	}

	// Use 127.0.0.1 instead of localhost to avoid IPv6 resolution issues.
	// Some containers (e.g. Alpine/busybox) resolve localhost to ::1 first,
	// but the app may only listen on IPv4.
	return shell.Quote(fmt.Sprintf("http://127.0.0.1:%d%s", port, path)), true
}
