package quadlet

import (
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
