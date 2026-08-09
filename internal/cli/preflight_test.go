package cli

import (
	"strings"
	"testing"
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
