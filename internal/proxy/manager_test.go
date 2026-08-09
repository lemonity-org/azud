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

func TestAdminListenMatchesNetworkMode(t *testing.T) {
	tests := []struct {
		name      string
		hostPorts bool
		want      string
	}{
		{name: "bridge", hostPorts: false, want: caddyAdminBridgeListen},
		{name: "host network", hostPorts: true, want: caddyAdminHostListen},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{hostPorts: tt.hostPorts}
			if got := manager.adminListen(); got != tt.want {
				t.Fatalf("admin listen = %q, want %q", got, tt.want)
			}
			if got := manager.buildBaseConfig().Admin.Listen; got != tt.want {
				t.Fatalf("base config admin listen = %q, want %q", got, tt.want)
			}
		})
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

func TestBuildServiceRoutePreservesWildcardHosts(t *testing.T) {
	route := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name:      "hooks",
		Host:      "hooklistener.com",
		Hosts:     []string{"*.hooklistener.com", "*.hook.events"},
		Upstreams: []string{"hooks:4000"},
	})

	if len(route.Match) != 1 {
		t.Fatalf("route matchers = %d, want 1", len(route.Match))
	}
	want := []string{"hooklistener.com", "*.hooklistener.com", "*.hook.events"}
	if !slices.Equal(route.Match[0].Host, want) {
		t.Fatalf("route hosts = %v, want %v", route.Match[0].Host, want)
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

func TestReconcileRawRouteWithoutDesiredUpstreamsRemainsStaleForDeletion(t *testing.T) {
	desired := (&Manager{}).buildServiceRoute(&ServiceConfig{Name: "retired", Host: "retired.example.com"})
	data, err := json.Marshal([]*Route{desired})
	if err != nil {
		t.Fatal(err)
	}
	rawRoutes, routes, err := parseRouteIdentities(data)
	if err != nil {
		t.Fatal(err)
	}
	status, owned, _ := reconcileRawRouteStatus(rawRoutes, routes, desired, desired.Match[0].Host[0], false)
	if status != ReconcileStale || owned != 0 {
		t.Fatalf("zero-upstream owned route = (%s, %d), want (stale, 0)", status, owned)
	}
}

func TestRouteAppendPayloadCreatesMissingArrayAndAppendsToExistingArray(t *testing.T) {
	var rawRoutes []json.RawMessage
	var routes []*Route
	var err error
	rawRoutes, routes, err = parseRouteIdentities([]byte("null"))
	if err != nil || rawRoutes != nil || routes != nil {
		t.Fatalf("null routes lost missing-array state: raw=%#v routes=%#v err=%v", rawRoutes, routes, err)
	}

	route := &Route{ID: "azud-route-shop"}
	missing, ok := routeAppendPayload(routes, route).([]*Route)
	if !ok || len(missing) != 1 || missing[0] != route {
		t.Fatalf("missing routes payload = %#v", missing)
	}
	existing := make([]*Route, 0)
	if got := routeAppendPayload(existing, route); got != route {
		t.Fatalf("existing routes payload = %#v, want route", got)
	}
}

func TestStringFieldMutationUsesPresenceForCaddyVerb(t *testing.T) {
	tests := []struct {
		name    string
		actual  string
		desired string
		present bool
		method  string
		mutate  bool
	}{
		{name: "present null is replaced", desired: "-1s", present: true, method: "PATCH", mutate: true},
		{name: "absent field is created", desired: "-1s", method: "PUT", mutate: true},
		{name: "present field is removed", present: true, method: "DELETE", mutate: true},
		{name: "equal present field is unchanged", actual: "-1s", desired: "-1s", present: true},
		{name: "equal zero value but present null is removed", present: true, method: "DELETE", mutate: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method, _, mutate := planStringFieldMutation(tt.actual, tt.desired, tt.present)
			if method != tt.method || mutate != tt.mutate {
				t.Fatalf("plan = (%q, %t), want (%q, %t)", method, mutate, tt.method, tt.mutate)
			}
		})
	}
}

func TestCommandUsesResumeForDirectAndQuadletCommands(t *testing.T) {
	for _, command := range [][]string{
		{"caddy", "run", "--resume"},
		{"/bin/sh", "-c", "if [ -s /azud-state/caddy-config.json ]; then exec caddy run --config /azud-state/caddy-config.json --resume; else exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile --resume; fi"},
	} {
		if !commandUsesResume(command) {
			t.Fatalf("resume was not detected in %#v", command)
		}
	}
	if commandUsesResume([]string{"caddy", "run", "--watch"}) {
		t.Fatal("watch command was mistaken for resume")
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

func TestProxyHostsOverlapMatchesCaddySemantics(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
		want   bool
	}{
		{name: "same exact host", first: "api.example.com", second: "API.EXAMPLE.COM", want: true},
		{name: "wildcard and direct child", first: "*.example.com", second: "api.example.com", want: true},
		{name: "direct child and wildcard", first: "api.example.com", second: "*.example.com", want: true},
		{name: "same wildcard", first: "*.example.com", second: "*.EXAMPLE.COM", want: true},
		{name: "wildcard excludes apex", first: "*.example.com", second: "example.com", want: false},
		{name: "wildcard covers one label only", first: "*.example.com", second: "v1.api.example.com", want: false},
		{name: "nested wildcards do not overlap", first: "*.example.com", second: "*.api.example.com", want: false},
		{name: "different wildcard suffix", first: "*.example.com", second: "*.example.net", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := proxyHostsOverlap(test.first, test.second); got != test.want {
				t.Fatalf("overlap(%q, %q) = %t, want %t", test.first, test.second, got, test.want)
			}
		})
	}
}

func TestEnsureNoForeignHostOwnerRejectsWildcardOverlap(t *testing.T) {
	desired := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name: "hooks", Host: "hooks.example.com", Hosts: []string{"*.example.com"}, Upstreams: []string{"hooks:4000"},
	})

	for name, foreignHost := range map[string]string{
		"foreign exact child": "tenant.example.com",
		"foreign wildcard":    "*.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			foreign := &Route{ID: "other-route", Match: []*Match{{Host: []string{foreignHost}}}}
			if err := ensureNoForeignHostOwner([]*Route{foreign}, desired); err == nil {
				t.Fatalf("foreign host %q overlap was not detected", foreignHost)
			}
		})
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
	if err := ensureNoForeignHostOwner([]*Route{foreign}, desired); err == nil {
		t.Fatal("overlapping ID-less alias route was not rejected")
	}
	foreign.Match[0].Host = []string{"shop.example.com"}
	if err := ensureNoForeignHostOwner([]*Route{foreign}, desired); err != nil {
		t.Fatalf("ID-less route with the exact primary host should remain adoptable: %v", err)
	}
}

func TestPlanServiceHostPatchRefreshesAliasesWithoutReplacingRoute(t *testing.T) {
	manager := &Manager{}
	existing := manager.buildServiceRoute(&ServiceConfig{
		Name: "shop", Host: "pacebeats.com", Upstreams: []string{"shop:3000"},
	})
	desired := manager.buildServiceRoute(&ServiceConfig{
		Name: "shop", Host: "pacebeats.com", Hosts: []string{"mcp.pacebeats.com"},
	})

	path, matches, err := planServiceHostPatch([]*Route{existing}, desired, "pacebeats.com")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/id/azud-route-shop/match" {
		t.Fatalf("patch path = %q, want /id/azud-route-shop/match", path)
	}
	want := []*Match{{Host: []string{"pacebeats.com", "mcp.pacebeats.com"}}}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("patched matchers = %#v, want %#v", matches, want)
	}
	handler, _, ok := reverseProxyHandler(existing)
	if !ok || len(handler.Upstreams) != 1 || handler.Upstreams[0].Dial != "shop:3000" {
		t.Fatalf("existing upstreams changed while planning host patch: %#v", handler)
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

func TestBuildServiceRouteIncludesStreamingOptions(t *testing.T) {
	route := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name:             "hooks",
		Host:             "hook.events",
		Upstreams:        []string{"hooks:4000"},
		FlushInterval:    "-1s",
		StreamCloseDelay: "5m",
	})
	handler, _, ok := reverseProxyHandler(route)
	if !ok {
		t.Fatal("generated route is missing reverse_proxy handler")
	}
	if handler.FlushInterval != "-1s" || handler.StreamCloseDelay != "5m" {
		t.Fatalf("streaming options were not propagated: %#v", handler)
	}
}

func TestApplyProxySettingsIncludesServerTransportContract(t *testing.T) {
	manager := &Manager{}
	cfg := manager.buildBaseConfig()
	manager.applyProxySettingsFrom(cfg, &ProxyConfig{
		MaxHeaderBytes:   2 * 1024 * 1024,
		EnableFullDuplex: true,
	})

	server := cfg.Apps.HTTP.Servers["srv0"]
	if server.MaxHeaderBytes != 2*1024*1024 || !server.EnableFullDuplex {
		t.Fatalf("server transport contract was not applied: %#v", server)
	}

	manager.applyProxySettingsFrom(cfg, &ProxyConfig{})
	if server.MaxHeaderBytes != 0 || server.EnableFullDuplex {
		t.Fatalf("server transport contract was not cleared: %#v", server)
	}
}

func TestCustomTLSDisablesAutomaticCertificateIssuance(t *testing.T) {
	manager := &Manager{}
	cfg := manager.buildBaseConfig()
	manager.applyProxySettingsFrom(cfg, &ProxyConfig{
		AutoHTTPS:         true,
		SSLRedirect:       true,
		SSLCertificate:    "certificate-pem",
		SSLPrivateKey:     "private-key-pem",
		CustomCertificate: true,
	})
	server := cfg.Apps.HTTP.Servers["srv0"]
	if server.AutoHTTPS == nil || !server.AutoHTTPS.DisableCertificates {
		t.Fatalf("custom TLS permits automatic issuance: %#v", server.AutoHTTPS)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"disable_certificates":true`) {
		t.Fatalf("custom TLS JSON does not fail closed: %s", encoded)
	}
	if strings.Contains(string(encoded), `"issuers"`) {
		t.Fatalf("custom TLS JSON contains misleading issuer policy: %s", encoded)
	}
}

func TestValidateProxyConfigFailsClosedForCustomTLS(t *testing.T) {
	for _, cfg := range []*ProxyConfig{
		{CustomCertificate: true},
		{CustomCertificate: true, SSLCertificate: "not a certificate", SSLPrivateKey: "not a key"},
	} {
		if err := validateProxyConfig(cfg); err == nil {
			t.Fatalf("invalid custom TLS config was accepted: %#v", cfg)
		}
	}
}

func TestCustomTLSValidationPrecedesRebootAndEnsureConfigMutations(t *testing.T) {
	invalid := &ProxyConfig{CustomCertificate: true}
	manager := &Manager{proxyConfig: invalid}

	if err := manager.Reboot("host", invalid); err == nil {
		t.Fatal("reboot accepted missing custom TLS material")
	}
	if err := manager.EnsureConfig("host"); err == nil {
		t.Fatal("EnsureConfig accepted missing custom TLS material")
	}
}

func TestRawConfigDecodePreservesLargeUnmodeledIntegers(t *testing.T) {
	object, err := decodeJSONObject([]byte(`{"unknown_counter":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"unknown_counter":9007199254740993}` {
		t.Fatalf("large unmodeled integer changed: %s", encoded)
	}
}

func TestRouteIdentityParsingIgnoresUnmodeledSiblingHandlers(t *testing.T) {
	data := []byte(`[
		{"handle":[{"handler":"static_response","status_code":"{http.error.status_code}"}]},
		{"@id":"azud-route-hooks","match":[{"host":["hook.events"]}],"handle":[
			{"handler":"encode","encodings":{"gzip":{}}},
			{"@id":"azud-proxy-hooks","handler":"reverse_proxy","upstreams":[{"dial":"hooks:4000"}]}
		]}
	]`)
	rawRoutes, routes, err := parseRouteIdentities(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[1].ID != "azud-route-hooks" {
		t.Fatalf("route identities = %#v", routes)
	}
	handler, index, err := parseReverseProxyIdentity(rawRoutes[1])
	if err != nil {
		t.Fatal(err)
	}
	if index != 1 || handler.ID != "azud-proxy-hooks" || handler.Upstreams[0].Dial != "hooks:4000" {
		t.Fatalf("reverse proxy identity = %#v at %d", handler, index)
	}
}

func TestMergeOwnedRouteRepairsManagedStateAndPreservesArbitraryJSON(t *testing.T) {
	desired := (&Manager{}).buildServiceRoute(&ServiceConfig{
		Name: "hooks", Host: "hook.events", Upstreams: []string{"hooks:4000"},
		HealthPath: "/up", MaxRequestBody: 2048, FlushInterval: "-1", StreamCloseDelay: "5m",
	})
	raw := json.RawMessage(`{
		"match":[{"host":["old.example"]}],"terminal":false,"manual_route_field":9007199254740993,
		"handle":[
			{"handler":"encode","encodings":{"gzip":{}}},
			{"handler":"reverse_proxy","manual_handler_field":{"keep":true},
			 "upstreams":[{"dial":"hooks:4000","max_requests":17}],
			 "health_checks":{"active":{"path":"/old","manual_health_field":"keep"}}}
		]
	}`)
	merged, err := mergeOwnedRoute(raw, desired)
	if err != nil {
		t.Fatal(err)
	}
	if merged["@id"] != desired.ID || merged["terminal"] != true {
		t.Fatalf("route ownership/state was not repaired: %#v", merged)
	}
	if merged["manual_route_field"] != json.Number("9007199254740993") {
		t.Fatalf("unmodeled large integer changed: %#v", merged["manual_route_field"])
	}
	handlers, err := jsonObjectSlice(merged["handle"])
	if err != nil {
		t.Fatal(err)
	}
	if findJSONHandler(handlers, "encode") == nil || findJSONHandler(handlers, "request_body") == nil {
		t.Fatalf("sibling or desired body handler missing: %#v", handlers)
	}
	reverse := findJSONHandler(handlers, "reverse_proxy")
	if reverse["manual_handler_field"] == nil {
		t.Fatalf("unmodeled reverse_proxy field was removed: %#v", reverse)
	}
	upstreams, err := jsonObjectSlice(reverse["upstreams"])
	if err != nil {
		t.Fatal(err)
	}
	if upstreams[0]["max_requests"] != json.Number("17") {
		t.Fatalf("unmodeled upstream field was removed: %#v", upstreams)
	}
	health := reverse["health_checks"].(map[string]interface{})["active"].(map[string]interface{})
	if health["path"] != "/up" || health["manual_health_field"] != "keep" {
		t.Fatalf("health check was not repaired/preserved: %#v", health)
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	rawRoutes, routes, err := parseRouteIdentities([]byte("[" + string(encoded) + "]"))
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ := reconcileRawRouteStatus(rawRoutes, routes, desired, "hook.events", true)
	if status != ReconcileInSync {
		t.Fatalf("merged route status = %s, want in-sync", status)
	}
}

func TestCopyManagedProxySettingsPreservesUnknownRoutesAndServerFields(t *testing.T) {
	current := map[string]interface{}{
		"admin": map[string]interface{}{"listen": "0.0.0.0:2019", "unknown": "keep"},
		"apps": map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"srv0": map[string]interface{}{
						"listen":    []interface{}{":80"},
						"protocols": []interface{}{"h1", "h2"},
						"routes": []interface{}{map[string]interface{}{
							"handle": []interface{}{map[string]interface{}{
								"handler": "encode", "encodings": map[string]interface{}{"gzip": map[string]interface{}{}},
							}},
						}},
					},
				},
			},
		},
	}
	desired := map[string]interface{}{
		"apps": map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"srv0": map[string]interface{}{
						"listen":             []interface{}{":443"},
						"max_header_bytes":   float64(2 * 1024 * 1024),
						"enable_full_duplex": true,
					},
				},
			},
			"tls": map[string]interface{}{"automation": map[string]interface{}{}},
		},
	}

	beforeRoutes := current["apps"].(map[string]interface{})["http"].(map[string]interface{})["servers"].(map[string]interface{})["srv0"].(map[string]interface{})["routes"]
	if err := copyManagedProxySettings(current, desired); err != nil {
		t.Fatal(err)
	}
	server := current["apps"].(map[string]interface{})["http"].(map[string]interface{})["servers"].(map[string]interface{})["srv0"].(map[string]interface{})
	if !reflect.DeepEqual(server["routes"], beforeRoutes) || !reflect.DeepEqual(server["protocols"], []interface{}{"h1", "h2"}) {
		t.Fatalf("unmanaged server state changed: %#v", server)
	}
	if server["max_header_bytes"] != float64(2*1024*1024) || server["enable_full_duplex"] != true {
		t.Fatalf("managed server state was not applied: %#v", server)
	}
	if current["admin"].(map[string]interface{})["unknown"] != "keep" {
		t.Fatalf("unmanaged top-level state changed: %#v", current)
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
				!strings.Contains(restore, "chmod 600 "+tt.file) {
				t.Fatalf("restore command does not repair modes: %q", restore)
			}
		})
	}
}
