package deploy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/adriancarayol/azud/internal/config"
)

const (
	defaultHealthcheckImage = "curlimages/curl:8.5.0"
	defaultHealthcheckPull  = "missing"
)

// BuildHTTPCheckCommand builds a shell command that performs an HTTP GET against
// localhost using the first available HTTP client (curl, wget, or busybox wget).
// Note: this requires a shell inside the container and is best for images that
// include /bin/sh. Readiness checks use exec candidates instead.
func BuildHTTPCheckCommand(port int, path string) string {
	if port <= 0 || path == "" {
		return ""
	}

	// Use 127.0.0.1 instead of localhost to avoid IPv6 resolution issues.
	// Some containers (e.g. Alpine/busybox) resolve localhost to ::1 first,
	// but the app may only listen on IPv4.
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	quotedURL := strconv.Quote(url)

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

// BuildHTTPCheckExecCandidates builds podman exec commands that do not require a shell.
func BuildHTTPCheckExecCandidates(container string, port int, path string) []string {
	if container == "" || port <= 0 || path == "" {
		return nil
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	quotedURL := strconv.Quote(url)

	return []string{
		fmt.Sprintf("podman exec %s curl -fsS %s", container, quotedURL),
		fmt.Sprintf("podman exec %s wget -qO- %s", container, quotedURL),
		fmt.Sprintf("podman exec %s busybox wget -qO- %s", container, quotedURL),
	}
}

// BuildHTTPCheckHelperCommand builds a helper container command to check readiness
// without requiring tools inside the target container. It shares the target
// container network namespace to avoid DNS and IP lookups.
func BuildHTTPCheckHelperCommand(container string, port int, path, image, pullPolicy string) string {
	if strings.TrimSpace(container) == "" || port <= 0 || path == "" {
		return ""
	}

	image = strings.TrimSpace(image)
	if image == "" {
		image = defaultHealthcheckImage
	}

	pullPolicy = strings.TrimSpace(pullPolicy)
	if pullPolicy == "" {
		pullPolicy = defaultHealthcheckPull
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	quotedURL := strconv.Quote(url)

	name := fmt.Sprintf("azud-hc-%s", container)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, ".", "-")

	return fmt.Sprintf(
		"podman run --rm --pull=%s --network container:%s --name %s %s -fsS -o /dev/null %s",
		pullPolicy,
		container,
		name,
		image,
		quotedURL,
	)
}
