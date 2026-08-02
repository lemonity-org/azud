package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/quadlet"
)

func TestResolveSystemdImagePreservesHistoryReferenceSemantics(t *testing.T) {
	previousCfg := cfg
	t.Cleanup(func() { cfg = previousCfg })

	digest := "sha256:" + strings.Repeat("d", 64)
	for _, tt := range []struct {
		name           string
		configured     string
		historyVersion string
		want           string
	}{
		{name: "tag history", configured: "ghcr.io/acme/test", historyVersion: "v1.2.3", want: "ghcr.io/acme/test:v1.2.3"},
		{name: "digest history", configured: "ghcr.io/acme/test", historyVersion: digest, want: "ghcr.io/acme/test@" + digest},
		{name: "registry port and digest history", configured: "localhost:5000/acme/test", historyVersion: digest, want: "localhost:5000/acme/test@" + digest},
		{name: "configured tag wins", configured: "ghcr.io/acme/test:stable", historyVersion: digest, want: "ghcr.io/acme/test:stable"},
		{name: "configured digest wins", configured: "ghcr.io/acme/test@" + digest, historyVersion: "v1.2.3", want: "ghcr.io/acme/test@" + digest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AZUD_STATE_DIR", t.TempDir())
			cfg = &config.Config{
				Service: "test-app",
				Image:   tt.configured,
				Deploy:  config.DeployConfig{RetainHistory: 20},
			}

			history := deploy.NewDurableHistoryStore(20, output.DefaultLogger)
			record := deploy.NewDeploymentRecord(cfg.Service, tt.configured, tt.historyVersion, "production", []string{"host"})
			record.Complete()
			if err := history.Record(record); err != nil {
				t.Fatalf("record deployment history: %v", err)
			}

			got := resolveSystemdImage(output.DefaultLogger)
			if got != tt.want {
				t.Fatalf("resolveSystemdImage() = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, ":sha256:") {
				t.Fatalf("resolved malformed digest-as-tag reference %q", got)
			}
		})
	}
}

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

	want := []string{
		"8080:80",
		"8443:443",
	}
	if !reflect.DeepEqual(unit.PublishPort, want) {
		t.Fatalf("unexpected published ports: want %v got %v", want, unit.PublishPort)
	}
	for _, port := range unit.PublishPort {
		if strings.Contains(port, "2019") {
			t.Fatalf("proxy unit publishes the Caddy admin port: %v", unit.PublishPort)
		}
	}
	if !strings.Contains(unit.Exec, proxy.CaddyRecoveryConfigFile) || strings.Contains(unit.Exec, "--watch") {
		t.Fatalf("proxy unit does not use the protected recovery config: %q", unit.Exec)
	}
	if unit.Entrypoint != proxy.CaddyRuntimeEntrypoint {
		t.Fatalf("proxy unit entrypoint = %q, want %q", unit.Entrypoint, proxy.CaddyRuntimeEntrypoint)
	}
	if unit.Environment["CADDY_ADMIN"] != proxy.CaddyAdminListen {
		t.Fatalf("proxy admin listener = %q", unit.Environment["CADDY_ADMIN"])
	}
	if unit.User != proxy.CaddyRuntimeUser || unit.Group != proxy.CaddyRuntimeGroup || !unit.ReadOnly || unit.ReadOnlyTmpfs == nil || *unit.ReadOnlyTmpfs {
		t.Fatalf("proxy user/read-only hardening is incomplete: %#v", unit)
	}
	if !reflect.DeepEqual(unit.DropCapability, []string{"ALL"}) || !reflect.DeepEqual(unit.AddCapability, []string{"NET_BIND_SERVICE"}) || !unit.NoNewPrivileges {
		t.Fatalf("proxy capability hardening is incomplete: drop=%v add=%v nnp=%t", unit.DropCapability, unit.AddCapability, unit.NoNewPrivileges)
	}
	if unit.Memory != proxy.CaddyMemoryLimit || unit.PidsLimit != proxy.CaddyPidsLimit || unit.ShmSize != proxy.CaddyShmSize {
		t.Fatalf("proxy resource limits are incomplete: memory=%s pids=%d shm=%s", unit.Memory, unit.PidsLimit, unit.ShmSize)
	}
	for _, volume := range unit.Volume {
		if strings.Contains(volume, "azud-state") {
			t.Fatalf("non-root proxy still mounts host state: %v", unit.Volume)
		}
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
