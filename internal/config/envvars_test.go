package config

import (
	"maps"
	"slices"
	"testing"
)

func TestIsReservedEnvVar(t *testing.T) {
	tests := []struct {
		name     string
		reserved bool
	}{
		{"AZUD_STATE_DIR", true},
		{"AZUD_SERVICE", true},
		{"AZUD_VERSION", true},
		{"AZUD_RUNTIME", true},
		{"AZUD_INSTALL_DIR", true},
		{"AZUD_SSH_KEY", true},
		{"AZUD_TEST_BIN", true},
		{"AZUD_INTEGRATION", true},
		{"AZUD_INTEGRATION_HOST", true},
		{"AZUD_APP_SECRET", false},
		{"AZUD_REGISTRY_PASSWORD", false},
		{"AZUD_SECRET_AZUD_STATE_DIR", false},
		{"DATABASE_URL", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReservedEnvVar(tt.name); got != tt.reserved {
				t.Errorf("IsReservedEnvVar(%q) = %v, want %v", tt.name, got, tt.reserved)
			}
		})
	}
}

func TestEnvSecretsSkipsReservedAndInfrastructureVariables(t *testing.T) {
	environ := []string{
		"AZUD_SSH_KEY=ssh-key-material",
		"AZUD_STATE_DIR=/var/lib/azud",
		"AZUD_INTEGRATION_HOST=203.0.113.10",
		"AZUD_REGISTRY_PASSWORD=reg-token",
		"AZUD_TLS_KEY=tls-key",
		"AZUD_APP_SECRET=app-secret-value",
		"AZUD_=empty-name",
		"PATH=/usr/bin",
	}
	refs := infrastructureSecretRefs(&Config{
		Registry: RegistryConfig{Password: []string{"AZUD_REGISTRY_PASSWORD"}},
		Proxy:    ProxyConfig{SSLCertificate: "AZUD_TLS_CERT", SSLPrivateKey: "AZUD_TLS_KEY"},
	})

	secrets, skipped := envSecrets(environ, "AZUD_", refs)

	if want := map[string]string{"APP_SECRET": "app-secret-value"}; !maps.Equal(secrets, want) {
		t.Fatalf("secrets = %#v, want %#v", secrets, want)
	}
	wantSkipped := []string{"AZUD_INTEGRATION_HOST", "AZUD_REGISTRY_PASSWORD", "AZUD_SSH_KEY", "AZUD_STATE_DIR", "AZUD_TLS_KEY"}
	if !slices.Equal(skipped, wantSkipped) {
		t.Fatalf("skipped = %v, want %v", skipped, wantSkipped)
	}
}

func TestEnvSecretsDedicatedPrefixUnaffected(t *testing.T) {
	environ := []string{
		"AZUD_SECRET_AZUD_REGISTRY_PASSWORD=reg-token",
		"AZUD_SECRET_DATABASE_URL=postgres://example",
		"AZUD_SECRET_TLS_CERT=tls-cert",
		"AZUD_SSH_KEY=ssh-key-material",
		"AZUD_STATE_DIR=/var/lib/azud",
	}
	refs := infrastructureSecretRefs(&Config{
		Registry: RegistryConfig{Password: []string{"AZUD_REGISTRY_PASSWORD"}},
		Proxy:    ProxyConfig{SSLCertificate: "TLS_CERT"},
	})

	secrets, skipped := envSecrets(environ, "AZUD_SECRET_", refs)

	// The documented AZUD_SECRET_<REF> form keeps working: the registry and
	// TLS references resolve through the loaded secrets, and env push filters
	// them out of the runtime file by their reference names.
	want := map[string]string{
		"AZUD_REGISTRY_PASSWORD": "reg-token",
		"DATABASE_URL":           "postgres://example",
		"TLS_CERT":               "tls-cert",
	}
	if !maps.Equal(secrets, want) {
		t.Fatalf("secrets = %#v, want %#v", secrets, want)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
}

func TestLoadSecretsFromEnvSkipsReservedVariables(t *testing.T) {
	previous := AllSecrets()
	t.Cleanup(func() { SetLoadedSecrets(previous) })
	t.Setenv("AZUD_SSH_KEY", "ssh-key-material")
	t.Setenv("AZUD_REGISTRY_PASSWORD", "reg-token")
	t.Setenv("AZUD_APP_SECRET", "app-secret-value")

	cfg := &Config{
		SecretsProvider:  "env",
		SecretsEnvPrefix: "AZUD_",
		Registry:         RegistryConfig{Password: []string{"AZUD_REGISTRY_PASSWORD"}},
	}
	if err := NewLoader("", "").loadSecretsFromEnv(cfg); err != nil {
		t.Fatalf("loadSecretsFromEnv: %v", err)
	}

	if got, ok := GetSecret("APP_SECRET"); !ok || got != "app-secret-value" {
		t.Fatalf("APP_SECRET = %q, %v; want app-secret-value", got, ok)
	}
	for _, name := range []string{"SSH_KEY", "REGISTRY_PASSWORD"} {
		if _, ok := GetSecret(name); ok {
			t.Errorf("%s was loaded as an application secret", name)
		}
	}
}
