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
	"sync"
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
// Control: cron lock mounts only a read-only per-job lock directory that
// lives outside the Azud state directory.
// ---------------------------------------------------------------------------

func cronLockConfig(user string, port int, keyPath string) string {
	return `service: shop
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
  user: ` + user + `
  port: ` + itoa(port) + `
  keys: ["` + filepath.ToSlash(keyPath) + `"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`
}

// legacyCronInspect is what `podman container inspect` reports for a cron
// container created by a release that mounted the whole state directory.
const legacyCronInspect = `[{"Id":"abc","Config":{"Labels":{"azud.cron":"backup","azud.managed":"true"}},` +
	`"Mounts":[{"Type":"bind","Source":"/var/lib/azud","Destination":"/var/lib/azud","RW":true}],"State":{"Running":true}}]`

// cronHandler fakes the commands of `cron boot` and `cron run`. While legacy
// is true the scheduled container exists with the legacy state mount; a
// `podman rm` of it makes it disappear.
func cronHandler(legacy bool) fakeHandler {
	var mu sync.Mutex
	exists := legacy
	return func(command string, stdin io.Reader) fakeResult {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(command, "echo LOCKED"):
			return fakeResult{Stdout: "LOCKED\n"}
		case strings.Contains(command, "podman rm") && strings.Contains(command, "shop-cron-backup"):
			exists = false
			return fakeResult{}
		case strings.Contains(command, "container inspect shop-cron-backup"):
			if !exists {
				return fakeResult{Stderr: `Error: no container with name or ID "shop-cron-backup" found: no such container`, Exit: 125}
			}
			if strings.Contains(command, "State.Running") {
				return fakeResult{Stdout: "true\n"}
			}
			return fakeResult{Stdout: legacyCronInspect}
		default:
			return fakeResult{}
		}
	}
}

func commandIndex(records []commandRecord, match func(string) bool) int {
	for i, record := range records {
		if match(record.Command) {
			return i
		}
	}
	return -1
}

func TestControl_CronLockMountsOnlyReadOnlyJobLockDir(t *testing.T) {
	tests := []struct {
		user     string
		mount    string
		lockFile string
	}{
		{
			user:     "root",
			mount:    "-v '/var/lib/azud-locks/cron/shop/backup:/run/azud-cron-lock:ro,Z'",
			lockFile: "/var/lib/azud-locks/cron/shop/backup/cron.lock",
		},
		{
			user:     "deploy",
			mount:    "-v ${HOME}/'.local/share/azud-locks/cron/shop/backup:/run/azud-cron-lock:ro,Z'",
			lockFile: `"${HOME}/".local/share/azud-locks/cron/shop/backup/cron.lock`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.user, func(t *testing.T) {
			fake := startFakeSSH(t, "127.0.0.1:0", cronHandler(false))
			dir := newWorkdir(t)
			configPath := writeConfig(t, dir, cronLockConfig(tt.user, fake.port(), clientKeyPath(t)))

			res := runAzud(t, dir, nil, "-c", configPath, "cron", "boot")
			if res.Exit != 0 {
				t.Fatalf("cron boot failed: %s", res)
			}

			run := fake.waitCommand(t, "--name shop-cron-backup ", 5*time.Second)
			if !strings.Contains(run.Command, tt.mount) {
				t.Fatalf("cron container does not mount only the read-only job lock dir %q:\n%s", tt.mount, run.Command)
			}
			if !strings.Contains(run.Command, "flock -n /run/azud-cron-lock/cron.lock") {
				t.Fatalf("crontab does not lock the mounted file:\n%s", run.Command)
			}
			stateDir := state.Dir(tt.user)
			for _, forbidden := range []string{stateDir + ":", "/var/lib/azud:", ":rw", proxy.CaddyConfigFileName} {
				if strings.Contains(run.Command, forbidden) {
					t.Fatalf("cron container command contains %q (state dir %s):\n%s", forbidden, stateDir, run.Command)
				}
			}

			records := fake.snapshot()
			prepare := commandIndex(records, func(command string) bool {
				return strings.Contains(command, "umask 077") && strings.Contains(command, "chmod 711") && strings.Contains(command, tt.lockFile)
			})
			runIndex := commandIndex(records, func(command string) bool { return strings.Contains(command, "--name shop-cron-backup ") })
			if prepare < 0 || prepare > runIndex {
				t.Fatalf("lock directory was not prepared before the container started (prepare=%d run=%d):\n%s", prepare, runIndex, commandDump(records))
			}
			if !strings.Contains(records[prepare].Command, "refusing symlinked lock") {
				t.Fatalf("lock preparation does not refuse symlinks:\n%s", records[prepare].Command)
			}
		})
	}
}

func TestControl_CronBootRecreatesLegacyLockMount(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", cronHandler(true))
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, cronLockConfig("root", fake.port(), clientKeyPath(t)))

	res := runAzud(t, dir, nil, "-c", configPath, "cron", "boot")
	if res.Exit != 0 {
		t.Fatalf("cron boot failed: %s", res)
	}
	if strings.Contains(res.Combined, "already running") {
		t.Fatalf("cron boot kept the legacy container running: %s", res)
	}

	records := fake.snapshot()
	pull := commandIndex(records, func(command string) bool { return strings.Contains(command, "podman pull") })
	check := commandIndex(records, func(command string) bool { return strings.Contains(command, "--name shop-cron-backup-runtime-check-") })
	prepare := commandIndex(records, func(command string) bool { return strings.Contains(command, "chmod 711") })
	stop := commandIndex(records, func(command string) bool { return strings.Contains(command, "podman stop -t 30 shop-cron-backup") })
	remove := commandIndex(records, func(command string) bool { return strings.Contains(command, "podman rm -f shop-cron-backup") })
	run := commandIndex(records, func(command string) bool { return strings.Contains(command, "--name shop-cron-backup ") })
	if pull < 0 || check < 0 || prepare < 0 || stop < pull || stop < check || stop < prepare {
		t.Fatalf("legacy cron container was stopped before the replacement was prepared (pull=%d check=%d prepare=%d stop=%d):\n%s", pull, check, prepare, stop, commandDump(records))
	}
	if stop < 0 || remove < stop || run < remove {
		t.Fatalf("legacy cron container was not stopped, removed and recreated in order (stop=%d rm=%d run=%d):\n%s", stop, remove, run, commandDump(records))
	}
	if !strings.Contains(records[run].Command, "/var/lib/azud-locks/cron/shop/backup:/run/azud-cron-lock:ro,Z") {
		t.Fatalf("recreated cron container does not use the narrow lock mount:\n%s", records[run].Command)
	}
}

func TestControl_CronBootKeepsLegacyContainerWhenPreparationFails(t *testing.T) {
	tests := []struct {
		name  string
		match string
	}{
		{name: "pull", match: "podman pull"},
		{name: "runtime", match: "--name shop-cron-backup-runtime-check-"},
		{name: "lock", match: "chmod 711"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacy := cronHandler(true)
			fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
				if strings.Contains(command, tt.match) {
					return fakeResult{Stderr: "simulated " + tt.name + " failure", Exit: 125}
				}
				return legacy(command, stdin)
			})
			dir := newWorkdir(t)
			configPath := writeConfig(t, dir, cronLockConfig("root", fake.port(), clientKeyPath(t)))

			res := runAzud(t, dir, nil, "-c", configPath, "cron", "boot")
			if res.Exit == 0 {
				t.Fatalf("cron boot exited 0 although %s failed: %s", tt.name, res)
			}
			if len(fake.commandsMatching(tt.match)) == 0 {
				t.Fatalf("cron boot never reached the %s step:\n%s", tt.name, commandDump(fake.snapshot()))
			}
			for _, destructive := range []string{"podman stop", "podman rm"} {
				if matches := fake.commandsMatching(destructive); len(matches) > 0 {
					t.Fatalf("cron boot ran %q on the legacy container although %s failed:\n%s", destructive, tt.name, commandDump(fake.snapshot()))
				}
			}
		})
	}
}

func TestControl_CronRunHoldsLegacyLockWhileLegacyContainerExists(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", cronHandler(true))
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, cronLockConfig("root", fake.port(), clientKeyPath(t)))

	res := runAzud(t, dir, nil, "-c", configPath, "cron", "run", "backup")
	if res.Exit != 0 {
		t.Fatalf("cron run failed: %s", res)
	}
	if !strings.Contains(res.Combined, "azud cron boot") {
		t.Fatalf("cron run did not tell the operator to recreate the legacy container: %s", res)
	}

	records := fake.snapshot()
	newLock := commandIndex(records, func(command string) bool {
		return strings.Contains(command, "flock -x") && strings.Contains(command, "/var/lib/azud-locks/cron/shop/backup/cron.lock")
	})
	check := commandIndex(records, func(command string) bool {
		return strings.Contains(command, "test ! -L /var/lib/azud/shop-cron-backup.lock")
	})
	legacyLock := commandIndex(records, func(command string) bool {
		return strings.Contains(command, "flock -x") && strings.Contains(command, "/var/lib/azud/shop-cron-backup.lock")
	})
	run := commandIndex(records, func(command string) bool { return strings.Contains(command, "--name shop-cron-backup-run") })
	if newLock < 0 || check < 0 || legacyLock < 0 || run < 0 {
		t.Fatalf("cron run did not hold both locks around the job (new=%d check=%d legacy=%d run=%d):\n%s", newLock, check, legacyLock, run, commandDump(records))
	}
	if check > legacyLock {
		t.Fatalf("legacy lock was taken before its symlink check:\n%s", commandDump(records))
	}
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
// Control: registry login only targets configured hosts, and registry.server
// can never become a podman option.
// ---------------------------------------------------------------------------

func registryConfig(t *testing.T, port int, webHost, server string) string {
	serverLine := ""
	if server != "" {
		serverLine = "  server: " + server + "\n"
	}
	return `service: shop
image: example/shop:v1
servers:
  web:
    hosts: [` + webHost + `]
proxy:
  host: shop.example.com
registry:
` + serverLine + `  username: deployer
  password: [REG_PASS]
ssh:
  user: ` + currentUser() + `
  port: ` + itoa(port) + `
  keys: ["` + filepath.ToSlash(clientKeyPath(t)) + `"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`
}

func TestControl_RegistryLoginRejectsUnconfiguredHost(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	// The configured host is 127.0.0.2; --host points at the fake 127.0.0.1.
	configPath := writeConfig(t, dir, registryConfig(t, fake.port(), "127.0.0.2", ""))
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "REG_PASS=registry-token\n", 0o600)

	for _, command := range []string{"login", "logout"} {
		res := runAzud(t, dir, nil, "-c", configPath, "registry", command, "--host", "127.0.0.1")
		if res.Exit == 0 {
			t.Fatalf("registry %s to an unconfigured host succeeded: %s", command, res)
		}
		if !strings.Contains(res.Combined, "is not configured") {
			t.Fatalf("registry %s: expected an unconfigured-host error, got: %s", command, res)
		}
	}
	if got := fake.snapshot(); len(got) != 0 {
		t.Fatalf("expected no SSH commands for an unconfigured host, got:\n%s", commandDump(got))
	}
}

func TestControl_RegistryServerIsValidated(t *testing.T) {
	for _, server := range []string{`"--tls-verify=false"`, `"--help"`, `"-R"`, `"--authfile=/tmp/x"`, `" ghcr.io"`} {
		t.Run(server, func(t *testing.T) {
			fake := startFakeSSH(t, "127.0.0.1:0", permissive())
			dir := newWorkdir(t)
			configPath := writeConfig(t, dir, registryConfig(t, fake.port(), "127.0.0.1", server))
			writeFile(t, filepath.Join(dir, ".azud", "secrets"), "REG_PASS=registry-token\n", 0o600)

			res := runAzud(t, dir, nil, "-c", configPath, "registry", "login")
			if res.Exit == 0 {
				t.Fatalf("registry login with server %s succeeded: %s", server, res)
			}
			if !strings.Contains(res.Combined, "registry.server") {
				t.Fatalf("expected a registry.server validation error, got: %s", res)
			}
			if got := fake.snapshot(); len(got) != 0 {
				t.Fatalf("expected no SSH commands with an invalid registry.server, got:\n%s", commandDump(got))
			}
		})
	}
}

func TestControl_RegistryLoginTerminatesPodmanOptions(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, registryConfig(t, fake.port(), "127.0.0.1", "ghcr.io"))
	writeFile(t, filepath.Join(dir, ".azud", "secrets"), "REG_PASS=registry-token\n", 0o600)

	res := runAzud(t, dir, nil, "-c", configPath, "registry", "login")
	if res.Exit != 0 {
		t.Fatalf("registry login failed: %s", res)
	}
	login := fake.waitCommand(t, "--password-stdin", 5*time.Second)
	if !strings.HasSuffix(login.Command, "--password-stdin -- ghcr.io") {
		t.Fatalf("podman login does not end options before the server:\n%s", login.Command)
	}
	if !strings.Contains(string(login.Stdin), "registry-token") {
		t.Fatalf("registry password was not sent on stdin:\n%s", login.Stdin)
	}
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
// Control: Caddy redacts Authorization, Cookie, Proxy-Authorization and
// Set-Cookie in access logs unless a server sets logs.should_log_credentials
// (the Caddyfile log_credentials option). Azud always removes it, so
// credential redaction cannot be switched off through the live or persisted
// config.
// ---------------------------------------------------------------------------

// credentialLoggingConfig is a persisted/live Caddy config whose srv0 turns
// Caddy's built-in credential redaction off.
const credentialLoggingConfig = `{"admin":{"listen":"0.0.0.0:2019"},"apps":{"http":{"servers":{"srv0":` +
	`{"listen":[":80"],"logs":{"should_log_credentials":true},"routes":[]}}}}}`

func TestControl_AccessLogKeepsCaddyCredentialRedaction(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		switch {
		case strings.Contains(command, "echo LOCKED"):
			return fakeResult{Stdout: "LOCKED\n"}
		case strings.Contains(command, "container inspect"):
			return fakeResult{Stderr: `Error: no container with name or ID "azud-proxy" found: no such container`, Exit: 125}
		case strings.Contains(command, "curl") && strings.Contains(command, "/load"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		case strings.Contains(command, "curl") && strings.Contains(command, "/config/"):
			return fakeResult{Stdout: credentialLoggingConfig}
		case strings.Contains(command, "test -f"):
			// The protected bootstrap copy that restoreConfig loads.
			return fakeResult{Stdout: credentialLoggingConfig}
		case strings.Contains(command, "cat > ") || strings.Contains(command, "umask 077"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		default:
			return fakeResult{}
		}
	})
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
	var loads [][]byte
	for _, record := range fake.snapshot() {
		if strings.Contains(record.Command, "/load") && len(record.Stdin) > 0 {
			loads = append(loads, record.Stdin)
		}
	}
	if len(loads) == 0 {
		t.Fatalf("no Caddy /load body captured:\n%s", commandDump(fake.snapshot()))
	}
	final := string(loads[len(loads)-1])
	if strings.Contains(final, "should_log_credentials") {
		t.Fatalf("Azud kept should_log_credentials in the reconciled Caddy config:\n%s", final)
	}
	if !strings.Contains(final, `"default_logger_name":"access"`) {
		t.Fatalf("reconciled Caddy config lost the access logger:\n%s", final)
	}
}
