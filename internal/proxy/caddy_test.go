package proxy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/ssh"
)

func TestCaddyAdminCommandUsesPrivateSocket(t *testing.T) {
	tests := []struct {
		name      string
		podmanCmd string
		method    string
		path      string
		withBody  bool
		want      string
	}{
		{
			name:      "read without body",
			podmanCmd: "podman",
			method:    "GET",
			path:      "/config/",
			want:      "podman exec azud-proxy curl -sSf --max-time 30 --unix-socket /run/azud-admin/admin.sock -X GET http://localhost/config/",
		},
		{
			name:      "load streams the body on stdin",
			podmanCmd: "podman",
			method:    "POST",
			path:      "/load",
			withBody:  true,
			want:      "podman exec -i azud-proxy curl -sSf --max-time 30 --unix-socket /run/azud-admin/admin.sock -X POST -H 'Content-Type: application/json' --data-binary @- http://localhost/load",
		},
		{
			name:      "rootful proxy with non-root SSH user",
			podmanCmd: "sudo -n podman",
			method:    "DELETE",
			path:      "/id/azud-route-shop",
			want:      "sudo -n podman exec azud-proxy curl -sSf --max-time 30 --unix-socket /run/azud-admin/admin.sock -X DELETE http://localhost/id/azud-route-shop",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewCaddyClient(nil, tt.podmanCmd, nil)
			got, err := client.adminCommand(tt.method, tt.path, tt.withBody)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("command:\n got %s\nwant %s", got, tt.want)
			}
			if strings.Contains(got, "2019") {
				t.Fatalf("command still targets the TCP admin port: %s", got)
			}
		})
	}
}

func TestCaddyAdminCommandRejectsInvalidMethodAndPath(t *testing.T) {
	client := NewCaddyClient(nil, "", nil)
	for _, tt := range []struct{ method, path string }{
		{method: "HEAD", path: "/config/"},
		{method: "GET; id", path: "/config/"},
		{method: "GET", path: "config/"},
		{method: "GET", path: ""},
		{method: "GET", path: "/config/ -o /etc/passwd"},
		{method: "GET", path: "/config/\nid"},
		{method: "GET", path: "/config/\x00"},
	} {
		if cmd, err := client.adminCommand(tt.method, tt.path, false); err == nil {
			t.Fatalf("%s %q was accepted: %s", tt.method, tt.path, cmd)
		}
	}
}

func TestCaddyAdminPathQuotingKeepsPathsAsOneArgument(t *testing.T) {
	cmd, err := NewCaddyClient(nil, "podman", nil).adminCommand("GET", "/id/a'$(id)`x`", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cmd, ` 'http://localhost/id/a'\''$(id)`+"`x`'") {
		t.Fatalf("admin path is not shell quoted: %s", cmd)
	}
}

func TestCaddyAdminRequestsCannotAddressAdminConfig(t *testing.T) {
	client := NewCaddyClient(nil, "podman", nil)
	for _, path := range []string{"/config/admin", "/config/admin/listen", "/config/admin/remote"} {
		if _, err := client.apiRequest("web-1", "PATCH", path, "0.0.0.0:2019"); err == nil || !strings.Contains(err.Error(), "refusing Caddy admin request") {
			t.Fatalf("%s: err = %v", path, err)
		}
	}
	for _, path := range []string{"/config/administrators", "/config/apps/http"} {
		if isCaddyAdminPath(path) {
			t.Fatalf("%s was treated as the admin config", path)
		}
	}
}

func TestReplacesWholeCaddyConfigSelectsLoadAndRootConfig(t *testing.T) {
	for path, want := range map[string]bool{
		"/load":                      true,
		"/config":                    true,
		"/config/":                   true,
		"/config/apps/http/servers":  false,
		"/id/azud-proxy-shop":        false,
		"/reverse_proxy/upstreams":   false,
		"/config/apps/http/servers/": false,
	} {
		if got := replacesWholeCaddyConfig(path); got != want {
			t.Fatalf("replacesWholeCaddyConfig(%q) = %t, want %t", path, got, want)
		}
	}
}

func TestNormalizeAdminConfigAddsMissingAdminAndRejectsNonObjects(t *testing.T) {
	normalized, dropped, err := normalizeAdminConfig([]byte(`{"apps":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
	object, err := decodeJSONObject(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(object["admin"], map[string]interface{}{"listen": CaddyAdminListen}) {
		t.Fatalf("admin = %#v", object["admin"])
	}
	for _, invalid := range []string{`[]`, `"x"`, `null`, `{`} {
		if _, _, err := normalizeAdminConfig([]byte(invalid)); err == nil {
			t.Fatalf("%s was accepted", invalid)
		}
	}
}

func TestAdminRequestErrorExplainsLegacyAndStoppedProxies(t *testing.T) {
	tests := []struct {
		name   string
		result *ssh.Result
		want   string
	}{
		{
			name:   "legacy proxy without socket",
			result: &ssh.Result{ExitCode: 7, Stderr: "curl: (7) Failed to connect to localhost over /run/azud-admin/admin.sock after 0 ms: Could not connect to server"},
			want:   "run 'azud proxy reboot'",
		},
		{
			name:   "moved listener leaves the socket unanswered",
			result: &ssh.Result{ExitCode: 28, Stderr: "curl: (28) Operation timed out after 30000 milliseconds with 0 bytes received"},
			want:   "run 'azud proxy reboot'",
		},
		{
			name:   "stopped proxy",
			result: &ssh.Result{ExitCode: 255, Stderr: "Error: can only create exec sessions on running containers: container state improper"},
			want:   "proxy container azud-proxy is not running",
		},
		{
			name:   "missing proxy",
			result: &ssh.Result{ExitCode: 125, Stderr: `Error: no container with name or ID "azud-proxy" found: no such container`},
			want:   "proxy container azud-proxy is not running",
		},
		{
			name:   "rejected request",
			result: &ssh.Result{ExitCode: 22, Stderr: "curl: (22) The requested URL returned error: 400"},
			want:   "API request failed: curl: (22)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := adminRequestError(tt.result); !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
