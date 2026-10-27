//go:build security

package jit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/state"
)

// ---------------------------------------------------------------------------
// Finding: the Caddy admin API is published to loopback but listens on
// 0.0.0.0 inside the container, and app/accessory/cron containers share the
// "azud" network with it. Any workload can therefore read or replace the
// proxy configuration.
// ---------------------------------------------------------------------------

func TestFinding_CaddyAdminReachableFromSharedNetwork(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", proxyBootHandler())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "boot")
	if res.Exit != 0 {
		t.Fatalf("proxy boot failed: %s", res)
	}

	boot := fake.waitCommand(t, "--name azud-proxy", 5*time.Second)
	for _, want := range []string{
		"CADDY_ADMIN=0.0.0.0:2019",
		"--network azud",
		"127.0.0.1:2019:2019",
	} {
		if !strings.Contains(boot.Command, want) {
			t.Fatalf("proxy boot command missing %q:\n%s", want, boot.Command)
		}
	}

	cfg, err := config.NewLoader(configPath, "").Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	app := deploy.NewAppContainerConfig(cfg, cfg.Image, "shop", "web", nil)
	if app.Network != "azud" {
		t.Fatalf("app network = %q, want azud", app.Network)
	}
	t.Logf("proxy boot command:\n%s", boot.Command)
	t.Logf("VULNERABLE: proxy listens on %s inside the %q network; app containers share it and can call http://azud-proxy:2019/config/", "0.0.0.0:2019", app.Network)
}

// testCertificate returns a self-signed cert/key pair as PEM.
func testCertificate(t *testing.T) (string, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "shop.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"shop.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return string(certPEM), string(keyPEM)
}

func TestFinding_CaddyConfigExposesTLSPrivateKeyToAdminAPI(t *testing.T) {
	certPEM, keyPEM := testCertificate(t)
	fake := startFakeSSH(t, "127.0.0.1:0", proxyBootHandler())
	dir := newWorkdir(t)

	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
  ssl: true
  ssl_certificate: TLS_CERT
  ssl_private_key: TLS_KEY
secrets_provider: env
secrets_env_prefix: JIT_
ssh:
  user: `+currentUser()+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)
	res := runAzud(t, dir, []string{
		"JIT_TLS_CERT=" + certPEM,
		"JIT_TLS_KEY=" + keyPEM,
	}, "-c", configPath, "proxy", "boot")
	if res.Exit != 0 {
		t.Fatalf("proxy boot with custom cert failed: %s", res)
	}

	var load commandRecord
	for _, record := range fake.snapshot() {
		if strings.Contains(record.Command, "-X POST") && strings.Contains(string(record.Stdin), "BEGIN") {
			load = record
		}
	}
	if load.Command == "" {
		t.Fatalf("no Caddy /load body containing PEM material was captured:\n%s", commandDump(fake.snapshot()))
	}
	if !strings.Contains(string(load.Stdin), "BEGIN PRIVATE KEY") {
		t.Fatalf("TLS key was not part of the Caddy config body:\n%s", load.Stdin)
	}
	t.Logf("VULNERABLE: custom TLS key is embedded in the Caddy config; a workload that can reach the admin API can GET /config/ and read it")
}

// ---------------------------------------------------------------------------
// Finding: cron lock mounts the whole state directory read-write.
// ---------------------------------------------------------------------------

func TestFinding_CronLockExposesAzudStateAndCaddyConfig(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	user := "root"
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
cron:
  backup:
    schedule: "0 2 * * *"
    command: bin/backup
    lock: true
ssh:
  user: `+user+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)
	res := runAzud(t, dir, nil, "-c", configPath, "cron", "boot")
	if res.Exit != 0 {
		t.Fatalf("cron boot failed: %s", res)
	}

	run := fake.waitCommand(t, "--name shop-cron-backup", 5*time.Second)
	if !strings.Contains(run.Command, "/var/lib/azud:rw") {
		t.Fatalf("cron container does not mount /var/lib/azud:rw:\n%s", run.Command)
	}
	stateDir := state.Dir(user)
	if !strings.Contains(run.Command, stateDir) {
		t.Fatalf("cron command does not reference state dir %q:\n%s", stateDir, run.Command)
	}
	caddyConfigDir := filepath.Dir(proxy.CaddyConfigFile(user))
	if caddyConfigDir != stateDir {
		t.Fatalf("Caddy config dir %q != mounted state dir %q", caddyConfigDir, stateDir)
	}
	t.Logf("VULNERABLE: cron.lock mounts %s (contains %s with custom TLS keys) read-write into the workload container", stateDir, proxy.CaddyConfigFile(user))
}

// ---------------------------------------------------------------------------
// Control: proxy stop fails closed, is idempotent for a missing container, and
// refuses to fight systemd over a Quadlet-managed proxy.
// ---------------------------------------------------------------------------

const runningProxyInspect = `[{"Id":"abc","Config":{"Labels":{"azud.managed":"true","azud.type":"proxy"}},"State":{"Running":true}}]`

func proxyStopHandler(inspect, stop fakeResult) fakeHandler {
	return func(command string, stdin io.Reader) fakeResult {
		switch {
		case strings.Contains(command, "container inspect"):
			return inspect
		case strings.Contains(command, "podman stop"):
			return stop
		default:
			return fakeResult{}
		}
	}
}

func TestControl_ProxyStopFailsClosed(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", proxyStopHandler(
		fakeResult{Stdout: runningProxyInspect},
		fakeResult{Stderr: "simulated stop failure", Exit: 1},
	))
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "stop")
	if res.Exit == 0 {
		t.Fatalf("proxy stop exited 0 although stopping failed: %s", res)
	}
	if strings.Contains(res.Combined, "Proxy stopped") {
		t.Fatalf("proxy stop printed a success line although stopping failed: %s", res)
	}
	for _, want := range []string{"proxy stop failed on 1 host(s)", "simulated stop failure"} {
		if !strings.Contains(res.Combined, want) {
			t.Fatalf("proxy stop output missing %q: %s", want, res)
		}
	}
	if len(fake.commandsMatching("podman stop")) == 0 {
		t.Fatalf("proxy stop never attempted podman stop:\n%s", commandDump(fake.snapshot()))
	}
}

func TestControl_ProxyStopTreatsMissingContainerAsStopped(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", proxyStopHandler(
		fakeResult{Stderr: `Error: no container with name or ID "azud-proxy" found: no such container`, Exit: 125},
		fakeResult{Stderr: "podman stop must not run for a missing container", Exit: 1},
	))
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "stop")
	if res.Exit != 0 {
		t.Fatalf("proxy stop failed for an absent proxy: %s", res)
	}
	for _, want := range []string{"Proxy not present", "Proxy stopped"} {
		if !strings.Contains(res.Combined, want) {
			t.Fatalf("proxy stop output missing %q: %s", want, res)
		}
	}
	if stops := fake.commandsMatching("podman stop"); len(stops) != 0 {
		t.Fatalf("proxy stop ran podman stop for an absent proxy:\n%s", commandDump(stops))
	}
}

func TestControl_ProxyStopRefusesSystemdManagedProxy(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", proxyStopHandler(
		fakeResult{Stdout: `[{"Id":"abc","Config":{"Labels":{"azud.type":"proxy","PODMAN_SYSTEMD_UNIT":"azud-proxy.service"}},"State":{"Running":true}}]`},
		fakeResult{},
	))
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "stop")
	if res.Exit == 0 {
		t.Fatalf("proxy stop reported success for a systemd-managed proxy: %s", res)
	}
	if strings.Contains(res.Combined, "Proxy stopped") {
		t.Fatalf("proxy stop printed a success line for a systemd-managed proxy: %s", res)
	}
	for _, want := range []string{"azud-proxy.service", "systemctl --user stop"} {
		if !strings.Contains(res.Combined, want) {
			t.Fatalf("proxy stop output missing %q: %s", want, res)
		}
	}
	if stops := fake.commandsMatching("podman stop"); len(stops) != 0 {
		t.Fatalf("proxy stop ran podman stop although systemd restarts the unit:\n%s", commandDump(stops))
	}
}

// ---------------------------------------------------------------------------
// Finding: registry login accepts an unconfigured --host and sends the
// registry password there.
// ---------------------------------------------------------------------------

func TestFinding_RegistryLoginAcceptsUnconfiguredHost(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	// The configured host is 127.0.0.2; --host points at the fake 127.0.0.1.
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.2]
proxy:
  host: shop.example.com
registry:
  username: deployer
  password: [REG_PASS]
ssh:
  user: `+currentUser()+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "REG_PASS=registry-token\n", 0o600)

	res := runAzud(t, dir, nil, "-c", configPath, "registry", "login", "--host", "127.0.0.1")
	if res.Exit != 0 {
		t.Fatalf("registry login failed: %s", res)
	}
	login := fake.waitCommand(t, "--password-stdin", 5*time.Second)
	if !strings.Contains(string(login.Stdin), "registry-token") {
		t.Fatalf("registry password was not sent to the unconfigured host:\n%s", login.Stdin)
	}
	t.Logf("VULNERABLE: registry login sent the registry password to --host 127.0.0.1, which is not in cfg.GetAllSSHHosts()")
}

func TestFinding_RegistryServerFlagInjection(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
registry:
  server: "--tls-verify=false"
  username: deployer
  password: [REG_PASS]
ssh:
  user: `+currentUser()+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "REG_PASS=registry-token\n", 0o600)

	res := runAzud(t, dir, nil, "-c", configPath, "registry", "login")
	if res.Exit != 0 {
		t.Fatalf("registry login failed: %s", res)
	}
	login := fake.waitCommand(t, "--password-stdin", 5*time.Second)
	if !strings.Contains(login.Command, "--tls-verify=false") {
		t.Fatalf("expected registry.server to become a podman option:\n%s", login.Command)
	}
	t.Logf("VULNERABLE: unvalidated registry.server became a podman login option (--tls-verify=false): %s", login.Command)
}

// ---------------------------------------------------------------------------
// Finding: env pull masks a remote read failure as "no secrets".
// ---------------------------------------------------------------------------

func TestFinding_EnvPullMasksRemoteReadFailure(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		if strings.Contains(command, "cat ") {
			return fakeResult{Stderr: "cat: permission denied", Exit: 1}
		}
		return fakeResult{}
	})
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "env", "pull", "--host", "127.0.0.1")
	if res.Exit != 0 {
		t.Fatalf("expected exit 0 despite the read failure: %s", res)
	}
	if !strings.Contains(res.Combined, "No secrets found") {
		t.Fatalf("expected the misleading message; got: %s", res)
	}
	t.Logf("VULNERABLE (low): env pull ignores the remote cat exit status and reports 'No secrets found'")
}

// ---------------------------------------------------------------------------
// Finding: remote output is printed without escaping terminal control bytes.
// ---------------------------------------------------------------------------

func TestFinding_TerminalEscapeInjectionFromRemoteOutput(t *testing.T) {
	payload := "\x1b]52;c;ZXZpbC1jbGlwYm9hcmQ=\x07\x1b[2J\x1b[31mFORGED\x1b[0m\n"
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		return fakeResult{Stdout: payload}
	})
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "server", "exec", "--host", "127.0.0.1", "true")
	if res.Exit != 0 {
		t.Fatalf("server exec failed: %s", res)
	}
	if !strings.Contains(res.Stdout, "\x1b]52;c;") {
		t.Fatalf("expected raw OSC sequence in output, got: %q", res.Stdout)
	}
	t.Logf("VULNERABLE (low): remote container output is written to the terminal without stripping ANSI/OSC control sequences")
}

// ---------------------------------------------------------------------------
// Hardening: the proxy container is not dropped to a non-root user and does
// not use the same isolation flags Azud applies to its helper container.
// ---------------------------------------------------------------------------

func TestFinding_ProxyContainerLacksHardeningFlags(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", proxyBootHandler())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "boot")
	if res.Exit != 0 {
		t.Fatalf("proxy boot failed: %s", res)
	}
	boot := fake.waitCommand(t, "--name azud-proxy", 5*time.Second)
	var missing []string
	for _, flag := range []string{"--cap-drop", "--security-opt", "--read-only", "--user"} {
		if !strings.Contains(boot.Command, flag) {
			missing = append(missing, flag)
		}
	}
	if len(missing) == 0 {
		t.Fatalf("proxy container now uses hardening flags; this finding may be fixed:\n%s", boot.Command)
	}
	t.Logf("HARDENING: proxy container lacks %s; it runs the upstream Caddy image as its default user and has the full default capability set", strings.Join(missing, ", "))
}

// ---------------------------------------------------------------------------
// Hardening: enabling access logging without a redact list produces a plain
// JSON encoder. Caddy's access log includes request headers, so
// Authorization/Cookie values are recorded unless the operator opts in to
// redaction.
// ---------------------------------------------------------------------------

func TestFinding_AccessLogRedactionIsOptIn(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", proxyBootHandler())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
proxy:
  host: shop.example.com
  logging:
    enabled: true
ssh:
  user: `+currentUser()+`
  port: `+itoa(fake.port())+`
  keys: ["`+filepath.ToSlash(clientKeyPath(t))+`"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`)
	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "boot")
	if res.Exit != 0 {
		t.Fatalf("proxy boot failed: %s", res)
	}
	var body []byte
	for _, record := range fake.snapshot() {
		if strings.Contains(record.Command, "-X POST") && len(record.Stdin) > 0 {
			body = record.Stdin
		}
	}
	if len(body) == 0 {
		t.Fatalf("no Caddy config body captured:\n%s", commandDump(fake.snapshot()))
	}
	if strings.Contains(string(body), `"filter"`) {
		t.Fatalf("access logging now includes a redaction filter; finding may be fixed:\n%s", body)
	}
	t.Logf("HARDENING: logging.enabled=true produced a plain JSON encoder with no redaction filter; Caddy access logs include request headers unless Authorization/Cookie are configured for deletion")
}
