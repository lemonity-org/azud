package deploy

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/localtest"
	"github.com/lemonity-org/azud/internal/output"
)

func restoreConfig() *config.Config {
	return &config.Config{
		Service: "shop",
		Image:   "example/shop@sha256:abc",
		Servers: map[string]config.RoleConfig{
			"web":    {Hosts: []string{"host"}},
			"worker": {Hosts: []string{"host"}, Cmd: "work"},
		},
		Proxy:   config.ProxyConfig{Host: "shop.example.com", AppPort: 3000},
		Podman:  config.PodmanConfig{Rootless: false},
		Deploy:  config.DeployConfig{},
		Volumes: nil,
	}
}

func restoreDeployer(containers *fakeContainerLifecycle, proxyManager *fakeDeploymentProxy) *Deployer {
	return &Deployer{
		cfg:        restoreConfig(),
		containers: containers,
		proxy:      proxyManager,
		log:        output.NewLogger(io.Discard, io.Discard, false),
	}
}

func TestRestoreRolesStartsWhatExistsAndLeavesWhatRuns(t *testing.T) {
	containers := newFakeContainerLifecycle("shop-worker")
	containers.exists["shop"] = true
	proxyManager := &fakeDeploymentProxy{}
	d := restoreDeployer(containers, proxyManager)

	if err := d.RestoreRoles(context.Background(), "host", nil, true); err != nil {
		t.Fatalf("RestoreRoles: %v", err)
	}
	if !containers.running["shop"] {
		t.Fatalf("web did not start: %v", containers.operations)
	}
	if slices.Contains(containers.operations, "start shop-worker") {
		t.Fatalf("a running role was started again: %v", containers.operations)
	}
	if proxyManager.booted != 1 {
		t.Fatalf("proxy booted %d times", proxyManager.booted)
	}
	if !slices.Equal(proxyManager.reconciled, []string{"shop:3000"}) {
		t.Fatalf("reconciled = %v; the route must name the web role", proxyManager.reconciled)
	}
}

func TestRestoreRolesLeavesStoppedRolesAndStillBootsTheProxy(t *testing.T) {
	containers := newFakeContainerLifecycle()
	containers.exists["shop"], containers.exists["shop-worker"] = true, true
	proxyManager := &fakeDeploymentProxy{}
	d := restoreDeployer(containers, proxyManager)

	if err := d.RestoreRoles(context.Background(), "host", []string{"web", "worker"}, true); err != nil {
		t.Fatalf("RestoreRoles: %v", err)
	}
	if containers.running["shop"] || containers.running["shop-worker"] {
		t.Fatalf("a stopped role started: %v", containers.operations)
	}
	if proxyManager.booted != 1 {
		t.Fatal("the proxy serves other services, so it boots whoever is stopped here")
	}
	if len(proxyManager.reconciled) != 0 {
		t.Fatalf("reconciled %v; a stopped role's route stays as it was", proxyManager.reconciled)
	}
}

func TestRestoreRolesOnlyStartsWhenNotAllowedToCreate(t *testing.T) {
	containers := newFakeContainerLifecycle()
	d := restoreDeployer(containers, &fakeDeploymentProxy{})
	if err := d.RestoreRoles(context.Background(), "host", nil, false); err != nil {
		t.Fatalf("RestoreRoles: %v", err)
	}
	if len(containers.exists) != 0 {
		t.Fatalf("created %v while a journal is open: %v", containers.exists, containers.operations)
	}
}

func TestRestoreRolesBootsNoProxyForARoleThatServesNoTraffic(t *testing.T) {
	containers := newFakeContainerLifecycle("shop-worker")
	proxyManager := &fakeDeploymentProxy{}
	d := restoreDeployer(containers, proxyManager)
	delete(d.cfg.Servers, "web")
	if err := d.RestoreRoles(context.Background(), "host", nil, true); err != nil {
		t.Fatalf("RestoreRoles: %v", err)
	}
	if proxyManager.booted != 0 {
		t.Fatal("booted a proxy nothing needs")
	}
}

func TestRestoreRolesTriesEveryRole(t *testing.T) {
	containers := newFakeContainerLifecycle()
	containers.exists["shop"], containers.exists["shop-worker"] = true, true
	containers.failBefore["start shop"] = true
	proxyManager := &fakeDeploymentProxy{bootErr: errors.New("proxy is down")}
	d := restoreDeployer(containers, proxyManager)
	delete(containers.failBefore, "start shop")
	containers.failBefore["start shop-worker"] = true

	err := d.RestoreRoles(context.Background(), "host", nil, true)
	if err == nil || !strings.Contains(err.Error(), "proxy is down") || !strings.Contains(err.Error(), "worker role") {
		t.Fatalf("RestoreRoles = %v", err)
	}
	if !containers.running["shop"] {
		t.Fatalf("web did not start after the proxy failed: %v", containers.operations)
	}
}

// restoreOverFake is a real deployer of a worker whose container does not exist.
func restoreOverFake(t *testing.T, answer func(cmd string) (string, int)) (*Deployer, *localtest.Fake) {
	t.Helper()
	return twoRoleDeployer(t, func(cmd string) (string, int) {
		if out, code := answer(cmd); out != "" || code != 0 {
			return out, code
		}
		if strings.Contains(cmd, "container inspect") && !strings.Contains(cmd, "RestartCount") && !strings.Contains(cmd, "-new-") {
			return "no such container", 1
		}
		return "", 0
	})
}

func TestRestoreRolesCreatesAMissingRoleWithoutPullingAnImageItHas(t *testing.T) {
	d, fake := restoreOverFake(t, func(string) (string, int) { return "", 0 })
	if err := d.RestoreRoles(context.Background(), "host", nil, true); err != nil {
		t.Fatalf("RestoreRoles: %v", err)
	}
	if !ranContainer(fake, "alpha") || !ranContainer(fake, "beta") {
		t.Fatalf("commands = %q", fake.Commands())
	}
	for _, cmd := range fake.Commands() {
		if strings.Contains(cmd, "pull") || strings.Contains(cmd, " login") {
			t.Fatalf("reached the registry for an image on this server: %q", cmd)
		}
		if strings.Contains(cmd, "pre-deploy") {
			t.Fatalf("ran a pre-deploy command: %q", cmd)
		}
	}
}

func TestRestoreRolesPullsAMissingImageAndReportsWhenItCannot(t *testing.T) {
	d, fake := restoreOverFake(t, func(cmd string) (string, int) {
		switch {
		case strings.Contains(cmd, "image inspect"):
			return "no such image", 1
		case strings.Contains(cmd, "pull"):
			return "registry unreachable", 1
		}
		return "", 0
	})
	err := d.RestoreRoles(context.Background(), "host", nil, true)
	if err == nil || !strings.Contains(err.Error(), "failed to pull image") {
		t.Fatalf("RestoreRoles = %v", err)
	}
	if ranContainer(fake, "alpha") {
		t.Fatal("started a role with no image")
	}
}

func TestRestoreRolesRefusesToCreateWithoutItsSecrets(t *testing.T) {
	d, fake := restoreOverFake(t, func(cmd string) (string, int) {
		if strings.Contains(cmd, "secrets") {
			return "missing", 1
		}
		return "", 0
	})
	d.cfg.Env.Secret = []string{"DATABASE_URL"}
	d.cfg.SecretsRemotePath = "/srv/secrets"
	err := d.RestoreRoles(context.Background(), "host", nil, true)
	if err == nil {
		t.Fatalf("RestoreRoles succeeded; commands = %q", fake.Commands())
	}
	if ranContainer(fake, "alpha") {
		t.Fatal("started a role without its secrets")
	}
}
