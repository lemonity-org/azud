package cli

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

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
	if !reflect.DeepEqual(unit.PodmanArgs, []string{"--memory=512M", "--cpus=0.5"}) {
		t.Fatalf("worker resources = %v", unit.PodmanArgs)
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

	want := []string{
		"8080:80",
		"8443:443",
		fmt.Sprintf("127.0.0.1:%d:%d", proxy.CaddyAdminPort, proxy.CaddyAdminPort),
	}
	if !reflect.DeepEqual(unit.PublishPort, want) {
		t.Fatalf("unexpected published ports: want %v got %v", want, unit.PublishPort)
	}
	if !strings.Contains(unit.Exec, "/azud-state/"+proxy.CaddyConfigFileName) {
		t.Fatalf("proxy unit does not restore persisted Caddy file: %q", unit.Exec)
	}
	if len(unit.After) != 0 || len(unit.Requires) != 0 {
		t.Fatalf("rootless proxy references system-manager targets: after=%v requires=%v", unit.After, unit.Requires)
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
