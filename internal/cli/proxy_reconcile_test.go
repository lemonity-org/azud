package cli

import (
	"reflect"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/podman"
)

func TestSelectProxyContainersRequiresExactIdentity(t *testing.T) {
	labels := func(role string) map[string]string {
		return map[string]string{"azud.managed": "true", "azud.service": "shop", "azud.role": role}
	}
	containers := []podman.Container{
		{Name: "shop-web", Labels: labels("web")},
		{Name: "shop-web-2", Labels: map[string]string{"azud.managed": "true", "azud.service": "shop", "azud.role": "web", "azud.instance": "2"}},
		{Name: "shop-web-deploy-1", Labels: labels("web")},
		{Name: "shop-web-canary", Labels: map[string]string{"azud.managed": "true", "azud.service": "shop", "azud.role": "web", "azud.canary": "true"}},
		{Name: "shopper-web", Labels: map[string]string{"azud.managed": "true", "azud.service": "shopper", "azud.role": "web"}},
	}
	want := []string{"shop-web"}
	if got := selectProxyContainers(containers, "shop", "shop-web"); !reflect.DeepEqual(got, want) {
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
