package cli

import (
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestSetupProxyEnabledRequiresWebRole(t *testing.T) {
	workerOnly := &config.Config{Servers: map[string]config.RoleConfig{
		"worker": {Hosts: []string{"worker.example.com"}},
	}}
	if setupProxyEnabled(workerOnly, false) {
		t.Fatal("worker-only setup attempted to enable the proxy")
	}

	web := &config.Config{Servers: map[string]config.RoleConfig{
		"web": {Hosts: []string{"web.example.com"}},
	}}
	if !setupProxyEnabled(web, false) {
		t.Fatal("web setup did not enable the proxy")
	}
	if setupProxyEnabled(web, true) {
		t.Fatal("--skip-proxy did not disable proxy setup")
	}
}
