package cli

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestRemoteRuntimeSecretsExcludesInfrastructureCredentials(t *testing.T) {
	loaded := map[string]string{
		"DATABASE_URL":                "postgres://example",
		"OPTIONAL_APP_TOKEN":          "optional",
		"REGISTRY_TOKEN":              "registry",
		"SSL_CERTIFICATE_PEM":         "certificate\nwith\nnewlines",
		"SSL_PRIVATE_KEY_PEM":         "private\nkey",
		"UNREFERENCED_PROVIDER_VALUE": "preserved-for-backward-compatibility",
	}
	cfg := &config.Config{
		Registry: config.RegistryConfig{Password: []string{"REGISTRY_TOKEN"}},
		Proxy: config.ProxyConfig{
			SSLCertificate: "SSL_CERTIFICATE_PEM",
			SSLPrivateKey:  "SSL_PRIVATE_KEY_PEM",
		},
		Env: config.EnvConfig{Secret: []string{"DATABASE_URL"}},
	}

	got := remoteRuntimeSecrets(cfg, loaded)
	want := map[string]string{
		"DATABASE_URL":                "postgres://example",
		"OPTIONAL_APP_TOKEN":          "optional",
		"UNREFERENCED_PROVIDER_VALUE": "preserved-for-backward-compatibility",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("runtime secrets = %#v, want %#v", got, want)
	}
	if len(loaded) != 6 {
		t.Fatal("filter mutated the provider secret map")
	}
}

func TestRemoteRuntimeSecretsNeverCopiesInfrastructureCredentials(t *testing.T) {
	loaded := map[string]string{
		"SHARED_CERTIFICATE": "certificate",
		"REGISTRY_TOKEN":     "registry",
	}
	cfg := &config.Config{
		Registry: config.RegistryConfig{Password: []string{"REGISTRY_TOKEN"}},
		Proxy:    config.ProxyConfig{SSLCertificate: "SHARED_CERTIFICATE"},
		Env: config.EnvConfig{
			Secret: []string{"SHARED_CERTIFICATE", "REGISTRY_TOKEN"},
		},
	}

	if got := remoteRuntimeSecrets(cfg, loaded); len(got) != 0 {
		t.Fatalf("infrastructure credentials reached runtime secrets: %#v", got)
	}
}

func TestRemoteRuntimeSecretsWithNilConfigReturnsCopy(t *testing.T) {
	loaded := map[string]string{"DATABASE_URL": "postgres://example"}
	got := remoteRuntimeSecrets(nil, loaded)
	got["DATABASE_URL"] = "changed"
	if loaded["DATABASE_URL"] != "postgres://example" {
		t.Fatal("nil-config fallback returned the provider map by reference")
	}
}

func TestSecretTargetHosts(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.RoleConfig{
			"web": {Hosts: []string{"web-1", "shared-builder"}},
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
		{name: "default targets workload hosts", host: "", want: []string{"web-1", "shared-builder", "db-1", "cron-1"}},
		{name: "server host", host: "web-1", want: []string{"web-1"}},
		{name: "accessory host", host: "db-1", want: []string{"db-1"}},
		{name: "cron host", host: "cron-1", want: []string{"cron-1"}},
		{name: "builder that also runs the app", host: "shared-builder", want: []string{"shared-builder"}},
		{name: "remote builder", host: "builder-1", wantErr: "does not run application containers"},
		{name: "ssh bastion", host: "bastion-1", wantErr: "does not run application containers"},
		{name: "unknown host", host: "elsewhere", wantErr: "is not configured"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := secretTargetHosts(cfg, tt.host)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("secretTargetHosts(%q) error = %v, want %q", tt.host, err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("secretTargetHosts(%q) returned hosts %v with an error", tt.host, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("secretTargetHosts(%q) error = %v", tt.host, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("secretTargetHosts(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}
