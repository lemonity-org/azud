package deploy

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/proxy"
)

type fakeDeploymentProxy struct {
	routeHosts []string
}

func (f *fakeDeploymentProxy) Boot(string, *proxy.ProxyConfig) error { return nil }
func (f *fakeDeploymentProxy) EnsureConfig(string) error             { return nil }

func (f *fakeDeploymentProxy) EnsureServiceHosts(_ string, service *proxy.ServiceConfig) error {
	f.routeHosts = append([]string(nil), service.Hosts...)
	return nil
}

func (f *fakeDeploymentProxy) RegisterService(_ string, service *proxy.ServiceConfig) error {
	f.routeHosts = append([]string(nil), service.Hosts...)
	return nil
}

func (f *fakeDeploymentProxy) AddUpstream(string, string, string) error          { return nil }
func (f *fakeDeploymentProxy) RemoveUpstream(string, string, string) error       { return nil }
func (f *fakeDeploymentProxy) DrainUpstream(string, string, time.Duration) error { return nil }

func TestExistingServiceDeployRefreshesProxyHosts(t *testing.T) {
	cfg := roleTestConfig()
	cfg.Proxy.Host = "pacebeats.com"
	cfg.Proxy.Hosts = []string{"mcp.pacebeats.com"}
	cfg.Proxy.Rootful = false
	cfg.Podman.Rootless = false
	cfg.Deploy.DrainTimeout = 0

	containers := newFakeContainerLifecycle("shop")
	proxyManager := &fakeDeploymentProxy{routeHosts: []string{"pacebeats.com"}}
	deployer := &Deployer{
		cfg:        cfg,
		containers: containers,
		proxy:      proxyManager,
		hooks:      NewHookRunner(t.TempDir(), 0, output.DefaultLogger),
		log:        output.DefaultLogger,
	}

	err := deployer.deployToTargetLocked(
		context.Background(),
		deploymentTarget{Host: "host", Role: "web"},
		cfg.Image,
		"latest",
		&DeployOptions{SkipHealthCheck: true},
	)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"pacebeats.com", "mcp.pacebeats.com"}
	if !slices.Equal(proxyManager.routeHosts, want) {
		t.Fatalf("Caddy route hosts after redeploy = %v, want %v", proxyManager.routeHosts, want)
	}
}
