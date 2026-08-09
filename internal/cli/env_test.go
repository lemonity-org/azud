package cli

import (
	"maps"
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
