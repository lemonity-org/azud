package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/proxy"
)

func TestProxyDNSLookupHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "exact DNS host", host: "app.example.com", want: "app.example.com"},
		{name: "IP address", host: "192.0.2.1", want: "192.0.2.1"},
		{
			name: "wildcard DNS host",
			host: "*.hooks.example.com",
			want: "azudwildcardpreflight.hooks.example.com",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := proxyDNSLookupHost(test.host); got != test.want {
				t.Fatalf("lookup host = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProxyDNSLookupHostStaysWithinDNSLengthLimit(t *testing.T) {
	suffix := strings.Repeat("a", 63) + "." +
		strings.Repeat("b", 63) + "." +
		strings.Repeat("c", 63) + "." +
		strings.Repeat("d", 59)
	host := "*." + suffix
	if len(host) != 253 {
		t.Fatalf("test wildcard length = %d, want 253", len(host))
	}

	lookupHost := proxyDNSLookupHost(host)
	if len(lookupHost) > 253 {
		t.Fatalf("lookup host length = %d, exceeds 253", len(lookupHost))
	}
	if strings.Contains(lookupHost, "*") || !strings.HasSuffix(lookupHost, "."+suffix) {
		t.Fatalf("lookup host = %q, want a concrete child of %q", lookupHost, suffix)
	}
}

func TestPreflightProxyStatusBlocksOutdatedSystemdManagedProxy(t *testing.T) {
	imperative := fmt.Errorf("%w: proxy on web-1 uses an outdated container definition", proxy.ErrOutdatedProxy)
	if got := preflightProxyStatus(nil, imperative); got != proxyOutdatedStatus {
		t.Fatalf("imperative outdated proxy status = %q, want %q", got, proxyOutdatedStatus)
	}
	// Boot never recreates a Quadlet-managed proxy, so it must not land in
	// runPreflight's non-blocking warnings.
	managed := fmt.Errorf("%w: proxy on web-1 is managed by systemd unit azud-proxy.service", proxy.ErrOutdatedManagedProxy)
	got := preflightProxyStatus(nil, managed)
	for _, warning := range []string{"ok", "n/a", "warn", "unknown", proxyOutdatedStatus} {
		if got == warning {
			t.Fatalf("systemd-managed outdated proxy status = %q, want a blocking status", got)
		}
	}
	if got := preflightProxyStatus(&proxy.ProxyStatus{Running: true}, nil); got != "ok" {
		t.Fatalf("running proxy status = %q, want ok", got)
	}
	if got := preflightProxyStatus(nil, errors.New("ssh failed")); got != "down" {
		t.Fatalf("failed proxy check status = %q, want down", got)
	}
}
