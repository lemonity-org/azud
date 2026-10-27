package deploy

import (
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

// Hook scripts can run azud themselves. Every variable Azud sets for hooks
// must be reserved so secrets_provider=env never loads it as a secret.
func TestHookEnvironmentVariablesAreReserved(t *testing.T) {
	ctx := &HookContext{
		Service:     "svc",
		Image:       "img",
		Version:     "v1",
		Hosts:       "h1",
		Destination: "prod",
		Performer:   "me",
		Role:        "web",
		HookName:    "pre-deploy",
		RecordedAt:  "2026-01-01T00:00:00Z",
		Runtime:     "1",
	}

	found := 0
	for _, entry := range ctx.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "AZUD_") {
			continue
		}
		found++
		if !config.IsReservedEnvVar(key) {
			t.Errorf("hook variable %s is not reserved in config.IsReservedEnvVar", key)
		}
	}
	if found != 10 {
		t.Fatalf("expected 10 AZUD_* hook variables, found %d", found)
	}
}
