package quadlet

import (
	"testing"
)

func TestNewQuadletDeployer_DefaultPath(t *testing.T) {
	deployer := NewQuadletDeployer(nil, nil, "")

	if deployer.path != "/etc/containers/systemd/" {
		t.Errorf("expected default path /etc/containers/systemd/, got %s", deployer.path)
	}
}

func TestNewQuadletDeployer_CustomPath(t *testing.T) {
	deployer := NewQuadletDeployer(nil, nil, "/custom/path/")

	if deployer.path != "/custom/path/" {
		t.Errorf("expected custom path /custom/path/, got %s", deployer.path)
	}
}

func TestNewQuadletDeployer_DefaultLogger(t *testing.T) {
	deployer := NewQuadletDeployer(nil, nil, "")

	if deployer.log == nil {
		t.Error("expected default logger to be set")
	}
}
