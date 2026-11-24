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
			Host:             "app.example.com",
			SSL:              true,
			SSLCertificate:   "CUSTOM_CERT",
			SSLPrivateKey:    "CUSTOM_KEY",
			MaxHeaderBytes:   2 * 1024 * 1024,
			EnableFullDuplex: true,
			FlushInterval:    "-1s",
			StreamCloseDelay: "5m",
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
	if !got.CustomCertificate || got.MaxHeaderBytes != 2*1024*1024 || !got.EnableFullDuplex {
		t.Fatalf("proxy-wide settings were not propagated: %#v", got)
	}

	route := BuildProxyServiceConfig(cfg, []string{"app:4000"}, nil)
	if route.FlushInterval != "-1s" || route.StreamCloseDelay != "5m" {
		t.Fatalf("route streaming settings were not propagated: %#v", route)
	}
}

func TestProxyConfigsCarryOnDemandTLS(t *testing.T) {
	cfg := &config.Config{
		Service: "app",
		Proxy: config.ProxyConfig{
			Host:        "app.example.com",
			SSL:         true,
			ACMEEmail:   "ops@example.com",
			AppPort:     4000,
			OnDemandTLS: config.OnDemandTLSConfig{Ask: "/tls/ask"},
		},
	}

	got := newProxyConfigFromCfg(cfg)
	if got.Service != "app" || got.AppPort != 4000 || got.OnDemandAsk != "/tls/ask" {
		t.Fatalf("on-demand settings were not propagated: %#v", got)
	}
	if route := BuildProxyServiceConfig(cfg, []string{"app:4000"}, nil); !route.OnDemand {
		t.Fatalf("route does not ask for an on-demand route: %#v", route)
	}

	cfg.Proxy.OnDemandTLS = config.OnDemandTLSConfig{}
	if got := newProxyConfigFromCfg(cfg); got.OnDemandAsk != "" {
		t.Fatalf("OnDemandAsk = %q without the option", got.OnDemandAsk)
	}
	if route := BuildProxyServiceConfig(cfg, []string{"app:4000"}, nil); route.OnDemand {
		t.Fatal("route asks for an on-demand route without the option")
	}
}
