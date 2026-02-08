package cli

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/proxy"
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

	unit := buildAppQuadletUnit("ghcr.io/acme/test:latest")
	want := []string{"127.0.0.1::3000"}
	if !reflect.DeepEqual(unit.PublishPort, want) {
		t.Fatalf("unexpected publish ports: want %v got %v", want, unit.PublishPort)
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
	if !reflect.DeepEqual(unit.Network, []string{"azud"}) {
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
