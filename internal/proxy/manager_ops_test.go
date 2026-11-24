package proxy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// runStateCommand runs a generated state-directory command with /bin/sh the
// way a remote login shell would, using home as ${HOME}.
func runStateCommand(t *testing.T, home, command, stdin string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("command output: %s", out)
	}
	return err
}

func TestPersistConfigCommandDoesNotFollowPlantedSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	home := t.TempDir()
	outside := t.TempDir()
	stateDir := filepath.Join(home, ".local", "share", "azud")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both the old fixed temp name and the final path point at the victim.
	for _, name := range []string{CaddyConfigFileName + ".tmp", CaddyConfigFileName} {
		if err := os.Symlink(victim, filepath.Join(stateDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	if err := runStateCommand(t, home, persistConfigCommand("deploy"), `{"apps":{}}`); err != nil {
		t.Fatalf("persist failed: %v", err)
	}

	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" || info.Mode().Perm() != 0o644 {
		t.Fatalf("persist wrote through a symlink: victim = %q mode %o", data, info.Mode().Perm())
	}
	configFile := filepath.Join(stateDir, CaddyConfigFileName)
	linfo, err := os.Lstat(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !linfo.Mode().IsRegular() || linfo.Mode().Perm() != 0o600 {
		t.Fatalf("persisted config mode = %v, want regular 0600", linfo.Mode())
	}
	if data, _ := os.ReadFile(configFile); string(data) != `{"apps":{}}` {
		t.Fatalf("persisted config = %q", data)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+CaddyConfigFileName+".") {
			t.Fatalf("persist left temp file %s behind", entry.Name())
		}
	}
}

func TestRestoreConfigCommandRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	home := t.TempDir()
	outside := t.TempDir()
	stateDir := filepath.Join(home, ".local", "share", "azud")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte(`{"secret":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(stateDir, CaddyConfigFileName)); err != nil {
		t.Fatal(err)
	}

	if err := runStateCommand(t, home, restoreConfigCommand("deploy"), ""); err == nil {
		t.Fatal("restore accepted a symlinked config file")
	}
	info, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("restore changed the symlink target's mode to %o", info.Mode().Perm())
	}
}

// The migration rollback state holds a raw Caddy autosave, which may carry
// private keys, so it gets the persisted config's protections.
func TestProxyRollbackStateIsAProtectedStateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	home := t.TempDir()
	outside := t.TempDir()
	stateDir := filepath.Join(home, ".local", "share", "azud")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(stateDir, proxyRollbackFileName)
	if err := os.Symlink(victim, stateFile); err != nil {
		t.Fatal(err)
	}
	if err := runStateCommand(t, home, readStateFileCommand("deploy", proxyRollbackFileName), ""); err == nil {
		t.Fatal("read accepted a symlinked rollback state file")
	}

	saved := `{"container":"abc","running":true,"snapshotted":true,"autosave":"{\"admin\":{\"listen\":\"0.0.0.0:2019\"}}"}`
	if err := runStateCommand(t, home, writeStateFileCommand("deploy", proxyRollbackFileName), saved); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "original" {
		t.Fatalf("write followed a symlink: victim = %q", data)
	}
	info, err := os.Lstat(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("rollback state mode = %v in a %v directory, want a regular 0600 file in 0700", info.Mode(), dirInfo.Mode().Perm())
	}
	cmd := exec.Command("sh", "-c", readStateFileCommand("deploy", proxyRollbackFileName))
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil || string(out) != saved {
		t.Fatalf("read = %q, %v; want %q", out, err, saved)
	}
}

func TestCopyManagedProxySettingsRemovesLogCredentials(t *testing.T) {
	newConfig := func(server map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"apps": map[string]interface{}{
				"http": map[string]interface{}{
					"servers": map[string]interface{}{"srv0": server},
				},
			},
		}
	}
	for _, loggingEnabled := range []bool{true, false} {
		current := newConfig(map[string]interface{}{
			"listen": []interface{}{":80"},
			"logs": map[string]interface{}{
				"default_logger_name":    "access",
				"should_log_credentials": true,
			},
		})
		manager := &Manager{}
		desiredConfig := manager.buildBaseConfig()
		manager.applyProxySettingsFrom(desiredConfig, &ProxyConfig{LoggingEnabled: loggingEnabled}, nil)
		data, err := json.Marshal(desiredConfig)
		if err != nil {
			t.Fatal(err)
		}
		var desired map[string]interface{}
		if err := json.Unmarshal(data, &desired); err != nil {
			t.Fatal(err)
		}

		if err := copyManagedProxySettings(current, desired); err != nil {
			t.Fatal(err)
		}
		server := current["apps"].(map[string]interface{})["http"].(map[string]interface{})["servers"].(map[string]interface{})["srv0"].(map[string]interface{})
		logs, hasLogs := server["logs"].(map[string]interface{})
		if hasLogs != loggingEnabled {
			t.Fatalf("logging enabled=%v but reconciled logs=%#v", loggingEnabled, server["logs"])
		}
		if _, ok := logs["should_log_credentials"]; ok {
			t.Fatalf("should_log_credentials survived reconciliation (logging enabled=%v): %#v", loggingEnabled, server)
		}
		if strings.Contains(string(data), "log_credentials") {
			t.Fatalf("desired Caddy config enables credential logging: %s", data)
		}
	}
}

func TestSystemdUnitFromInspect(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "direct podman container",
			raw:  `[{"Config":{"Labels":{"azud.managed":"true","azud.type":"proxy"}}}]`,
		},
		{
			name: "quadlet container",
			raw:  `[{"Config":{"Labels":{"azud.type":"proxy","PODMAN_SYSTEMD_UNIT":"azud-proxy.service"}}}]`,
			want: "azud-proxy.service",
		},
		{
			name: "no labels",
			raw:  `[{"Config":{}}]`,
		},
		{
			name:    "empty result",
			raw:     `[]`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			raw:     ``,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := systemdUnitFromInspect(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got unit %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unit = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSystemctlStopCommandMatchesProxyRuntime(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		rootful bool
		want    string
	}{
		{name: "root", user: "root", want: "systemctl stop azud-proxy.service"},
		{name: "root rootful", user: "root", rootful: true, want: "systemctl stop azud-proxy.service"},
		{name: "rootful via sudo", user: "deploy", rootful: true, want: "sudo systemctl stop azud-proxy.service"},
		{name: "rootless", user: "deploy", want: "systemctl --user stop azud-proxy.service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{user: tt.user, rootful: tt.rootful}
			if got := manager.systemctlStopCommand("azud-proxy.service"); got != tt.want {
				t.Fatalf("systemctl command = %q, want %q", got, tt.want)
			}
		})
	}
}

// onDemandTestConfig is a live proxy with one unrelated service route.
const onDemandTestConfig = `{"admin":{"listen":"unix//run/azud-admin/admin.sock|0600"},"apps":{"http":{"servers":{"srv0":{"listen":[":443"],"routes":[
	{"@id":"azud-route-blog","match":[{"host":["blog.example.org"]}],"handle":[{"@id":"azud-proxy-blog","handler":"reverse_proxy","upstreams":[{"dial":"blog:80"}]}],"terminal":true}
]}}}}}`

func onDemandTestManager(t *testing.T, config string, hostPorts bool) (*Manager, *fakeCaddyAdmin) {
	t.Helper()
	h := newFakeProxyHost("", map[string]*fakeProxyContainer{CaddyContainerName: {running: true}})
	h.admin = newFakeCaddyAdmin(t, config)
	manager, _ := startFakeProxySSH(t, h)
	manager.hostPorts = hostPorts
	return manager, h.admin
}

func onDemandService(upstream string) *ServiceConfig {
	return &ServiceConfig{
		Name:       "app",
		Host:       "app.example.com",
		Hosts:      []string{"app.example.com"},
		Upstreams:  []string{upstream},
		HealthPath: "/up",
		HTTPS:      true,
		OnDemand:   true,
	}
}

func TestOnDemandRouteIsAddedLastAndStaysLast(t *testing.T) {
	manager, admin := onDemandTestManager(t, onDemandTestConfig, false)

	if err := manager.RegisterService("127.0.0.1", onDemandService("app:3000")); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	if got, want := admin.routeIDs(t), []string{"azud-route-blog", "azud-route-app", "azud-on-demand-app"}; !slices.Equal(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	mirror := admin.route(t, "azud-on-demand-app")
	if _, hasMatch := mirror["match"]; hasMatch || !slices.Equal(upstreamDials(mirror), []string{"app:3000"}) {
		t.Fatalf("on-demand route is not a host-less copy of the service route: %v", mirror)
	}

	// Another service's first deploy is inserted before the catch-all.
	other := &ServiceConfig{Name: "docs", Host: "docs.example.org", Upstreams: []string{"docs:8080"}}
	if err := manager.RegisterService("127.0.0.1", other); err != nil {
		t.Fatalf("RegisterService(docs): %v", err)
	}
	if got, want := admin.routeIDs(t), []string{"azud-route-blog", "azud-route-app", "azud-route-docs", "azud-on-demand-app"}; !slices.Equal(got, want) {
		t.Fatalf("routes after another service = %v, want %v", got, want)
	}

	// A route appended by hand after it is moved ahead on the owner's next deploy.
	if _, status := admin.apply("POST", "/config/apps/http/servers/srv0/routes", map[string]interface{}{"@id": "manual"}); status != 200 {
		t.Fatalf("append manual route: %d", status)
	}
	if err := manager.EnsureServiceHosts("127.0.0.1", onDemandService("")); err != nil {
		t.Fatalf("EnsureServiceHosts: %v", err)
	}
	if got := admin.routeIDs(t); got[len(got)-1] != "azud-on-demand-app" {
		t.Fatalf("on-demand route is not last after the owner's deploy: %v", got)
	}
}

func TestOnDemandRouteFollowsUpstreamSwapsAndCanaryWeights(t *testing.T) {
	manager, admin := onDemandTestManager(t, onDemandTestConfig, false)
	if err := manager.RegisterService("127.0.0.1", onDemandService("app:3000")); err != nil {
		t.Fatal(err)
	}

	if err := manager.AddUpstream("127.0.0.1", "app.example.com", "app-new-1:3000"); err != nil {
		t.Fatal(err)
	}
	if got := upstreamDials(admin.route(t, "azud-on-demand-app")); !slices.Equal(got, []string{"app:3000", "app-new-1:3000"}) {
		t.Fatalf("on-demand upstreams after add = %v", got)
	}
	if err := manager.RemoveUpstream("127.0.0.1", "app.example.com", "app:3000"); err != nil {
		t.Fatal(err)
	}
	if got := upstreamDials(admin.route(t, "azud-on-demand-app")); !slices.Equal(got, []string{"app-new-1:3000"}) {
		t.Fatalf("on-demand upstreams after remove = %v", got)
	}

	if err := manager.SetCanaryWeights("127.0.0.1", "app.example.com", "app-new-1:3000", 75, "app-canary:3000", 25); err != nil {
		t.Fatal(err)
	}
	mirror := admin.route(t, "azud-on-demand-app")
	if got := upstreamDials(mirror); !slices.Equal(got, []string{"app-new-1:3000", "app-new-1:3000", "app-new-1:3000", "app-canary:3000"}) {
		t.Fatalf("on-demand upstreams after canary weights = %v", got)
	}
	if policy := jsonPath(mirror, "handle"); policy == nil || !strings.Contains(admin.dump(), `"policy": "random"`) {
		t.Fatalf("canary policy did not reach the on-demand route: %s", admin.dump())
	}
	primary, _ := json.Marshal(withoutCaddyIDs(admin.route(t, "azud-route-app")["handle"]))
	copied, _ := json.Marshal(mirror["handle"])
	if string(primary) != string(copied) {
		t.Fatalf("on-demand handlers differ from the service route:\n%s\n%s", primary, copied)
	}

	if err := manager.DeregisterService("127.0.0.1", "app.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := admin.routeIDs(t); !slices.Equal(got, []string{"azud-route-blog"}) {
		t.Fatalf("routes after deregistration = %v", got)
	}
}

func TestOnDemandPermissionFollowsHostPortSwaps(t *testing.T) {
	manager, admin := onDemandTestManager(t, onDemandTestConfig, true)
	settings := &ProxyConfig{
		Hosts: []string{"app.example.com"}, AutoHTTPS: true, SSLRedirect: true, Email: "ops@example.com",
		Service: "app", AppPort: 3000, OnDemandAsk: "/tls/ask",
	}
	manager.SetProxyConfig(settings)
	endpoint := func() interface{} {
		return admin.value("apps", "tls", "automation", "on_demand", "permission", "endpoint")
	}

	// First deploy: no route yet, so the permission check refuses until one exists.
	if err := manager.EnsureConfig("127.0.0.1"); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	if got := endpoint(); got != "http://app:3000/tls/ask" {
		t.Fatalf("endpoint before the first route = %v", got)
	}
	if err := manager.RegisterService("127.0.0.1", onDemandService("127.0.0.1:41000")); err != nil {
		t.Fatal(err)
	}
	if got := endpoint(); got != "http://127.0.0.1:41000/tls/ask" {
		t.Fatalf("endpoint after the first route = %v", got)
	}

	// A rolling deploy adds the new container's port, then removes the old one.
	if err := manager.EnsureConfig("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddUpstream("127.0.0.1", "app.example.com", "127.0.0.1:42000"); err != nil {
		t.Fatal(err)
	}
	if got := endpoint(); got != "http://127.0.0.1:41000/tls/ask" {
		t.Fatalf("endpoint while both containers serve = %v", got)
	}
	if err := manager.RemoveUpstream("127.0.0.1", "app.example.com", "127.0.0.1:41000"); err != nil {
		t.Fatal(err)
	}
	if got := endpoint(); got != "http://127.0.0.1:42000/tls/ask" {
		t.Fatalf("endpoint after the swap = %v", got)
	}
}

func TestSecondOnDemandOwnerIsRefusedWithoutChanges(t *testing.T) {
	config := `{"admin":{"listen":"unix//run/azud-admin/admin.sock|0600"},"apps":{"http":{"servers":{"srv0":{"listen":[":443"],"routes":[
		{"@id":"azud-route-shop","match":[{"host":["shop.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"shop:4000"}]}],"terminal":true},
		{"@id":"azud-on-demand-shop","handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"shop:4000"}]}],"terminal":true}
	]}}},"tls":{"automation":{"policies":[{"subjects":["shop.example.com"]},{"on_demand":true}],"on_demand":{"permission":{"module":"http","endpoint":"http://shop:4000/check"}}}}}}`
	manager, admin := onDemandTestManager(t, config, false)
	before := admin.dump()

	manager.SetProxyConfig(&ProxyConfig{
		Hosts: []string{"app.example.com"}, AutoHTTPS: true, SSLRedirect: true, Email: "ops@example.com",
		Service: "app", AppPort: 3000, OnDemandAsk: "/tls/ask",
	})
	if err := manager.EnsureConfig("127.0.0.1"); err == nil || !strings.Contains(err.Error(), "belongs to service shop") {
		t.Fatalf("EnsureConfig accepted a second on-demand owner: %v", err)
	}
	if err := manager.RegisterService("127.0.0.1", onDemandService("app:3000")); err == nil || !strings.Contains(err.Error(), "belongs to service shop") {
		t.Fatalf("RegisterService accepted a second on-demand owner: %v", err)
	}
	if after := admin.dump(); after != before {
		t.Fatalf("a refused owner changed the proxy:\n%s", after)
	}

	// The same app without on-demand TLS deploys and keeps the owner's check.
	manager.SetProxyConfig(&ProxyConfig{
		Hosts: []string{"app.example.com"}, AutoHTTPS: true, SSLRedirect: true, Email: "ops@example.com",
		Service: "app", AppPort: 3000,
	})
	if err := manager.EnsureConfig("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if got := admin.value("apps", "tls", "automation", "on_demand", "permission", "endpoint"); got != "http://shop:4000/check" {
		t.Fatalf("another app's deploy dropped the owner's permission check: %v", got)
	}
	plain := onDemandService("app:3000")
	plain.OnDemand = false
	if err := manager.RegisterService("127.0.0.1", plain); err != nil {
		t.Fatal(err)
	}
	if got, want := admin.routeIDs(t), []string{"azud-route-shop", "azud-route-app", "azud-on-demand-shop"}; !slices.Equal(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
}

func TestTurningOnDemandOffRemovesTheRouteAndPolicy(t *testing.T) {
	manager, admin := onDemandTestManager(t, onDemandTestConfig, false)
	settings := &ProxyConfig{
		Hosts: []string{"app.example.com"}, AutoHTTPS: true, SSLRedirect: true, Email: "ops@example.com",
		Service: "app", AppPort: 3000, OnDemandAsk: "/tls/ask",
	}
	manager.SetProxyConfig(settings)
	if err := manager.EnsureConfig("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterService("127.0.0.1", onDemandService("app:3000")); err != nil {
		t.Fatal(err)
	}

	off := *settings
	off.OnDemandAsk = ""
	manager.SetProxyConfig(&off)
	if err := manager.EnsureConfig("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	service := onDemandService("")
	service.OnDemand = false
	if err := manager.EnsureServiceHosts("127.0.0.1", service); err != nil {
		t.Fatal(err)
	}
	if got := admin.routeIDs(t); !slices.Equal(got, []string{"azud-route-blog", "azud-route-app"}) {
		t.Fatalf("routes after turning on-demand TLS off = %v", got)
	}
	tlsApp, _ := json.Marshal(admin.value("apps", "tls"))
	if want := `{"automation":{"policies":[{"issuers":[{"email":"ops@example.com","module":"acme"}]}]}}`; string(tlsApp) != want {
		t.Fatalf("TLS app after turning on-demand TLS off = %s, want %s", tlsApp, want)
	}
}

func TestReconcileServiceReportsAndRepairsTheOnDemandRoute(t *testing.T) {
	manager, admin := onDemandTestManager(t, onDemandTestConfig, false)
	plain := onDemandService("app:3000")
	plain.OnDemand = false
	if err := manager.RegisterService("127.0.0.1", plain); err != nil {
		t.Fatal(err)
	}

	status, err := manager.ReconcileService("127.0.0.1", onDemandService("app:3000"), false)
	if err != nil || status != ReconcileStale {
		t.Fatalf("status with a missing on-demand route = %s, %v", status, err)
	}
	if _, err := manager.ReconcileService("127.0.0.1", onDemandService("app:3000"), true); err != nil {
		t.Fatal(err)
	}
	if got := admin.routeIDs(t); got[len(got)-1] != "azud-on-demand-app" {
		t.Fatalf("repair did not add the on-demand route: %v", got)
	}
	status, err = manager.ReconcileService("127.0.0.1", onDemandService("app:3000"), false)
	if err != nil || status != ReconcileInSync {
		t.Fatalf("status after repair = %s, %v", status, err)
	}
}
