package deploy

import (
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestNewProxyConfigFromCfgIncludesCustomCertificates(t *testing.T) {
	config.SetLoadedSecrets(map[string]string{
		"CUSTOM_CERT": "cert-pem",
		"CUSTOM_KEY":  "key-pem",
	})
	t.Cleanup(func() {
		config.SetLoadedSecrets(nil)
	})

	cfg := &config.Config{
		Proxy: config.ProxyConfig{
			Host:           "app.example.com",
			SSL:            true,
			SSLCertificate: "CUSTOM_CERT",
			SSLPrivateKey:  "CUSTOM_KEY",
		},
	}

	got := newProxyConfigFromCfg(cfg)
	if got.SSLCertificate != "cert-pem" {
		t.Fatalf("SSLCertificate = %q, want cert-pem", got.SSLCertificate)
	}
	if got.SSLPrivateKey != "key-pem" {
		t.Fatalf("SSLPrivateKey = %q, want key-pem", got.SSLPrivateKey)
	}
	if len(got.Hosts) != 1 || got.Hosts[0] != "app.example.com" {
		t.Fatalf("Hosts = %v, want [app.example.com]", got.Hosts)
	}
}
