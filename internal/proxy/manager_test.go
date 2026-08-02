package proxy

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAdminListenIsContainerLoopbackInEveryNetworkMode(t *testing.T) {
	for _, hostPorts := range []bool{false, true} {
		manager := &Manager{hostPorts: hostPorts}
		if got := manager.buildBaseConfig().Admin.Listen; got != CaddyAdminListen {
			t.Fatalf("base config admin listen = %q, want %q", got, CaddyAdminListen)
		}
	}
}

func TestManagerTracksExpectedProxyHostPorts(t *testing.T) {
	manager := NewManagerWithOptions(nil, nil, "deploy", false, false, 8080, 8443)
	if manager.httpPort != 8080 || manager.httpsPort != 8443 {
		t.Fatalf("constructor ports = %d/%d", manager.httpPort, manager.httpsPort)
	}
	manager.SetProxyConfig(&ProxyConfig{HTTPPort: 9080, HTTPSPort: 9443})
	if manager.httpPort != 9080 || manager.httpsPort != 9443 {
		t.Fatalf("configured ports = %d/%d", manager.httpPort, manager.httpsPort)
	}
	manager.SetProxyConfig(&ProxyConfig{})
	if manager.httpPort != CaddyHTTPPort || manager.httpsPort != CaddyHTTPSPort {
		t.Fatalf("defaulted ports = %d/%d", manager.httpPort, manager.httpsPort)
	}
}

func TestReverseProxyHandlerSkipsRequestBodyHandler(t *testing.T) {
	requestBody := &Handler{Handler: "request_body", MaxSize: 1024}
	reverseProxy := &Handler{Handler: "reverse_proxy", Upstreams: []*Upstream{{Dial: "app:3000"}}}
	route := &Route{Handle: []*Handler{requestBody, reverseProxy}}

	got, index, ok := reverseProxyHandler(route)
	if !ok || got != reverseProxy || index != 1 {
		t.Fatalf("handler=%p index=%d ok=%v", got, index, ok)
	}
	if route.Handle[0] != requestBody {
		t.Fatal("request_body handler was not preserved")
	}
}

func TestBuildServiceRouteAssignsStableAzudIDs(t *testing.T) {
	manager := &Manager{}
	route := manager.buildServiceRoute(&ServiceConfig{
		Name:      "shop-api",
		Host:      "shop.example.com",
		Upstreams: []string{"shop-api:3000"},
	})

	if route.ID != "azud-route-shop-api" {
		t.Fatalf("route ID = %q", route.ID)
	}
	handler, _, ok := reverseProxyHandler(route)
	if !ok || handler.ID != "azud-proxy-shop-api" {
		t.Fatalf("reverse proxy handler ID = %q, ok = %t", handler.ID, ok)
	}
	if got := routeAPIPath("/config/routes", 4, route); got != "/id/azud-route-shop-api" {
		t.Fatalf("route API path = %q", got)
	}
	if got := handlerAPIPath("/config/routes", 4, 1, handler); got != "/id/azud-proxy-shop-api" {
		t.Fatalf("handler API path = %q", got)
	}
}

func TestBuildServiceRouteRetriesWhileNoUpstreamIsAvailable(t *testing.T) {
	// A container swap can leave the route without an available upstream for a
	// moment, and passive health checking can extend that to a full
	// fail_duration. Without a try window Caddy answers 503 immediately.
	route := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name:       "shop",
		Host:       "shop.example.com",
		Upstreams:  []string{"shop:3000"},
		HealthPath: "/up",
	})

	handler, _, ok := reverseProxyHandler(route)
	if !ok {
		t.Fatal("reverse proxy handler missing")
	}
	if handler.LoadBalancing.TryDuration == "" || handler.LoadBalancing.TryInterval == "" {
		t.Fatalf("route does not retry unavailable upstreams: %#v", handler.LoadBalancing)
	}

	tryDuration, err := time.ParseDuration(handler.LoadBalancing.TryDuration)
	if err != nil {
		t.Fatalf("try_duration is not a valid duration: %v", err)
	}
	failDuration, err := time.ParseDuration(handler.HealthChecks.Passive.FailDuration)
	if err != nil {
		t.Fatalf("fail_duration is not a valid duration: %v", err)
	}
	// Long enough to absorb a swap gap, short enough that a real outage fails
	// instead of queueing requests for the whole fail_duration.
	if tryDuration < time.Second || tryDuration >= failDuration {
		t.Fatalf("try_duration %s must be at least 1s and shorter than fail_duration %s", tryDuration, failDuration)
	}

	encoded, err := json.Marshal(handler.LoadBalancing)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"try_duration":`, `"try_interval":`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("load balancing JSON missing %s: %s", want, encoded)
		}
	}
}

func TestBuildServiceRouteUsesReducedWeightedUpstreams(t *testing.T) {
	route := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name: "shop", Host: "shop.example.com",
		UpstreamWeights: []UpstreamWeight{{Dial: "stable:3000", Weight: 80}, {Dial: "canary:3000", Weight: 20}},
	})
	handler, _, ok := reverseProxyHandler(route)
	if !ok || handler.LoadBalancing.SelectionPolicy.Policy != "random" {
		t.Fatalf("weighted policy = %#v", handler.LoadBalancing)
	}
	want := []*Upstream{{Dial: "stable:3000"}, {Dial: "stable:3000"}, {Dial: "stable:3000"}, {Dial: "stable:3000"}, {Dial: "canary:3000"}}
	if !reflect.DeepEqual(handler.Upstreams, want) {
		t.Fatalf("weighted upstreams = %#v, want %#v", handler.Upstreams, want)
	}
}

func TestReconcileRouteStatusOnlyOwnsStableIDOrLegacyHost(t *testing.T) {
	desired := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name: "shop", Host: "shop.example.com", Upstreams: []string{"shop:3000"},
	})
	tests := []struct {
		name       string
		routes     []*Route
		hasDesired bool
		want       ReconcileStatus
	}{
		{name: "missing", hasDesired: true, want: ReconcileMissing},
		{name: "no desired route", routes: []*Route{{ID: "other"}}, want: ReconcileInSync},
		{name: "owned stale with no upstream", routes: []*Route{desired}, want: ReconcileStale},
		{name: "owned in sync", routes: []*Route{desired}, hasDesired: true, want: ReconcileInSync},
		{name: "legacy adoption", routes: []*Route{{Match: []*Match{{Host: []string{"shop.example.com"}}}}}, hasDesired: true, want: ReconcileLegacy},
		{name: "other ID is not adopted", routes: []*Route{{ID: "manual", Match: []*Match{{Host: []string{"shop.example.com"}}}}}, hasDesired: true, want: ReconcileMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _ := reconcileRouteStatus(tt.routes, desired, "shop.example.com", tt.hasDesired)
			if got != tt.want {
				t.Fatalf("status = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestRouteAppendPayloadCreatesMissingArrayAndAppendsToExistingArray(t *testing.T) {
	route := &Route{ID: "azud-route-shop"}
	missing, ok := routeAppendPayload(nil, route).([]*Route)
	if !ok || len(missing) != 1 || missing[0] != route {
		t.Fatalf("missing routes payload = %#v", missing)
	}
	existing := make([]*Route, 0)
	if got := routeAppendPayload(existing, route); got != route {
		t.Fatalf("existing routes payload = %#v, want route", got)
	}
}

func TestRoutesEquivalentIgnoresUpstreamOrderAndCompletedCanaryPolicy(t *testing.T) {
	desired := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name: "shop", Host: "shop.example.com", Upstreams: []string{"shop:3000", "shop-2:3000", "shop-10:3000"},
	})
	actual := cloneRoute(desired)
	handler, _, _ := reverseProxyHandler(actual)
	handler.Upstreams[1], handler.Upstreams[2] = handler.Upstreams[2], handler.Upstreams[1]
	handler.LoadBalancing.SelectionPolicy.Policy = "random"
	if !routesEquivalent(actual, desired) {
		t.Fatal("uniform random route with reordered upstreams should be equivalent to desired round-robin route")
	}
	handler.Upstreams = append(handler.Upstreams, &Upstream{Dial: "shop-10:3000"})
	if routesEquivalent(actual, desired) {
		t.Fatal("non-uniform upstream multiplicity should remain drift")
	}
}

func TestEnsureNoForeignHostOwnerChecksAllAliases(t *testing.T) {
	desired := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name: "shop", Host: "shop.example.com", Hosts: []string{"www.example.com"}, Upstreams: []string{"shop:3000"},
	})
	foreign := &Route{ID: "manual-route", Match: []*Match{{Host: []string{"www.example.com"}}}}
	if err := ensureNoForeignHostOwner([]*Route{foreign}, desired); err == nil {
		t.Fatal("foreign owner of an alias was not detected")
	}
	foreign.ID = ""
	if err := ensureNoForeignHostOwner([]*Route{foreign}, desired); err != nil {
		t.Fatalf("ID-less legacy route should remain adoptable: %v", err)
	}
}

func TestBuildServiceRouteConfiguresUpstreamProtocol(t *testing.T) {
	tests := []struct {
		protocol string
		wantH2C  bool
		wantTLS  bool
	}{
		{protocol: "http"},
		{protocol: "h2c", wantH2C: true},
		{protocol: "https", wantTLS: true},
	}

	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			route := (&Manager{}).buildServiceRoute(&ServiceConfig{
				Name:             "shop",
				Host:             "shop.example.com",
				Upstreams:        []string{"shop:3000"},
				UpstreamProtocol: tt.protocol,
			})
			handler, _, ok := reverseProxyHandler(route)
			if !ok || handler.Transport == nil || handler.Transport.Protocol != "http" {
				t.Fatalf("transport = %#v, handler found = %t", handler.Transport, ok)
			}
			gotH2C := reflect.DeepEqual(handler.Transport.Versions, []string{"h2c", "2"})
			if gotH2C != tt.wantH2C || (handler.Transport.TLS != nil) != tt.wantTLS {
				t.Fatalf("transport = %#v", handler.Transport)
			}
		})
	}
}

func TestAddUpstreamIfMissingIsIdempotent(t *testing.T) {
	upstreams := []*Upstream{{Dial: "app:3000"}}
	got := addUpstreamIfMissing(upstreams, "app:3000")
	if !reflect.DeepEqual(got, upstreams) || len(got) != 1 {
		t.Fatalf("duplicate add changed upstreams: %#v", got)
	}
	got = addUpstreamIfMissing(got, "app-2:3000")
	if len(got) != 2 || got[1].Dial != "app-2:3000" {
		t.Fatalf("new upstream was not added: %#v", got)
	}
}

func TestWeightedUpstreamsUseStockCaddySchema(t *testing.T) {
	upstreams := weightedUpstreams(
		UpstreamWeight{Dial: "stable:3000", Weight: 90},
		UpstreamWeight{Dial: "canary:3000", Weight: 10},
	)
	if len(upstreams) != 10 {
		t.Fatalf("weighted upstream count = %d, want reduced ratio of 10", len(upstreams))
	}
	weights := extractWeights(upstreams)
	if len(weights) != 2 || weights[0] != (UpstreamWeight{Dial: "stable:3000", Weight: 90}) || weights[1] != (UpstreamWeight{Dial: "canary:3000", Weight: 10}) {
		t.Fatalf("extracted weights = %#v", weights)
	}

	payload, err := json.Marshal(&Handler{Handler: "reverse_proxy", Upstreams: upstreams})
	if err != nil {
		t.Fatalf("marshal handler: %v", err)
	}
	if strings.Contains(string(payload), "weight") || strings.Contains(string(payload), "weighted_round_robin") {
		t.Fatalf("stock Caddy payload contains unsupported weighted fields: %s", payload)
	}
}

func TestReverseProxyHeadersUseStockCaddySchema(t *testing.T) {
	route := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Host:      "app.example.com",
		Upstreams: []string{"app:3000"},
		HTTPS:     true,
	})
	handler, _, ok := reverseProxyHandler(route)
	if !ok {
		t.Fatal("generated route is missing reverse_proxy handler")
	}

	payload, err := json.Marshal(handler)
	if err != nil {
		t.Fatalf("marshal handler: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("unmarshal handler fields: %v", err)
	}
	if _, exists := fields["header_up"]; exists {
		t.Fatalf("payload contains Caddyfile-only header_up field: %s", payload)
	}

	rawHeaders, exists := fields["headers"]
	if !exists {
		t.Fatalf("payload is missing stock Caddy headers field: %s", payload)
	}

	var got HeadersConfig
	if err := json.Unmarshal(rawHeaders, &got); err != nil {
		t.Fatalf("unmarshal stock Caddy headers: %v", err)
	}
	if got.Request == nil {
		t.Fatal("headers.request is nil")
	}
	wantSet := map[string][]string{
		"X-Forwarded-For":   {"{http.request.remote.host}"},
		"X-Forwarded-Proto": {"{http.request.scheme}"},
		"X-Forwarded-Host":  {"{http.request.host}"},
		"X-Forwarded-Port":  {"{http.request.port}"},
		"X-Real-IP":         {"{http.request.remote.host}"},
	}
	if !maps.EqualFunc(got.Request.Set, wantSet, slices.Equal) {
		t.Fatalf("headers.request.set = %#v, want %#v", got.Request.Set, wantSet)
	}
}

func TestApplyProxySettingsClearsDisabledManagedState(t *testing.T) {
	manager := &Manager{}
	cfg := manager.buildBaseConfig()
	cfg.Logging = &LoggingConfig{Logs: map[string]*Log{"access": {Level: "INFO"}}}
	cfg.Apps.HTTP.Servers["srv0"].Logs = &ServerLogs{DefaultLoggerName: "access"}
	cfg.Apps.HTTP.Servers["srv0"].AutoHTTPS = &AutoHTTPSConfig{DisableRedirects: true}
	cfg.Apps.TLS = &TLSApp{Certificates: &CertificatesConfig{LoadPEM: []LoadedCertificate{{Certificate: "old-cert", Key: "old-key"}}}}

	manager.applyProxySettingsFrom(cfg, &ProxyConfig{AutoHTTPS: false, LoggingEnabled: false})
	server := cfg.Apps.HTTP.Servers["srv0"]
	if server.AutoHTTPS == nil || !server.AutoHTTPS.Disable {
		t.Fatalf("automatic HTTPS was not explicitly disabled: %#v", server.AutoHTTPS)
	}
	if server.Logs != nil || cfg.Logging != nil {
		t.Fatalf("logging state was not cleared: server=%#v global=%#v", server.Logs, cfg.Logging)
	}
	if cfg.Apps.TLS != nil {
		t.Fatalf("stale TLS state was not cleared: %#v", cfg.Apps.TLS)
	}

	manager.applyProxySettingsFrom(cfg, &ProxyConfig{AutoHTTPS: true, SSLRedirect: true})
	if server.AutoHTTPS != nil {
		t.Fatalf("default HTTPS redirects should clear overrides: %#v", server.AutoHTTPS)
	}
}

func TestApplyProxySettingsUsesProtocolCorrectListeners(t *testing.T) {
	tests := []struct {
		name   string
		config *ProxyConfig
		want   []string
	}{
		{
			name:   "HTTP only never listens with plaintext on the TLS port",
			config: &ProxyConfig{},
			want:   []string{":80"},
		},
		{
			name: "automatic HTTPS owns a dedicated redirect listener",
			config: &ProxyConfig{
				AutoHTTPS:   true,
				SSLRedirect: true,
			},
			want: []string{":443"},
		},
		{
			name: "redirect disabled intentionally serves both protocols",
			config: &ProxyConfig{
				AutoHTTPS: true,
			},
			want: []string{":80", ":443"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{}
			cfg := manager.buildBaseConfig()
			manager.applyProxySettingsFrom(cfg, tt.config)

			got := cfg.Apps.HTTP.Servers["srv0"].Listen
			if !slices.Equal(got, tt.want) {
				t.Fatalf("listeners = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPersistConfigCommandsProtectPrivateCaddyState(t *testing.T) {
	tests := []struct {
		name string
		user string
		dir  string
		file string
	}{
		{name: "root", user: "root", dir: "/var/lib/azud", file: "/var/lib/azud/caddy-config.json"},
		{name: "rootless", user: "deploy", dir: `"${HOME}/.local/share/azud"`, file: `"${HOME}/.local/share/azud/caddy-config.json"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			persist := persistConfigCommand(tt.user)
			for _, want := range []string{
				"umask 077",
				"mkdir -p " + tt.dir,
				"chmod 700 " + tt.dir,
				"chmod 600 " + tt.file,
				"mv ",
			} {
				if !strings.Contains(persist, want) {
					t.Fatalf("persist command %q missing %q", persist, want)
				}
			}

			restore := restoreConfigCommand(tt.user)
			if !strings.Contains(restore, "chmod 700 "+tt.dir) ||
				!strings.Contains(restore, "chmod 600 "+tt.file) ||
				!strings.Contains(restore, "exit 44") {
				t.Fatalf("restore command does not repair modes: %q", restore)
			}
		})
	}
}

func TestValidateQuadletStateRejectsMissingOrStoppedProxy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		exists  bool
		running bool
		want    string
	}{
		{name: "missing", want: "does not exist"},
		{name: "stopped", exists: true, want: "stopped container"},
		{name: "running", exists: true, running: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateQuadletState(tt.exists, tt.running)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("running proxy was rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestValidateQuadletHandoffStateRequiresSecureRunningProxy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		exists  bool
		running bool
		drift   []string
		want    string
	}{
		{name: "missing", want: "does not exist"},
		{name: "stopped", exists: true, want: "stopped container"},
		{name: "drifted", exists: true, running: true, drift: []string{"admin port is published"}, want: "drifted proxy runtime"},
		{name: "secure and running", exists: true, running: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateQuadletHandoffState(tt.exists, tt.running, tt.drift)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("secure running proxy was rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
