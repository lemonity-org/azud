//go:build e2e

// Package e2e runs the real azud binary against real Podman, Caddy and a registry,
// with no sshd. It covers the path pull mode runs in production: azud on the server,
// over the local transport. It needs a Linux host where the test's user can run
// Podman; see scripts/e2e-in-container.sh for running it from any machine with Docker.
package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	registryHost      = "127.0.0.1:5443"
	registryContainer = "azud-e2e-registry"
	trackTag          = "production"
	proxyContainer    = "azud-proxy"
	netnsKeeper       = "azud-e2e-netns"
)

// h is shared by every test: one registry and one proxy, as on a real server.
var h *harness

type harness struct {
	binary       string
	workDir      string
	caFile       string
	user         string
	home         string
	root         bool
	proxyRootful bool
	httpPort     int
	httpsPort    int
	registry     *http.Client
	web          *http.Client
}

func TestMain(m *testing.M) {
	code, err := runTests(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runTests(m *testing.M) (int, error) {
	workDir, err := os.MkdirTemp("", "azud-e2e-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	current, err := user.Current()
	if err != nil {
		return 0, err
	}
	h = &harness{
		workDir:      workDir,
		user:         current.Username,
		home:         current.HomeDir,
		root:         os.Getuid() == 0,
		proxyRootful: os.Getenv("AZUD_E2E_PROXY_ROOTFUL") == "true",
		web:          &http.Client{Timeout: 5 * time.Second},
	}
	h.httpPort, h.httpsPort = 8080, 8443
	if h.proxyRootful {
		h.httpPort, h.httpsPort = 80, 443
	}

	if h.binary, err = buildAzud(workDir); err != nil {
		return 0, err
	}
	defer h.cleanupShared()
	if err := h.bootstrap(); err != nil {
		return 0, err
	}
	if err := h.startRegistry(); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

// bootstrap does what `azud setup` leaves behind on v1 that a deploy cannot create for
// itself: without the azud network, a deploy fails with "network not found".
func (h *harness) bootstrap() error {
	if out, err := podman("network", "create", "--ignore", "azud"); err != nil {
		return fmt.Errorf("create azud network: %w\n%s", err, out)
	}
	if h.root {
		return nil
	}
	// With a rootful proxy, an app can be the only rootless container on the network.
	// Replacing it then makes Podman tear down the rootless network namespace, which on
	// GitHub's runners fails with "kill network process: permission denied". These tests
	// are not about that, so something else keeps the namespace in use.
	_, _ = podman("rm", "-f", netnsKeeper)
	if out, err := podman("run", "-d", "--name", netnsKeeper, "--network", "azud",
		"docker.io/library/busybox:1.37", "sleep", "infinity"); err != nil {
		return fmt.Errorf("start %s: %w\n%s", netnsKeeper, err, out)
	}
	return nil
}

// buildAzud returns AZUD_E2E_BINARY when set, because a service user running a
// compiled test binary may have no Go toolchain, and builds the binary otherwise.
func buildAzud(workDir string) (string, error) {
	if binary := os.Getenv("AZUD_E2E_BINARY"); binary != "" {
		return binary, nil
	}
	binary := filepath.Join(workDir, "azud")
	build := exec.Command("go", "build", "-o", binary, "../cmd/azud")
	if output, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build azud: %w\n%s", err, output)
	}
	return binary, nil
}

// startRegistry runs a real registry behind TLS from a throwaway CA, because the
// agent's registry client speaks only HTTPS. Go reads the CA from SSL_CERT_FILE and
// Podman from certs.d.
func (h *harness) startRegistry() error {
	certDir := filepath.Join(h.workDir, "registry-certs")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return err
	}
	caPEM, err := writeTestCerts(certDir)
	if err != nil {
		return err
	}
	h.caFile = filepath.Join(certDir, "ca.crt")

	trustDir := filepath.Join(h.home, ".config/containers/certs.d", registryHost)
	if h.root {
		trustDir = filepath.Join("/etc/containers/certs.d", registryHost)
	}
	if err := os.MkdirAll(trustDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(trustDir, "ca.crt"), caPEM, 0o644); err != nil {
		return err
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	h.registry = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}

	_, _ = podman("rm", "-f", registryContainer)
	if out, err := podman("run", "-d", "--name", registryContainer,
		"-p", registryHost+":5000",
		"-v", certDir+":/certs:ro",
		"-e", "REGISTRY_HTTP_TLS_CERTIFICATE=/certs/server.crt",
		"-e", "REGISTRY_HTTP_TLS_KEY=/certs/server.key",
		"docker.io/library/registry:2"); err != nil {
		return fmt.Errorf("start registry: %w\n%s", err, out)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := h.registry.Get("https://" + registryHost + "/v2/")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registry did not answer: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (h *harness) cleanupShared() {
	_, _ = podman("rm", "-f", registryContainer)
	if h.proxyRootful && !h.root {
		_, _ = run("sudo", "-n", "podman", "rm", "-f", proxyContainer)
	} else {
		_, _ = podman("rm", "-f", proxyContainer)
	}
	_, _ = podman("rm", "-f", netnsKeeper)
	_, _ = podman("network", "rm", "azud")
}

func writeTestCerts(dir string) ([]byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "azud e2e CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return nil, err
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	files := map[string][]byte{
		"ca.crt":     caPEM,
		"server.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		"server.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return nil, err
		}
	}
	return caPEM, nil
}

// service is one azud service on the shared server, with its own config, state,
// repository and proxy route, so tests do not depend on each other.
type service struct {
	t      *testing.T
	name   string
	dir    string
	state  string
	config string
	signed string
	repo   string
	host   string
	built  int
}

func newService(t *testing.T, name string) *service {
	t.Helper()
	dir := t.TempDir()
	s := &service{
		t:      t,
		name:   name,
		dir:    dir,
		state:  filepath.Join(dir, "state"),
		config: filepath.Join(dir, "deploy.yml"),
		signed: filepath.Join(dir, "signed"),
		repo:   registryHost + "/e2e/" + name,
		host:   name + ".e2e.test",
	}
	if err := os.MkdirAll(s.signed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.config, []byte(s.renderConfig()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.removeContainers)
	return s
}

// renderConfig is a server's config for the v1 agent. verify_command stands in for
// cosign: only a digest the test has signed passes, which is the gate under test.
// ssh.command_timeout is raised because the default also bounds podman pull.
func (s *service) renderConfig() string {
	rootless := !h.root
	return fmt.Sprintf(`service: %s
image: %s

servers:
  web:
    hosts:
      - 127.0.0.1

proxy:
  host: %s
  ssl: false
  rootful: %t
  http_port: %d
  https_port: %d
  app_port: 8080
  healthcheck:
    path: /
    interval: 1s
    timeout: 2s

deploy:
  deploy_timeout: 30s
  drain_timeout: 1s

podman:
  rootless: %t

ssh:
  user: %s
  command_timeout: 5m

agent:
  track: %s
  interval: 1m
  verify_command: test -f "%s/$$AZUD_AGENT_DIGEST"
`, s.name, s.repo, s.host, h.proxyRootful, h.httpPort, h.httpsPort, rootless, h.user, trackTag, s.signed)
}

// roleContainer is the stable name azud gives the web role: the service name.
func (s *service) roleContainer() string { return s.name }

func (s *service) removeContainers() {
	out, err := podman("ps", "-aq", "--filter", "label=azud.service="+s.name)
	if err != nil {
		return
	}
	for _, id := range strings.Fields(out) {
		_, _ = podman("rm", "-f", id)
	}
}

// publish builds an image that serves version on :8080 and pushes it by digest,
// without moving the track tag.
func (s *service) publish(version string) string {
	return s.publishImage(version, `CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]`)
}

// publishBroken builds an image that runs but never opens the app port, so it fails
// readiness.
func (s *service) publishBroken(version string) string {
	return s.publishImage(version, `CMD ["sleep", "3600"]`)
}

func (s *service) publishImage(version, cmd string) string {
	s.t.Helper()
	s.built++
	ctx := filepath.Join(s.dir, fmt.Sprintf("build-%d", s.built))
	if err := os.MkdirAll(ctx, 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctx, "index.html"), []byte(version+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
	containerfile := "FROM docker.io/library/busybox:1.37\nCOPY index.html /www/index.html\n" + cmd + "\n"
	if err := os.WriteFile(filepath.Join(ctx, "Containerfile"), []byte(containerfile), 0o644); err != nil {
		s.t.Fatal(err)
	}
	image := s.repo + ":" + version
	if out, err := podman("build", "-q", "-t", image, ctx); err != nil {
		s.t.Fatalf("build %s: %v\n%s", image, err, out)
	}
	digestFile := filepath.Join(ctx, "digest")
	if out, err := podman("push", "--digestfile", digestFile, image); err != nil {
		s.t.Fatalf("push %s: %v\n%s", image, err, out)
	}
	digest, err := os.ReadFile(digestFile)
	if err != nil {
		s.t.Fatal(err)
	}
	return strings.TrimSpace(string(digest))
}

// sign marks a digest as passing verify_command.
func (s *service) sign(digest string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.signed, digest), nil, 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// promote moves the track tag to digest by re-putting the manifest's exact bytes, as
// crane tag does, so the tag resolves to the digest that was signed.
func (s *service) promote(digest string) {
	s.t.Helper()
	path := strings.TrimPrefix(s.repo, registryHost+"/")
	get, err := http.NewRequest(http.MethodGet, "https://"+registryHost+"/v2/"+path+"/manifests/"+digest, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	get.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	resp, err := h.registry.Do(get)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("read manifest %s: %s\n%s", digest, resp.Status, body)
	}

	put, err := http.NewRequest(http.MethodPut, "https://"+registryHost+"/v2/"+path+"/manifests/"+trackTag, bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	put.Header.Set("Content-Type", resp.Header.Get("Content-Type"))
	putResp, err := h.registry.Do(put)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = putResp.Body.Close() }()
	if putResp.StatusCode != http.StatusCreated {
		out, _ := io.ReadAll(putResp.Body)
		s.t.Fatalf("move tag: %s\n%s", putResp.Status, out)
	}
}

// release publishes, signs and promotes a working version.
func (s *service) release(version string) string {
	s.t.Helper()
	digest := s.publish(version)
	s.sign(digest)
	s.promote(digest)
	return digest
}

// agentOnce runs one agent check and returns its output and error.
func (s *service) agentOnce(args ...string) (string, error) {
	command := exec.Command(h.binary, append([]string{"agent", "run", "--once", "-c", s.config}, args...)...)
	command.Dir = s.dir
	command.Env = append(os.Environ(), "AZUD_STATE_DIR="+s.state, "SSL_CERT_FILE="+h.caFile)
	out, err := command.CombinedOutput()
	return string(out), err
}

// deploys runs one check that must succeed.
func (s *service) deploys() string {
	s.t.Helper()
	out, err := s.agentOnce()
	if err != nil {
		s.t.Fatalf("agent run --once: %v\n%s", err, out)
	}
	return out
}

// agentState is the agent's state file, read as the operator would see it.
type agentState struct {
	DeployedDigest string `json:"deployed_digest"`
	FailedDigest   string `json:"failed_digest"`
	LastError      string `json:"last_error"`
}

func (s *service) agentState() agentState {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join(s.state, "agent", s.name+".json"))
	if err != nil {
		s.t.Fatal(err)
	}
	var st agentState
	if err := json.Unmarshal(data, &st); err != nil {
		s.t.Fatal(err)
	}
	return st
}

// serves returns what the service answers through the proxy.
func (s *service) serves() (string, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", h.httpPort), nil)
	if err != nil {
		return "", err
	}
	req.Host = s.host
	resp, err := h.web.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}

func (s *service) waitServes(want string) {
	s.t.Helper()
	var got string
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if got, err = s.serves(); err == nil && got == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.t.Fatalf("proxy never served %q: last got %q, err %v", want, got, err)
}

// inspect reads one field of the role's container.
func (s *service) inspect(format string) string {
	s.t.Helper()
	out, err := podman("container", "inspect", "--format", format, s.roleContainer())
	if err != nil {
		s.t.Fatalf("inspect %s: %v\n%s", s.roleContainer(), err, out)
	}
	return strings.TrimSpace(out)
}

func podman(args ...string) (string, error) { return run("podman", args...) }

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
