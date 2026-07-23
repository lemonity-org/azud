package cli

import (
	"reflect"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/podman"
)

func TestSelectProxyContainersRequiresExactIdentity(t *testing.T) {
	labels := func(role string) map[string]string {
		return map[string]string{"azud.managed": "true", "azud.service": "shop", "azud.role": role}
	}
	containers := []podman.Container{
		{Name: "shop-web", Labels: labels("web")},
		{Name: "shop-web-2", Labels: map[string]string{"azud.managed": "true", "azud.service": "shop", "azud.role": "web", "azud.instance": "2"}},
		{Name: "shop-web-3", Labels: labels("web")}, // missing instance label
		{Name: "shop-web-deploy-1", Labels: labels("web")},
		{Name: "shop-web-canary", Labels: labels("")},
		{Name: "shopper-web", Labels: map[string]string{"azud.managed": "true", "azud.service": "shopper", "azud.role": "web"}},
	}
	want := []string{"shop-web", "shop-web-2", "shop-web-canary"}
	if got := selectProxyContainers(containers, "shop", "shop-web", "shop-web-canary"); !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
}

func TestGetProxyRouteHostsOnlyTargetsWebRole(t *testing.T) {
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = &config.Config{Servers: map[string]config.RoleConfig{
		"web":    {Hosts: []string{"web-1", "web-1", "shared"}},
		"worker": {Hosts: []string{"worker-1", "shared"}},
	}}
	if got, want := getProxyRouteHosts(""), []string{"web-1", "shared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("web hosts = %v, want %v", got, want)
	}
	if got := getProxyRouteHosts("worker-1"); got != nil {
		t.Fatalf("worker-only host selected: %v", got)
	}
}

func TestUnmanagedCanaryStateRequiresPersistedWeights(t *testing.T) {
	containers := []podman.Container{{
		Name: "shop-canary",
		Labels: map[string]string{
			"azud.managed": "true", "azud.service": "shop", "azud.role": "web", "azud.canary": "true",
		},
	}}
	if got := unmanagedCanaryState(containers, "shop", "shop", nil, "web-1"); got != "shop-canary" {
		t.Fatalf("untracked canary = %q", got)
	}
	state := &deploy.CanaryState{Status: deploy.CanaryStatusRunning, Hosts: []string{"web-1"}, CanaryContainer: "shop-canary"}
	if got := unmanagedCanaryState(containers, "shop", "shop", state, "web-1"); got != "" {
		t.Fatalf("tracked canary rejected: %q", got)
	}
	promoted := []podman.Container{{
		Name: "shop",
		Labels: map[string]string{
			"azud.managed": "true", "azud.service": "shop", "azud.role": "web", "azud.canary": "true",
		},
	}}
	if got := unmanagedCanaryState(promoted, "shop", "shop", nil, "web-1"); got != "" {
		t.Fatalf("promoted stable container rejected: %q", got)
	}
}

func TestValidateCanaryContainersRejectsScaledInstances(t *testing.T) {
	if err := validateCanaryContainers([]string{"shop", "shop-canary"}, "shop", "shop-canary"); err != nil {
		t.Fatalf("valid canary containers rejected: %v", err)
	}
	if err := validateCanaryContainers([]string{"shop"}, "shop", "shop-canary"); err == nil {
		t.Fatal("missing canary container accepted")
	}
	if err := validateCanaryContainers([]string{"shop", "shop-canary", "shop-2"}, "shop", "shop-canary"); err == nil {
		t.Fatal("scaled container during canary accepted")
	}
}
