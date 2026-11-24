//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// onDemandApp answers the proxy's permission check on /cgi-bin/ask: yes for
// allowed*.e2e.test when the request has no X-Forwarded-For, as Caddy's own
// check has none, and no otherwise. /cgi-bin/echo shows the version, the host
// and X-Forwarded-For.
var onDemandApp = map[string]string{
	"cgi-bin/ask": `#!/bin/sh
case "$QUERY_STRING" in
domain=allowed*.e2e.test)
	if [ -z "$HTTP_X_FORWARDED_FOR" ]; then
		printf 'Content-Type: text/plain\r\n\r\nyes\n'
		exit 0
	fi
	;;
esac
printf 'Status: 404 Not Found\r\nContent-Type: text/plain\r\n\r\nno\n'
`,
	"cgi-bin/echo": `#!/bin/sh
printf 'Content-Type: text/plain\r\n\r\n%s %s %s\n' "$(cat /www/index.html)" "$HTTP_HOST" "$HTTP_X_FORWARDED_FOR"
`,
}

// TestOnDemandTLS deploys a service with proxy.on_demand_tls and checks it
// through the real proxy. Caddy's internal CA stands in for ACME: the test
// swaps the issuers after each deploy, as the hosts have no public DNS.
func TestOnDemandTLS(t *testing.T) {
	s := newService(t, "ondemand")
	config := strings.Replace(s.renderConfig(), "  ssl: false\n", `  ssl: true
  ssl_redirect: true
  acme_email: ops@example.com
  acme_staging: true
  on_demand_tls:
    ask: /cgi-bin/ask
`, 1)
	if err := os.WriteFile(s.config, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Later tests share the proxy; leave no catch-all route behind.
		_, _ = proxyAdmin("DELETE", "/id/azud-on-demand-"+s.name, "")
	})

	release := func(version string) {
		digest := s.publishImageWith(version, `CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]`, onDemandApp)
		s.sign(digest)
		s.promote(digest)
		s.deploys()
		useInternalIssuer(t)
	}

	release("v1")
	routes := routeIDs(t)
	if len(routes) == 0 || routes[len(routes)-1] != "azud-on-demand-"+s.name {
		t.Fatalf("the on-demand route is not last: %v", routes)
	}

	body, cert := waitHTTPS(t, "allowed.e2e.test", "/cgi-bin/echo")
	fields := strings.Fields(body)
	if len(fields) != 3 || fields[0] != "v1" || fields[1] != "allowed.e2e.test" {
		t.Fatalf("allowed.e2e.test answered %q, want v1 from the app", body)
	}
	if !slices.Contains(cert.DNSNames, "allowed.e2e.test") {
		t.Fatalf("certificate names %v, want allowed.e2e.test", cert.DNSNames)
	}

	if _, _, err := httpsGet("denied.e2e.test", "/"); err == nil {
		t.Fatal("denied.e2e.test completed a handshake although the permission check refused it")
	}

	status, _, err := httpsGet("allowed.e2e.test", "/cgi-bin/ask?domain=allowed.e2e.test")
	if err != nil || status != http.StatusNotFound {
		t.Fatalf("a proxied permission check answered %d, %v; it must reach the app with X-Forwarded-For", status, err)
	}

	redirect := plainHTTP(t, "unknown.e2e.test", "/path")
	if redirect != "https://unknown.e2e.test/path" {
		t.Fatalf("plain HTTP for an unknown host redirected to %q", redirect)
	}

	// A rolling deploy moves on-demand hosts, and the permission check, to the
	// new container: with host-port upstreams its address changes.
	release("v2")
	if body, _ := waitHTTPS(t, "allowed.e2e.test", "/cgi-bin/echo"); !strings.HasPrefix(body, "v2 ") {
		t.Fatalf("allowed.e2e.test still answers %q after the v2 deploy", body)
	}
	if body, _ := waitHTTPS(t, "allowed2.e2e.test", "/cgi-bin/echo"); !strings.HasPrefix(body, "v2 allowed2.e2e.test ") {
		t.Fatalf("a new host after the v2 deploy answered %q", body)
	}

	// A recreated proxy finds the certificate in its data volume instead of
	// obtaining another.
	if out, err := proxyPodman("rm", "-f", proxyContainer); err != nil {
		t.Fatalf("remove proxy: %v\n%s", err, out)
	}
	release("v3")
	_, again := waitHTTPS(t, "allowed.e2e.test", "/cgi-bin/echo")
	if again.SerialNumber.Cmp(cert.SerialNumber) != 0 {
		t.Fatalf("the certificate for allowed.e2e.test was obtained again after the proxy was recreated (serial %s, then %s)", cert.SerialNumber, again.SerialNumber)
	}
}

// proxyPodman runs podman for the proxy container, through sudo when the
// proxy is rootful and the test is not root.
func proxyPodman(args ...string) (string, error) {
	if h.proxyRootful && !h.root {
		return run("sudo", append([]string{"-n", "podman"}, args...)...)
	}
	return podman(args...)
}

// proxyAdmin sends one request to the proxy's admin socket.
func proxyAdmin(method, path, body string) (string, error) {
	args := []string{"exec", "-i", proxyContainer, "curl", "-sSf", "--max-time", "30",
		"--unix-socket", "/run/azud-admin/admin.sock", "-X", method}
	if body != "" {
		args = append(args, "-H", "Content-Type: application/json", "--data-binary", "@-")
	}
	args = append(args, "http://localhost"+path)
	name, prefix := "podman", []string{}
	if h.proxyRootful && !h.root {
		name, prefix = "sudo", []string{"-n", "podman"}
	}
	command := exec.Command(name, append(prefix, args...)...)
	command.Stdin = strings.NewReader(body)
	out, err := command.CombinedOutput()
	return string(out), err
}

// useInternalIssuer points every TLS policy at Caddy's internal CA.
func useInternalIssuer(t *testing.T) {
	t.Helper()
	out, err := proxyAdmin("GET", "/config/apps/tls/automation/policies", "")
	if err != nil {
		t.Fatalf("read TLS policies: %v\n%s", err, out)
	}
	var policies []map[string]any
	if err := json.Unmarshal([]byte(out), &policies); err != nil || len(policies) == 0 {
		t.Fatalf("TLS policies %q: %v", out, err)
	}
	for _, policy := range policies {
		policy["issuers"] = []any{map[string]any{"module": "internal"}}
	}
	data, err := json.Marshal(policies)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := proxyAdmin("PATCH", "/config/apps/tls/automation/policies", string(data)); err != nil {
		t.Fatalf("use the internal CA: %v\n%s", err, out)
	}
}

func routeIDs(t *testing.T) []string {
	t.Helper()
	out, err := proxyAdmin("GET", "/config/apps/http/servers/srv0/routes", "")
	if err != nil {
		t.Fatalf("read routes: %v\n%s", err, out)
	}
	var routes []struct {
		ID string `json:"@id"`
	}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		t.Fatalf("routes %q: %v", out, err)
	}
	var ids []string
	for _, route := range routes {
		ids = append(ids, route.ID)
	}
	return ids
}

// internalRoots returns a pool with Caddy's internal root certificate.
func internalRoots() (*x509.CertPool, error) {
	out, err := proxyAdmin("GET", "/pki/ca/local", "")
	if err != nil {
		return nil, fmt.Errorf("read the internal CA: %w\n%s", err, out)
	}
	var ca struct {
		Root string `json:"root_certificate"`
	}
	if err := json.Unmarshal([]byte(out), &ca); err != nil {
		return nil, fmt.Errorf("internal CA %q: %w", out, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca.Root)) {
		return nil, fmt.Errorf("internal CA has no root certificate: %q", out)
	}
	return pool, nil
}

// httpsGet requests https://host/path from the proxy, verifying the
// certificate against Caddy's internal CA.
func httpsGet(host, path string) (int, string, error) {
	status, body, _, err := httpsGetCert(host, path)
	return status, body, err
}

// httpsGetCert is httpsGet that also returns the proxy's leaf certificate.
func httpsGetCert(host, path string) (int, string, *x509.Certificate, error) {
	roots, err := internalRoots()
	if err != nil {
		return 0, "", nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", h.httpsPort))
			},
			TLSClientConfig:   &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get("https://" + host + path)
	if err != nil {
		return 0, "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", nil, err
	}
	return resp.StatusCode, strings.TrimSpace(string(body)), resp.TLS.PeerCertificates[0], nil
}

// waitHTTPS retries until host answers 200 over HTTPS: the first handshake
// for an on-demand host waits for the permission check and the certificate.
func waitHTTPS(t *testing.T, host, path string) (string, *x509.Certificate) {
	t.Helper()
	var status int
	var body string
	var cert *x509.Certificate
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if status, body, cert, err = httpsGetCert(host, path); err == nil && status == http.StatusOK {
			return body, cert
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s%s never answered over HTTPS: last %d %q, err %v", host, path, status, body, err)
	return "", nil
}

// plainHTTP requests http://host/path from the proxy and returns where it
// redirects to.
func plainHTTP(t *testing.T, host, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", h.httpPort, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("plain HTTP for %s: %v", host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("plain HTTP for %s answered %s, want a redirect", host, resp.Status)
	}
	return resp.Header.Get("Location")
}
