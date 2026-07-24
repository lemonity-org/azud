package cli

import (
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func TestRunScaleRejectsStopFirstRole(t *testing.T) {
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = &config.Config{
		Service: "shop",
		Servers: map[string]config.RoleConfig{
			"worker": {
				Hosts:    []string{"worker.example.com"},
				Strategy: "stop_first",
			},
		},
	}

	err := runScale(nil, []string{"worker=2"})
	if err == nil || !strings.Contains(err.Error(), "cannot be scaled") {
		t.Fatalf("expected singleton scale rejection, got %v", err)
	}
}
