package deploy

import (
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestNewProxyConfigFromCfgIncludesCustomCertificates(t *testing.T) {
	certificate := "-----BEGIN CERTIFICATE-----\ncert-pem\n-----END CERTIFICATE-----\n"
	privateKey := "-----BEGIN PRIVATE KEY-----\nkey-pem\n-----END PRIVATE KEY-----\n"
	config.SetLoadedSecrets(map[string]string{
		"CUSTOM_CERT": certificate,
		"CUSTOM_KEY":  privateKey,
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
	if got.SSLCertificate != certificate {
		t.Fatalf("SSLCertificate = %q, want %q", got.SSLCertificate, certificate)
	}
	if got.SSLPrivateKey != privateKey {
		t.Fatalf("SSLPrivateKey = %q, want %q", got.SSLPrivateKey, privateKey)
	}
	if len(got.Hosts) != 1 || got.Hosts[0] != "app.example.com" {
		t.Fatalf("Hosts = %v, want [app.example.com]", got.Hosts)
	}
}
