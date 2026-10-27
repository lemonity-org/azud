package proxy

import (
	"testing"
)

func TestSystemdUnitFromInspect(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "direct podman container",
			raw:  `[{"Config":{"Labels":{"azud.managed":"true","azud.type":"proxy"}}}]`,
		},
		{
			name: "quadlet container",
			raw:  `[{"Config":{"Labels":{"azud.type":"proxy","PODMAN_SYSTEMD_UNIT":"azud-proxy.service"}}}]`,
			want: "azud-proxy.service",
		},
		{
			name: "no labels",
			raw:  `[{"Config":{}}]`,
		},
		{
			name:    "empty result",
			raw:     `[]`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			raw:     ``,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := systemdUnitFromInspect(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got unit %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unit = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSystemctlStopCommandMatchesProxyRuntime(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		rootful bool
		want    string
	}{
		{name: "root", user: "root", want: "systemctl stop azud-proxy.service"},
		{name: "root rootful", user: "root", rootful: true, want: "systemctl stop azud-proxy.service"},
		{name: "rootful via sudo", user: "deploy", rootful: true, want: "sudo systemctl stop azud-proxy.service"},
		{name: "rootless", user: "deploy", want: "systemctl --user stop azud-proxy.service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{user: tt.user, rootful: tt.rootful}
			if got := manager.systemctlStopCommand("azud-proxy.service"); got != tt.want {
				t.Fatalf("systemctl command = %q, want %q", got, tt.want)
			}
		})
	}
}
