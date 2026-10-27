package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/quadlet"
)

func TestBuildAppQuadletUnit_MixedModePublishesLoopbackPort(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	cfg = &config.Config{
		Service: "test-app",
		Podman:  config.PodmanConfig{Rootless: true},
		Proxy: config.ProxyConfig{
			Rootful: true,
			AppPort: 3000,
		},
	}

	unit := buildAppQuadletUnit("ghcr.io/acme/test:latest", "web")
	want := []string{"127.0.0.1::3000"}
	if !reflect.DeepEqual(unit.PublishPort, want) {
		t.Fatalf("unexpected publish ports: want %v got %v", want, unit.PublishPort)
	}
	if len(unit.After) != 0 || len(unit.Requires) != 0 {
		t.Fatalf("rootless unit references system-manager targets: after=%v requires=%v", unit.After, unit.Requires)
	}
}

func TestPinQuadletHostPortPreservesMixedModeRoute(t *testing.T) {
	unit := &quadlet.ContainerUnit{PublishPort: []string{"127.0.0.1::3000"}}
	pinQuadletHostPort(unit, 49152, 3000)
	if !reflect.DeepEqual(unit.PublishPort, []string{"127.0.0.1:49152:3000"}) {
		t.Fatalf("pinned publish port = %v", unit.PublishPort)
	}
}

func TestBuildAppQuadletUnitWorkerMatchesRole(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = &config.Config{
		Service: "test-app",
		SSH:     config.SSHConfig{User: "deployer"},
		Podman:  config.PodmanConfig{Rootless: true},
		Servers: map[string]config.RoleConfig{
			"worker": {
				Cmd:     "bundle exec jobs",
				Env:     map[string]string{"QUEUE": "critical"},
				Labels:  map[string]string{"team": "jobs"},
				Options: map[string]string{"memory": "512M", "cpus": "0.5"},
				Runtime: config.RoleRuntimeConfig{
					User:            "10001:10001",
					ReadOnly:        true,
					CapDrop:         []string{"ALL"},
					NoNewPrivileges: true,
					Tmpfs: []config.RoleTmpfsConfig{{
						Path: "/tmp",
						Size: "64m",
						Mode: "1777",
					}},
					DisableHealthcheck: true,
					StopTimeout:        45 * time.Second,
				},
			},
		},
		Env: config.EnvConfig{Secret: []string{"TOKEN"}},
		Proxy: config.ProxyConfig{
			AppPort:     3000,
			Healthcheck: config.HealthcheckConfig{LivenessPath: "/live"},
		},
	}

	unit := buildAppQuadletUnit("ghcr.io/acme/test:latest", "worker")
	if unit.ContainerName != "test-app-worker" || unit.Exec != "bundle exec jobs" {
		t.Fatalf("worker identity not preserved: name=%q exec=%q", unit.ContainerName, unit.Exec)
	}
	if len(unit.PublishPort) != 0 || unit.HealthCmd != "" {
		t.Fatalf("worker inherited web behavior: ports=%v health=%q", unit.PublishPort, unit.HealthCmd)
	}
	if unit.Environment["QUEUE"] != "critical" || unit.Label["team"] != "jobs" {
		t.Fatalf("worker environment/labels missing: env=%v labels=%v", unit.Environment, unit.Label)
	}
	if !reflect.DeepEqual(unit.PodmanArgs, []string{
		"--memory=512M",
		"--cpus=0.5",
		"--user=10001:10001",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--tmpfs=/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777",
		"--no-healthcheck",
		"--stop-timeout=45",
	}) {
		t.Fatalf("worker resources = %v", unit.PodmanArgs)
	}
	if unit.TimeoutStopSec != 50 {
		t.Fatalf("worker stop timeout = %d", unit.TimeoutStopSec)
	}
	if !reflect.DeepEqual(unit.EnvironmentFile, []string{"%h/.azud/secrets"}) {
		t.Fatalf("worker secrets path = %v", unit.EnvironmentFile)
	}
}

func TestBuildProxyQuadletUnit_MixedModeUsesHostNetwork(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	cfg = &config.Config{
		Podman: config.PodmanConfig{Rootless: true},
		Proxy: config.ProxyConfig{
			Rootful: true,
		},
	}

	unit := buildProxyQuadletUnit()
	if !reflect.DeepEqual(unit.Network, []string{"host"}) {
		t.Fatalf("expected host network, got %v", unit.Network)
	}
	if len(unit.PublishPort) != 0 {
		t.Fatalf("expected no published ports in mixed mode, got %v", unit.PublishPort)
	}
	if !reflect.DeepEqual(unit.After, []string{"network-online.target"}) || !reflect.DeepEqual(unit.Requires, []string{"network-online.target"}) {
		t.Fatalf("rootful proxy dependencies = after %v requires %v", unit.After, unit.Requires)
	}
}

func TestBuildProxyQuadletUnit_DefaultNetworkPublishesConfiguredPorts(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	cfg = &config.Config{
		Podman: config.PodmanConfig{Rootless: true},
		Proxy: config.ProxyConfig{
			Rootful:   false,
			HTTPPort:  8080,
			HTTPSPort: 8443,
		},
	}

	unit := buildProxyQuadletUnit()
	if !reflect.DeepEqual(unit.Network, []string{"azud.network"}) {
		t.Fatalf("expected azud network, got %v", unit.Network)
	}

	want := []string{"8080:80", "8443:443"}
	if !reflect.DeepEqual(unit.PublishPort, want) {
		t.Fatalf("unexpected published ports: want %v got %v", want, unit.PublishPort)
	}
	if len(unit.After) != 0 || len(unit.Requires) != 0 {
		t.Fatalf("rootless proxy references system-manager targets: after=%v requires=%v", unit.After, unit.Requires)
	}
}

func TestBuildProxyQuadletUnit_ServesAdminOnlyOnPrivateSocket(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	for _, tt := range []struct {
		name    string
		proxy   config.ProxyConfig
		podman  config.PodmanConfig
		sshUser string
	}{
		{name: "rootless bridge", podman: config.PodmanConfig{Rootless: true}, sshUser: "deploy"},
		{name: "rootful bridge", sshUser: "root"},
		{name: "mixed host network", podman: config.PodmanConfig{Rootless: true}, proxy: config.ProxyConfig{Rootful: true}, sshUser: "deploy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg = &config.Config{Podman: tt.podman, Proxy: tt.proxy, SSH: config.SSHConfig{User: tt.sshUser}}
			unit := buildProxyQuadletUnit()
			file := quadlet.GenerateContainerFile(unit)

			if unit.Environment["CADDY_ADMIN"] != proxy.CaddyAdminListen {
				t.Fatalf("CADDY_ADMIN = %q", unit.Environment["CADDY_ADMIN"])
			}
			if !strings.Contains(file, `Environment="CADDY_ADMIN=unix//run/azud-admin/admin.sock|0600"`) {
				t.Fatalf("unit does not set the socket listener:\n%s", file)
			}
			wantArgs := []string{
				"--read-only",
				"--cap-drop=all",
				"--cap-add=NET_BIND_SERVICE",
				"--security-opt=no-new-privileges",
				"--tmpfs=" + proxy.CaddyAdminTmpfs,
			}
			if !reflect.DeepEqual(unit.PodmanArgs, wantArgs) {
				t.Fatalf("proxy PodmanArgs = %v, want %v", unit.PodmanArgs, wantArgs)
			}
			if strings.Contains(file, "--user") {
				t.Fatalf("proxy unit changes the image user:\n%s", file)
			}
			if unit.Label[proxy.ProxySpecLabel] != proxy.ProxySpecVersion {
				t.Fatalf("spec label = %q", unit.Label[proxy.ProxySpecLabel])
			}
			if unit.Exec != "caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume" {
				t.Fatalf("unexpected Exec: %q", unit.Exec)
			}
			if !reflect.DeepEqual(unit.Volume, []string{"caddy_data:/data", "caddy_config:/config"}) {
				t.Fatalf("proxy unit mounts more than Caddy's volumes: %v", unit.Volume)
			}
			for _, forbidden := range []string{"2019", "0.0.0.0", "/azud-state", ".local/share/azud", "/var/lib/azud"} {
				if strings.Contains(file, forbidden) {
					t.Fatalf("proxy unit contains %q:\n%s", forbidden, file)
				}
			}
		})
	}
}

func TestNeedsAzudNetworkUnit(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	t.Run("app enabled always needs network", func(t *testing.T) {
		cfg = &config.Config{
			Podman: config.PodmanConfig{Rootless: true},
			Proxy:  config.ProxyConfig{Rootful: true},
		}
		if !needsAzudNetworkUnit(false, true) {
			t.Fatal("expected network unit to be required when app units are enabled")
		}
	})

	t.Run("proxy only mixed mode does not need network", func(t *testing.T) {
		cfg = &config.Config{
			Podman: config.PodmanConfig{Rootless: true},
			Proxy:  config.ProxyConfig{Rootful: true},
		}
		if needsAzudNetworkUnit(true, false) {
			t.Fatal("expected no network unit requirement for proxy-only mixed mode")
		}
	})

	t.Run("proxy only bridge mode needs network", func(t *testing.T) {
		cfg = &config.Config{
			Podman: config.PodmanConfig{Rootless: true},
			Proxy:  config.ProxyConfig{Rootful: false},
		}
		if !needsAzudNetworkUnit(true, false) {
			t.Fatal("expected network unit requirement for proxy-only bridge mode")
		}
	})
}

func TestBuiltQuadletUnitsPassValidation(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = &config.Config{
		Service: "test-app",
		Podman:  config.PodmanConfig{Rootless: true},
		Proxy:   config.ProxyConfig{AppPort: 3000},
	}

	if err := buildAppQuadletUnit("ghcr.io/acme/test:latest", "web").Validate(); err != nil {
		t.Fatalf("app quadlet unit failed validation: %v", err)
	}
	if err := buildProxyQuadletUnit().Validate(); err != nil {
		t.Fatalf("proxy quadlet unit failed validation: %v", err)
	}
	if err := quadlet.ValidateNetworkName("azud"); err != nil {
		t.Fatalf("azud network name failed validation: %v", err)
	}
}
