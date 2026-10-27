//go:build security

package jit

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const caddyAdminListen = "unix//run/azud-admin/admin.sock|0600"

// proxyTopology is one supported proxy deployment mode.
type proxyTopology struct {
	name    string
	user    string
	podman  string // command prefix Azud must use for the proxy
	network string // proxy container network
	extra   string // additional YAML appended to the base config
}

func proxyTopologies() []proxyTopology {
	return []proxyTopology{
		{name: "rootful root", user: "root", podman: "podman", network: "azud"},
		{
			name: "rootless bridge", user: "deploy", podman: "podman", network: "azud",
			extra: "podman:\n  rootless: true\n",
		},
		{
			// Rootless apps with a rootful, host-networked proxy driven through
			// passwordless sudo by a non-root SSH user.
			name: "mixed host network", user: "deploy", podman: "sudo -n podman", network: "host",
			extra: "podman:\n  rootless: true\n",
		},
	}
}

func (p proxyTopology) config(port int, keyPath string) string {
	proxyBlock := "proxy:\n  host: shop.example.com\n"
	switch p.name {
	case "rootless bridge":
		proxyBlock += "  http_port: 8080\n  https_port: 8443\n"
	case "mixed host network":
		proxyBlock += "  rootful: true\n"
	}
	return fmt.Sprintf(`service: shop
image: example/shop:v1
servers:
  web:
    hosts: [127.0.0.1]
%s%sssh:
  user: %s
  port: %d
  keys: ["%s"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`, proxyBlock, p.extra, p.user, port, filepath.ToSlash(keyPath))
}

// assertAdminCallsUsePrivateSocket checks every Caddy admin request Azud sent:
// each one execs the image's curl against the private socket with the
// expected podman prefix, and every complete config it loads keeps the admin
// listener on that socket.
func assertAdminCallsUsePrivateSocket(t *testing.T, records []commandRecord, podmanPrefix string) {
	t.Helper()
	var calls, loads int
	for _, record := range records {
		for _, forbidden := range []string{":2019", "localhost:2019", "127.0.0.1:2019"} {
			if strings.Contains(record.Command, forbidden) {
				t.Fatalf("command still targets the TCP admin port:\n%s", record.Command)
			}
		}
		if !strings.Contains(record.Command, "curl") {
			continue
		}
		if !isCaddyAdminCall(record.Command) {
			t.Fatalf("curl command does not use the private admin transport:\n%s", record.Command)
		}
		if !strings.HasPrefix(record.Command, podmanPrefix+" exec ") ||
			!strings.Contains(record.Command, " azud-proxy curl -sSf --max-time 30 --unix-socket /run/azud-admin/admin.sock -X ") {
			t.Fatalf("admin call does not exec into azud-proxy with %q:\n%s", podmanPrefix, record.Command)
		}
		calls++
		if strings.HasSuffix(record.Command, " http://localhost/load") {
			loads++
			assertAdminListenerIsPrivateSocket(t, record.Stdin)
		}
	}
	if calls == 0 || loads == 0 {
		t.Fatalf("expected admin calls and a /load (calls=%d loads=%d):\n%s", calls, loads, commandDump(records))
	}
}

func assertAdminListenerIsPrivateSocket(t *testing.T, body []byte) {
	t.Helper()
	var config map[string]interface{}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatalf("Caddy config body is not a JSON object: %v\n%s", err, body)
	}
	want := map[string]interface{}{"listen": caddyAdminListen}
	if !reflect.DeepEqual(config["admin"], want) {
		t.Fatalf("Caddy admin object = %#v, want %#v", config["admin"], want)
	}
}

const legacyProxyInspect = `[{
	"Config":{
		"Cmd":["caddy","run","--config","/etc/caddy/Caddyfile","--adapter","caddyfile","--resume"],
		"Env":["CADDY_ADMIN=0.0.0.0:2019","PATH=/usr/bin"],
		"Labels":{"azud.managed":"true","azud.type":"proxy"%s}
	},
	"HostConfig":{"NetworkMode":"bridge","PortBindings":{"2019/tcp":[{"HostIp":"127.0.0.1","HostPort":"2019"}]}},
	"NetworkSettings":{"Networks":{"azud":{}}},
	"State":{"Running":true}
}]`

// legacyAutosave is what an older proxy leaves in caddy_config: a TCP admin
// listener plus admin keys planted through the formerly reachable API.
const legacyAutosave = `{"admin":{"listen":"0.0.0.0:2019","remote":{"listen":":2021"},"config":{"load":{"module":"http","url":"http://attacker.invalid/"}}},` +
	`"apps":{"http":{"servers":{"srv0":{"listen":[":80"],"routes":[{"@id":"azud-route-shop","match":[{"host":["shop.example.com"]}]}]}}}}}`

const runningCaddyConfig = `{"admin":{"listen":"` + caddyAdminListen + `"},"apps":{"http":{"servers":{"srv0":{"listen":[":80"],"routes":[]}}}}}`

// legacyProxyHost simulates a host running a proxy container created by an
// older Azud release. The container disappears once Azud removes it, and
// moves to azud-proxy-legacy while Boot starts its replacement.
type legacyProxyHost struct {
	mu        sync.Mutex
	labels    string
	autosave  string
	persisted string
	removed   bool
	moved     bool // the legacy container is named azud-proxy-legacy
	replaced  bool // a replacement azud-proxy container exists
	// replacementExits makes the replacement exit at once, as the hardened
	// container does under a forced user namespace.
	replacementExits bool
}

const noSuchProxyContainer = "Error: no container with name or ID \"azud-proxy\" found: no such container"

func (h *legacyProxyHost) handler() fakeHandler {
	return func(command string, stdin io.Reader) fakeResult {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case strings.Contains(command, "echo LOCKED"):
			return fakeResult{Stdout: "LOCKED\n"}
		case strings.Contains(command, "container inspect azud-proxy-legacy"):
			if !h.moved {
				return fakeResult{Exit: 125, Stderr: noSuchProxyContainer}
			}
			return fakeResult{Stdout: "legacy0123\n"}
		case strings.Contains(command, "container inspect azud-proxy") && h.replaced:
			if strings.Contains(command, "{{.State.Running}}") {
				return fakeResult{Stdout: fmt.Sprintf("%t\n", !h.replacementExits)}
			}
			return fakeResult{Stdout: "replacement0123\n"}
		case strings.Contains(command, "container inspect azud-proxy"):
			if h.removed || h.moved {
				return fakeResult{Exit: 125, Stderr: noSuchProxyContainer}
			}
			switch {
			case strings.Contains(command, "{{.State.Running}}"):
				return fakeResult{Stdout: "true\n"}
			case strings.Contains(command, "{{.Id}}"):
				return fakeResult{Stdout: "legacy0123\n"}
			default:
				return fakeResult{Stdout: fmt.Sprintf(legacyProxyInspect, h.labels)}
			}
		case strings.HasSuffix(command, " rename azud-proxy azud-proxy-legacy"):
			h.moved = true
			return fakeResult{}
		case strings.HasSuffix(command, " rename azud-proxy-legacy azud-proxy"):
			h.moved = false
			return fakeResult{}
		case strings.HasSuffix(command, " rm -f azud-proxy-legacy"):
			h.moved, h.removed = false, true
			return fakeResult{}
		case strings.HasSuffix(command, " rm -f azud-proxy"):
			if h.replaced {
				h.replaced = false
			} else {
				h.removed = true
			}
			return fakeResult{}
		case strings.Contains(command, "--name azud-proxy "):
			h.replaced = true
			return fakeResult{}
		case isCaddyAdminCall(command) && h.replaced && h.replacementExits:
			return fakeResult{Exit: 255, Stderr: "Error: can only create exec sessions on running containers: container state improper"}
		case strings.Contains(command, "if [ -s /config/caddy/autosave.json ]"):
			return fakeResult{Stdout: h.autosave}
		case strings.Contains(command, "autosave.json.azud-tmp"):
			data, _ := io.ReadAll(stdin)
			h.autosave = string(data)
			return fakeResult{}
		case strings.Contains(command, "test -f"):
			if h.persisted == "" {
				return fakeResult{Exit: 1}
			}
			return fakeResult{Stdout: h.persisted}
		case isCaddyAdminWrite(command):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		case isCaddyAdminCall(command):
			return fakeResult{Stdout: runningCaddyConfig}
		case strings.Contains(command, "cat > ") || strings.Contains(command, "QUADLET_EOF"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		default:
			return fakeResult{}
		}
	}
}

func mustCommandIndex(t *testing.T, records []commandRecord, substr string) int {
	t.Helper()
	for i, record := range records {
		if strings.Contains(record.Command, substr) {
			return i
		}
	}
	t.Fatalf("no command containing %q:\n%s", substr, commandDump(records))
	return -1
}

func assertCommandOrder(t *testing.T, records []commandRecord, steps ...string) []int {
	t.Helper()
	indexes := make([]int, len(steps))
	for i, step := range steps {
		indexes[i] = mustCommandIndex(t, records, step)
		if i > 0 && indexes[i] <= indexes[i-1] {
			t.Fatalf("%q ran before %q:\n%s", step, steps[i-1], commandDump(records))
		}
	}
	return indexes
}

// ---------------------------------------------------------------------------
// Control: Boot recreates a proxy created by an older release once, in the
// stop -> normalize autosave -> rename -> run order, before any admin call,
// and removes the old container only once the replacement answers. The
// autosave it resumes is rewritten to the private socket and loses planted
// admin keys.
// ---------------------------------------------------------------------------

func TestControl_ProxyBootRecreatesLegacyAdminContainer(t *testing.T) {
	legacy := &legacyProxyHost{autosave: legacyAutosave}
	fake := startFakeSSH(t, "127.0.0.1:0", legacy.handler())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "boot")
	if res.Exit != 0 {
		t.Fatalf("proxy boot failed: %s", res)
	}
	if !strings.Contains(res.Combined, "Recreating proxy container") ||
		!strings.Contains(res.Combined, "admin API listens on 0.0.0.0:2019") {
		t.Fatalf("migration was not reported: %s", res)
	}
	if !strings.Contains(res.Combined, "Removed unexpected Caddy admin settings (config, remote)") {
		t.Fatalf("stripped admin keys were not reported: %s", res)
	}

	records := fake.snapshot()
	steps := assertCommandOrder(t, records,
		"podman stop -t 30 azud-proxy",
		"if [ -s /config/caddy/autosave.json ]",
		"podman rename azud-proxy azud-proxy-legacy",
		"autosave.json.azud-tmp",
		"--name azud-proxy",
		"podman rm -f azud-proxy-legacy",
	)
	for _, record := range records[:steps[4]] {
		if isCaddyAdminCall(record.Command) {
			t.Fatalf("admin API was called before the proxy was recreated:\n%s", record.Command)
		}
	}
	answered := false
	for _, record := range records[steps[4]:steps[5]] {
		answered = answered || isCaddyAdminCall(record.Command)
	}
	if !answered {
		t.Fatalf("legacy proxy was removed before the replacement answered:\n%s", commandDump(records))
	}
	for _, record := range records {
		if strings.HasSuffix(record.Command, "podman rm -f azud-proxy") {
			t.Fatalf("legacy proxy was deleted instead of moved aside:\n%s", commandDump(records))
		}
	}
	install := records[steps[3]]
	assertAdminListenerIsPrivateSocket(t, install.Stdin)
	if !strings.Contains(string(install.Stdin), "azud-route-shop") {
		t.Fatalf("autosave routes were not preserved:\n%s", install.Stdin)
	}
	if !strings.Contains(records[steps[4]].Command, "CADDY_ADMIN=unix//run/azud-admin/admin.sock|0600") {
		t.Fatalf("replacement proxy does not use the private socket:\n%s", records[steps[4]].Command)
	}
	assertAdminCallsUsePrivateSocket(t, records, "podman")
}

// ---------------------------------------------------------------------------
// Control: when the hardened replacement does not start, Boot removes it,
// puts back the old container with its original autosave, restarts it, and
// fails with the reason instead of leaving the host without a proxy.
// ---------------------------------------------------------------------------

func TestControl_ProxyBootRestoresLegacyProxyWhenReplacementFails(t *testing.T) {
	legacy := &legacyProxyHost{autosave: legacyAutosave, replacementExits: true}
	fake := startFakeSSH(t, "127.0.0.1:0", legacy.handler())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "boot")
	if res.Exit == 0 {
		t.Fatalf("proxy boot succeeded with a replacement that exited: %s", res)
	}
	if !strings.Contains(res.Combined, "rolled back to its previous, outdated container") ||
		!strings.Contains(res.Combined, "proxy container azud-proxy is not running") {
		t.Fatalf("rollback and its reason were not reported: %s", res)
	}

	records := fake.snapshot()
	steps := assertCommandOrder(t, records,
		"podman rename azud-proxy azud-proxy-legacy",
		"--name azud-proxy",
		"podman logs --tail 20 azud-proxy",
		"podman rm -f azud-proxy",
		"podman rename azud-proxy-legacy azud-proxy",
		"podman start azud-proxy",
	)
	// The old container resumes its autosave, so the original (TCP admin)
	// copy is reinstalled before it is renamed back and started.
	restore := -1
	for i := steps[3]; i < steps[4]; i++ {
		if strings.Contains(records[i].Command, "autosave.json.azud-tmp") {
			restore = i
		}
	}
	if restore < 0 || string(records[restore].Stdin) != legacyAutosave {
		t.Fatalf("original autosave was not restored before the rename:\n%s", commandDump(records))
	}
	if legacy.replaced || legacy.moved || legacy.removed {
		t.Fatalf("host was not returned to the legacy proxy: replaced=%t moved=%t removed=%t", legacy.replaced, legacy.moved, legacy.removed)
	}
	for _, record := range records {
		if strings.Contains(record.Command, "rm -f azud-proxy-legacy") {
			t.Fatalf("legacy proxy was removed although the replacement failed:\n%s", record.Command)
		}
	}
}

// ---------------------------------------------------------------------------
// Control: a replacement that exits is reported with the admin probe's
// reason as soon as it is seen, not after the full readiness timeout.
// ---------------------------------------------------------------------------

func TestControl_ProxyBootReportsAdminProbeError(t *testing.T) {
	base := proxyBootHandler()
	fake := startFakeSSH(t, "127.0.0.1:0", func(command string, stdin io.Reader) fakeResult {
		if isCaddyAdminCall(command) {
			return fakeResult{Exit: 255, Stderr: "Error: can only create exec sessions on running containers: container state improper\n"}
		}
		return base(command, stdin)
	})
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "boot")
	if res.Exit == 0 {
		t.Fatalf("proxy boot succeeded without an admin API: %s", res)
	}
	if !strings.Contains(res.Combined, "proxy container azud-proxy is not running") {
		t.Fatalf("admin probe error was not reported: %s", res)
	}
	var probes int
	for _, record := range fake.snapshot() {
		if isCaddyAdminCall(record.Command) {
			probes++
		}
	}
	if probes != 1 {
		t.Fatalf("admin API probed %d times after the container stopped, want 1", probes)
	}
}

// ---------------------------------------------------------------------------
// Control: a Quadlet-managed legacy proxy is never recreated imperatively;
// Azud fails closed and points at `azud systemd enable`.
// ---------------------------------------------------------------------------

func TestControl_ProxyBootRefusesSystemdManagedLegacyProxy(t *testing.T) {
	legacy := &legacyProxyHost{labels: `,"PODMAN_SYSTEMD_UNIT":"azud-proxy.service"`, autosave: legacyAutosave}
	fake := startFakeSSH(t, "127.0.0.1:0", legacy.handler())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "boot")
	if res.Exit == 0 {
		t.Fatalf("proxy boot succeeded against a systemd-managed legacy proxy: %s", res)
	}
	if !strings.Contains(res.Combined, "azud systemd enable") || !strings.Contains(res.Combined, "azud-proxy.service") {
		t.Fatalf("missing systemd migration hint: %s", res)
	}
	for _, record := range fake.snapshot() {
		for _, forbidden := range []string{" stop ", " rm ", " run ", "curl"} {
			if strings.Contains(record.Command, "podman") && strings.Contains(record.Command, forbidden) {
				t.Fatalf("Azud touched the systemd-managed proxy (%q):\n%s", forbidden, record.Command)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Control: commands that do not boot the proxy refuse to treat a legacy
// proxy as healthy and name the migration command.
// ---------------------------------------------------------------------------

func TestControl_ProxyStatusReportsLegacyProxy(t *testing.T) {
	legacy := &legacyProxyHost{autosave: legacyAutosave}
	fake := startFakeSSH(t, "127.0.0.1:0", legacy.handler())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "status")
	if res.Exit == 0 {
		t.Fatalf("proxy status succeeded against a legacy proxy: %s", res)
	}
	if !strings.Contains(res.Combined, "azud proxy reboot") {
		t.Fatalf("missing reboot hint: %s", res)
	}
	for _, record := range fake.snapshot() {
		if strings.Contains(record.Command, " rm ") || strings.Contains(record.Command, " stop ") {
			t.Fatalf("proxy status modified the proxy:\n%s", record.Command)
		}
	}
}

// ---------------------------------------------------------------------------
// Control: preflight warns about an outdated proxy that the next deploy
// recreates, but fails for a Quadlet-managed one that every deploy refuses
// until `azud systemd enable` is rerun.
// ---------------------------------------------------------------------------

func TestControl_PreflightBlocksOnlySystemdManagedLegacyProxy(t *testing.T) {
	tests := []struct {
		name     string
		labels   string
		wantExit bool
		want     string
	}{
		{name: "imperative", want: "127.0.0.1/proxy=outdated"},
		{name: "systemd managed", labels: `,"PODMAN_SYSTEMD_UNIT":"azud-proxy.service"`, wantExit: true, want: "azud systemd enable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacy := &legacyProxyHost{labels: tt.labels, autosave: legacyAutosave}
			fake := startFakeSSH(t, "127.0.0.1:0", legacy.handler())
			dir := newWorkdir(t)
			// localhost resolves offline, so DNS does not block preflight.
			cfg := strings.Replace(baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"), "host: shop.example.com", "host: localhost", 1)
			configPath := writeConfig(t, dir, cfg)

			res := runAzud(t, dir, nil, "-c", configPath, "preflight")
			if (res.Exit != 0) != tt.wantExit || !strings.Contains(res.Combined, tt.want) {
				t.Fatalf("preflight exit=%d, want failure=%t and %q: %s", res.Exit, tt.wantExit, tt.want, res)
			}
			if tt.wantExit && !strings.Contains(res.Combined, "127.0.0.1/proxy=outdated-unit") {
				t.Fatalf("outdated unit was not reported as a blocker: %s", res)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Control: a persisted caddy-config.json written by an older release (TCP
// admin listener, planted admin keys) cannot move the admin endpoint when it
// is restored.
// ---------------------------------------------------------------------------

func TestControl_RestoredPersistedConfigCannotMoveAdmin(t *testing.T) {
	legacy := &legacyProxyHost{removed: true, persisted: legacyAutosave}
	fake := startFakeSSH(t, "127.0.0.1:0", legacy.handler())
	dir := newWorkdir(t)
	configPath := writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop"))

	res := runAzud(t, dir, nil, "-c", configPath, "proxy", "boot")
	if res.Exit != 0 {
		t.Fatalf("proxy boot failed: %s", res)
	}
	var restored commandRecord
	for _, record := range fake.snapshot() {
		if strings.HasSuffix(record.Command, " http://localhost/load") && strings.Contains(string(record.Stdin), "azud-route-shop") {
			restored = record
			break
		}
	}
	if restored.Command == "" {
		t.Fatalf("persisted config was not restored:\n%s", commandDump(fake.snapshot()))
	}
	assertAdminListenerIsPrivateSocket(t, restored.Stdin)
	if strings.Contains(string(restored.Stdin), "attacker.invalid") || strings.Contains(string(restored.Stdin), "2021") {
		t.Fatalf("planted admin settings survived the restore:\n%s", restored.Stdin)
	}
	assertAdminCallsUsePrivateSocket(t, fake.snapshot(), "podman")
}

// ---------------------------------------------------------------------------
// Control: `azud systemd enable` migrates the proxy: it installs a unit with
// the private socket and no state-directory mount, stops the previous proxy,
// normalizes (or seeds) the autosave, and only then starts the unit.
// ---------------------------------------------------------------------------

func TestControl_SystemdEnableMigratesProxyToPrivateSocket(t *testing.T) {
	tests := []struct {
		name      string
		autosave  string
		persisted string
	}{
		{name: "normalizes legacy autosave", autosave: legacyAutosave},
		{name: "seeds autosave from persisted config", persisted: legacyAutosave},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacy := &legacyProxyHost{autosave: tt.autosave, persisted: tt.persisted}
			fake := startFakeSSH(t, "127.0.0.1:0", legacy.handler())
			dir := newWorkdir(t)
			topology := proxyTopologies()[1] // rootless bridge
			configPath := writeConfig(t, dir, topology.config(fake.port(), clientKeyPath(t)))

			res := runAzud(t, dir, nil, "-c", configPath, "systemd", "enable", "--skip-app")
			if res.Exit != 0 {
				t.Fatalf("systemd enable failed: %s", res)
			}

			records := fake.snapshot()
			steps := assertCommandOrder(t, records,
				"azud-proxy.container",
				"systemctl --user stop azud-proxy",
				"podman rm -f azud-proxy",
				"autosave.json.azud-tmp",
				"systemctl --user start azud-proxy",
			)
			unit := records[steps[0]].Command
			for _, want := range []string{
				`Environment="CADDY_ADMIN=unix//run/azud-admin/admin.sock|0600"`,
				"PodmanArgs=--read-only\nPodmanArgs=--cap-drop=all\nPodmanArgs=--cap-add=NET_BIND_SERVICE\nPodmanArgs=--security-opt=no-new-privileges\nPodmanArgs=--tmpfs=/run/azud-admin:rw,nosuid,nodev,noexec,mode=0700,size=64k",
				`Label="azud.proxy.spec=2"`,
				"Exec=caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume",
			} {
				if !strings.Contains(unit, want) {
					t.Fatalf("proxy unit missing %q:\n%s", want, unit)
				}
			}
			for _, forbidden := range []string{"2019", "0.0.0.0", "/azud-state"} {
				if strings.Contains(unit, forbidden) {
					t.Fatalf("proxy unit contains %q:\n%s", forbidden, unit)
				}
			}
			install := records[steps[3]]
			assertAdminListenerIsPrivateSocket(t, install.Stdin)
			if !strings.Contains(string(install.Stdin), "azud-route-shop") || strings.Contains(string(install.Stdin), "attacker.invalid") {
				t.Fatalf("installed autosave lost routes or kept planted admin keys:\n%s", install.Stdin)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Control: when `azud systemd enable` cannot prepare the autosave after it
// stopped the proxy, it still starts the unit and reports the failure, so
// the host is not left without a proxy.
// ---------------------------------------------------------------------------

func TestControl_SystemdEnableStartsProxyUnitWhenPreparationFails(t *testing.T) {
	legacy := &legacyProxyHost{autosave: `[]`}
	fake := startFakeSSH(t, "127.0.0.1:0", legacy.handler())
	dir := newWorkdir(t)
	topology := proxyTopologies()[1] // rootless bridge
	configPath := writeConfig(t, dir, topology.config(fake.port(), clientKeyPath(t)))

	res := runAzud(t, dir, nil, "-c", configPath, "systemd", "enable", "--skip-app")
	if res.Exit == 0 {
		t.Fatalf("systemd enable succeeded with an invalid autosave: %s", res)
	}
	if !strings.Contains(res.Combined, "started the unit with its existing autosave") ||
		!strings.Contains(res.Combined, "invalid Caddy autosave JSON") {
		t.Fatalf("preparation failure and restart were not reported: %s", res)
	}
	assertCommandOrder(t, fake.snapshot(),
		"systemctl --user stop azud-proxy",
		"podman rm -f azud-proxy",
		"if [ -s /config/caddy/autosave.json ]",
		"systemctl --user start azud-proxy",
	)
}
