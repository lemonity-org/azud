package quadlet

import (
	"strings"
	"testing"
)

func TestNewQuadletDeployer_DefaultPath(t *testing.T) {
	deployer := NewQuadletDeployer(nil, nil, "", false)

	if deployer.path != "/etc/containers/systemd/" {
		t.Errorf("expected default path /etc/containers/systemd/, got %s", deployer.path)
	}
	if deployer.sudo {
		t.Error("expected sudo to be disabled by default")
	}
}

func TestNewQuadletDeployer_CustomPath(t *testing.T) {
	deployer := NewQuadletDeployer(nil, nil, "/custom/path/", false)

	if deployer.path != "/custom/path/" {
		t.Errorf("expected custom path /custom/path/, got %s", deployer.path)
	}
}

func TestNewQuadletDeployer_DefaultLogger(t *testing.T) {
	deployer := NewQuadletDeployer(nil, nil, "", false)

	if deployer.log == nil {
		t.Error("expected default logger to be set")
	}
}

func TestNewQuadletDeployerWithOptions_Sudo(t *testing.T) {
	deployer := NewQuadletDeployerWithOptions(nil, nil, "", false, true)
	if !deployer.sudo {
		t.Error("expected sudo to be enabled")
	}
	if got := deployer.systemctlCmd("daemon-reload"); got != "sudo -n systemctl daemon-reload" {
		t.Fatalf("unexpected systemctl command: %s", got)
	}
}

func TestNewQuadletDeployerWithOptions_UserModeDisablesSudo(t *testing.T) {
	deployer := NewQuadletDeployerWithOptions(nil, nil, "~/.config/containers/systemd/", true, true)
	if deployer.sudo {
		t.Error("expected sudo to be disabled in user mode")
	}
	if got := deployer.systemctlCmd("daemon-reload"); got != "systemctl --user daemon-reload" {
		t.Fatalf("unexpected systemctl command: %s", got)
	}
}

func TestQuoteRemotePathExpandsRootlessHomeSafely(t *testing.T) {
	for _, input := range []string{
		"~/.config/containers/systemd/",
		"$HOME/.config/containers/systemd/",
		"${HOME}/.config/containers/systemd/",
	} {
		if got, want := quoteRemotePath(input), `"${HOME}"/.config/containers/systemd/`; got != want {
			t.Fatalf("quoteRemotePath(%q) = %q, want %q", input, got, want)
		}
	}

	if got, want := quoteRemotePath("/etc/containers/systemd/"), "/etc/containers/systemd/"; got != want {
		t.Fatalf("absolute path = %q, want %q", got, want)
	}
}

func TestValidateGeneratedUnitStateRequiresLoadedQuadletOutput(t *testing.T) {
	for _, tt := range []struct {
		name    string
		service string
		state   string
		wantErr string
	}{
		{
			name:    "rootful generator",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=/run/systemd/generator/azud-proxy.service\nWantedBy=default.target\n",
		},
		{
			name:    "rootful generator early",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=/run/systemd/generator.early/azud-proxy.service\nWantedBy=default.target\n",
		},
		{
			name:    "rootless generator late",
			service: "pacebeats.service",
			state:   "FragmentPath=/run/user/1000/systemd/generator.late/pacebeats.service\nWantedBy=default.target\nLoadState=loaded\n",
		},
		{
			name:    "generator rejected unit",
			service: "azud-proxy",
			state:   "LoadState=not-found\nFragmentPath=\n",
			wantErr: "LoadState",
		},
		{
			name:    "missing fragment",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=\n",
			wantErr: "FragmentPath is empty",
		},
		{
			name:    "static unit collision",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=/etc/systemd/system/azud-proxy.service\n",
			wantErr: "not Quadlet generator output",
		},
		{
			name:    "generator substring outside runtime tree",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=/tmp/systemd/generator/azud-proxy.service\nWantedBy=default.target\n",
			wantErr: "not Quadlet generator output",
		},
		{
			name:    "noncanonical generator subdirectory",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=/run/systemd/generator/extra/azud-proxy.service\nWantedBy=default.target\n",
			wantErr: "not Quadlet generator output",
		},
		{
			name:    "rootless generator requires numeric uid",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=/run/user/deploy/systemd/generator/azud-proxy.service\nWantedBy=default.target\n",
			wantErr: "not Quadlet generator output",
		},
		{
			name:    "different generated unit",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=/run/systemd/generator/other.service\n",
			wantErr: "does not identify azud-proxy.service",
		},
		{
			name:    "missing boot activation edge",
			service: "azud-proxy",
			state:   "LoadState=loaded\nFragmentPath=/run/systemd/generator/azud-proxy.service\nWantedBy=\n",
			wantErr: "WantedBy does not include default.target",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGeneratedUnitState(tt.service, "default.target", tt.state)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("valid generated unit rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}

	if err := validateGeneratedUnitState(
		"azud-network",
		"",
		"LoadState=loaded\nFragmentPath=/run/systemd/generator/azud-network.service\nWantedBy=\n",
	); err != nil {
		t.Fatalf("generated dependency-only network unit rejected: %v", err)
	}
}
