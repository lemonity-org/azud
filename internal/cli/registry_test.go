package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestRegistryTargetHosts(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.RoleConfig{
			"web": {Hosts: []string{"web-1", "web-2"}},
		},
		Accessories: map[string]config.AccessoryConfig{
			"db": {Host: "db-1"},
		},
		Cron: map[string]config.CronConfig{
			"report": {Host: "cron-1"},
		},
		Builder: config.BuilderConfig{Remote: config.RemoteBuilderConfig{Host: "builder-1"}},
		SSH:     config.SSHConfig{Proxy: config.SSHProxyConfig{Host: "bastion-1"}},
	}

	tests := []struct {
		name    string
		host    string
		want    []string
		wantErr string
	}{
		{name: "default keeps server hosts", host: "", want: []string{"web-1", "web-2"}},
		{name: "server host", host: "web-2", want: []string{"web-2"}},
		{name: "accessory host", host: "db-1", want: []string{"db-1"}},
		{name: "cron host", host: "cron-1", want: []string{"cron-1"}},
		{name: "remote builder", host: "builder-1", want: []string{"builder-1"}},
		{name: "ssh bastion", host: "bastion-1", wantErr: "SSH bastion"},
		{name: "unknown host", host: "203.0.113.99", wantErr: "is not configured"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := registryTargetHosts(cfg, tt.host)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("registryTargetHosts(%q) error = %v, want %q", tt.host, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("registryTargetHosts(%q) error = %v", tt.host, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("registryTargetHosts(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestRegistryTargetHostsRequiresConfiguredServers(t *testing.T) {
	if _, err := registryTargetHosts(&config.Config{}, ""); err == nil || !strings.Contains(err.Error(), "no hosts configured") {
		t.Fatalf("expected no hosts configured error, got %v", err)
	}
}
