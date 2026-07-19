package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestSelectedAccessoryHostsMakesMultiHostBehaviorExplicit(t *testing.T) {
	previousHost := accessoryHost
	accessoryHost = ""
	t.Cleanup(func() { accessoryHost = previousHost })
	accessory := config.AccessoryConfig{Host: "one", Hosts: []string{"one", "two"}}

	hosts, err := selectedAccessoryHosts("redis", accessory, false)
	if err != nil || !reflect.DeepEqual(hosts, []string{"one", "two"}) {
		t.Fatalf("lifecycle hosts = (%v, %v)", hosts, err)
	}
	if _, err := selectedAccessoryHosts("redis", accessory, true); err == nil || !strings.Contains(err.Error(), "select one with --host") {
		t.Fatalf("expected ambiguous logs/exec error, got %v", err)
	}

	accessoryHost = "two"
	hosts, err = selectedAccessoryHosts("redis", accessory, true)
	if err != nil || !reflect.DeepEqual(hosts, []string{"two"}) {
		t.Fatalf("selected host = (%v, %v)", hosts, err)
	}
	accessoryHost = "other"
	if _, err := selectedAccessoryHosts("redis", accessory, false); err == nil {
		t.Fatal("expected unconfigured accessory host to fail")
	}
}
